package session

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/logging"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// Issue #2683: pressing ← in Claude Code's agent view moves the conversation
// to the background supervisor under a NEW session id, and the old
// transcript ends with {"type":"continued-in","continuedInSessionId":"<new>"}.
// The instance stays bound to the old id, so a restart reopened the stalled
// pre-handoff copy. followClaudeContinuedIn walks that pointer chain before
// the resume command is built.

const (
	// maxContinuedInHops bounds the chain walk; a cycle or a long chain
	// stops at whatever was reached.
	maxContinuedInHops = 8
	// continuedInTailBytes is how much of a transcript's tail is read to find
	// its last record.
	continuedInTailBytes = 64 * 1024
)

var claudeSessionIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// followClaudeContinuedIn rebinds the instance to the newest successor of its
// own conversation. Only an id this instance owns (the #1815 identity guard)
// is followed, and only to a successor transcript in the same project
// directory, so the pointer can never pull in another project's conversation.
func (i *Instance) followClaudeContinuedIn() {
	if i == nil || !i.TranscriptIsResolvableLocally() {
		return
	}
	start := i.ClaudeSessionID
	if !i.resumeIdentityAllowed(start).Allow {
		return
	}
	path := claudeTranscriptPathIn(GetClaudeConfigDirForInstance(i), i, start)
	if path == "" {
		return
	}
	cur := start
	seen := map[string]bool{cur: true}
	for hop := 0; hop < maxContinuedInHops; hop++ {
		next := continuedInSuccessor(path)
		if next == "" || seen[next] {
			break
		}
		nextPath := filepath.Join(filepath.Dir(path), next+".jsonl")
		if info, err := os.Stat(nextPath); err != nil || !info.Mode().IsRegular() {
			break
		}
		seen[next] = true
		cur, path = next, nextPath
	}
	if cur == start {
		return
	}
	i.ClaudeSessionID = cur
	i.markClaudeSessionIDVerified()
	i.ClaudeDetectedAt = time.Now()
	i.publishClaudeContinuedInRebind(cur)
	_ = WriteSessionIDLifecycleEvent(SessionIDLifecycleEvent{
		InstanceID: i.ID,
		Tool:       "claude",
		Action:     "rebind",
		Source:     "continued_in",
		OldID:      start,
		NewID:      cur,
		Reason:     "transcript_continued_in",
	})
	sessionLog.Info("resume: followed continued-in",
		slog.String("instance_id", logging.SanitizeValue(i.ID)),
		slog.String("old_session_id", logging.SanitizeValue(start)),
		slog.String("claude_session_id", logging.SanitizeValue(cur)))
}

// publishClaudeContinuedInRebind makes a continued-in rebind durable and
// visible, the same way bindClaudeSessionFromHook does for a hook rebind.
//
// The Restart path that reuses the existing pane (RespawnPane) never re-runs
// the start-time SetEnvironment, so without this the pane env keeps the old
// id: the respawned claude inherits it, and RefreshLiveSessionIDs later reads
// it back and reverts ClaudeSessionID to the stalled predecessor. The SQLite
// write keeps a fresh load (another process, the next CLI call, a reboot)
// from resurrecting the old id too.
func (i *Instance) publishClaudeContinuedInRebind(sessionID string) {
	if i.tmuxSession != nil && i.tmuxSession.Exists() {
		_ = i.tmuxSession.SetEnvironment("CLAUDE_SESSION_ID", sessionID)
	}
	db := statedb.GetGlobal()
	if db == nil {
		return
	}
	if err := db.WriteClaudeSessionBinding(i.ID, sessionID, i.ClaudeDetectedAt); err != nil {
		sessionLog.Warn("claude_continued_in_persist_failed",
			slog.String("instance_id", logging.SanitizeValue(i.ID)),
			slog.String("new_id", logging.SanitizeValue(sessionID)),
			slog.String("error", logging.SanitizeValue(err.Error())))
		return
	}
	i.linkedClaudeSessionID = sessionID
}

// continuedInSuccessor returns the continuedInSessionId of the transcript's
// LAST record when that record is a continued-in handoff, else "".
func continuedInSuccessor(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := info.Size() - continuedInTailBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	tail = bytes.TrimRight(tail, "\r\n\t ")
	if idx := bytes.LastIndexByte(tail, '\n'); idx >= 0 {
		tail = tail[idx+1:]
	}
	var rec struct {
		Type                 string `json:"type"`
		ContinuedInSessionID string `json:"continuedInSessionId"`
	}
	if json.Unmarshal(tail, &rec) != nil || rec.Type != "continued-in" {
		return ""
	}
	if !claudeSessionIDRE.MatchString(rec.ContinuedInSessionID) {
		return ""
	}
	return rec.ContinuedInSessionID
}
