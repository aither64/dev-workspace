package teamruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/userstate"
)

const deletionTestOperation = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type deletionFixture struct {
	service           Service
	client            *testClient
	journal, manifest string
	unknown           Member
}

func newDeletionFixture(t *testing.T) deletionFixture {
	t.Helper()
	service, client, _ := archivePreflightFixture(t, false)
	client.lostStart = true
	client.hideThreads = true // The attempted native start has no discoverable acknowledgement.
	if _, err := service.Add(context.Background(), "one", "root-one", filepath.Join(service.Store.workspace, "work", "one"), nil, "reviewer", "gpt-6-sol", "high"); err == nil {
		t.Fatal("fixture must retain an unknown start outcome")
	}
	client.hideThreads = false
	roster, err := service.Store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	unknown := roster.Members[len(roster.Members)-1]
	if unknown.Thread != "" || !unknown.CreateAttempted || unknown.BootstrapAttempted {
		t.Fatalf("unexpected unknown reservation: %#v", unknown)
	}
	return deletionFixtureForRoster(t, service, client, unknown)
}

func deletionFixtureForRoster(t *testing.T, service Service, client *testClient, unknown Member) deletionFixture {
	t.Helper()
	workspace := service.Store.workspace
	directory := filepath.Join(workspace, "work", "one")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(directory, "portal.yml")
	for path, data := range map[string]string{
		manifest:                             "schema: 2\nslug: one\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: root-one\ncreation:\n  state: creating\n  initial_goal_attempted: false\n  initial_goal_sent: false\n",
		filepath.Join(directory, "state.md"): "---\nlifecycle: active\n---\nState\n",
		filepath.Join(directory, "plan.md"):  "Plan\n",
	} {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recovery := filepath.Join(userstate.WorkspaceDirectory(service.Store.stateRoot, "removed", workspace), "20261024-one-1")
	if err := os.MkdirAll(recovery, 0o700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(workspace, "worktrees", ".locks", "one.removal.json")
	if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"schema": 1, "slug": "one", "workspace": workspace,
		"created_at": time.Now().UTC().Format(time.RFC3339Nano), "operation_id": deletionTestOperation,
		"state": "prepared", "phase": "validated", "force": false, "tracking": "work", "recovery": recovery})
	if err := os.WriteFile(journal, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return deletionFixture{service: service, client: client, journal: journal, manifest: manifest, unknown: unknown}
}

func (fixture deletionFixture) changeJournal(t *testing.T, change func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(fixture.journal)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	change(value)
	data, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.journal, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture deletionFixture) changeRoster(t *testing.T, change func(*Roster)) {
	t.Helper()
	roster, err := fixture.service.Store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	change(roster)
	data, err := json.MarshalIndent(roster, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.service.Store.path("one"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func deletionRosterBytes(t *testing.T, fixture deletionFixture) []byte {
	t.Helper()
	data, err := os.ReadFile(fixture.service.Store.path("one"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPrepareDeletePreservesUnknownAndRetiresKnownWithoutNewStarts(t *testing.T) {
	fixture := newDeletionFixture(t)
	before := deletionRosterBytes(t, fixture)
	starts, bootstraps, projects := len(fixture.client.starts), len(fixture.client.bootstrapCalls), fixture.client.projectCreates
	if err := fixture.service.PrepareDelete(context.Background(), "one", "root-one", deletionTestOperation, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, deletionRosterBytes(t, fixture)) {
		t.Fatal("check-only changed the roster")
	}
	// Crash before publication: the durable phase alone has no roster effect.
	fixture.changeJournal(t, func(value map[string]any) { value["phase"] = "thread_retiring" })
	restarted := fixture.service
	store, err := NewStore(fixture.service.Store.stateRoot, fixture.service.Store.workspace)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Store = store
	if err := restarted.PrepareDelete(context.Background(), "one", "root-one", deletionTestOperation, false); err != nil {
		t.Fatal(err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	removed := roster.Members[len(roster.Members)-1]
	if removed.State != "removed" || removed.RemovedAt == nil {
		t.Fatalf("reservation not tombstoned: %#v", removed)
	}
	retained := removed
	retained.State, retained.RemovedAt = fixture.unknown.State, fixture.unknown.RemovedAt
	if !reflect.DeepEqual(retained, fixture.unknown) {
		t.Fatal("unknown project, attempts or settings changed")
	}
	// Crash after publication: replay under the same operation preserves bytes/time.
	published := deletionRosterBytes(t, fixture)
	if err := restarted.PrepareDelete(context.Background(), "one", "root-one", deletionTestOperation, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(published, deletionRosterBytes(t, fixture)) {
		t.Fatal("replay rewrote a tombstone")
	}
	if err := restarted.ArchiveAll(context.Background(), "one", "root-one"); err != nil {
		t.Fatal(err)
	}
	if len(fixture.client.archives) != 1 || len(fixture.client.starts) != starts || len(fixture.client.bootstrapCalls) != bootstraps || fixture.client.projectCreates != projects || len(fixture.client.resumes) != 0 || len(fixture.client.deleted) != 0 {
		t.Fatal("deletion created, resumed, bootstrapped or recycled a member")
	}
	for _, thread := range fixture.client.clearedThreads {
		if thread == fixture.unknown.Thread {
			t.Fatal("unknown submission state was cleared")
		}
	}
}

func TestPrepareDeleteRefusesInvalidContextWithoutPublication(t *testing.T) {
	for _, failure := range []string{"missing journal", "operation", "root", "workspace", "schema", "phase", "missing force", "null force", "invalid force", "extra field", "unsafe journal", "wrong manifest root"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newDeletionFixture(t)
			operation, root := deletionTestOperation, "root-one"
			switch failure {
			case "missing journal":
				if err := os.Remove(fixture.journal); err != nil {
					t.Fatal(err)
				}
			case "operation":
				operation = strings.Repeat("a", 64)
			case "root":
				root = "other-root"
			case "unsafe journal":
				if err := os.Chmod(fixture.journal, 0o644); err != nil {
					t.Fatal(err)
				}
			case "wrong manifest root":
				data, _ := os.ReadFile(fixture.manifest)
				if err := os.WriteFile(fixture.manifest, []byte(strings.ReplaceAll(string(data), "root-one", "other-root")), 0o644); err != nil {
					t.Fatal(err)
				}
			default:
				fixture.changeJournal(t, func(value map[string]any) {
					switch failure {
					case "workspace":
						value["workspace"] = "/other"
					case "schema":
						value["schema"] = 2
					case "phase":
						value["phase"] = "prepared"
					case "missing force":
						delete(value, "force")
					case "null force":
						value["force"] = nil
					case "invalid force":
						value["force"] = "false"
					case "extra field":
						value["approval"] = true
					}
				})
			}
			before := deletionRosterBytes(t, fixture)
			if err := fixture.service.PrepareDelete(context.Background(), "one", root, operation, true); err == nil {
				t.Fatal("invalid deletion context passed")
			}
			if !reflect.DeepEqual(before, deletionRosterBytes(t, fixture)) {
				t.Fatal("refusal changed roster")
			}
		})
	}
	fixture := newDeletionFixture(t)
	if err := fixture.service.PrepareDelete(context.Background(), "one", "root-one", deletionTestOperation, false); err == nil {
		t.Fatal("validated journal authorized mutation")
	}
}

func TestPrepareDeleteRefusesIneligibleReservationsAtomically(t *testing.T) {
	for _, failure := range []string{"unattempted", "missing project", "legacy project", "bootstrap", "retirement", "fork", "manifest fork", "goal attempted", "goal sent", "attempt missing", "sent missing", "sent null", "second ineligible"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newDeletionFixture(t)
			fixture.changeJournal(t, func(value map[string]any) { value["phase"] = "thread_retiring" })
			switch failure {
			case "manifest fork", "goal attempted", "goal sent", "attempt missing", "sent missing", "sent null":
				data, _ := os.ReadFile(fixture.manifest)
				value := string(data)
				switch failure {
				case "manifest fork":
					value += "forked_from: source\n"
				case "goal attempted":
					value = strings.ReplaceAll(value, "initial_goal_attempted: false", "initial_goal_attempted: true")
				case "goal sent":
					value = strings.ReplaceAll(value, "initial_goal_sent: false", "initial_goal_sent: true")
				case "attempt missing":
					value = strings.ReplaceAll(value, "  initial_goal_attempted: false\n", "")
				case "sent missing":
					value = strings.ReplaceAll(value, "  initial_goal_sent: false\n", "")
				case "sent null":
					value = strings.ReplaceAll(value, "initial_goal_sent: false", "initial_goal_sent: null")
				}
				if err := os.WriteFile(fixture.manifest, []byte(value), 0o644); err != nil {
					t.Fatal(err)
				}
			default:
				fixture.changeRoster(t, func(roster *Roster) {
					member := &roster.Members[len(roster.Members)-1]
					switch failure {
					case "unattempted":
						member.CreateAttempted = false
					case "missing project":
						member.ProjectID = ""
					case "legacy project":
						member.ProjectID = memberProjectKey(roster.Workspace, roster.Slug, roster.RootThreadID, member.Address)
					case "bootstrap":
						member.BootstrapAttempted = true
					case "retirement":
						member.RetireIntent = "remove"
					case "fork":
						roster.ForkSource = &ForkSourceSnapshot{Workspace: roster.Workspace, Slug: "source", RootThreadID: "source-root", Revision: 1, Digest: strings.Repeat("a", 64)}
					case "second ineligible":
						second := *member
						second.Address, second.Index, second.CreateAttempted = "reviewer1", 1, false
						roster.Members = append(roster.Members, second)
					}
				})
			}
			before := deletionRosterBytes(t, fixture)
			if err := fixture.service.PrepareDelete(context.Background(), "one", "root-one", deletionTestOperation, false); err == nil {
				t.Fatal("ineligible reservation passed")
			}
			if !reflect.DeepEqual(before, deletionRosterBytes(t, fixture)) {
				t.Fatal("one candidate changed before another refused")
			}
		})
	}
}

type deletionIdleHook struct {
	*testClient
	hook func()
}

func (client deletionIdleHook) RequireThreadIdle(ctx context.Context, thread, cwd string) error {
	err := client.testClient.RequireThreadIdle(ctx, thread, cwd)
	client.hook()
	return err
}

func TestPrepareDeleteKeepsKnownIdleChecksAndRevalidatesForce(t *testing.T) {
	fixture := newDeletionFixture(t)
	fixture.client.idleError = errors.New("active turn")
	if err := fixture.service.PrepareDelete(context.Background(), "one", "root-one", deletionTestOperation, true); err == nil {
		t.Fatal("active known member passed non-force deletion")
	}
	fixture.changeJournal(t, func(value map[string]any) { value["force"] = true })
	if err := fixture.service.PrepareDelete(context.Background(), "one", "root-one", deletionTestOperation, true); err != nil {
		t.Fatalf("force lost its ordinary interruption route: %v", err)
	}
	fixture = newDeletionFixture(t)
	fixture.changeJournal(t, func(value map[string]any) { value["phase"] = "thread_retiring" })
	fixture.service.Client = deletionIdleHook{fixture.client, func() { fixture.changeJournal(t, func(value map[string]any) { value["force"] = true }) }}
	before := deletionRosterBytes(t, fixture)
	if err := fixture.service.PrepareDelete(context.Background(), "one", "root-one", deletionTestOperation, false); err == nil {
		t.Fatal("changed force authorized publication")
	}
	if !reflect.DeepEqual(before, deletionRosterBytes(t, fixture)) {
		t.Fatal("force change modified reservation")
	}
}

func TestPrepareDeleteSaveFailureLeavesRetiringJournalAndReservation(t *testing.T) {
	fixture := newDeletionFixture(t)
	fixture.changeJournal(t, func(value map[string]any) { value["phase"] = "thread_retiring" })
	fixture.changeRoster(t, func(roster *Roster) {
		data, _ := json.MarshalIndent(roster, "", "  ")
		member := &roster.Members[len(roster.Members)-1]
		member.Model += strings.Repeat("m", maxRosterBytes-len(data)-2)
	})
	before := deletionRosterBytes(t, fixture)
	if err := fixture.service.PrepareDelete(context.Background(), "one", "root-one", deletionTestOperation, false); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("save bound was not exercised: %v", err)
	}
	if !reflect.DeepEqual(before, deletionRosterBytes(t, fixture)) {
		t.Fatal("failed save changed reservation")
	}
	data, _ := os.ReadFile(fixture.journal)
	if !strings.Contains(string(data), "thread_retiring") {
		t.Fatal("failed publication lost its durable recovery owner")
	}
}

func TestPrepareDeleteDoesNotRelaxOtherRetirementOrReuseProjectKeys(t *testing.T) {
	for _, action := range []string{"archive", "remove", "retire"} {
		fixture := newDeletionFixture(t)
		var err error
		switch action {
		case "archive":
			err = fixture.service.ArchiveAll(context.Background(), "one", "root-one")
		case "remove":
			err = fixture.service.Remove(context.Background(), "one", "root-one", fixture.unknown.Address)
		case "retire":
			err = fixture.service.RetireAll(context.Background(), "one", "root-one")
		}
		if err == nil {
			t.Fatalf("ordinary %s accepted unknown reservation", action)
		}
	}
	if memberProjectKey("/workspace", "one", "old-root", "reviewer0") == memberProjectKey("/workspace", "one", "new-root", "reviewer0") {
		t.Fatal("fresh root reused old project key")
	}
}

func TestPrepareDeletePreservesSourceProducedRemovedRecords(t *testing.T) {
	for _, producer := range []string{"ordinary Remove", "inherited Fork"} {
		for _, creation := range []string{"ready", "noncreating"} {
			for _, force := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/force=%t", producer, creation, force), func(t *testing.T) {
					workspace := filepath.Join(t.TempDir(), "workspace")
					store, err := NewStore(t.TempDir(), workspace)
					if err != nil {
						t.Fatal(err)
					}
					client := &testClient{projectReadError: errors.New("project verification unavailable")}
					service := Service{Store: store, Client: client, Workspace: workspace}
					slug, root := "one", "root-one"
					if producer == "inherited Fork" {
						slug, root = "source", "source-root"
					}
					if _, err := service.Add(context.Background(), slug, root, filepath.Join(workspace, "work", slug), nil, "reviewer", "gpt-6-sol", "xhigh"); err == nil {
						t.Fatal("fixture must stop before its native thread start")
					}
					client.projectReadError = nil
					if err := service.Remove(context.Background(), slug, root, "reviewer0"); err != nil {
						t.Fatalf("produce ordinary unattempted removal: %v", err)
					}
					roster, err := store.Load(slug, root)
					if err != nil {
						t.Fatal(err)
					}
					removed := roster.Members[0]
					if removed.State != "removed" || removed.RemovedAt == nil || removed.CreateAttempted || removed.Thread != "" || !projectUUIDPattern.MatchString(removed.ProjectID) {
						t.Fatalf("ordinary Remove did not produce the expected supported tombstone: %#v", removed)
					}
					if producer == "inherited Fork" {
						roster, err = service.Fork(context.Background(), roster, "one", "root-one", filepath.Join(workspace, "work", "one"), nil)
						if err != nil {
							t.Fatalf("produce fork-inherited removal: %v", err)
						}
						inherited := roster.Members[0]
						if roster.ForkSource == nil || inherited.State != "removed" || inherited.Thread != "" || inherited.ProjectID != "" || inherited.CreateAttempted || inherited.BootstrapAttempted || !reflect.DeepEqual(inherited.RemovedAt, removed.RemovedAt) {
							t.Fatalf("Fork did not preserve and clear the expected fields: %#v", inherited)
						}
					}
					fixture := deletionFixtureForRoster(t, service, client, roster.Members[0])
					manifest := "schema: 1\nslug: one\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: root-one\n"
					if creation == "ready" {
						manifest += "creation:\n  state: ready\n"
					}
					if err := os.WriteFile(fixture.manifest, []byte(manifest), 0o644); err != nil {
						t.Fatal(err)
					}
					fixture.changeJournal(t, func(value map[string]any) { value["force"] = force })
					before := deletionRosterBytes(t, fixture)
					for _, checkOnly := range []bool{true, false, false} {
						if !checkOnly {
							fixture.changeJournal(t, func(value map[string]any) { value["phase"] = "thread_retiring" })
						}
						if err := service.PrepareDelete(context.Background(), "one", "root-one", deletionTestOperation, checkOnly); err != nil {
							t.Fatalf("existing valid removal refused (checkOnly=%t): %v", checkOnly, err)
						}
						if !reflect.DeepEqual(before, deletionRosterBytes(t, fixture)) {
							t.Fatal("existing removal identity, time or roster bytes changed")
						}
					}
					if len(client.starts) != 0 || len(client.resumes) != 0 || len(client.bootstrapCalls) != 0 || len(client.sends) != 0 || len(client.archives) != 0 || len(client.deleted) != 0 || len(client.clearedThreads) != 0 || len(client.idleChecks) != 0 {
						t.Fatal("removed record triggered native work or cleanup")
					}
				})
			}
		}
	}
}
