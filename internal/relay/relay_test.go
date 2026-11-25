package relay

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/genail/cc-agent/internal/config"
	"github.com/genail/cc-agent/internal/sot"
)

type fakeAPI struct {
	registered map[string]sot.Registration
	inbox      []sot.Message
	pulledFor  [][]string
	acked      []string
	ackErr     error
	removed    []string
}

func (f *fakeAPI) Register(_ context.Context, r sot.Registration) error {
	f.registered[r.SessionID] = r
	return nil
}
func (f *fakeAPI) Heartbeat(_ context.Context, ids []string) ([]string, error) {
	var known []string
	for _, id := range ids {
		if _, ok := f.registered[id]; ok {
			known = append(known, id)
		}
	}
	return known, nil
}
func (f *fakeAPI) Unregister(_ context.Context, id string) error {
	delete(f.registered, id)
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeAPI) Pull(_ context.Context, ids []string) ([]sot.Message, error) {
	f.pulledFor = append(f.pulledFor, ids)
	var out []sot.Message
	for _, m := range f.inbox {
		for _, id := range ids {
			if m.ToSessionID == id {
				out = append(out, m)
			}
		}
	}
	return out, nil
}
func (f *fakeAPI) Ack(_ context.Context, ids []string) error {
	if f.ackErr != nil {
		return f.ackErr
	}
	f.acked = append(f.acked, ids...)
	var rest []sot.Message
	for _, m := range f.inbox {
		keep := true
		for _, id := range ids {
			keep = keep && m.ID != id
		}
		if keep {
			rest = append(rest, m)
		}
	}
	f.inbox = rest
	return nil
}

type fakeLocal struct {
	live      map[string]string // id -> name
	delivered map[string][]string
	err       error
	broken    string // session id that refuses messages
}

func (f *fakeLocal) Live() (map[string]Session, error) {
	out := map[string]Session{}
	for id, name := range f.live {
		out[id] = Session{ID: id, Name: name}
	}
	return out, nil
}
func (f *fakeLocal) Deliver(_ context.Context, id, _, text string) error {
	if f.err != nil {
		return f.err
	}
	if id == f.broken {
		return errors.New("broken")
	}
	f.delivered[id] = append(f.delivered[id], text)
	return nil
}

func setup(t *testing.T) (*Relay, *fakeAPI, *fakeLocal, config.Dir) {
	t.Helper()
	dir := config.Dir(t.TempDir())
	if err := dir.SaveConfig(config.Config{Servers: []config.Server{{Name: "work", URL: "https://x", Token: "t"}}}); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{registered: map[string]sot.Registration{}}
	local := &fakeLocal{live: map[string]string{"s1": "api"}, delivered: map[string][]string{}}
	r, err := New(dir, local, func(config.Server) API { return api }, "laptop", func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	return r, api, local, dir
}

func register(t *testing.T, r *Relay) {
	t.Helper()
	if err := r.Register(context.Background(), config.Registration{Server: "work", SessionID: "s1", VisibleTo: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
}

func uuid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func message(id string) sot.Message {
	return sot.Message{ID: id, FromUser: "bob", FromSessionName: "w", ToSessionID: "s1", Body: "hi " + id}
}

func TestNothingIsPulledWithoutOptIn(t *testing.T) {
	r, api, local, _ := setup(t)
	api.inbox = []sot.Message{message(uuid(1))}
	r.Tick(context.Background())
	if len(api.pulledFor) != 0 || len(local.delivered) != 0 {
		t.Fatalf("pulled %v, delivered %v", api.pulledFor, local.delivered)
	}
}

func TestRegisteredSessionReceivesAndAcknowledges(t *testing.T) {
	r, api, local, _ := setup(t)
	register(t, r)
	api.inbox = []sot.Message{message(uuid(1)), message(uuid(2))}
	r.Tick(context.Background())
	if got := local.delivered["s1"]; len(got) != 2 || !strings.Contains(got[0], uuid(1)) || !strings.Contains(got[1], uuid(2)) {
		t.Fatalf("delivered %v", got)
	}
	if !reflect.DeepEqual(api.acked, []string{uuid(1), uuid(2)}) {
		t.Fatalf("acked %v", api.acked)
	}
}

func TestFailedDeliveryIsRetriedAndNotAcknowledged(t *testing.T) {
	r, api, local, _ := setup(t)
	register(t, r)
	api.inbox = []sot.Message{message(uuid(1))}
	local.err = errors.New("socket gone")
	r.Tick(context.Background())
	if len(api.acked) != 0 {
		t.Fatalf("acked %v", api.acked)
	}
	local.err = nil
	r.Tick(context.Background())
	if len(local.delivered["s1"]) != 1 || len(api.acked) != 1 {
		t.Fatalf("delivered %v, acked %v", local.delivered, api.acked)
	}
}

func TestFailedAckDoesNotDeliverTwice(t *testing.T) {
	r, api, local, _ := setup(t)
	register(t, r)
	api.inbox = []sot.Message{message(uuid(1))}
	api.ackErr = errors.New("offline")
	r.Tick(context.Background())
	api.ackErr = nil
	r.Tick(context.Background())
	if len(local.delivered["s1"]) != 1 || len(api.acked) != 1 {
		t.Fatalf("delivered %v, acked %v", local.delivered, api.acked)
	}
}

func TestEndedSessionIsUnregistered(t *testing.T) {
	r, api, local, dir := setup(t)
	register(t, r)
	local.live = map[string]string{}
	r.Tick(context.Background())
	if !reflect.DeepEqual(api.removed, []string{"s1"}) {
		t.Fatalf("removed %v", api.removed)
	}
	if st, _ := dir.LoadState(); len(st.Registrations) != 0 {
		t.Fatalf("state %+v", st)
	}
}

func TestLostRegistrationIsRestored(t *testing.T) {
	r, api, _, _ := setup(t)
	register(t, r)
	delete(api.registered, "s1")
	r.Tick(context.Background())
	if got := api.registered["s1"]; !reflect.DeepEqual(got.VisibleTo, []string{"bob"}) {
		t.Fatalf("registered %+v", api.registered)
	}
}

func TestRegistrationSurvivesRestart(t *testing.T) {
	r, api, local, dir := setup(t)
	register(t, r)
	r, err := New(dir, local, func(config.Server) API { return api }, "laptop", func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	api.inbox = []sot.Message{message(uuid(1))}
	r.Tick(context.Background())
	if len(local.delivered["s1"]) != 1 {
		t.Fatalf("delivered %v", local.delivered)
	}
}

func TestSenderReceivesRepliesWithoutBeingRegistered(t *testing.T) {
	r, api, local, _ := setup(t)
	if err := r.AddSender(config.SessionRef{Server: "work", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	api.inbox = []sot.Message{message(uuid(1))}
	r.Tick(context.Background())
	if len(local.delivered["s1"]) != 1 || len(api.registered) != 0 {
		t.Fatalf("delivered %v, registered %v", local.delivered, api.registered)
	}
}

func TestUnknownSessionCannotBeRegistered(t *testing.T) {
	r, api, _, _ := setup(t)
	err := r.Register(context.Background(), config.Registration{Server: "work", SessionID: "nope", Everyone: true})
	if err == nil || len(api.registered) != 0 {
		t.Fatalf("err %v, registered %v", err, api.registered)
	}
}

func TestUnregisterStopsDelivery(t *testing.T) {
	r, api, local, _ := setup(t)
	register(t, r)
	if err := r.Unregister(context.Background(), config.SessionRef{Server: "work", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	api.inbox = []sot.Message{message(uuid(1))}
	r.Tick(context.Background())
	if len(local.delivered) != 0 || !reflect.DeepEqual(api.removed, []string{"s1"}) {
		t.Fatalf("delivered %v, removed %v", local.delivered, api.removed)
	}
}

// rogueAPI answers with messages regardless of the sessions asked for.
type rogueAPI struct{ *fakeAPI }

func (f rogueAPI) Pull(context.Context, []string) ([]sot.Message, error) { return f.inbox, nil }

func TestServerCannotReachSessionsThatDidNotOptIn(t *testing.T) {
	r, api, local, _ := setup(t)
	register(t, r)
	local.live["private"] = "private"
	r.connect = func(config.Server) API { return rogueAPI{api} }
	m := message(uuid(1))
	m.ToSessionID = "private"
	api.inbox = []sot.Message{m}
	r.Tick(context.Background())
	if len(local.delivered) != 0 {
		t.Fatalf("delivered %v", local.delivered)
	}
}

func TestMessageWithMalformedIDIsNotDelivered(t *testing.T) {
	r, api, local, _ := setup(t)
	register(t, r)
	api.inbox = []sot.Message{message("x; rm -rf ~")}
	r.Tick(context.Background())
	if len(local.delivered) != 0 {
		t.Fatalf("delivered %v", local.delivered)
	}
}

func TestBrokenSessionDoesNotBlockOthers(t *testing.T) {
	r, api, local, _ := setup(t)
	register(t, r)
	local.live["s2"] = "web"
	if err := r.Register(context.Background(), config.Registration{Server: "work", SessionID: "s2", Everyone: true}); err != nil {
		t.Fatal(err)
	}
	local.broken = "s1"
	second := message(uuid(3))
	second.ToSessionID = "s2"
	api.inbox = []sot.Message{message(uuid(1)), message(uuid(2)), second}
	r.Tick(context.Background())
	if len(local.delivered["s2"]) != 1 || len(local.delivered["s1"]) != 0 || r.ServerError("work") == "" {
		t.Fatalf("delivered %v, error %q", local.delivered, r.ServerError("work"))
	}
	local.broken = ""
	r.Tick(context.Background())
	if got := local.delivered["s1"]; len(got) != 2 || !strings.Contains(got[0], uuid(1)) {
		t.Fatalf("delivered %v", local.delivered)
	}
}
