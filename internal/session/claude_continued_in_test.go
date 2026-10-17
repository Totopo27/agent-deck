package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #2683: Claude Code's agent view moves a backgrounded conversation to a
// new session id and ends the old transcript with a continued-in record. A
// restart must resume the successor, not reopen the stalled pre-handoff copy.

func setupContinuedInProject(t *testing.T) (projectPath, projectsDir string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)
	projectPath = filepath.Join(tmpHome, "project")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	projectsDir = filepath.Join(tmpHome, ".claude", "projects", ConvertToClaudeDirName(projectPath))
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return projectPath, projectsDir
}

func writeContinuedInTranscript(t *testing.T, dir, id, continuedIn string) {
	t.Helper()
	body := `{"type":"user","sessionId":"` + id + `","message":{"role":"user","content":"hi"}}` + "\n"
	if continuedIn != "" {
		body += `{"type":"continued-in","continuedInSessionId":"` + continuedIn + `"}` + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBuildClaudeResumeCommand_FollowsContinuedInChain(t *testing.T) {
	projectPath, dir := setupContinuedInProject(t)
	const (
		oldID = "11111111-1111-4111-8111-111111111111"
		midID = "22222222-2222-4222-8222-222222222222"
		newID = "33333333-3333-4333-8333-333333333333"
	)
	writeContinuedInTranscript(t, dir, oldID, midID)
	writeContinuedInTranscript(t, dir, midID, newID)
	writeContinuedInTranscript(t, dir, newID, "")

	inst := NewInstance("handoff", projectPath)
	inst.Tool = "claude"
	inst.ClaudeSessionID = oldID

	cmd := inst.buildClaudeResumeCommand()
	if !strings.Contains(cmd, "--resume "+newID) {
		t.Fatalf("restart must resume the newest continued-in successor %s, got %q", newID, cmd)
	}
	if inst.ClaudeSessionID != newID {
		t.Fatalf("binding must move to the successor, got %q", inst.ClaudeSessionID)
	}
}

func TestBuildClaudeResumeCommand_ContinuedInSuccessorMustBeInSameProject(t *testing.T) {
	projectPath, dir := setupContinuedInProject(t)
	const (
		oldID     = "44444444-4444-4444-8444-444444444444"
		foreignID = "55555555-5555-4555-8555-555555555555"
	)
	writeContinuedInTranscript(t, dir, oldID, foreignID)
	other := filepath.Join(filepath.Dir(dir), "-elsewhere")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	writeContinuedInTranscript(t, other, foreignID, "")

	inst := NewInstance("handoff-foreign", projectPath)
	inst.Tool = "claude"
	inst.ClaudeSessionID = oldID

	cmd := inst.buildClaudeResumeCommand()
	if !strings.Contains(cmd, "--resume "+oldID) || inst.ClaudeSessionID != oldID {
		t.Fatalf("a successor outside the project dir must not be followed, got %q (id %q)", cmd, inst.ClaudeSessionID)
	}
}

func TestBuildClaudeResumeCommand_ContinuedInCycleIsBounded(t *testing.T) {
	projectPath, dir := setupContinuedInProject(t)
	const (
		aID = "66666666-6666-4666-8666-666666666666"
		bID = "77777777-7777-4777-8777-777777777777"
	)
	writeContinuedInTranscript(t, dir, aID, bID)
	writeContinuedInTranscript(t, dir, bID, aID)

	inst := NewInstance("handoff-cycle", projectPath)
	inst.Tool = "claude"
	inst.ClaudeSessionID = aID

	cmd := inst.buildClaudeResumeCommand()
	if !strings.Contains(cmd, "--resume ") {
		t.Fatalf("a cyclic chain must still resume something, got %q", cmd)
	}
}
