package teamruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func reportingFixture(t *testing.T, store *Store, slug, root, thread string) {
	t.Helper()
	dir := filepath.Join(store.workspace, "work", slug)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"portal.yml": fmt.Sprintf("schema: 1\nslug: %s\ncodex:\n  thread_id: %s\nrepositories: []\nartifacts: []\n", slug, root), "state.md": "---\nlifecycle: active\n---\n", "plan.md": "Reporting fixture\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := store.Update(context.Background(), slug, root, true, func(r *Roster) error {
		r.Members = []Member{{Address: "implementer0", Role: "implementer", Thread: thread, State: "ready", AddedAt: time.Now().UTC(), Behavior: "implementer", Access: "workspace_write", ProjectID: "11111111-1111-1111-1111-111111111111"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveNativeReporterAcrossSessions(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reportingFixture(t, store, "one", "root-one", "thread-one")
	reportingFixture(t, store, "two", "root-two", "thread-two")
	for _, slug := range []string{"one", "two"} {
		reporter, err := store.ResolveReporter("thread-" + slug)
		if err != nil || reporter.Slug != slug || reporter.RootThreadID != "root-"+slug || reporter.Member.Address != "implementer0" {
			t.Fatalf("%s: %#v %v", slug, reporter, err)
		}
	}
	for _, id := range []string{"", "unknown", "root-one"} {
		if _, err := store.ResolveReporter(id); err == nil {
			t.Fatalf("accepted %q", id)
		}
	}
}

func TestResolveReporterRejectsStaleAndAmbiguousIdentities(t *testing.T) {
	for _, scenario := range []string{"retiring", "removed", "archived", "lead-changed", "duplicate", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			store, err := NewStore(t.TempDir(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			reportingFixture(t, store, "one", "root-one", "thread-one")
			switch scenario {
			case "retiring", "removed":
				_, err = store.Update(context.Background(), "one", "root-one", false, func(r *Roster) error {
					if scenario == "retiring" {
						r.Members[0].RetireIntent = "remove"
					} else {
						now := time.Now().UTC()
						r.Members[0].State = "removed"
						r.Members[0].RemovedAt = &now
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			case "archived":
				if err := os.WriteFile(filepath.Join(store.workspace, "work", "one", "state.md"), []byte("---\nlifecycle: archived\n---\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "lead-changed":
				if err := os.WriteFile(filepath.Join(store.workspace, "work", "one", "portal.yml"), []byte("schema: 1\nslug: one\ncodex:\n  thread_id: replacement\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				reportingFixture(t, store, "two", "root-two", "thread-one")
			case "malformed":
				if err := os.WriteFile(store.path("one"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.ResolveReporter("thread-one"); err == nil {
				t.Fatalf("accepted %s", scenario)
			}
		})
	}
}

func TestReportRevalidatesExpectedIdentityAfterOperationLock(t *testing.T) {
	for _, change := range []string{"member", "lead", "retirement"} {
		t.Run(change, func(t *testing.T) {
			store, err := NewStore(t.TempDir(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			reportingFixture(t, store, "one", "root-one", "thread-one")
			unlock, err := store.LockOperation(context.Background(), "one")
			if err != nil {
				t.Fatal(err)
			}
			client := &testClient{}
			service := Service{Store: store, Client: client, ExpectedReporterThread: "thread-one"}
			result := make(chan error, 1)
			go func() {
				_, err := service.Assign(context.Background(), "one", "root-one", "implementer0", "lead", "report", "", "", "0123456789abcdef0123456789abcdef")
				result <- err
			}()
			if change == "lead" {
				if err := os.WriteFile(filepath.Join(store.workspace, "work", "one", "portal.yml"), []byte("schema: 1\nslug: one\ncodex:\n  thread_id: replacement\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				_, err := store.Update(context.Background(), "one", "root-one", false, func(r *Roster) error {
					if change == "member" {
						r.Members[0].Thread = "replacement"
					} else {
						r.Members[0].RetireIntent = "remove"
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			unlock()
			if err := <-result; err == nil || len(client.sends) != 0 {
				t.Fatalf("stale report delivered: %v %#v", err, client.sends)
			}
		})
	}
}
