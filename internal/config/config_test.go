package config

import (
	"os"
	"path/filepath"
	"testing"
)

func home(t *testing.T) Dir {
	t.Helper()
	return Dir(filepath.Join(t.TempDir(), "cc-agent"))
}

func TestSaveKeepsTokenPrivate(t *testing.T) {
	dir := home(t)
	if err := dir.SaveConfig(Config{Servers: []Server{{Name: "work", URL: "https://sot.example.com", Token: "secret"}}}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{string(dir): 0o700, dir.configPath(): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
	cfg, err := dir.LoadConfig()
	if err != nil || len(cfg.Servers) != 1 || cfg.Servers[0].Token != "secret" {
		t.Fatalf("LoadConfig = %+v, %v", cfg, err)
	}
}

func TestLoadRefusesConfigReadableByOthers(t *testing.T) {
	dir := home(t)
	if err := dir.SaveConfig(Config{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir.configPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.LoadConfig(); err == nil {
		t.Fatal("config readable by others was accepted")
	}
}

func TestLoadWithoutFileIsEmpty(t *testing.T) {
	cfg, err := home(t).LoadConfig()
	if err != nil || len(cfg.Servers) != 0 {
		t.Fatalf("LoadConfig = %+v, %v", cfg, err)
	}
}

func TestNewServerNormalizesURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://sot.example.com":           "https://sot.example.com",
		"https://sot.example.com/":          "https://sot.example.com",
		"https://sot.example.com/mcp":       "https://sot.example.com",
		"https://sot.example.com:39482/mcp": "https://sot.example.com:39482",
		"http://localhost:39482":            "http://localhost:39482",
		"http://127.0.0.1:1":                "http://127.0.0.1:1",
	} {
		s, err := NewServer("work", in, "t")
		if err != nil || s.URL != want {
			t.Errorf("NewServer(%q) = %q, %v; want %q", in, s.URL, err, want)
		}
	}
}

func TestNewServerRejectsInvalidInput(t *testing.T) {
	for _, c := range [][3]string{
		{"work", "http://sot.example.com", "t"}, // would send the token in clear text
		{"work", "ftp://sot.example.com", "t"},
		{"work", "sot.example.com", "t"},
		{"work", "https://user:pass@sot.example.com", "t"},
		{"work", "https://sot.example.com", ""},
		{"", "https://sot.example.com", "t"},
		{"bad name", "https://sot.example.com", "t"},
		{"-x", "https://sot.example.com", "t"},
	} {
		if _, err := NewServer(c[0], c[1], c[2]); err == nil {
			t.Errorf("NewServer(%q, %q, %q) was accepted", c[0], c[1], c[2])
		}
	}
}

func TestSelectServer(t *testing.T) {
	one := Config{Servers: []Server{{Name: "a"}}}
	two := Config{Servers: []Server{{Name: "a"}, {Name: "b"}}}
	if s, err := one.Select(""); err != nil || s.Name != "a" {
		t.Errorf("only server not selected: %v %v", s, err)
	}
	if _, err := two.Select(""); err == nil {
		t.Error("ambiguous selection accepted")
	}
	if s, err := two.Select("b"); err != nil || s.Name != "b" {
		t.Errorf("named server not selected: %v %v", s, err)
	}
	if _, err := two.Select("c"); err == nil {
		t.Error("unknown server accepted")
	}
	if _, err := (Config{}).Select(""); err == nil {
		t.Error("selection without servers accepted")
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := home(t)
	st, err := dir.LoadState()
	if err != nil || len(st.Registrations) != 0 {
		t.Fatalf("initial state = %+v, %v", st, err)
	}
	st.Register(Registration{Server: "a", SessionID: "s", VisibleTo: []string{"bob"}})
	st.Register(Registration{Server: "a", SessionID: "s", Everyone: true})
	st.AddSender(SessionRef{"a", "s2"})
	st.AddSender(SessionRef{"a", "s2"})
	if err := dir.SaveState(st); err != nil {
		t.Fatal(err)
	}
	got, err := dir.LoadState()
	if err != nil || len(got.Registrations) != 1 || !got.Registrations[0].Everyone || len(got.Senders) != 1 {
		t.Fatalf("state = %+v, %v", got, err)
	}
	if !got.Unregister(SessionRef{"a", "s"}) || got.Unregister(SessionRef{"a", "s"}) {
		t.Error("Unregister must report whether it removed something")
	}
	if ids := got.SessionIDs("a"); len(ids) != 1 || ids[0] != "s2" {
		t.Errorf("SessionIDs = %v", ids)
	}
	got.Forget("s2")
	if ids := got.SessionIDs("a"); len(ids) != 0 {
		t.Errorf("SessionIDs after Forget = %v", ids)
	}
}
