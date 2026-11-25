package control

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/genail/cc-agent/internal/config"
	"github.com/genail/cc-agent/internal/relay"
)

type noSessions struct{}

func (noSessions) Live() (map[string]relay.Session, error)               { return nil, nil }
func (noSessions) Deliver(context.Context, string, string, string) error { return nil }

// shortDir stays below the path length limit of unix sockets.
func shortDir(t *testing.T) config.Dir {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cca")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return config.Dir(filepath.Join(dir, "h"))
}

func serve(t *testing.T, dir config.Dir) {
	t.Helper()
	r, err := relay.New(dir, noSessions{}, nil, "m", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: Handler(dir, r, "1.2.3")}
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
}

func TestCallReachesDaemon(t *testing.T) {
	dir := shortDir(t)
	serve(t, dir)
	var status Status
	if err := NewClient(dir).Call("/status", struct{}{}, &status); err != nil || status.Version != "1.2.3" {
		t.Fatalf("status %+v, %v", status, err)
	}
}

func TestCallReportsDaemonErrors(t *testing.T) {
	dir := shortDir(t)
	serve(t, dir)
	err := NewClient(dir).Call("/sessions", ServerRequest{}, nil)
	if err == nil || errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v", err)
	}
}

func TestCallWithoutDaemon(t *testing.T) {
	if err := NewClient(shortDir(t)).Call("/status", struct{}{}, nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v", err)
	}
}

func TestSocketIsPrivateAndSingle(t *testing.T) {
	dir := shortDir(t)
	serve(t, dir)
	info, err := os.Stat(dir.SocketPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", info.Mode(), err)
	}
	if _, err := Listen(dir); err == nil {
		t.Fatal("second daemon was allowed")
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	dir := shortDir(t)
	if err := dir.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir.SocketPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	serve(t, dir)
}
