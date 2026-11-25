package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// serverDirEnv points to a sot-mcp-server checkout. Without it the test is skipped.
const serverDirEnv = "SOT_SERVER_DIR"

type person struct {
	name, token, home, claude, session string
	inbox                              chan string
	bin                                string
}

func TestTwoPeopleExchangeMessages(t *testing.T) {
	serverDir := os.Getenv(serverDirEnv)
	if serverDir == "" {
		t.Skip(serverDirEnv + " is not set")
	}
	root, err := os.MkdirTemp("/tmp", "cca") // unix socket paths must stay short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	bin := filepath.Join(root, "cc-agent")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	url, tokens := startServer(t, serverDir, root, "alice", "bob", "carol")
	people := map[string]*person{}
	for name, token := range tokens {
		p := newPerson(t, root, bin, name, token)
		p.mustRun(t, token+"\n", "server", "add", "work", "--url", url+"/mcp")
		p.startDaemon(t)
		people[name] = p
	}
	alice, bob, carol := people["alice"], people["bob"], people["carol"]

	if out := bob.mustRun(t, "", "list"); strings.Contains(out, "alice-session") {
		t.Fatalf("session is visible before registration:\n%s", out)
	}
	alice.mustRun(t, "", "register", "--to", "bob")
	if out := bob.mustRun(t, "", "list"); !strings.Contains(out, "alice-session") || !strings.Contains(out, "online") {
		t.Fatalf("bob does not see alice:\n%s", out)
	}
	if out := carol.mustRun(t, "", "list"); strings.Contains(out, "alice-session") {
		t.Fatalf("carol sees alice:\n%s", out)
	}
	if out, err := carol.run("", "send", "--to-user", "alice", "--to-session", "alice-session", "hi"); err == nil {
		t.Fatalf("carol reached alice: %s", out)
	}

	bob.mustRun(t, "what is <T>?\nsecond line\n", "send", "--to-user", "alice", "--to-session", "alice-session", "-")
	got := alice.receive(t)
	for _, want := range []string{"NOT from your user", `user "bob"`, "what is &lt;T&gt;?\nsecond line"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}

	// bob never registered, but gets the reply.
	id := strings.Fields(strings.SplitN(got, "--reply-to ", 2)[1])[0]
	alice.mustRun(t, "", "send", "--server", "work", "--reply-to", id, "a type parameter")
	if got := bob.receive(t); !strings.Contains(got, "a type parameter") || !strings.Contains(got, `user "alice"`) {
		t.Fatalf("unexpected reply:\n%s", got)
	}

	alice.mustRun(t, "", "unregister")
	if out, err := bob.run("", "send", "--to-user", "alice", "--to-session", "alice-session", "again"); err == nil {
		t.Fatalf("bob reached alice after unregister: %s", out)
	}
	select {
	case extra := <-alice.inbox:
		t.Fatalf("unexpected message:\n%s", extra)
	case <-time.After(6 * time.Second): // longer than one poll
	}
}

func startServer(t *testing.T, dir, root string, users ...string) (string, map[string]string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	env := append(os.Environ(), "SOT_DB_PATH="+filepath.Join(root, "sot.sqlite3"), "RACK_ENV=production", fmt.Sprintf("PORT=%d", port))

	script := `require "./config/boot"; SOT::Role.find_or_create(name: "member") { |r| r.description = "d" };` +
		`puts JSON.generate(ARGV.to_h { |n| [n, SOT::User.create_with_token(name: n).last] })`
	seed := exec.Command("bundle", append([]string{"exec", "ruby", "-rjson", "-e", script}, users...)...)
	seed.Dir, seed.Env, seed.Stderr = dir, env, os.Stderr
	out, err := seed.Output()
	if err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	if err := json.Unmarshal(out, &tokens); err != nil {
		t.Fatalf("%v: %s", err, out)
	}

	server := exec.Command("bundle", "exec", "puma", "-C", "config/puma.rb")
	server.Dir, server.Env = dir, env
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(server) })
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitFor(t, func() bool {
		resp, err := http.Get(url + "/api/users")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	})
	return url, tokens
}

// newPerson creates a separate home and one fake Claude Code session.
func newPerson(t *testing.T, root, bin, name, token string) *person {
	t.Helper()
	p := &person{name: name, token: token, bin: bin, home: filepath.Join(root, name), claude: filepath.Join(root, name+"-claude"),
		session: "session-of-" + name, inbox: make(chan string, 16)}
	sessions := filepath.Join(p.claude, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, name+".sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	entry, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": p.session, "name": name + "-session",
		"messagingSocketPath": socket, "peerProtocol": 1, "startedAt": time.Now().UnixMilli()})
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", os.Getpid())), entry, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			scanner := bufio.NewScanner(conn)
			scanner.Buffer(nil, 1<<20)
			for scanner.Scan() {
				var frame struct {
					Type    string
					Message struct{ Content string }
				}
				if json.Unmarshal(scanner.Bytes(), &frame) == nil && frame.Type == "user" {
					p.inbox <- frame.Message.Content
				}
			}
			conn.Close()
		}
	}()
	return p
}

func (p *person) command(args ...string) *exec.Cmd {
	cmd := exec.Command(p.bin, args...)
	cmd.Env = append(os.Environ(), "CC_AGENT_HOME="+p.home, "CLAUDE_CONFIG_DIR="+p.claude, sessionEnv+"="+p.session)
	return cmd
}

func (p *person) run(stdin string, args ...string) (string, error) {
	cmd := p.command(args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (p *person) mustRun(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	out, err := p.run(stdin, args...)
	if err != nil {
		t.Fatalf("%s: cc-agent %v: %v\n%s", p.name, args, err, out)
	}
	return out
}

func (p *person) startDaemon(t *testing.T) {
	t.Helper()
	cmd := p.command("daemon")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(cmd) })
	waitFor(t, func() bool { _, err := p.run("", "status"); return err == nil })
}

func (p *person) receive(t *testing.T) string {
	t.Helper()
	select {
	case m := <-p.inbox:
		return m
	case <-time.After(15 * time.Second):
		t.Fatalf("%s received nothing", p.name)
		return ""
	}
}

func stop(cmd *exec.Cmd) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	kill := time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
	defer kill.Stop()
	_ = cmd.Wait()
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}
