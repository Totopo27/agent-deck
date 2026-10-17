package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// Issue #2683: Claude Code's agent-view supervisor and its pre-spawned
// bg-spare workers keep the environment of the pane that first started them,
// AGENTDECK_INSTANCE_ID included. A conversation later backgrounded from a
// DIFFERENT pane runs in that worker, so its hooks name the wrong instance.
// Such hooks cannot be attributed reliably and are dropped: the status file,
// the session-id binding and every other side effect belong to the pane's
// own Claude process only.

// maxBgWorkerAncestorHops bounds the walk from the hook process up to the
// Claude process that fired it (the hook may run under an intermediate shell).
const maxBgWorkerAncestorHops = 3

// hookFromClaudeBackgroundWorker reports whether this hook was fired by a
// Claude background/supervisor worker: CLAUDE_JOB_DIR is set in its env, or
// the nearest ancestor with a ~/.claude/sessions/<pid>.json record says
// kind "bg".
func hookFromClaudeBackgroundWorker() bool {
	if strings.TrimSpace(os.Getenv("CLAUDE_JOB_DIR")) != "" {
		return true
	}
	claudeDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if claudeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		claudeDir = filepath.Join(home, ".claude")
	}
	pid := os.Getppid()
	for hop := 0; hop < maxBgWorkerAncestorHops && pid > 1; hop++ {
		data, err := os.ReadFile(filepath.Join(claudeDir, "sessions", strconv.Itoa(pid)+".json"))
		if err == nil {
			var rec struct {
				Kind string `json:"kind"`
			}
			return json.Unmarshal(data, &rec) == nil && rec.Kind == "bg"
		}
		pid = parentPIDOf(pid)
	}
	return false
}

// parentPIDOf returns pid's parent via /proc, or 0 where /proc is absent
// (macOS: only the direct parent is checked there).
func parentPIDOf(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// Format: pid (comm) state ppid ...; comm may contain spaces/parens.
	s := string(data)
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 {
		return 0
	}
	fields := strings.Fields(s[idx+1:])
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

// logBgWorkerHookDropOnce records one session-id lifecycle reject per
// (instance, worker session), so a busy worker does not flood the log.
func logBgWorkerHookDropOnce(instanceID, sessionID, event string) {
	hooksDir := getHooksDir()
	markerPath := filepath.Join(hooksDir, filepath.Base(instanceID)+".bg-worker-dropped")
	if existing, err := os.ReadFile(markerPath); err == nil && strings.TrimSpace(string(existing)) == sessionID {
		return
	}
	_ = session.WriteSessionIDLifecycleEvent(session.SessionIDLifecycleEvent{
		InstanceID: instanceID,
		Tool:       "claude",
		Action:     "reject",
		Source:     "hook_payload",
		Candidate:  sessionID,
		Reason:     "hook_from_background_worker",
		HookEvent:  event,
	})
	if err := os.MkdirAll(hooksDir, 0o700); err == nil {
		_ = os.WriteFile(markerPath, []byte(sessionID), 0o600)
	}
}
