package teamruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type semanticTestClient struct {
	*testClient
	tokens       map[string]string
	failure      string
	busy         string
	afterObserve func(string)
	rootStates   []workspacecodex.ArchiveState
}

func (client *semanticTestClient) ProveArchivedRootThread(ctx context.Context, id, cwd string) (workspacecodex.ArchiveState, error) {
	if len(client.rootStates) > 0 {
		value := client.rootStates[0]
		client.rootStates = client.rootStates[1:]
		return value, nil
	}
	return client.ProveArchivedThread(ctx, id, cwd, "")
}
func (client *semanticTestClient) ObserveThreadIdentity(_ context.Context, id, cwd, project string, _ workspacecodex.ArchiveState) (workspacecodex.ThreadObservation, error) {
	if client.afterObserve != nil {
		client.afterObserve(id)
	}
	if id == client.failure {
		return workspacecodex.ThreadObservation{}, &workspacecodex.ObservationError{Code: "submission_unverified", Message: "opaque failure"}
	}
	return workspacecodex.ThreadObservation{Schema: 1, ThreadID: id, Cwd: cwd, ActivityKnown: true, Idle: id != client.busy, ActivityToken: client.tokens[id], Diagnostics: []workspacecodex.ObservationDiagnostic{}}, nil
}

func TestBoundRetainedObservationUsesCurrentAndSealedHistoryOnly(t *testing.T) {
	for _, fault := range []string{"none", "busy-ancestor", "unknown-ancestor", "foreign-ancestor", "stale-binding", "archived-no-authority", "foreign-unsealed"} {
		t.Run(fault, func(t *testing.T) {
			store, err := NewStore(t.TempDir(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			statePath, authorityPath := boundReportingFixture(t, store)
			raw, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			var state codexproviders.State
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			op1, op2 := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
			digest1 := ""
			inc := state.Bindings["bound"].Incarnation
			for i, op := range []string{op1, op2} {
				evidence := codexproviders.BindingEvidence{Schema: 1, OperationID: op, Workspace: state.Workspace, Root: state.Root, Socket: state.Socket, PreviousBindingRevision: uint64(i + 1), CommittedBindingRevision: uint64(i + 2), SnapshotDigest: strings.Repeat("e", 64)}
				for _, row := range [][2]string{{"lead", "origin-root"}, {"implementer0", "origin-member"}} {
					old, next := row[1], "middle-"+row[1]
					previous := ""
					if i == 1 {
						old, next, previous = next, "current-"+strings.TrimPrefix(row[1], "origin-"), op1
					}
					evidence.Participants = append(evidence.Participants, codexproviders.BindingParticipant{Key: codexproviders.BindingKey{Workspace: store.workspace, Slug: "bound", RootThreadID: "origin-root", Incarnation: inc, Member: row[0], OriginalThreadID: row[1]}, OldThreadID: old, NewThreadID: next, OldProvider: "openai", NewProvider: "openai", PreviousOperationID: previous, PreviousEvidenceDigest: digest1})
				}
				b, _ := json.Marshal(evidence)
				sum := sha256.Sum256(b)
				digest := hex.EncodeToString(sum[:])
				if err := os.WriteFile(filepath.Join(filepath.Dir(statePath), op+".snapshot.json.evidence"), b, 0600); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					digest1 = digest
				} else {
					for original, c := range state.Bindings["bound"].Conversations {
						c.OperationID = op
						c.EvidenceDigest = digest
						state.Bindings["bound"].Conversations[original] = c
					}
				}
			}
			state.BindingRevision = 3
			binding := state.Bindings["bound"]
			binding.Revision = 3
			state.Bindings["bound"] = binding
			b, _ := json.Marshal(state)
			if err := os.WriteFile(statePath, b, 0600); err != nil {
				t.Fatal(err)
			}
			a, err := os.ReadFile(authorityPath)
			if err != nil {
				t.Fatal(err)
			}
			a = []byte(strings.Replace(string(a), `"codex_binding_revision":2`, `"codex_binding_revision":3`, 1))
			if err := os.WriteFile(authorityPath, a, 0600); err != nil {
				t.Fatal(err)
			}
			project := "11111111-1111-1111-1111-111111111111"
			cwd := filepath.Join(store.workspace, "work", "bound")
			base := &testClient{archived: map[string]bool{}}
			tokens := map[string]string{}
			for _, id := range []string{"current-root", "middle-origin-root", "origin-root", "current-member", "middle-origin-member", "origin-member"} {
				source := "vscode"
				var p *string
				if strings.Contains(id, "member") {
					source = "appServer"
					p = &project
				}
				base.threads = append(base.threads, codex.ThreadMetadata{ID: id, Cwd: cwd, Source: source, ProjectID: p})
				tokens[id] = id + "-token"
			}
			client := &semanticTestClient{testClient: base, tokens: tokens}
			if fault == "busy-ancestor" {
				client.busy = "origin-root"
			}
			if fault == "unknown-ancestor" {
				client.failure = "middle-origin-member"
			}
			if fault == "foreign-ancestor" {
				base.threads[1].Cwd += "-foreign"
			}
			if fault == "foreign-unsealed" {
				base.threads = append(base.threads, codex.ThreadMetadata{ID: "unsealed", Cwd: cwd, Source: "exec"})
			}
			if fault == "stale-binding" {
				client.afterObserve = func(string) {
					state.BindingRevision = 4
					b := state.Bindings["bound"]
					b.Revision = 4
					state.Bindings["bound"] = b
					raw, _ := json.Marshal(state)
					if err := os.WriteFile(statePath, raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if fault == "archived-no-authority" {
				manifest, err := os.ReadFile(filepath.Join(cwd, "portal.yml"))
				if err != nil {
					t.Fatal(err)
				}
				manifest = append(manifest, []byte("finalized_at: '2026-11-25T00:00:00Z'\n")...)
				if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), manifest, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cwd, "state.md"), []byte("---\nlifecycle: archived\n---\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(store.workspace, "archive"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(cwd, filepath.Join(store.workspace, "archive", "bound")); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(authorityPath); err != nil {
					t.Fatal(err)
				}
				for _, thread := range base.threads {
					base.archived[thread.ID] = true
				}
			}
			service := Service{Store: store, Client: client, Workspace: store.workspace}
			result, err := service.ObserveRetained(context.Background(), "bound", "origin-root", "retained-identity")
			if err != nil {
				t.Fatal(err)
			}
			if fault == "none" || fault == "archived-no-authority" || fault == "busy-ancestor" {
				if !result.ActivityKnown || len(result.Subjects) != 6 || result.Subjects[0].ThreadID != "current-root" {
					t.Fatal("retained lineage not proved", result)
				}
				if result.Idle == (fault == "busy-ancestor") {
					t.Fatal("ancestor activity omitted", result)
				}
			} else if result.ActivityKnown || result.Idle {
				t.Fatal("unsafe retained proof", fault, result)
			}
			if len(base.archives) != 0 || len(base.resumes) != 0 || len(base.sends) != 0 {
				t.Fatal("observation performed native effects", base)
			}
			if fault == "archived-no-authority" {
				if _, err := store.ResolveConversation("bound", "origin-root", "lead", "origin-root"); err == nil {
					t.Fatal("archived observation granted execution")
				}
			}
		})
	}
}

func TestObserveRetainedAggregatesRootAndArchivedMemberWithoutSettingsActivity(t *testing.T) {
	service, base, members := archivePreflightFixture(t, false)
	client := &semanticTestClient{testClient: base, tokens: map[string]string{"root-one": "root-token", members[0].Thread: "archived-token", members[1].Thread: "member-token"}}
	service.Client = client
	first, err := service.ObserveRetained(context.Background(), "one", "root-one", "bound-target")
	if err != nil || !first.ActivityKnown || !first.Idle || len(first.Subjects) != 3 || first.LastActivityAt != nil {
		t.Fatalf("observation=%#v, %v", first, err)
	}
	if first.Subjects[0].ArchiveState != "active" || first.Subjects[1].ArchiveState != "archived" || first.Subjects[2].ArchiveState != "active" {
		t.Fatalf("archival evidence=%#v", first.Subjects)
	}
	_, err = service.Store.Update(context.Background(), "one", "root-one", false, func(roster *Roster) error { roster.Members[0].Model = "another-model"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	settings, err := service.ObserveRetained(context.Background(), "one", "root-one", "bound-target")
	if err != nil || settings.ActivityToken != first.ActivityToken {
		t.Fatalf("settings became activity: %#v %v", settings, err)
	}
	client.tokens[members[1].Thread] = "new-turn"
	changed, _ := service.ObserveRetained(context.Background(), "one", "root-one", "bound-target")
	if changed.ActivityToken == first.ActivityToken {
		t.Fatal("member turn did not change aggregate")
	}
	if len(base.archives) != 0 || len(base.clearedThreads) != 0 || len(base.resumes) != 0 {
		t.Fatal("observation mutated retained conversations")
	}
}

func TestRetainedActivityTokenExcludesArchivalProofAndKeepsOldPayload(t *testing.T) {
	identities := [][3]string{{"member", "thread", "project"}}
	subjects := []ObservationSubject{{Address: "lead", ThreadID: "root", ActivityToken: "turn", Idle: true}}
	old := workspacecodex.ObservationToken([]any{1, identities, subjects})
	for _, state := range []string{"active", "fresh", "archived"} {
		subjects[0].ArchiveState = state
		if token := retainedActivityToken(identities, subjects); token != old {
			t.Fatalf("%s changed activity", state)
		}
	}
	subjects[0].ActivityToken = "new-turn"
	if retainedActivityToken(identities, subjects) == old {
		t.Fatal("actual turn was omitted")
	}
}

func TestObserveRetainedRefusesUnknownOrDriftingArchivalState(t *testing.T) {
	for _, states := range [][]workspacecodex.ArchiveState{{workspacecodex.ArchiveUnknown}, {workspacecodex.ArchiveActive, workspacecodex.ArchiveArchived}} {
		service, base, members := archivePreflightFixture(t, false)
		service.Client = &semanticTestClient{testClient: base, tokens: map[string]string{"root-one": "root", members[0].Thread: "one", members[1].Thread: "two"}, rootStates: states}
		result, err := service.ObserveRetained(context.Background(), "one", "root-one", "bound")
		if err != nil || result.ActivityKnown || result.Idle {
			t.Fatalf("unsafe archival state=%#v %v", result, err)
		}
	}
}

func TestObserveRetainedUnknownSubmissionAndForeignSourceFailClosed(t *testing.T) {
	service, base, members := archivePreflightFixture(t, true)
	client := &semanticTestClient{testClient: base, tokens: map[string]string{"root-one": "root", members[0].Thread: "member", members[1].Thread: "member"}, failure: members[1].Thread}
	service.Client = client
	result, err := service.ObserveRetained(context.Background(), "one", "root-one", "bound-target")
	if err != nil || result.ActivityKnown || result.Idle || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "submission_unverified" {
		t.Fatalf("unknown=%#v, %v", result, err)
	}
	client.failure = ""
	listedBefore := len(base.listOptions)
	base.threads = append(base.threads, codex.ThreadMetadata{ID: "unknown-native", Cwd: base.threads[0].Cwd, Source: "exec"})
	result, _ = service.ObserveRetained(context.Background(), "one", "root-one", "bound-target")
	if result.ActivityKnown || result.Idle {
		t.Fatal("unknown noninteractive same-CWD conversation succeeded")
	}
	for _, options := range base.listOptions[listedBefore:] {
		if !reflect.DeepEqual(options.SourceKinds, workspacecodex.ArchiveDiscoverySourceKinds()) {
			t.Fatalf("incomplete discovery sources: %#v", options)
		}
	}
}
