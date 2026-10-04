package teamruntime

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aither64/codex-web/codex"
)

func archivePreflightFixture(t *testing.T, rootArchived bool) (Service, *testClient, []Member) {
	t.Helper()
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{archived: map[string]bool{"root-one": rootArchived}}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	client.threads = append(client.threads, codex.ThreadMetadata{ID: "root-one", Cwd: cwd, Source: "vscode"})
	var members []Member
	for _, role := range []string{"architect", "implementer"} {
		member, err := service.Add(context.Background(), "one", "root-one", cwd, nil, role, "gpt-6-sol", "high")
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, member)
	}
	client.archived[members[0].Thread] = true // Lost acknowledgement before the roster write.
	return service, client, members
}

func TestRequireArchiveReadyAllProvesActiveAndArchivedRootsWithPartialTeam(t *testing.T) {
	for _, rootArchived := range []bool{false, true} {
		t.Run(map[bool]string{false: "active root", true: "archived root"}[rootArchived], func(t *testing.T) {
			service, client, _ := archivePreflightFixture(t, rootArchived)
			client.listPageSize = 1
			before, err := service.Store.Load("one", "root-one")
			if err != nil {
				t.Fatal(err)
			}
			listedBefore := len(client.listOptions)
			if err := service.RequireArchiveReadyAll(context.Background(), "one", "root-one"); err != nil {
				t.Fatal(err)
			}
			wantSources := []string{"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview",
				"subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown"}
			for _, options := range client.listOptions[listedBefore:] {
				if !reflect.DeepEqual(options.SourceKinds, wantSources) {
					t.Fatalf("archive discovery omitted source kinds: %#v", options)
				}
			}
			after, err := service.Store.Load("one", "root-one")
			if err != nil || !reflect.DeepEqual(before, after) || len(client.archives) != 0 ||
				len(client.starts) != 2 || len(client.resumes) != 0 || len(client.deleted) != 0 || len(client.clearedThreads) != 0 {
				t.Fatalf("read-only proof changed state: before=%#v after=%#v err=%v", before, after, err)
			}
		})
	}
}

func TestRequireArchiveReadyAllRefusesUnknownThreadsWithEitherRootState(t *testing.T) {
	for _, rootArchived := range []bool{false, true} {
		for _, source := range []string{"cli", "exec", "appServer", "subAgent", "unknown"} {
			service, client, _ := archivePreflightFixture(t, rootArchived)
			client.threads = append(client.threads, codex.ThreadMetadata{
				ID: "foreign", Cwd: filepath.Join(service.Workspace, "work", "one"), Source: source,
			})
			if err := service.RequireArchiveReadyAll(context.Background(), "one", "root-one"); err == nil {
				t.Fatalf("unknown %s conversation passed with archived root=%v", source, rootArchived)
			}
			if len(client.archives) != 0 || len(client.deleted) != 0 || len(client.clearedThreads) != 0 {
				t.Fatal("unknown-thread refusal mutated conversations")
			}
		}
	}
}

func TestRequireArchiveReadyAllRefusesBusyUnresolvedAndUnstableRetainedMembers(t *testing.T) {
	for _, failure := range []string{"busy", "queued", "prompt", "unresolved active", "unresolved archived", "root attempts",
		"unmaterialized", "wrong cwd", "wrong project", "project drift", "archive state drift", "creating", "replace", "remove", "missing discovery", "bad cursor"} {
		t.Run(failure, func(t *testing.T) {
			service, client, members := archivePreflightFixture(t, true)
			active, archived := members[1], members[0]
			switch failure {
			case "busy", "queued", "prompt":
				client.idleErrors = map[string]error{active.Thread: errors.New(failure)}
			case "unresolved active", "unresolved archived", "root attempts":
				thread := active.Thread
				if failure == "unresolved archived" {
					thread = archived.Thread
				} else if failure == "root attempts" {
					thread = "root-one"
				}
				client.unresolvedAttempts = map[string]bool{thread: true}
			case "unmaterialized":
				client.unmaterialized = map[string]bool{active.Thread: true}
			case "project drift", "archive state drift":
				client.archiveIdleAfter = func(threadID string) {
					if threadID != archived.Thread {
						return
					}
					if failure == "archive state drift" {
						client.archived[threadID] = false
						return
					}
					for i := range client.threads {
						if client.threads[i].ID == threadID {
							wrong := "00000000-0000-7000-8000-000000000999"
							client.threads[i].ProjectID = &wrong
						}
					}
				}
			case "wrong cwd", "wrong project":
				for i := range client.threads {
					if client.threads[i].ID == active.Thread {
						if failure == "wrong cwd" {
							client.threads[i].Cwd += "-other"
							client.ignoreListCwd = true
						} else {
							wrong := "00000000-0000-7000-8000-000000000999"
							client.threads[i].ProjectID = &wrong
						}
					}
				}
			case "creating", "replace", "remove":
				_, err := service.Store.Update(context.Background(), "one", "root-one", false, func(roster *Roster) error {
					if failure == "creating" {
						roster.Members[1].State = "creating"
					} else {
						roster.Members[1].RetireIntent = failure
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			case "missing discovery":
				client.hideThreads = true
			case "bad cursor":
				client.archived["root-one"] = false
				client.listPageSize, client.emptyNextCursor = 1, true
			}
			if err := service.RequireArchiveReadyAll(context.Background(), "one", "root-one"); err == nil {
				t.Fatal("invalid archive preflight passed")
			}
			if len(client.archives) != 0 || len(client.starts) != 2 || len(client.deleted) != 0 || len(client.resumes) != 0 || len(client.clearedThreads) != 0 {
				t.Fatal("preflight refusal mutated retained state")
			}
		})
	}
}

func TestRequireArchiveReadyAllHandlesRootOnlyAndRemovedMembers(t *testing.T) {
	service, client, members := archivePreflightFixture(t, false)
	_, err := service.Store.Update(context.Background(), "one", "root-one", false, func(roster *Roster) error {
		for i := range roster.Members {
			roster.Members[i].State = "removed"
			removedAt := roster.Members[i].AddedAt
			roster.Members[i].RemovedAt = &removedAt
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	client.threads = client.threads[:1]
	if err := service.RequireArchiveReadyAll(context.Background(), "one", "root-one"); err != nil {
		t.Fatalf("removed members %v blocked root proof: %v", members, err)
	}
	if err := service.RequireArchiveReadyAll(context.Background(), "two", "root-one"); err == nil {
		t.Fatal("root from another directory passed root-only proof")
	}
	service.Store, err = NewStore(t.TempDir(), service.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RequireArchiveReadyAll(context.Background(), "one", "root-one"); err != nil {
		t.Fatalf("root-only session without a roster: %v", err)
	}
}
