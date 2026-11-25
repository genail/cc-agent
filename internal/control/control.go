// Package control is the API between the cc-agent commands and the daemon. It
// is HTTP on a unix socket that only its owner can open.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/genail/cc-agent/internal/config"
	"github.com/genail/cc-agent/internal/relay"
	"github.com/genail/cc-agent/internal/sot"
)

const (
	socketMode     = 0o600
	requestTimeout = 30 * time.Second
	// Plenty for a message body at the server's limit once JSON-encoded.
	maxRequestBytes = 1 << 20
)

// ErrNotRunning means that no daemon listens on the socket.
var ErrNotRunning = errors.New("the cc-agent daemon is not running; start it with `cc-agent service install` or `cc-agent daemon`")

// Status describes the daemon.
type Status struct {
	Version       string                `json:"version"`
	Servers       []ServerStatus        `json:"servers"`
	Registrations []config.Registration `json:"registrations"`
}

// ServerStatus is a server and its last polling error.
type ServerStatus struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

// ServerRequest names a server; empty means the only one.
type ServerRequest struct {
	Server string `json:"server"`
}

// SendRequest sends a message or a reply from a local session.
type SendRequest struct {
	Server    string `json:"server"`
	SessionID string `json:"session_id"`
	ToUser    string `json:"to_user"`
	ToSession string `json:"to_session"`
	ReplyTo   string `json:"reply_to"`
	Body      string `json:"body"`
}

// Listen binds the control socket, refusing when a daemon already runs.
func Listen(dir config.Dir) (net.Listener, error) {
	if err := dir.Ensure(); err != nil {
		return nil, err
	}
	path := dir.SocketPath()
	if conn, err := net.Dial("unix", path); err == nil {
		conn.Close()
		return nil, errors.New("the cc-agent daemon is already running")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path) // the directory is private, so there is no window before Chmod
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, socketMode); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Handler serves the control API.
func Handler(dir config.Dir, r *relay.Relay, version string) http.Handler {
	mux := http.NewServeMux()
	handle(mux, "/status", func(context.Context, struct{}) (any, error) {
		cfg, err := dir.LoadConfig()
		if err != nil {
			return nil, err
		}
		status := Status{Version: version, Registrations: r.Registrations()}
		for _, s := range cfg.Servers {
			status.Servers = append(status.Servers, ServerStatus{s.Name, s.URL, r.ServerError(s.Name)})
		}
		return status, nil
	})
	handle(mux, "/sessions", func(ctx context.Context, in ServerRequest) (any, error) {
		_, client, err := connect(dir, in.Server)
		if err != nil {
			return nil, err
		}
		return client.Sessions(ctx)
	})
	handle(mux, "/users", func(ctx context.Context, in ServerRequest) (any, error) {
		_, client, err := connect(dir, in.Server)
		if err != nil {
			return nil, err
		}
		return client.Users(ctx)
	})
	handle(mux, "/register", func(ctx context.Context, in config.Registration) (any, error) {
		server, _, err := connect(dir, in.Server)
		if err != nil {
			return nil, err
		}
		in.Server = server.Name
		return in, r.Register(ctx, in)
	})
	handle(mux, "/unregister", func(ctx context.Context, in config.SessionRef) (any, error) {
		server, _, err := connect(dir, in.Server)
		if err != nil {
			return nil, err
		}
		in.Server = server.Name
		return in, r.Unregister(ctx, in)
	})
	handle(mux, "/send", func(ctx context.Context, in SendRequest) (any, error) {
		server, client, err := connect(dir, in.Server)
		if err != nil {
			return nil, err
		}
		session, err := r.Session(in.SessionID)
		if err != nil {
			return nil, err
		}
		sent, err := client.Send(ctx, sot.Outgoing{
			ToUser: in.ToUser, ToSession: in.ToSession, ReplyTo: in.ReplyTo, Body: in.Body,
			FromSessionID: session.ID, FromSessionName: session.Name,
		})
		if err != nil {
			return nil, err
		}
		return sent, r.AddSender(config.SessionRef{Server: server.Name, SessionID: session.ID})
	})
	return mux
}

func connect(dir config.Dir, name string) (config.Server, *sot.Client, error) {
	cfg, err := dir.LoadConfig()
	if err != nil {
		return config.Server{}, nil, err
	}
	server, err := cfg.Select(name)
	if err != nil {
		return config.Server{}, nil, err
	}
	return server, sot.New(server.URL, server.Token), nil
}

type failure struct {
	Error string `json:"error"`
}

func handle[In any](mux *http.ServeMux, path string, f func(context.Context, In) (any, error)) {
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, req *http.Request) {
		var in In
		w.Header().Set("Content-Type", "application/json")
		out, err := any(nil), json.NewDecoder(http.MaxBytesReader(w, req.Body, maxRequestBytes)).Decode(&in)
		if err == nil {
			out, err = f(req.Context(), in)
		}
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			out = failure{err.Error()}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}

// Client calls the daemon.
type Client struct{ http *http.Client }

// NewClient returns a client of the daemon that uses dir.
func NewClient(dir config.Dir) *Client {
	return &Client{&http.Client{Timeout: requestTimeout, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, "unix", dir.SocketPath())
			if err != nil {
				return nil, ErrNotRunning
			}
			return conn, nil
		},
	}}}
}

// Call posts in to path and decodes the answer into out.
func (c *Client) Call(path string, in, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := c.http.Post("http://cc-agent"+path, "application/json", bytes.NewReader(data))
	if errors.Is(err, ErrNotRunning) {
		return ErrNotRunning
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var f failure
		if json.NewDecoder(resp.Body).Decode(&f) != nil || f.Error == "" {
			return fmt.Errorf("daemon answered HTTP %d", resp.StatusCode)
		}
		return errors.New(f.Error)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
