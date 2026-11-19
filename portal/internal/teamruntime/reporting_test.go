package teamruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestResolveReporterValidatesMixedRosterFamilies(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reportingFixture(t, store, "one", "root-one", "thread-one")
	for _, schema := range []int{2, 3, 4, 5} {
		roster := piRosterFixture(store.workspace)
		roster.Schema, roster.Slug = schema, fmt.Sprintf("pi-%d", schema)
		if schema >= 4 {
			roster.ForkCreationID = strings.Repeat("f", 64)
		}
		if schema == 5 {
			now := time.Now().UTC()
			roster.Members[0].State = "removed"
			roster.Members[0].RemovedAt = &now
		}
		if err := roster.Validate(); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(roster)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(store.path(roster.Slug), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	reporter, err := store.ResolveReporter("thread-one")
	if err != nil || reporter.Slug != "one" || reporter.Member.Thread != "thread-one" {
		t.Fatal(reporter, err)
	}
	for _, id := range []string{strings.Repeat("c", 64), strings.Repeat("d", 64), "unknown"} {
		if _, err := store.ResolveReporter(id); err == nil {
			t.Fatal("Pi or foreign identity selected Codex caller", id)
		}
	}
}

func TestResolveReporterRefusesMalformedMixedRosters(t *testing.T) {
	for _, kind := range []string{"bad-json", "duplicate", "unknown-schema", "wrong-backend", "foreign-workspace", "foreign-slug", "missing-incarnation", "unknown-field", "mixed-root", "codex-backend", "bad-permissions"} {
		t.Run(kind, func(t *testing.T) {
			store, err := NewStore(t.TempDir(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			reportingFixture(t, store, "one", "root-one", "thread-one")
			pi := piRosterFixture(store.workspace)
			pi.Slug = "z-pi"
			data, _ := json.Marshal(pi)
			body := string(data)
			switch kind {
			case "bad-json":
				body = "{"
			case "duplicate":
				body = strings.Replace(body, `"schema":2`, `"schema":2,"schema":1`, 1)
			case "unknown-schema":
				body = strings.Replace(body, `"schema":2`, `"schema":99`, 1)
			case "wrong-backend":
				body = strings.Replace(body, `"backend":"pi"`, `"backend":"other"`, 1)
			case "foreign-workspace":
				body = strings.Replace(body, store.workspace, store.workspace+"-other", 1)
			case "foreign-slug":
				body = strings.Replace(body, `"slug":"z-pi"`, `"slug":"other"`, 1)
			case "missing-incarnation":
				body = strings.Replace(body, `"sessionIncarnation":"`+pi.SessionIncarnation+`",`, "", 1)
			case "unknown-field":
				body = strings.TrimSuffix(body, "}") + `,"unknown":true}`
			case "mixed-root":
				body = strings.TrimSuffix(body, "}") + `,"rootThreadId":"thread-one"}`
			case "codex-backend":
				data, err := os.ReadFile(store.path("one"))
				if err != nil {
					t.Fatal(err)
				}
				body = strings.Replace(strings.TrimSpace(string(data)), `"slug": "one"`, `"slug": "z-pi"`, 1)
				body = strings.TrimSuffix(body, "}") + `,"backend":"pi"}`
			}
			if err := os.WriteFile(store.path("z-pi"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "bad-permissions" {
				if err := os.Chmod(store.path("z-pi"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.ResolveReporter("thread-one"); err == nil {
				t.Fatal("malformed/foreign record skipped", kind)
			}
		})
	}
}
