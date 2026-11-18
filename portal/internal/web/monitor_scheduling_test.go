package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
)

func schedulingDigest(v any) string {
	b, _ := json.Marshal(v)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

type schedulingFixture struct {
	server        *Server
	delivery      *monitoroperation.DeliveryService
	ci            *monitoroperation.Service
	communication communication.Service
	ownerCalls    int
}

// Ordinary validated disk formats, with terminal history and no live transport.
func newSchedulingFixture(t testing.TB, operations, observers, messages int) *schedulingFixture {
	t.Helper()
	root, workspace := t.TempDir(), t.TempDir()
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", filepath.Join(root, "absent-catalog"))
	profile := filepath.Join(root, "profile")
	if err := os.Symlink(workspace, profile); err != nil {
		t.Fatal(err)
	}
	identity, err := readHostProfileIdentity(profile)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{config: Config{Workspace: workspace, UserStateRoot: root, HostProfile: profile}, hostProfile: identity}
	store, err := monitoroperation.NewStore(root, workspace)
	if err != nil {
		t.Fatal(err)
	}
	owner := monitoroperation.Owner{Slug: "monitor-test", RootThreadID: "lead", Incarnation: "fixture", OriginTurnID: "turn", Socket: filepath.Join(root, "native.sock"), SocketEpoch: "epoch", RuntimeSchema: 1}
	f := &schedulingFixture{server: s}
	f.delivery = &monitoroperation.DeliveryService{Store: store, Resolve: func(string) (monitoroperation.Owner, error) { f.ownerCalls++; return owner, nil }, LockSession: func(context.Context, string) (func(), error) { return func() {}, nil }, Observer: &monitorWorkerObserver{}}
	f.ci = &monitoroperation.Service{Store: store, ObserverEpoch: strings.Repeat("e", 32), Resolve: f.delivery.Resolve, LockSession: f.delivery.LockSession, Admit: func(context.Context, monitoroperation.Owner) error { f.ownerCalls++; return nil }}
	directory := filepath.Join(root, "monitor-operations", schedulingDigestRaw(workspace))
	write := func(path string, v any) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	exit := 0
	for i := 0; i < operations; i++ {
		id := fmt.Sprintf("%032x", i+1)
		req := monitoroperation.StartRequest{ID: id, Brief: "synthetic settled check", Source: monitoroperation.Source{Kind: "local", Argv: []string{"fixture"}, Cwd: workspace, InputIdentity: "fixture", Revision: "fixture", InvocationID: id}}
		op := monitoroperation.Operation{Schema: 1, ID: id, Workspace: workspace, Owner: owner, Source: req.Source, Brief: req.Brief, RequestDigest: schedulingDigest(req), CreatedAt: at, UtilityPolicy: monitoroperation.UtilityPolicy{PackageRoot: workspace, CatalogDigest: strings.Repeat("a", 64), NativeName: "watcher", Model: "gpt-6-luna", Effort: "low", Instructions: "fixture", Sandbox: "danger-full-access", ApprovalPolicy: "never", MaxConcurrent: 1}, Launch: monitoroperation.Launch{MessageID: schedulingDigest([]string{"monitor", id, "launch"})[:32], Envelope: "fixture", TerminatedAt: &at, Termination: "completed"}}
		if err := monitoroperation.AddEvent(&op, monitoroperation.ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &exit}, at); err != nil {
			t.Fatal(err)
		}
		op.Events[0].Delivery.Status = "accepted"
		write(filepath.Join(directory, id+".json"), op)
	}
	for i := 0; i < observers; i++ {
		id := fmt.Sprintf("%032x", 10000+i)
		source := monitoroperation.Source{Kind: "ci", Provider: "github", Repository: "example/project", WorkflowID: "12", RunID: "34", RunAttempt: 1, Commit: strings.Repeat("a", 40)}
		event := monitoroperation.Operation{ID: id, Owner: owner, Source: source}
		if err := monitoroperation.AddEvent(&event, monitoroperation.ReportRequest{Slot: "terminal", Outcome: "passed", Conclusion: "success"}, at); err != nil {
			t.Fatal(err)
		}
		event.Events[0].Delivery.Status = "accepted"
		write(filepath.Join(directory, "observers", id+".json"), monitoroperation.ObserverRecord{Schema: 1, ID: id, Workspace: workspace, Owner: owner, Driver: "direct-ci", RequestDigest: strings.Repeat("a", 64), Source: source, SourceDigest: monitoroperation.SourceDigest(source), CreatedAt: at, UpdatedAt: at, Revision: 1, Phase: "observing", Events: event.Events})
	}
	cs, err := communication.NewStore(root, workspace)
	if err != nil {
		t.Fatal(err)
	}
	f.communication = communication.Service{Store: cs}
	a, b := communication.Identity{Slug: "peer-a", Thread: "a", Incarnation: "a"}, communication.Identity{Slug: "peer-b", Thread: "b", Incarnation: "b"}
	grant := communication.Grant{ID: strings.Repeat("a", 32), A: a, B: b, ApprovedBy: a.Slug, Approval: "fixture", CreatedAt: at}
	ms := []communication.Message{}
	for i := 0; i < messages; i++ {
		ms = append(ms, communication.Message{ID: fmt.Sprintf("%032x", i+1), GrantID: grant.ID, From: a, To: b, Text: strings.Repeat("fixture ", 64), CreatedAt: at, Status: "canceled"})
	}
	cd := filepath.Join(root, "session-communication", schedulingDigestRaw(workspace))
	write(filepath.Join(cd, "state.json"), map[string]any{"schema": 1, "workspace": workspace, "grants": []communication.Grant{grant}, "messages": ms})
	if err := os.WriteFile(filepath.Join(cd, "state.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}
func schedulingDigestRaw(v string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(v))) }
func (f *schedulingFixture) pass() error {
	a, b, c, d := 0, 0, 0, 0
	return errors.Join(f.server.runMonitorPass(context.Background(), f.delivery, f.ci, &a, &b, &c), runCommunicationPass(context.Background(), f.communication, &d, func(context.Context, string) error { return errors.New("unexpected settled delivery") }))
}
func TestSchedulingWorkerSettledHistory(t *testing.T) {
	for _, n := range []struct {
		name                            string
		operations, observers, messages int
	}{{"small", 16, 8, 16}, {"large", 300, 100, 500}} {
		t.Run(n.name, func(t *testing.T) {
			f := newSchedulingFixture(t, n.operations, n.observers, n.messages)
			if err := f.pass(); err != nil {
				t.Fatal(err)
			}
			f.ownerCalls = 0
			monitorReads, communicationReads := f.delivery.Store.RecordReads(), f.communication.Store.RecordReads()
			if err := f.pass(); err != nil {
				t.Fatal(err)
			}
			if f.delivery.Store.RecordReads() != monitorReads || f.communication.Store.RecordReads() != communicationReads {
				t.Fatal("warm worker pass reread settled history bodies")
			}
			if f.ownerCalls != 0 {
				t.Fatalf("settled worker pass performed %d owner/admission calls", f.ownerCalls)
			}
		})
	}
}

func BenchmarkSchedulingWorkerPass(b *testing.B) {
	for _, n := range []struct {
		name                     string
		ops, observers, messages int
	}{{"small", 16, 8, 16}, {"large", 300, 100, 500}} {
		b.Run(n.name, func(b *testing.B) {
			f := newSchedulingFixture(b, n.ops, n.observers, n.messages)
			_ = f.pass()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = f.pass()
			}
		})
	}
}
