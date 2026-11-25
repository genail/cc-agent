// Package sot talks to the federation API of a SoT server.
package sot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	requestTimeout   = 15 * time.Second
	maxResponseBytes = 16 << 20
	federationPath   = "/api/federation"
	usersPath        = "/api/users"
	visibleToAll     = "all"
)

// Client is one user's connection to one server.
type Client struct {
	base, token string
	http        *http.Client
}

// New returns a client. It never follows redirects, which could carry the token elsewhere.
func New(base, token string) *Client {
	return &Client{base: base, token: token, http: &http.Client{
		Timeout:       requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Registration makes a session reachable for everyone or for the named users.
type Registration struct {
	SessionID, Name, Machine string
	Everyone                 bool
	VisibleTo                []string
}

// Session is a session the user may reach.
type Session struct {
	Owner     string `json:"owner"`
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Machine   string `json:"machine"`
	Mine      bool   `json:"mine"`
	Online    bool   `json:"online"`
}

// Message is a received message.
type Message struct {
	ID              string `json:"id"`
	FromUser        string `json:"from_user"`
	FromSessionName string `json:"from_session_name"`
	ToSessionID     string `json:"to_session_id"`
	Body            string `json:"body"`
	ReplyTo         string `json:"reply_to"`
}

// Outgoing is a message to a shared session, or a reply.
type Outgoing struct {
	ToUser          string `json:"to_user,omitempty"`
	ToSession       string `json:"to_session,omitempty"`
	ReplyTo         string `json:"reply_to,omitempty"`
	Body            string `json:"body"`
	FromSessionID   string `json:"from_session_id"`
	FromSessionName string `json:"from_session_name"`
}

// Sent is the server's answer to Outgoing.
type Sent struct {
	ID     string `json:"id"`
	Online bool   `json:"online"`
}

// WhoAmI returns the user name the token belongs to.
func (c *Client) WhoAmI(ctx context.Context) (string, error) {
	var out struct {
		Name string `json:"name"`
	}
	err := c.do(ctx, http.MethodGet, federationPath+"/whoami", nil, &out)
	if err == nil && out.Name == "" {
		err = errors.New("the server does not support federation")
	}
	return out.Name, err
}

// Users lists the active users of the server.
func (c *Client) Users(ctx context.Context) ([]string, error) {
	var out struct {
		Users []struct {
			Name string `json:"name"`
		} `json:"users"`
	}
	if err := c.do(ctx, http.MethodGet, usersPath, nil, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Users))
	for _, u := range out.Users {
		names = append(names, u.Name)
	}
	return names, nil
}

// Sessions lists the sessions the user may reach.
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	var out struct {
		Sessions []Session `json:"sessions"`
	}
	err := c.do(ctx, http.MethodGet, federationPath+"/sessions", nil, &out)
	return out.Sessions, err
}

// Register makes a session reachable.
func (c *Client) Register(ctx context.Context, r Registration) error {
	var visible any = r.VisibleTo
	if r.Everyone {
		visible = visibleToAll
	}
	return c.do(ctx, http.MethodPost, federationPath+"/sessions", map[string]any{
		"session_id": r.SessionID, "name": r.Name, "machine": r.Machine, "visible_to": visible,
	}, nil)
}

// Heartbeat refreshes sessions and returns those the server knows.
func (c *Client) Heartbeat(ctx context.Context, sessionIDs []string) ([]string, error) {
	var out struct {
		SessionIDs []string `json:"session_ids"`
	}
	err := c.do(ctx, http.MethodPost, federationPath+"/sessions/heartbeat", map[string]any{"session_ids": sessionIDs}, &out)
	return out.SessionIDs, err
}

// Unregister makes a session unreachable. An unknown session is not an error.
func (c *Client) Unregister(ctx context.Context, sessionID string) error {
	err := c.do(ctx, http.MethodDelete, federationPath+"/sessions/"+url.PathEscape(sessionID), nil, nil)
	var apiErr *Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// Send sends a message.
func (c *Client) Send(ctx context.Context, m Outgoing) (Sent, error) {
	var out Sent
	err := c.do(ctx, http.MethodPost, federationPath+"/messages", m, &out)
	return out, err
}

// Pull returns unacknowledged messages for the given sessions.
func (c *Client) Pull(ctx context.Context, sessionIDs []string) ([]Message, error) {
	var out struct {
		Messages []Message `json:"messages"`
	}
	err := c.do(ctx, http.MethodPost, federationPath+"/inbox/pull", map[string]any{"session_ids": sessionIDs}, &out)
	return out.Messages, err
}

// Ack confirms delivery.
func (c *Client) Ack(ctx context.Context, messageIDs []string) error {
	return c.do(ctx, http.MethodPost, federationPath+"/inbox/ack", map[string]any{"message_ids": messageIDs}, nil)
}

// Error is an error answered by the server.
type Error struct {
	Status int
	Text   string
}

func (e *Error) Error() string { return e.Text }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return serverError(resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unexpected answer from %s: %w", c.base, err)
	}
	return nil
}

func serverError(status int, data []byte) error {
	var answer struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &answer) == nil && answer.Error != "" {
		return &Error{status, answer.Error}
	}
	if status == http.StatusNotFound {
		return errors.New("the server does not support federation (update sot-mcp-server)")
	}
	return fmt.Errorf("server answered HTTP %d", status)
}
