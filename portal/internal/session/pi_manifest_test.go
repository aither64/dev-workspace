package session

import (
	"errors"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"gopkg.in/yaml.v3"
)

func TestPiManifestStrictProjectionAndBinding(t *testing.T) {
	data := "schema: 4\nslug: example\nbackend:\n  kind: pi\n  contract: 1\n  session_incarnation: \"" + strings.Repeat("a", 64) + "\"\ncreation:\n  state: creating\n  goal_sha256: \"" + strings.Repeat("b", 64) + "\"\n  initial_goal_sent: false\nrepositories: []\n"
	var manifest Manifest
	if err := decodeManifest([]byte(data), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Validate("example"); err != nil {
		t.Fatal(err)
	}
	if !manifest.scopeExplicit {
		t.Fatal("explicit empty repository scope lost")
	}
	encoded, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "repositories: []") || strings.Contains(string(encoded), "codex:") {
		t.Fatalf("invalid Pi projection: %s", encoded)
	}
	missingScope := manifest
	missingScope.Repositories = nil
	if _, err := yaml.Marshal(missingScope); err == nil {
		t.Fatal("typed Pi writer emitted null repository scope")
	}
	var reopened Manifest
	if err := decodeManifest(encoded, &reopened); err != nil {
		t.Fatal(err)
	}
	binding := backendBindingFixture()
	if err := reopened.ValidateBackendBinding(binding); err != nil {
		t.Fatal(err)
	}
	binding.SessionIncarnation = strings.Repeat("f", 64)
	if err := reopened.ValidateBackendBinding(binding); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatalf("foreign binding accepted: %v", err)
	}
	for _, changed := range []string{
		strings.Replace(data, "schema: 4", "schema: 1", 1),
		strings.Replace(data, "repositories: []\n", "", 1),
		strings.Replace(data, "contract: 1", "contract: 2", 1),
		strings.Replace(data, "kind: pi", "kind: codex", 1),
		strings.Replace(data, "state: creating", "state: ready", 1),
		strings.Replace(data, "initial_goal_sent: false", "initial_goal_sent: null", 1),
		data + "codex: {}\n",
		data + "tmux: {}\n",
		strings.Replace(data, "  contract: 1", "  contract: 1\n  contract: 1", 1),
	} {
		var invalid Manifest
		err := decodeManifest([]byte(changed), &invalid)
		if err == nil {
			err = invalid.Validate("example")
		}
		if err == nil {
			t.Fatalf("invalid Pi manifest accepted: %s", changed)
		}
	}
}

func TestPiManifestArchivePreservesVersionAndIdentity(t *testing.T) {
	for _, schema := range []int{4, 5} {
		manifest := Manifest{Schema: schema, Slug: "example", Backend: &ManifestBackend{Kind: agentbackend.Pi, Contract: 1, SessionIncarnation: strings.Repeat("a", 64)}, Creation: Creation{State: "ready", GoalSHA256: strings.Repeat("b", 64), InitialGoalSent: true}, Repositories: []Repository{{Name: "source", Project: "source", Branch: "feature", DefaultBranch: "master"}}, ArchiveOperationID: strings.Repeat("c", 64), FinalizedAt: "2026-11-22T09:00:00Z"}
		if schema == 5 {
			manifest.ForkedFrom = "source"
			manifest.Creation.GoalSHA256 = ""
			manifest.Creation.InitialGoalSent = false
		}
		encoded, err := yaml.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		var reopened Manifest
		if err := decodeManifest(encoded, &reopened); err != nil {
			t.Fatal(err)
		}
		if err := reopened.Validate("example"); err != nil {
			t.Fatal(err)
		}
		if reopened.Schema != schema || *reopened.Backend != *manifest.Backend || reopened.ArchiveOperationID != manifest.ArchiveOperationID || reopened.FinalizedAt != manifest.FinalizedAt {
			t.Fatal("archive lost Pi identity", reopened)
		}
		reopened.ArchiveOperationID = ""
		if reopened.Validate("example") == nil {
			t.Fatal("unpaired Pi finalization accepted")
		}
	}
}
