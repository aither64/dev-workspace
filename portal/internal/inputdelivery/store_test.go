package inputdelivery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func fixtureAdmission(workspace, id string) Admission {
	ref := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: workspace, Slug: "2026-11-23-test", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "native"}
	s := Selection{Handle: agentbackend.ExecutionHandle{Conversation: ref, Generation: strings.Repeat("c", 64)}, Settings: piruntime.EffectiveSettings{Provider: "provider", AccountRef: "account", Model: "model", Thinking: "high", PolicyRevision: "one", Revision: 1}, InstructionsDigest: strings.Repeat("d", 64), Access: "workspace_write", BindingRevision: 1}
	s.Runtime = piruntime.RuntimeRef{Session: ref.Session, Generation: s.Handle.Generation, InstanceID: strings.Repeat("e", 64), RequestID: strings.Repeat("f", 64), Digest: strings.Repeat("1", 64)}
	a := Admission{Version: 1, Scope: ConversationScope(ref), Request: backend.DeliveryRequest{Version: 1, ClientUserMessageID: id, Message: "saved", Delivery: "steer", Selection: Digest(s)}, Wire: "saved", Selection: s, Input: piruntime.SendInput{Message: "saved", ActionContext: "pi-delivery-v1:steer"}, Ref: piruntime.SubmissionRef{Conversation: ref, OperationID: id, Action: "send"}}
	a.Ref.Digest = piruntime.SubmissionDigest(a.Ref, a.Input, s.Settings)
	return a
}

func TestIndexedAdmissionIsImmutableAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root, workspace := t.TempDir(), t.TempDir()
	s, err := Open(ctx, root, workspace, testGate)
	if err != nil {
		t.Fatal(err)
	}
	a := fixtureAdmission(workspace, "11111111-1111-4111-8111-111111111111")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s.Admit(ctx, a)
			if e == nil && (r.Status.Sequence != 1 || r.Status.State != "queued") {
				e = ErrInvalid
			}
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	b := a
	b.Request.Message = "changed"
	b.Wire = "changed"
	b.Input.Message = "changed"
	b.Ref.Digest = piruntime.SubmissionDigest(b.Ref, b.Input, b.Selection.Settings)
	if _, err = s.Admit(ctx, b); err != ErrConflict {
		t.Fatal("changed identity", err)
	}
	if err = s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, root, workspace, testGate)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	r, err := s.Get(ctx, a.Scope, a.Request.ClientUserMessageID)
	if err != nil || r.Status.State != "queued" {
		t.Fatal(r, err)
	}
	list, err := s.List(ctx, a.Scope)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	if _, err = s.Get(ctx, "different", a.Request.ClientUserMessageID); err != ErrNotFound {
		t.Fatal("cross-scope read", err)
	}
	if n, e := Inspect(ctx, root, workspace); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}

func TestDispatchClaimNeverReturnsToQueue(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir(), t.TempDir(), testGate)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	a := fixtureAdmission(s.workspace, "11111111111111111111111111111111")
	r, err := s.Admit(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	r.Status.State = "dispatching"
	r.DispatchAtMS = time.Now().UnixMilli()
	r.DispatchGeneration = a.Selection.Handle.Generation
	r.DispatchRuntime = a.Selection.Runtime
	r.DispatchHandle = a.Selection.Handle
	if won, e := s.Update(ctx, r, "queued"); e != nil || !won {
		t.Fatal(won, e)
	}
	if won, e := s.Update(ctx, r, "queued"); e != nil || won {
		t.Fatal("second dispatch winner", won, e)
	}
	r.Status.State = "unknown"
	if won, e := s.Update(ctx, r, "dispatching"); e != nil || !won {
		t.Fatal(won, e)
	}
	r.Status.State = "queued"
	if _, e := s.Update(ctx, r, "unknown"); e != ErrInvalid {
		t.Fatal("ambiguity replay", e)
	}
}

func TestCapacityAndDueScopesPreserveBarriers(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	s, e := Open(ctx, t.TempDir(), workspace, testGate)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close(context.Background())
	for i := 1; i <= 20; i++ {
		a := fixtureAdmission(workspace, fmt.Sprintf("%032x", i))
		if _, e = s.Admit(ctx, a); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.Admit(ctx, fixtureAdmission(workspace, fmt.Sprintf("%032x", 21))); e != ErrCapacity {
		t.Fatal("capacity", e)
	}
	due, e := s.Due(ctx, "", 4)
	if e != nil || len(due) != 1 || due[0].Status.Sequence != 1 {
		t.Fatal(due, e)
	}
	r := due[0]
	r.Status.State = "dispatching"
	r.DispatchAtMS = time.Now().UnixMilli()
	r.DispatchRuntime = r.Admission.Selection.Runtime
	r.DispatchHandle = r.Admission.Selection.Handle
	r.DispatchGeneration = r.DispatchHandle.Generation
	if _, e = s.Update(ctx, r, "queued"); e != nil {
		t.Fatal(e)
	}
	r.Status.State = "pending"
	if _, e = s.Update(ctx, r, "dispatching"); e != nil {
		t.Fatal(e)
	}
	due, e = s.Due(ctx, "", 4)
	if e != nil || len(due) != 1 || due[0].Status.Sequence != 2 {
		t.Fatal("pending prevents steer", due, e)
	}
	r = due[0]
	r.Status.State = "dispatching"
	r.DispatchAtMS = time.Now().UnixMilli()
	r.DispatchRuntime = r.Admission.Selection.Runtime
	r.DispatchHandle = r.Admission.Selection.Handle
	r.DispatchGeneration = r.DispatchHandle.Generation
	s.Update(ctx, r, "queued")
	r.Status.State = "unknown"
	s.Update(ctx, r, "dispatching")
	due, e = s.Due(ctx, "", 4)
	if e != nil || len(due) != 1 || due[0].Status.Sequence != 2 {
		t.Fatal("unknown bypass", due, e)
	}
}

func TestInspectionDoesNotCreateStateAndRefusesCorruption(t *testing.T) {
	ctx := context.Background()
	root, workspace := t.TempDir(), t.TempDir()
	if n, e := Inspect(ctx, root, workspace); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	if _, e := os.Stat(filepath.Join(root, "portal")); !os.IsNotExist(e) {
		t.Fatal("inspection created directory", e)
	}
	s, e := Open(ctx, root, workspace, testGate)
	if e != nil {
		t.Fatal(e)
	}
	a := fixtureAdmission(workspace, "11111111111111111111111111111111")
	if _, e = s.Admit(ctx, a); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.ExecContext(ctx, "UPDATE deliveries SET phase='sent'"); e != nil {
		t.Fatal(e)
	}
	if _, e = Inspect(ctx, root, workspace); e == nil {
		t.Fatal("contradictory index accepted")
	}
	s.Close(context.Background())
}

func testGate(ctx context.Context, run func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return run()
}

func TestTerminalCloseCancelsAndJoinsTransitionWaiters(t *testing.T) {
	ctx := context.Background()
	root, workspace := t.TempDir(), t.TempDir()
	s, err := Open(ctx, root, workspace, testGate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = testGate(ctx, func() error { return errors.Join(s.db.Close(), s.pool.Close()) }) }()
	a := fixtureAdmission(workspace, "11111111111111111111111111111111")
	if _, err = s.Admit(ctx, a); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path(root, workspace) + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	var once sync.Once
	s.gate = func(ctx context.Context, _ func() error) error {
		once.Do(func() { close(blocked) })
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() { _, err := s.Get(ctx, a.Scope, a.Ref.OperationID); done <- err }()
	<-blocked
	closing, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err = s.Close(closing); !errors.Is(err, ErrDeferredClose) {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("store waiter not canceled", err)
	}
	if _, err = s.Get(ctx, a.Scope, a.Ref.OperationID); err == nil {
		t.Fatal("closed store admitted another gate waiter")
	}
	if after, e := os.ReadFile(Path(root, workspace) + "-wal"); e != nil || string(before) != string(after) {
		t.Fatal("shutdown touched retained WAL", e)
	}
}

func TestRetainedWALInspectionNeverOpensSourceSQLite(t *testing.T) {
	ctx := context.Background()
	root, workspace, retained := t.TempDir(), t.TempDir(), t.TempDir()
	s, err := Open(ctx, root, workspace, testGate)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if _, err = s.db.ExecContext(ctx, "PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	} // fixture retains committed WAL only
	a := fixtureAdmission(workspace, "11111111111111111111111111111111")
	if _, err = s.Admit(ctx, a); err != nil {
		t.Fatal(err)
	}
	path := Path(retained, workspace)
	if err = ensureDirectory(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal"} {
		if err = copySnapshot(ctx, Path(root, workspace)+suffix, path+suffix); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]string{}
	for _, suffix := range []string{"", "-wal"} {
		data, e := os.ReadFile(path + suffix)
		if e != nil {
			t.Fatal(e)
		}
		before[suffix] = string(data)
	}
	assertUnchanged := func() {
		t.Helper()
		for suffix, expected := range before {
			data, e := os.ReadFile(path + suffix)
			info, statErr := os.Stat(path + suffix)
			if e != nil || statErr != nil || string(data) != expected || info.Mode().Perm() != 0600 {
				t.Fatal("inspection changed retained source", suffix, e, statErr)
			}
		}
		if _, e := os.Stat(path + "-shm"); !os.IsNotExist(e) {
			t.Fatal("source SHM was created", e)
		}
	}
	if n, e := Inspect(ctx, retained, workspace); e != nil || n != 1 {
		t.Fatal("committed WAL row missing", n, e)
	}
	assertUnchanged()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e := Inspect(canceled, retained, workspace); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	assertUnchanged()
	if err = os.WriteFile(path, []byte("corrupt queue"), 0600); err != nil {
		t.Fatal(err)
	}
	before[""] = "corrupt queue"
	if err = os.WriteFile(path+"-wal", []byte("corrupt queue WAL"), 0600); err != nil {
		t.Fatal(err)
	}
	before["-wal"] = "corrupt queue WAL"
	if _, e := Inspect(ctx, retained, workspace); e == nil {
		t.Fatal("corrupt queue accepted")
	}
	assertUnchanged()
}

func TestDeniedTransitionDoesNotOpenOrCloseSQLite(t *testing.T) {
	ctx := context.Background()
	root, workspace := t.TempDir(), t.TempDir()
	denied := errors.New("exclusive transition held")
	gate := func(context.Context, func() error) error { return denied }
	if _, err := Open(ctx, root, workspace, gate); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "portal")); !os.IsNotExist(err) {
		t.Fatal("denied initialization wrote state", err)
	}
	s, err := Open(ctx, root, workspace, testGate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = testGate(ctx, func() error { return errors.Join(s.db.Close(), s.pool.Close()) }) }() // disposable fixture teardown only
	a := fixtureAdmission(workspace, "11111111111111111111111111111111")
	if _, err = s.Admit(ctx, a); err != nil {
		t.Fatal(err)
	}
	wal, err := os.ReadFile(Path(root, workspace) + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	s.gate = gate
	if err = s.Close(ctx); !errors.Is(err, ErrDeferredClose) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(Path(root, workspace) + "-wal")
	if err != nil || string(wal) != string(after) {
		t.Fatal("denied close checkpointed source", err)
	}
	// The Store is terminal even though its retained connection is intentionally
	// inert until process exit. No retry/reopen is scheduled.
	s.gate = testGate
	if _, err = s.Get(ctx, a.Scope, a.Ref.OperationID); err == nil {
		t.Fatal("terminal Store accepted another call")
	}
	if err = s.Close(ctx); !errors.Is(err, ErrDeferredClose) {
		t.Fatal("deferred close was retried", err)
	}
	if again, e := os.ReadFile(Path(root, workspace) + "-wal"); e != nil || string(again) != string(wal) {
		t.Fatal("repeated close touched source", e)
	}
	next, e := Open(ctx, root, workspace, testGate)
	if e != nil {
		t.Fatal(e)
	}
	defer next.Close(ctx)
	if recovered, e := next.Get(ctx, a.Scope, a.Ref.OperationID); e != nil || recovered.Status.State != "queued" {
		t.Fatal("next-start WAL recovery", recovered, e)
	}
}
