package relay

import (
	"context"
	"fmt"

	ccsock "github.com/PeterSR/claude-code-socket-transport"
)

// ClaudeCode delivers through Claude Code's cross-session messaging sockets.
type ClaudeCode struct{}

// Live lists the running sessions that have an inbox.
func (ClaudeCode) Live() (map[string]Session, error) {
	found, err := find()
	if err != nil {
		return nil, err
	}
	live := make(map[string]Session, len(found))
	for id, s := range found {
		name := s.Name
		if name == "" {
			name = id
		}
		live[id] = Session{ID: id, Name: name}
	}
	return live, nil
}

// Deliver sends without the session's auth token: an outside message must not
// pass as one of the user's own processes, and a session that bypasses
// permissions then holds it for approval. It carries no reply address, because
// replies go through the cc-agent command.
func (ClaudeCode) Deliver(ctx context.Context, sessionID, fromName, text string) error {
	found, err := find()
	if err != nil {
		return err
	}
	s, ok := found[sessionID]
	if !ok {
		return fmt.Errorf("session %s is not running", sessionID)
	}
	client := &ccsock.Client{NoAuth: true}
	_, err = client.Send(ctx, s, ccsock.Message{Text: text, FromName: fromName, SessionID: sessionID})
	return err
}

func find() (map[string]ccsock.Session, error) {
	sessions, err := ccsock.ListSessions()
	if err != nil {
		return nil, err
	}
	found := map[string]ccsock.Session{}
	for _, s := range sessions {
		if s.SessionID == "" || s.SocketPath == "" || !s.Running() {
			continue
		}
		found[s.SessionID] = s // sorted by start time: the newest process wins
	}
	return found, nil
}
