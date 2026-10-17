package session

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// hostileTitles carry a newline, a carriage return and an ANSI escape: the
// characters that let a session title forge or corrupt log records (CodeQL
// go/log-injection on the conductor_title_drift and TELEGRAM_STATE_DIR strip
// warnings).
var hostileTitles = []string{
	"evil\nlevel=ERROR msg=forged",
	"evil\r\nforged",
	"evil\x1b[2Jcleared",
}

// captureSessionLogJSON swaps sessionLog for a JSON handler and returns its
// buffer. The handler escapes control characters itself, so the check that
// matters is on the decoded attribute values: the sanitizer must have removed
// them before the value reached the log call (other sinks, such as a text
// handler or a log viewer, would otherwise print them raw).
func captureSessionLogJSON(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := sessionLog
	sessionLog = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { sessionLog = orig })
	return &buf
}

// assertOneCleanRecord checks buf holds exactly one log record and that no
// attribute value in it carries a raw control character once decoded.
func assertOneCleanRecord(t *testing.T, buf *bytes.Buffer, title string) {
	t.Helper()
	out := strings.TrimRight(buf.String(), "\n")
	if out == "" {
		t.Fatalf("title %q: nothing logged", title)
	}
	if n := strings.Count(out, "\n"); n != 0 {
		t.Fatalf("title %q: log spans %d lines, want 1:\n%s", title, n+1, out)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("title %q: record is not one JSON object: %v\n%s", title, err, out)
	}
	for k, v := range rec {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if strings.ContainsAny(s, "\n\r\x1b") {
			t.Errorf("title %q: attribute %q carries a raw control character: %q", title, k, s)
		}
	}
}

func TestConductorTitleDriftLog_ControlCharsInTitle(t *testing.T) {
	for n, title := range hostileTitles {
		buf := captureSessionLogJSON(t)
		inst := &Instance{ID: "drift-inject-" + string(rune('a'+n)) + "-" + t.Name(), Title: title, ConductorName: "personal"}
		t.Cleanup(func() { conductorTitleDriftWarned.Delete(inst.ID) })
		if got := inst.ResolveConductorName(); got != "personal" {
			t.Fatalf("ResolveConductorName = %q, want personal", got)
		}
		if !strings.Contains(buf.String(), "conductor_title_drift") {
			t.Fatalf("title %q: drift warning not logged:\n%s", title, buf.String())
		}
		assertOneCleanRecord(t, buf, title)
	}
}

func TestTelegramStripWarningLog_ControlCharsInTitle(t *testing.T) {
	withTempAgentDeckHome(t, "")
	for _, title := range hostileTitles {
		inst := NewInstanceWithTool("plain-child", t.TempDir(), "claude")
		inst.Title = title
		inst.Wrapper = "env TELEGRAM_STATE_DIR=/tmp/x {command}"
		buf := captureSessionLogJSON(t)
		if got := inst.telegramStripSources(); len(got) != 2 {
			t.Fatalf("title %q: want warning + strip clauses, got %q", title, got)
		}
		if !strings.Contains(buf.String(), "TELEGRAM_STATE_DIR stripped") {
			t.Fatalf("title %q: strip warning not logged:\n%s", title, buf.String())
		}
		assertOneCleanRecord(t, buf, title)
	}
}
