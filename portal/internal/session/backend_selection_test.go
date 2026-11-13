package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestBackendSelectionRequiresPositiveEvidenceAndNeverFallsBack(t *testing.T) {
	legacy := RuntimeAuthority{Schema: 1, State: "ready", Workspace: "/srv/workspace", Slug: "example", TmuxSocket: "/run/tmux.sock", TmuxSessionID: "$1"}
	directory := filepath.Join(t.TempDir(), "authority")
	path := filepath.Join(directory, "example.json")
	legacyBytes := backendJSON(t, legacy)
	backendWrite(t, path, legacyBytes)
	validatedLegacy, err := LoadRuntimeAuthority(directory, "example", "/srv/workspace")
	if err != nil {
		t.Fatal(err)
	}
	retained := agentbackend.ConversationRef{
		Session: agentbackend.SessionRef{Workspace: legacy.Workspace, Slug: legacy.Slug},
		Backend: agentbackend.Codex, Member: "lead", NativeID: "retained-root",
	}
	binding, authority := backendBindingFixture(), backendAuthorityFixture()
	conflicting := authority
	conflicting.BindingRevision++
	unsupported := binding
	unsupported.BackendContract++
	invalidLegacy := legacy
	invalidLegacy.Schema = 2
	for _, tc := range []struct {
		name     string
		evidence BackendEvidence
		kind     agentbackend.Kind
		want     error
	}{
		{"absence", BackendEvidence{}, "", agentbackend.ErrNotBound},
		{"terminal-only-legacy", BackendEvidence{LegacyAuthority: &validatedLegacy}, agentbackend.Codex, nil},
		{"cold-retained-legacy", BackendEvidence{LegacyConversation: &retained}, agentbackend.Codex, nil},
		{"pi-held-without-runtime", BackendEvidence{PiBinding: &binding}, agentbackend.Pi, nil},
		{"pi-live", BackendEvidence{PiBinding: &binding, PiAuthority: &authority}, agentbackend.Pi, nil},
		{"orphan-projection", BackendEvidence{PiAuthority: &authority}, "", agentbackend.ErrBindingConflict},
		{"mixed-live", BackendEvidence{PiBinding: &binding, LegacyAuthority: &validatedLegacy}, "", agentbackend.ErrBindingConflict},
		{"mixed-retained", BackendEvidence{PiBinding: &binding, LegacyConversation: &retained}, "", agentbackend.ErrBindingConflict},
		{"orphan-and-legacy", BackendEvidence{PiAuthority: &authority, LegacyAuthority: &validatedLegacy}, "", agentbackend.ErrBindingConflict},
		{"revision-conflict", BackendEvidence{PiBinding: &binding, PiAuthority: &conflicting}, "", agentbackend.ErrBindingConflict},
		{"unknown-no-fallback", BackendEvidence{PiBinding: &unsupported, LegacyAuthority: &validatedLegacy}, "", agentbackend.ErrUnsupportedBinding},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected, err := SelectBackend(legacy.Workspace, legacy.Slug, tc.evidence)
			if selected.Kind != tc.kind || (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("selected %#v, err %v; want %s, %v", selected, err, tc.kind, tc.want)
			}
		})
	}
	if _, err := SelectBackend(legacy.Workspace, legacy.Slug, BackendEvidence{LegacyAuthority: &invalidLegacy}); err == nil {
		t.Fatal("invalid legacy record became Codex evidence")
	}
	retained.Session.Workspace = "/other"
	if _, err := SelectBackend(legacy.Workspace, legacy.Slug, BackendEvidence{LegacyConversation: &retained}); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatalf("foreign retained evidence accepted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(legacyBytes, after) {
		t.Fatalf("legacy evidence rewritten: %v", err)
	}
}
