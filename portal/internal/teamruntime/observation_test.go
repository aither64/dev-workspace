package teamruntime

import (
	"context"
	"reflect"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type semanticTestClient struct {
	*testClient
	tokens     map[string]string
	failure    string
	rootStates []workspacecodex.ArchiveState
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
	if id == client.failure {
		return workspacecodex.ThreadObservation{}, &workspacecodex.ObservationError{Code: "submission_unverified", Message: "opaque failure"}
	}
	return workspacecodex.ThreadObservation{Schema: 1, ThreadID: id, Cwd: cwd, ActivityKnown: true, Idle: true, ActivityToken: client.tokens[id], Diagnostics: []workspacecodex.ObservationDiagnostic{}}, nil
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
