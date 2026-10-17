package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupConductorIdentityFixture isolates HOME, writes a config.toml with a
// [conductors.personal.claude] block (env_file + config_dir), and creates the
// "personal" conductor with a meta.json env_file. Returns the tmp home.
//
// This is the shape of the owner-reported failure: a voice daemon decorated
// the conductor title ("🎙 conductor-personal") and every lookup keyed on
// strings.TrimPrefix(Title, "conductor-") stopped finding the conductor, so
// env_file (TELEGRAM_STATE_DIR) and config_dir silently dropped.
func setupConductorIdentityFixture(t *testing.T) string {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AGENTDECK_PROFILE", "")
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)

	cfgPath, err := GetUserConfigPath()
	if err != nil {
		t.Fatalf("GetUserConfigPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfg := `
[conductors.personal.claude]
config_dir = "~/.claude-personal"
env_file = "~/personal-claude.env"
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	ClearUserConfigCache()

	if err := SetupConductor("personal", "default", true, true, "", "", "", "", nil, "~/personal-meta.env"); err != nil {
		t.Fatalf("SetupConductor: %v", err)
	}
	return tmpHome
}

func assertConductorSettingsKept(t *testing.T, inst *Instance, tmpHome string) {
	t.Helper()

	metaEnv := filepath.Join(tmpHome, "personal-meta.env")
	if got := inst.getConductorEnv(true); !strings.Contains(got, metaEnv) {
		t.Errorf("getConductorEnv for title %q lost meta.json env_file %s; got %q", inst.Title, metaEnv, got)
	}

	envCmd := inst.buildEnvSourceCommand()
	if !strings.Contains(envCmd, metaEnv) {
		t.Errorf("buildEnvSourceCommand for title %q lost meta.json env_file %s; got %q", inst.Title, metaEnv, envCmd)
	}
	claudeEnv := filepath.Join(tmpHome, "personal-claude.env")
	if !strings.Contains(envCmd, claudeEnv) {
		t.Errorf("buildEnvSourceCommand for title %q lost [conductors.personal.claude].env_file %s; got %q", inst.Title, claudeEnv, envCmd)
	}

	if strings.Contains(envCmd, "unset TELEGRAM_STATE_DIR") {
		t.Errorf("buildEnvSourceCommand for title %q strips TELEGRAM_* from the conductor (the bot owner); got %q", inst.Title, envCmd)
	}

	wantDir := filepath.Join(tmpHome, ".claude-personal")
	if got := GetClaudeConfigDirForInstance(inst); got != wantDir {
		t.Errorf("GetClaudeConfigDirForInstance for title %q = %q, want %q", inst.Title, got, wantDir)
	}
	if !inst.ConductorClearOnCompact() {
		t.Errorf("ConductorClearOnCompact for title %q = false, want true (meta default)", inst.Title)
	}
}

// TestConductorIdentity_DecoratedTitleKeepsSettings: an emoji/symbol-decorated
// title on an old record (no stable conductor name stored) still resolves to
// the conductor through the tolerant title fallback.
func TestConductorIdentity_DecoratedTitleKeepsSettings(t *testing.T) {
	for _, title := range []string{"🎙 conductor-personal", "🎙️conductor-personal", "* conductor-personal", "  conductor-personal"} {
		t.Run(title, func(t *testing.T) {
			tmpHome := setupConductorIdentityFixture(t)
			inst := &Instance{ID: "decorated-" + title, Title: title, Tool: "claude", ProjectPath: tmpHome}
			assertConductorSettingsKept(t, inst, tmpHome)
		})
	}
}

// TestConductorIdentity_OldRecordResolvesViaTitle: a legacy record with the
// plain "conductor-<name>" title and no stored name keeps working, and the
// lookup lazily fills the stable name because a matching meta.json exists.
func TestConductorIdentity_OldRecordResolvesViaTitle(t *testing.T) {
	tmpHome := setupConductorIdentityFixture(t)
	inst := &Instance{ID: "old-record", Title: "conductor-personal", Tool: "claude", ProjectPath: tmpHome, IsConductor: true}
	assertConductorSettingsKept(t, inst, tmpHome)
	if got := inst.ResolveConductorName(); got != "personal" {
		t.Fatalf("ResolveConductorName = %q, want personal", got)
	}
	if inst.ConductorName != "personal" {
		t.Fatalf("stable conductor name not lazily filled: %q", inst.ConductorName)
	}

	// No meta.json for the parsed name: resolve (legacy behavior) but do not
	// persist a guess.
	stray := &Instance{ID: "stray", Title: "conductor-ghost", Tool: "claude", IsConductor: true}
	if got := stray.ResolveConductorName(); got != "ghost" {
		t.Fatalf("ResolveConductorName(stray) = %q, want ghost", got)
	}
	if stray.ConductorName != "" {
		t.Fatalf("stable name filled without a matching meta.json: %q", stray.ConductorName)
	}

	// A conductor-titled session not flagged as a conductor (a worker someone
	// named "conductor-personal") resolves for legacy compatibility but never
	// adopts the identity.
	worker := &Instance{ID: "worker", Title: "conductor-personal", Tool: "claude"}
	if got := worker.ResolveConductorName(); got != "personal" {
		t.Fatalf("ResolveConductorName(worker) = %q, want personal", got)
	}
	if worker.ConductorName != "" {
		t.Fatalf("unflagged session adopted a conductor identity: %q", worker.ConductorName)
	}
}

// TestConductorIdentity_RenamedConductorKeepsSettings: once the stable name is
// stored, the title can change to anything and the conductor keeps its
// settings; the drift is reported.
func TestConductorIdentity_RenamedConductorKeepsSettings(t *testing.T) {
	tmpHome := setupConductorIdentityFixture(t)
	inst := &Instance{ID: "renamed", Title: "my assistant", Tool: "claude", ProjectPath: tmpHome, ConductorName: "personal"}
	assertConductorSettingsKept(t, inst, tmpHome)

	name, drifted := inst.ConductorTitleDrift()
	if name != "personal" || !drifted {
		t.Fatalf("ConductorTitleDrift = (%q, %v), want (personal, true)", name, drifted)
	}
	// A decorated title for the same name is not drift.
	inst.Title = "🎙 conductor-personal"
	if _, drifted := inst.ConductorTitleDrift(); drifted {
		t.Fatal("decorated title for the stored name reported as drift")
	}
}

func TestConductorIdentity_NonConductorTitles(t *testing.T) {
	for _, title := range []string{"", "main", "conductor-", "my conductor-x", "xconductor-foo", "🎙 conductor-"} {
		if got := ConductorNameFromTitle(title); got != "" {
			t.Errorf("ConductorNameFromTitle(%q) = %q, want empty", title, got)
		}
	}
	for title, want := range map[string]string{
		"conductor-personal":    "personal",
		"🎙 conductor-personal":  "personal",
		"Conductor-ops":         "ops",
		"conductor-a.b_c-d":     "a.b_c-d",
		"[*] conductor-ops-two": "ops-two",
	} {
		if got := ConductorNameFromTitle(title); got != want {
			t.Errorf("ConductorNameFromTitle(%q) = %q, want %q", title, got, want)
		}
	}
}

// TestConductorIdentity_SQLiteRoundTrip: the stable name survives a
// SaveWithGroups -> LoadWithGroups cycle, and a row without it still loads.
func TestConductorIdentity_SQLiteRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	storage := newTestStorage(t)

	inst := NewInstance("renamed title", "/tmp")
	inst.Tool = "shell"
	inst.IsConductor = true
	inst.ConductorName = "personal"
	legacy := NewInstance("conductor-legacy", "/tmp")
	legacy.Tool = "shell"

	all := []*Instance{inst, legacy}
	if err := storage.SaveWithGroups(all, NewGroupTreeWithGroups(all, nil)); err != nil {
		t.Fatalf("SaveWithGroups: %v", err)
	}
	loaded, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatalf("LoadWithGroups: %v", err)
	}
	byID := map[string]*Instance{}
	for _, l := range loaded {
		byID[l.ID] = l
	}
	if got := byID[inst.ID]; got == nil || got.ConductorName != "personal" {
		t.Fatalf("ConductorName not preserved across SQLite round-trip (found=%v)", got != nil)
	}
	if got := byID[legacy.ID]; got == nil || got.ConductorName != "" {
		t.Fatalf("legacy row gained a conductor name (found=%v)", got != nil)
	}

	// A save that does not carry the name (an older binary, or an in-memory
	// copy that never resolved it) must not erase the stored identity.
	reloaded := byID[inst.ID]
	reloaded.ConductorName = ""
	if err := storage.SaveWithGroups(loaded, NewGroupTreeWithGroups(loaded, nil)); err != nil {
		t.Fatalf("SaveWithGroups (no name): %v", err)
	}
	loaded2, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatalf("LoadWithGroups (2): %v", err)
	}
	for _, l := range loaded2 {
		if l.ID == inst.ID && l.ConductorName != "personal" {
			t.Fatalf("stored conductor identity erased by a save without it: %q", l.ConductorName)
		}
	}
}

// TestFindConductorInstance prefers the stable name, then the exact title,
// then a decorated title for the same name.
func TestFindConductorInstance(t *testing.T) {
	other := &Instance{ID: "o", Title: "conductor-other"}
	decorated := &Instance{ID: "d", Title: "🎙 conductor-personal"}
	if got := FindConductorInstance([]*Instance{other, decorated}, "personal"); got != decorated {
		t.Fatalf("decorated title not found: %+v", got)
	}
	renamed := &Instance{ID: "r", Title: "something else", ConductorName: "personal"}
	plain := &Instance{ID: "p", Title: "conductor-personal"}
	if got := FindConductorInstance([]*Instance{plain, renamed}, "personal"); got != renamed {
		t.Fatalf("stable name should win over title: %+v", got)
	}
	if got := FindConductorInstance([]*Instance{other}, "personal"); got != nil {
		t.Fatalf("unexpected match: %+v", got)
	}
}

// TestConductorIdentity_InboxConfigFollowsConductor: the
// [conductors.<name>.inbox] override applies to a decorated title and, via
// the instance form, to a renamed conductor.
func TestConductorIdentity_InboxConfigFollowsConductor(t *testing.T) {
	setupConductorIdentityFixture(t)
	cfgPath, err := GetUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("[conductors.personal.inbox]\nmax_text_bytes = 1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ClearUserConfigCache()

	if got := ResolveInboxConfig("🎙 conductor-personal").MaxTextBytes; got != 1234 {
		t.Errorf("ResolveInboxConfig(decorated).MaxTextBytes = %d, want 1234", got)
	}
	renamed := &Instance{ID: "inbox-renamed", Title: "my assistant", ConductorName: "personal"}
	if got := ResolveInboxConfigForInstance(renamed).MaxTextBytes; got != 1234 {
		t.Errorf("ResolveInboxConfigForInstance(renamed).MaxTextBytes = %d, want 1234", got)
	}
	if got := ResolveInboxConfigForInstance(nil).MaxTextBytes; got != 0 {
		t.Errorf("ResolveInboxConfigForInstance(nil).MaxTextBytes = %d, want 0", got)
	}
}
