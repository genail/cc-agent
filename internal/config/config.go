// Package config stores the SoT servers and the sessions the user opted in.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	dirMode  = 0o700
	fileMode = 0o600

	homeEnv = "CC_AGENT_HOME"
	// mcpSuffix is the path users know from their MCP configuration.
	mcpSuffix = "/mcp"
)

var serverNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Dir is the directory holding configuration, state and the control socket.
type Dir string

// DefaultDir is $CC_AGENT_HOME, or ~/.config/cc-agent.
func DefaultDir() (Dir, error) {
	if dir := os.Getenv(homeEnv); dir != "" {
		abs, err := filepath.Abs(dir)
		return Dir(abs), err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return Dir(filepath.Join(home, ".config", "cc-agent")), nil
}

func (d Dir) configPath() string { return filepath.Join(string(d), "config.json") }
func (d Dir) statePath() string  { return filepath.Join(string(d), "state.json") }

// SocketPath is the daemon's control socket.
func (d Dir) SocketPath() string { return filepath.Join(string(d), "daemon.sock") }

// LogPath is the daemon's log when run as a service.
func (d Dir) LogPath() string { return filepath.Join(string(d), "daemon.log") }

// Ensure creates the directory, accessible to its owner only.
func (d Dir) Ensure() error {
	if err := os.MkdirAll(string(d), dirMode); err != nil {
		return err
	}
	return os.Chmod(string(d), dirMode)
}

// Server is one SoT server and the user's identity on it.
type Server struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token"`
}

// NewServer validates its arguments. The URL may be the MCP address.
func NewServer(name, rawURL, token string) (Server, error) {
	if !serverNameRe.MatchString(name) {
		return Server{}, fmt.Errorf("server name %q: use lowercase letters, digits and hyphens", name)
	}
	if token == "" {
		return Server{}, errors.New("token is empty")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return Server{}, err
	}
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Server{}, fmt.Errorf("invalid server URL %q", rawURL)
	}
	loopback := slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return Server{}, fmt.Errorf("server URL %q must use https", rawURL)
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), mcpSuffix)
	return Server{Name: name, URL: u.String(), Token: token}, nil
}

// Config lists the configured servers.
type Config struct {
	Servers []Server `json:"servers"`
}

// Select returns the named server, or the only one when name is empty.
func (c Config) Select(name string) (Server, error) {
	if name == "" {
		switch len(c.Servers) {
		case 0:
			return Server{}, errors.New("no server configured; run: cc-agent server add <name> --url <url>")
		case 1:
			return c.Servers[0], nil
		}
		return Server{}, errors.New("several servers configured; choose one with --server")
	}
	for _, s := range c.Servers {
		if s.Name == name {
			return s, nil
		}
	}
	return Server{}, fmt.Errorf("unknown server %q", name)
}

// LoadConfig returns an empty Config when none was saved.
func (d Dir) LoadConfig() (Config, error) {
	var c Config
	return c, load(d.configPath(), &c)
}

// SaveConfig writes the configuration.
func (d Dir) SaveConfig(c Config) error { return d.save(d.configPath(), c) }

// SessionRef identifies a local session on one server.
type SessionRef struct {
	Server    string `json:"server"`
	SessionID string `json:"session_id"`
}

// Registration is a session made visible on one server.
type Registration struct {
	Server    string   `json:"server"`
	SessionID string   `json:"session_id"`
	Everyone  bool     `json:"everyone"`
	VisibleTo []string `json:"visible_to"`
}

// Ref returns the session the registration is about.
func (r Registration) Ref() SessionRef { return SessionRef{r.Server, r.SessionID} }

// State survives daemon restarts. Senders are sessions that sent a message and
// may therefore get replies without being registered.
type State struct {
	Registrations []Registration `json:"registrations"`
	Senders       []SessionRef   `json:"senders"`
}

// LoadState returns an empty State when none was saved.
func (d Dir) LoadState() (State, error) {
	var s State
	return s, load(d.statePath(), &s)
}

// SaveState writes the state.
func (d Dir) SaveState(s State) error { return d.save(d.statePath(), s) }

// Register adds or replaces a registration.
func (s *State) Register(r Registration) {
	s.Unregister(r.Ref())
	s.Registrations = append(s.Registrations, r)
}

// Unregister reports whether there was anything to remove.
func (s *State) Unregister(ref SessionRef) bool {
	before := len(s.Registrations)
	s.Registrations = slices.DeleteFunc(s.Registrations, func(r Registration) bool { return r.Ref() == ref })
	return len(s.Registrations) < before
}

// AddSender adds a session that expects replies.
func (s *State) AddSender(ref SessionRef) {
	if !slices.Contains(s.Senders, ref) {
		s.Senders = append(s.Senders, ref)
	}
}

// Forget drops everything about an ended session.
func (s *State) Forget(sessionID string) {
	s.Registrations = slices.DeleteFunc(s.Registrations, func(r Registration) bool { return r.SessionID == sessionID })
	s.Senders = slices.DeleteFunc(s.Senders, func(r SessionRef) bool { return r.SessionID == sessionID })
}

// SessionIDs lists the sessions that may receive messages from a server.
func (s State) SessionIDs(server string) []string {
	var ids []string
	add := func(ref SessionRef) {
		if ref.Server == server && !slices.Contains(ids, ref.SessionID) {
			ids = append(ids, ref.SessionID)
		}
	}
	for _, r := range s.Registrations {
		add(r.Ref())
	}
	for _, r := range s.Senders {
		add(r)
	}
	return ids
}

func load(path string, into any) error {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is accessible to other users; run: chmod 600 %s", path, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (d Dir) save(path string, v any) error {
	if err := d.Ensure(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(string(d), ".tmp-*") // created with mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
