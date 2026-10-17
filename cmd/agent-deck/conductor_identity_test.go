package main

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// `conductor setup` stamps the stable conductor name on the session it
// creates, and on a re-run finds the conductor even after its title was
// decorated or renamed (instead of registering a duplicate), re-stamping it.
func TestRegisterConductorSession_StoresStableName(t *testing.T) {
	sandboxConductorHome(t)
	spec, err := session.GetConductorAgentSpec(session.ConductorAgentClaude)
	if err != nil {
		t.Fatal(err)
	}

	instances, inst, existed := registerConductorSession(nil, "personal", spec)
	if existed || len(instances) != 1 {
		t.Fatalf("fresh setup: existed=%v len=%d", existed, len(instances))
	}
	if inst.ConductorName != "personal" || !inst.IsConductor {
		t.Fatalf("fresh setup did not stamp identity: name=%q is_conductor=%v", inst.ConductorName, inst.IsConductor)
	}

	// Renamed after setup: the re-run must reuse it, not add a second one.
	inst.Title = "my assistant"
	instances, again, existed := registerConductorSession(instances, "personal", spec)
	if !existed || again != inst || len(instances) != 1 {
		t.Fatalf("re-run after rename: existed=%v same=%v len=%d", existed, again == inst, len(instances))
	}

	// Legacy record (no stored name) with a decorated title: reused and
	// stamped so later renames keep resolving.
	legacy := session.NewInstanceWithGroupAndTool("🎙 conductor-ops", "/tmp", "conductor", spec.Agent)
	got := []*session.Instance{legacy}
	got, reused, existed := registerConductorSession(got, "ops", spec)
	if !existed || reused != legacy || len(got) != 1 {
		t.Fatalf("legacy decorated re-run: existed=%v same=%v len=%d", existed, reused == legacy, len(got))
	}
	if legacy.ConductorName != "ops" || !legacy.IsConductor {
		t.Fatalf("legacy re-run did not stamp identity: name=%q is_conductor=%v", legacy.ConductorName, legacy.IsConductor)
	}
}
