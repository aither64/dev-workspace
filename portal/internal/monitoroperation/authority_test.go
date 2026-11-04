package monitoroperation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestOperationActivationPreservesExactRootAndOtherThreads(t *testing.T) {
	ctx := context.Background()
	workspace, state := t.TempDir(), t.TempDir()
	slug := "activation-test"
	tracking := filepath.Join(workspace, "work", slug)
	if err := os.MkdirAll(tracking, 0700); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{"portal.yml": "schema: 1\nslug: activation-test\ncodex:\n  thread_id: lead\nrepositories: []\nartifacts: []\n", "plan.md": "Disposable activation fixture\n", "state.md": "---\nlifecycle: active\n---\n"} {
		if err := os.WriteFile(filepath.Join(tracking, file), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	socket, _ := shortMonitorSocket(t)
	authority := filepath.Join(state, "authority")
	if err := os.Mkdir(authority, 0700); err != nil {
		t.Fatal(err)
	}
	record := session.RuntimeAuthority{Schema: 1, State: "ready", Slug: slug, Workspace: workspace, TmuxSocket: filepath.Join(state, "tmux.sock"), TmuxSessionID: "$1", CodexThreadID: "lead", CodexSocketPath: socket, CodexClientVersion: "0.165.0"}
	data, _ := json.Marshal(record)
	if err := os.WriteFile(filepath.Join(authority, slug+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	recovery := session.RecoveryStore{Root: state, Workspace: workspace}
	if err := recovery.Set(ctx, slug, "lead", socket, true, true); err != nil {
		t.Fatal(err)
	}
	if err := recovery.Update(ctx, slug, func(r *session.Recovery) error {
		r.ActiveThreads = append(r.ActiveThreads, "retained-member")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	n := NativeAuthority{Workspace: workspace, StateRoot: state, AuthorityDir: authority, Socket: socket}
	owner, err := n.Resolve(slug)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := n.Activate(ctx, owner, "exact-utility"); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := recovery.Load(slug)
	if err != nil || !reflect.DeepEqual(saved.ActiveThreads, []string{"lead", "retained-member", "exact-utility"}) {
		t.Fatalf("activation changed retained permission: %#v %v", saved, err)
	}
	old := owner
	old.SocketEpoch = "previous-daemon"
	if err := n.Activate(ctx, old, "another-utility"); err == nil {
		t.Fatal("old daemon epoch granted new execution")
	}
	if err := recovery.Update(ctx, slug, func(r *session.Recovery) error { r.ActiveThreads = []string{}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := n.Activate(ctx, owner, "another-utility"); err == nil {
		t.Fatal("utility activation unheld stopped root")
	}
	saved, _ = recovery.Load(slug)
	if len(saved.ActiveThreads) != 0 {
		t.Fatal("held root permission changed")
	}
}

func TestOperationHeldAdmissionDoesNotCreateUtilityOrReservation(t *testing.T) {
	s, f, req, _ := operationFixture(t)
	s.Admit = func(context.Context, Owner) error { return errors.New("root is held") }
	if _, err := s.Start(context.Background(), "lead", req); err == nil {
		t.Fatal("held root launched monitoring")
	}
	view, err := s.Store.List(context.Background(), "")
	if err != nil || len(view) != 0 || f.creates != 0 || f.starts != 0 {
		t.Fatal("held admission created native or durable work")
	}
}
