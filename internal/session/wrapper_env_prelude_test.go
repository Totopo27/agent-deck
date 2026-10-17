package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// --- Wrapper + env prelude composition -------------------------------------
// A session wrapper such as "env TELEGRAM_STATE_DIR=/x {command}" used to be
// spliced textually in front of the env prelude from buildEnvSourceCommand,
// yielding "env TELEGRAM_STATE_DIR=/x export A=... && ... exec claude ...".
// env then tries to exec a program called "export" and the agent runs (if at
// all) without the wrapper's variable. These tests execute the real composed
// launch line under bash against a fake agent and check what it actually saw.

// wrapperPreludeFixture sets up an isolated HOME whose config gives the
// conductor "wrapfix" an env_file (the prelude) and points its claude command
// at a fake agent script that records its env and argv into outFile.
func wrapperPreludeFixture(t *testing.T) (inst *Instance, outFile, tgDir string) {
	t.Helper()
	dir := t.TempDir()
	outFile = filepath.Join(dir, "agent.out")
	tgDir = filepath.Join(dir, "tg-state")
	envFile := filepath.Join(dir, "conductor.env")
	agent := filepath.Join(dir, "fake-agent")

	if err := os.WriteFile(envFile, []byte("export PRELUDE_MARK=from-env-file\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	script := "#!/bin/bash\n" +
		"printf 'TG=%s\\nPRELUDE=%s\\nARGS=%s\\n' \"$TELEGRAM_STATE_DIR\" \"$PRELUDE_MARK\" \"$*\" > '" + outFile + "'\n"
	if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}

	withTempAgentDeckHome(t, `
[conductors.wrapfix.claude]
env_file = "`+envFile+`"
command = "`+agent+`"
`)
	t.Setenv("TELEGRAM_STATE_DIR", "")
	t.Setenv("PRELUDE_MARK", "")

	inst = NewInstanceWithTool("conductor-wrapfix", dir, "claude")
	return inst, outFile, tgDir
}

// runLaunchLine composes the fresh-start launch line exactly like Start()
// (buildClaudeCommand → prepareCommand), runs it under bash and returns the
// composed line plus what the fake agent recorded.
func runLaunchLine(t *testing.T, inst *Instance, outFile string) (line, recorded string) {
	t.Helper()
	cmd, _, err := inst.prepareCommand(inst.buildClaudeCommand("claude"))
	if err != nil {
		t.Fatalf("prepareCommand: %v", err)
	}
	run := exec.Command("bash", "-c", cmd)
	run.Dir = inst.ProjectPath
	if out, err := run.CombinedOutput(); err != nil {
		t.Logf("launch line exited with %v; output:\n%s", err, out)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("fake agent never ran (launch line broken): %v\n  line: %s", err, cmd)
	}
	return cmd, string(data)
}

// envExportSplice matches the broken shape: a wrapper env assignment followed
// directly by a shell builtin from the prelude.
var envExportSplice = regexp.MustCompile(`[A-Z_]+=[^\s;&|']+ (?:export|source|unset|cd|exec|\[) `)

func TestWrapperEnvPrelude_EnvWrapperVarReachesAgent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wrapper string
	}{
		{"env program", "env TELEGRAM_STATE_DIR=%s {command}"},
		{"bare assignment", "TELEGRAM_STATE_DIR=%s {command}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, outFile, tgDir := wrapperPreludeFixture(t)
			inst.Wrapper = strings.Replace(tc.wrapper, "%s", tgDir, 1)

			line, got := runLaunchLine(t, inst, outFile)
			if envExportSplice.MatchString(line) {
				t.Errorf("wrapper assignment spliced in front of a prelude builtin:\n  line: %s", line)
			}
			if !strings.Contains(got, "TG="+tgDir+"\n") {
				t.Errorf("agent did not see the wrapper's TELEGRAM_STATE_DIR:\n  recorded: %q\n  line: %s", got, line)
			}
			if !strings.Contains(got, "PRELUDE=from-env-file\n") {
				t.Errorf("agent did not see the env_file prelude var:\n  recorded: %q\n  line: %s", got, line)
			}
		})
	}
}

// TestWrapperEnvPrelude_SuffixFlagsStillReachAgent keeps issue #601 intact
// for a wrapper that has BOTH a program prefix and suffix flags: the flags
// must reach the agent's argv, not bash's positional parameters.
func TestWrapperEnvPrelude_SuffixFlagsStillReachAgent(t *testing.T) {
	inst, outFile, tgDir := wrapperPreludeFixture(t)
	inst.Wrapper = "env TELEGRAM_STATE_DIR=" + tgDir + " {command} --wrapper-flag 'two words'"

	line, got := runLaunchLine(t, inst, outFile)
	if !strings.Contains(got, "TG="+tgDir+"\n") {
		t.Errorf("agent did not see the wrapper's TELEGRAM_STATE_DIR:\n  recorded: %q\n  line: %s", got, line)
	}
	if !strings.Contains(got, "--wrapper-flag two words\n") {
		t.Errorf("wrapper suffix flags did not reach the agent argv:\n  recorded: %q\n  line: %s", got, line)
	}
}

// TestApplyWrapper_PrefixWrapperOverCompoundCommand pins the string shape:
// a compound command under a prefix wrapper becomes one bash -c argv.
func TestApplyWrapper_PrefixWrapperOverCompoundCommand(t *testing.T) {
	inst := NewInstance("wrap-compound", "/tmp")
	inst.Tool = "claude"
	inst.Wrapper = "env X=1 {command}"

	got, err := inst.applyWrapper(`export A='b' && exec claude`)
	if err != nil {
		t.Fatal(err)
	}
	want := `env X=1 bash -c 'export A='"'"'b'"'"' && exec claude'`
	if got != want {
		t.Fatalf("applyWrapper shape wrong.\n  got:  %q\n  want: %q", got, want)
	}

	// A simple command keeps the plain textual substitution.
	got, err = inst.applyWrapper(`claude --resume abc`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `env X=1 claude --resume abc` {
		t.Fatalf("simple command should substitute textually, got %q", got)
	}
}

// TestWrapperEnvPrelude_StrippedTelegramVarIsNotSilent: on a non-conductor,
// non-channel claude session the S8/#1133 strip still removes TELEGRAM_*
// (a wrapper is copied onto forks, so it cannot be a bot-ownership opt-in),
// but the prelude must say so instead of silently discarding the wrapper's
// assignment.
func TestWrapperEnvPrelude_StrippedTelegramVarIsNotSilent(t *testing.T) {
	withTempAgentDeckHome(t, "")
	inst := NewInstanceWithTool("plain-child", t.TempDir(), "claude")
	inst.Wrapper = "env TELEGRAM_STATE_DIR=/tmp/x {command}"

	prelude := inst.buildEnvSourceCommand()
	if !strings.Contains(prelude, "unset TELEGRAM_STATE_DIR") {
		t.Fatalf("strip must still fire for a non-owning child: %q", prelude)
	}
	if !strings.Contains(prelude, "agent-deck: warning:") || !strings.Contains(prelude, "TELEGRAM_STATE_DIR") {
		t.Fatalf("no pane warning that the wrapper's TELEGRAM_STATE_DIR is stripped: %q", prelude)
	}

	// Conductors own the bot: no strip, no warning.
	cond := NewInstanceWithTool("conductor-owner", t.TempDir(), "claude")
	cond.Wrapper = "env TELEGRAM_STATE_DIR=/tmp/x {command}"
	if p := cond.buildEnvSourceCommand(); strings.Contains(p, "TELEGRAM_STATE_DIR") {
		t.Fatalf("conductor prelude must not touch TELEGRAM_STATE_DIR: %q", p)
	}
}
