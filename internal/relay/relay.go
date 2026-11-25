// Package relay moves messages from SoT servers into local Claude Code sessions.
package relay

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/genail/cc-agent/internal/config"
	"github.com/genail/cc-agent/internal/sot"
)

const (
	pollInterval      = 5 * time.Second
	heartbeatInterval = 30 * time.Second
)

var messageIDRe = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

// API is the part of the SoT client the relay polls with.
type API interface {
	Register(context.Context, sot.Registration) error
	Heartbeat(context.Context, []string) ([]string, error)
	Unregister(context.Context, string) error
	Pull(context.Context, []string) ([]sot.Message, error)
	Ack(context.Context, []string) error
}

// Session is a running local session.
type Session struct{ ID, Name string }

// Local gives access to the Claude Code sessions on this machine.
type Local interface {
	Live() (map[string]Session, error)
	Deliver(ctx context.Context, sessionID, fromName, text string) error
}

// Relay holds the opted-in sessions and delivers their messages.
type Relay struct {
	dir     config.Dir
	local   Local
	connect func(config.Server) API
	machine string
	logf    func(string, ...any)

	mu            sync.Mutex
	state         config.State
	unacked       map[string]bool      // delivered messages whose ack failed
	lastHeartbeat map[string]time.Time // by server
	serverErrors  map[string]string    // by server
}

// New loads the saved state.
func New(dir config.Dir, local Local, connect func(config.Server) API, machine string, logf func(string, ...any)) (*Relay, error) {
	state, err := dir.LoadState()
	if err != nil {
		return nil, err
	}
	return &Relay{dir: dir, local: local, connect: connect, machine: machine, logf: logf, state: state,
		unacked: map[string]bool{}, lastHeartbeat: map[string]time.Time{}, serverErrors: map[string]string{}}, nil
}

// Run polls until ctx ends.
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Session returns a running local session.
func (r *Relay) Session(id string) (Session, error) {
	live, err := r.local.Live()
	if err != nil {
		return Session{}, err
	}
	s, ok := live[id]
	if !ok {
		return Session{}, fmt.Errorf("no running Claude Code session %q on this machine", id)
	}
	return s, nil
}

// Register makes a local session reachable.
func (r *Relay) Register(ctx context.Context, reg config.Registration) error {
	server, err := r.server(reg.Server)
	if err != nil {
		return err
	}
	session, err := r.Session(reg.SessionID)
	if err != nil {
		return err
	}
	if err := r.connect(server).Register(ctx, r.registration(reg, session)); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Register(reg)
	return r.dir.SaveState(r.state)
}

// Unregister makes a local session unreachable. Delivery stops even when the server cannot be told.
func (r *Relay) Unregister(ctx context.Context, ref config.SessionRef) error {
	server, err := r.server(ref.Server)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.state.Unregister(ref)
	err = r.dir.SaveState(r.state)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	return r.connect(server).Unregister(ctx, ref.SessionID)
}

// AddSender lets a session receive replies.
func (r *Relay) AddSender(ref config.SessionRef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.AddSender(ref)
	return r.dir.SaveState(r.state)
}

// Registrations lists the reachable sessions.
func (r *Relay) Registrations() []config.Registration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.state.Registrations)
}

// ServerError returns the last polling error of a server, or "".
func (r *Relay) ServerError(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.serverErrors[name]
}

// Tick polls every server once.
func (r *Relay) Tick(ctx context.Context) {
	cfg, err := r.dir.LoadConfig()
	if err != nil {
		r.logf("config: %v", err)
		return
	}
	live, err := r.local.Live()
	if err != nil {
		r.logf("local sessions: %v", err)
		return
	}
	r.forgetEnded(ctx, cfg, live)
	for _, server := range cfg.Servers {
		r.report(server.Name, r.poll(ctx, server, live))
	}
}

func (r *Relay) poll(ctx context.Context, server config.Server, live map[string]Session) error {
	r.mu.Lock()
	ids := r.state.SessionIDs(server.Name)
	r.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	api := r.connect(server)
	return errors.Join(r.heartbeat(ctx, api, server.Name, live), r.deliver(ctx, api, server.Name, ids))
}

// heartbeat keeps sessions online and restores those the server lost.
func (r *Relay) heartbeat(ctx context.Context, api API, server string, live map[string]Session) error {
	r.mu.Lock()
	due := time.Since(r.lastHeartbeat[server]) >= heartbeatInterval
	registered := slices.DeleteFunc(slices.Clone(r.state.Registrations),
		func(reg config.Registration) bool { return reg.Server != server })
	r.mu.Unlock()
	if !due || len(registered) == 0 {
		return nil
	}
	ids := make([]string, 0, len(registered))
	for _, reg := range registered {
		ids = append(ids, reg.SessionID)
	}
	known, err := api.Heartbeat(ctx, ids)
	if err != nil {
		return err
	}
	for _, reg := range registered {
		if slices.Contains(known, reg.SessionID) || !r.stillRegistered(reg.Ref()) {
			continue
		}
		if err := api.Register(ctx, r.registration(reg, live[reg.SessionID])); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.lastHeartbeat[server] = time.Now()
	r.mu.Unlock()
	return nil
}

func (r *Relay) deliver(ctx context.Context, api API, server string, ids []string) error {
	messages, err := api.Pull(ctx, ids)
	if err != nil {
		return err
	}
	var problems []error
	failed := map[string]bool{} // sessions whose later messages must wait, to keep the order
	for _, m := range messages {
		if failed[m.ToSessionID] {
			continue
		}
		if err := r.deliverOne(ctx, api, server, m); err != nil {
			failed[m.ToSessionID] = true
			problems = append(problems, fmt.Errorf("message %q: %w", m.ID, err))
		}
	}
	return errors.Join(problems...)
}

func (r *Relay) deliverOne(ctx context.Context, api API, server string, m sot.Message) error {
	// The id ends up in a command shown to the agent, and the server must not
	// choose which local sessions it reaches.
	if !messageIDRe.MatchString(m.ID) {
		return errors.New("malformed id")
	}
	if !r.mayReceive(server, m.ToSessionID) {
		return errors.New("addressed to a session that did not opt in")
	}
	if !r.unacked[m.ID] {
		fromName := fmt.Sprintf("EXTERNAL %s@%s (cc-agent)", m.FromUser, server)
		if err := r.local.Deliver(ctx, m.ToSessionID, fromName, Format(server, m, rand.Text())); err != nil {
			return err
		}
		r.unacked[m.ID] = true
		r.logf("%s: delivered %s from %s to session %s", server, m.ID, m.FromUser, m.ToSessionID)
	}
	if err := api.Ack(ctx, []string{m.ID}); err != nil {
		return err
	}
	delete(r.unacked, m.ID)
	return nil
}

func (r *Relay) mayReceive(server, sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.state.SessionIDs(server), sessionID)
}

func (r *Relay) forgetEnded(ctx context.Context, cfg config.Config, live map[string]Session) {
	r.mu.Lock()
	var ended []config.Registration
	for _, reg := range r.state.Registrations {
		if _, ok := live[reg.SessionID]; !ok {
			ended = append(ended, reg)
		}
	}
	changed := len(ended) > 0
	for _, reg := range ended {
		r.state.Forget(reg.SessionID)
	}
	for _, sender := range slices.Clone(r.state.Senders) {
		if _, ok := live[sender.SessionID]; !ok {
			r.state.Forget(sender.SessionID)
			changed = true
		}
	}
	if changed {
		if err := r.dir.SaveState(r.state); err != nil {
			r.logf("state: %v", err)
		}
	}
	r.mu.Unlock()

	for _, reg := range ended {
		server, err := cfg.Select(reg.Server)
		if err != nil {
			continue // the server was removed from the configuration
		}
		// On failure the server shows the session as offline after its heartbeat window.
		if err := r.connect(server).Unregister(ctx, reg.SessionID); err != nil {
			r.logf("%s: unregistering ended session %s: %v", reg.Server, reg.SessionID, err)
		}
	}
}

func (r *Relay) stillRegistered(ref config.SessionRef) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.state.Registrations, func(reg config.Registration) bool { return reg.Ref() == ref })
}

func (r *Relay) registration(reg config.Registration, s Session) sot.Registration {
	return sot.Registration{SessionID: reg.SessionID, Name: s.Name, Machine: r.machine, Everyone: reg.Everyone, VisibleTo: reg.VisibleTo}
}

func (r *Relay) server(name string) (config.Server, error) {
	cfg, err := r.dir.LoadConfig()
	if err != nil {
		return config.Server{}, err
	}
	return cfg.Select(name)
}

// report logs a server's state only when it changes.
func (r *Relay) report(server string, err error) {
	text := ""
	if err != nil {
		text = err.Error()
	}
	r.mu.Lock()
	changed := r.serverErrors[server] != text
	r.serverErrors[server] = text
	r.mu.Unlock()
	if changed && text != "" {
		r.logf("%s: %s", server, text)
	} else if changed {
		r.logf("%s: working again", server)
	}
}
