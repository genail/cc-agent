package service

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestLaunchdPlistIsValidXMLAndEscapesPaths(t *testing.T) {
	out := launchdPlist("/opt/a&b/cc-agent", "/tmp/log <x>", map[string]string{"CLAUDE_CONFIG_DIR": "/c&d"})
	d := xml.NewDecoder(strings.NewReader(out))
	for {
		if _, err := d.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("%v in:\n%s", err, out)
		}
	}
	for _, want := range []string{"/opt/a&amp;b/cc-agent", "daemon", "/c&amp;d"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSystemdUnitQuotesPaths(t *testing.T) {
	out := systemdUnit(`/home/jan k/bin/cc-agent`, map[string]string{"CLAUDE_CONFIG_DIR": `/home/50%/x`})
	for _, want := range []string{`ExecStart="/home/jan k/bin/cc-agent" daemon`, `Environment="CLAUDE_CONFIG_DIR=/home/50%%/x"`, "Restart=on-failure"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
