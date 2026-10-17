// Conductor identity: which conductor a session is.
//
// Historically every caller derived the conductor name with
// strings.TrimPrefix(Title, "conductor-"). The title is a display string that
// other tools rewrite (the voice daemon prefixed "🎙 " to the mic session) and
// that users rename, and the moment it changed the conductor silently lost its
// [conductors.<name>] settings and meta.json env (env_file ->
// TELEGRAM_STATE_DIR -> the Telegram plugin exited).
//
// The stable key is Instance.ConductorName, stamped by `conductor setup` and
// persisted in the tool_data extras zone (no schema migration). Old records
// without it fall back to the title, tolerating leading decoration, and
// lazily adopt the parsed name once it is confirmed by a matching meta.json so
// a later rename keeps working. ResolveConductorName is the single helper;
// call it wherever the question is "which conductor is this".
package session

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"unicode"
)

const toolDataConductorNameKey = "conductor_name"

// conductorTitleDriftWarned dedupes the title-drift warning per instance ID so
// a status poll that resolves the name every tick logs it once per process.
var conductorTitleDriftWarned sync.Map

// ConductorNameFromTitle parses a conductor name out of a session title of the
// form "conductor-<name>". Leading decoration (emoji, variation selectors,
// whitespace, symbols such as "* " or "[x] ") is skipped and the prefix is
// matched case-insensitively. Returns "" when the title is not a conductor
// title. This is only the fallback for records without a stored name; prefer
// Instance.ResolveConductorName.
func ConductorNameFromTitle(title string) string {
	t := strings.TrimLeftFunc(title, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(t) < len(ConductorSessionTitlePrefix) ||
		!strings.EqualFold(t[:len(ConductorSessionTitlePrefix)], ConductorSessionTitlePrefix) {
		return ""
	}
	return strings.TrimRightFunc(t[len(ConductorSessionTitlePrefix):], unicode.IsSpace)
}

// storedConductorName reads the persisted stable name under its lock (the
// lazy fill below can write it from a status/spawn goroutine).
func (i *Instance) storedConductorName() string {
	i.conductorNameMu.Lock()
	defer i.conductorNameMu.Unlock()
	return i.ConductorName
}

// ResolveConductorName returns the conductor this session is, or "" for a
// non-conductor session. Order:
//
//  1. the stored ConductorName (stable across renames and decoration);
//  2. the title fallback (ConductorNameFromTitle) for old records. When the
//     session is flagged IsConductor and a meta.json exists for the parsed
//     name, the name is adopted as the stable key in memory; the next save
//     persists it.
//
// A stored name whose title no longer parses to it is logged once.
func (i *Instance) ResolveConductorName() string {
	if i == nil {
		return ""
	}
	if name := i.storedConductorName(); name != "" {
		i.warnConductorTitleDrift(name)
		return name
	}
	name := ConductorNameFromTitle(i.Title)
	if name == "" {
		return ""
	}
	// Adopt only a confirmed conductor: the IsConductor flag guards against a
	// worker that merely carries a conductor-shaped title, and the meta.json
	// check against persisting a guess for a conductor that was never set up.
	if i.IsConductor && IsConductorSetup(name) {
		i.conductorNameMu.Lock()
		if i.ConductorName == "" {
			i.ConductorName = name
		}
		i.conductorNameMu.Unlock()
	}
	return name
}

// ConductorTitleDrift reports the session's conductor name and whether its
// title no longer names it (renamed to something that does not parse as
// "conductor-<name>"). A decorated title for the same name is not drift.
// Used by `conductor status` to surface the mismatch.
func (i *Instance) ConductorTitleDrift() (name string, drifted bool) {
	name = i.ResolveConductorName()
	if name == "" {
		return "", false
	}
	return name, ConductorNameFromTitle(i.Title) != name
}

func (i *Instance) warnConductorTitleDrift(name string) {
	if ConductorNameFromTitle(i.Title) == name {
		return
	}
	if _, loaded := conductorTitleDriftWarned.LoadOrStore(i.ID, struct{}{}); loaded {
		return
	}
	sessionLog.Warn("conductor_title_drift",
		slog.String("session_id", i.ID),
		slog.String("title", i.Title),
		slog.String("conductor", name),
		slog.String("detail", "title no longer matches the conductor name; settings still resolve by the stored name"))
}

// FindConductorInstance returns the session of the named conductor in
// instances, or nil. A stored ConductorName wins; then the canonical title;
// then a decorated title that parses to name. A session whose stored name is
// a different conductor never matches by title.
func FindConductorInstance(instances []*Instance, name string) *Instance {
	if name == "" {
		return nil
	}
	for _, inst := range instances {
		if inst != nil && inst.storedConductorName() == name {
			return inst
		}
	}
	title := ConductorSessionTitle(name)
	var decorated *Instance
	for _, inst := range instances {
		if inst == nil || inst.storedConductorName() != "" {
			continue
		}
		if inst.Title == title {
			return inst
		}
		if decorated == nil && ConductorNameFromTitle(inst.Title) == name {
			decorated = inst
		}
	}
	return decorated
}

// IsConductorNamed reports whether this session is the named conductor, judged
// on title (which the caller reads under its own locking, e.g.
// GetTitleThreadSafe). A stored name decides alone; otherwise the title must
// parse to name.
func (i *Instance) IsConductorNamed(name, title string) bool {
	if i == nil || name == "" {
		return false
	}
	if stored := i.storedConductorName(); stored != "" {
		return stored == name
	}
	return ConductorNameFromTitle(title) == name
}

// WriteConductorNameToToolData merges conductor_name into the tool_data blob.
// An empty name leaves the blob untouched rather than deleting the key: the
// identity is only ever stamped (setup, lazy fill), never cleared, so a save
// from a copy that never resolved it must not erase it, and
// MergeToolDataExtras then carries the stored value forward.
func WriteConductorNameToToolData(td json.RawMessage, name string) json.RawMessage {
	if name == "" {
		return td
	}
	m := map[string]json.RawMessage{}
	if len(td) > 0 {
		_ = json.Unmarshal(td, &m)
	}
	raw, err := json.Marshal(name)
	if err != nil {
		return td
	}
	m[toolDataConductorNameKey] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return td
	}
	return out
}

// ReadConductorNameFromToolData extracts conductor_name from the blob.
// Returns "" for missing, malformed, or legacy rows (title fallback applies).
func ReadConductorNameFromToolData(td json.RawMessage) string {
	if len(td) == 0 {
		return ""
	}
	var blob struct {
		ConductorName string `json:"conductor_name"`
	}
	_ = json.Unmarshal(td, &blob)
	return blob.ConductorName
}
