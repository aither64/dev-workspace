package session

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"gopkg.in/yaml.v3"
)

func piForkIntentFixture(t *testing.T) (piproviders.Catalog, PiCreationIntent) {
	t.Helper()
	c, old := piIntentFixture(t)
	i := old
	i.Schema = 5
	i.Slug = "fork"
	i.SessionIncarnation = strings.Repeat("7", 64)
	i.ReceiptID = strings.Repeat("8", 64)
	i.GoalSHA256 = ""
	i.InitialInputID = ""
	i.Conversations = []PiReservedConversation{{"lead", strings.Repeat("9", 64)}, {"implementer0", strings.Repeat("a", 64)}}
	policy := old.ProviderSelection.Policy.Public()
	policy.Session = i.SessionRef()
	policy.Overrides = append([]piproviders.Override{}, policy.Overrides...)
	for n := range policy.Overrides {
		policy.Overrides[n].Member.Incarnation = i.Conversations[1].Incarnation
	}
	var err error
	i.ProviderSelection, err = FreezePiCreationSelection(c, policy, i.Conversations)
	if err != nil {
		t.Fatal(err)
	}
	i.Fork = &PiForkIntent{Source: old.SessionRef(), SourceCreationID: old.ReceiptID, SourceBindingRevision: 2, SourceRosterRevision: 1, SelectedMember: "implementer0", LeadAccess: "workspace_write", RuntimeRequestID: "33333333-3333-4333-8333-333333333333", Members: []PiForkMember{}}
	for n, d := range i.Conversations {
		source := agentbackend.ConversationRef{Session: old.SessionRef(), Backend: agentbackend.Pi, Member: d.Address, Incarnation: old.Conversations[n].Incarnation, NativeID: []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}[n]}
		nativeID := []string{"44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555"}[n]
		selection := policy.Default
		for _, o := range policy.Overrides {
			if o.Member.Address == d.Address {
				selection = o.Value
			}
		}
		selection.Revision = 3
		origin := &piruntime.ForkOrigin{Source: agentbackend.ExecutionHandle{Conversation: source, Generation: strings.Repeat("b", 64)}, Snapshot: piruntime.ForkSnapshot{Native: piruntime.NativeForkSnapshot{SourceFile: filepath.Join(old.Workspace, "history", d.Address+".jsonl"), SourceCwd: filepath.Join(old.Workspace, "work", old.Slug), SourceNativeID: source.NativeID, EntriesDigest: strings.Repeat("c", 64), EntryCount: 2, LeafID: "leaf"}, HistoryRef: strings.Repeat("d", 64), Settings: old.ProviderSelection.Members[n].Settings.Effective()}}
		origin.Snapshot.Settings.Revision = 4
		i.Fork.Members = append(i.Fork.Members, PiForkMember{Address: d.Address, SourceIncarnation: source.Incarnation, CreationID: i.ReceiptID, NativeID: nativeID, State: "ready", AddedAt: "2026-11-18T20:00:00Z", Origin: origin, Selection: &selection, SelectionSource: "session_default", CatalogDigest: c.Digest, PolicyRevision: 5})
	}
	if err = i.Validate(c); err != nil {
		t.Fatal(err)
	}
	return c, i
}
func TestPiForkIntentStrictNoInputUnionAndAppliedProvenance(t *testing.T) {
	var generic any
	if DecodePiCreationJSON([]byte(`{"field":null}`), &generic, 4096) == nil {
		t.Fatal("untyped decoding accepted null outside native snapshot policy")
	}
	_, i := piForkIntentFixture(t)
	for _, value := range []any{i, PiCreationJournal{PiCreationIntent: i, State: "creating"}} {
		data, _ := json.Marshal(value)
		if strings.Contains(string(data), "goal_sha256") || strings.Contains(string(data), "initial_input_id") {
			t.Fatal("fork invented an input")
		}
		if _, ok := value.(PiCreationJournal); ok {
			if _, e := DecodePiCreationJournal(data); e != nil {
				t.Fatal(e)
			}
		} else {
			if _, e := DecodePiCreationIntent(data); e != nil {
				t.Fatal(e)
			}
		}
		for _, key := range []string{"goal_sha256", "initial_input_id"} {
			var raw map[string]any
			_ = json.Unmarshal(data, &raw)
			raw[key] = ""
			bad, _ := json.Marshal(raw)
			if _, ok := value.(PiCreationJournal); ok {
				if _, e := DecodePiCreationJournal(bad); e == nil {
					t.Fatal("fork key accepted", key)
				}
			} else if _, e := DecodePiCreationIntent(bad); e == nil {
				t.Fatal("fork key accepted", key)
			}
		}
	}
	if i.Fork.Members[0].Origin.Snapshot.Settings.Revision != 4 || i.ProviderSelection.Members[0].Settings.Revision != 1 {
		t.Fatal("applied state relabeled as desired")
	}
	data, _ := json.Marshal(i)
	var changed PiCreationIntent
	_ = json.Unmarshal(data, &changed)
	changed.Fork.Members[1].Origin.Snapshot.Settings.Model = "other"
	if changed.ValidateShape() == nil {
		t.Fatal("applied/desired mismatch accepted")
	}
	_, old := piIntentFixture(t)
	data, _ = json.Marshal(old)
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	raw["fork"] = nil
	data, _ = json.Marshal(raw)
	if _, e := DecodePiCreationIntent(data); e == nil {
		t.Fatal("schema4 accepted fork key")
	}
}
func TestPiForkManifestHasNoBootstrapProjection(t *testing.T) {
	_, i := piForkIntentFixture(t)
	m := Manifest{Schema: 5, Slug: i.Slug, ForkedFrom: i.Fork.Source.Slug, Backend: &ManifestBackend{Kind: agentbackend.Pi, Contract: 1, SessionIncarnation: i.SessionIncarnation}, Creation: Creation{State: "ready"}, Repositories: []Repository{}}
	data, e := yaml.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(data), "goal_sha256") || strings.Contains(string(data), "initial_goal") || strings.Contains(string(data), "codex:") {
		t.Fatal(string(data))
	}
	var decoded Manifest
	if e = decodeManifest(data, &decoded); e != nil {
		t.Fatal(e)
	}
	if e = decoded.Validate(i.Slug); e != nil {
		t.Fatal(e)
	}
}
