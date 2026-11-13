package piruntime

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type fakeHost struct {
	t           *testing.T
	m           *Manager
	s           *supervisor
	count       int
	metadata    workerMetadata
	listener    *net.UnixListener
	ctx         context.Context
	observation HostObservation
	mu          sync.Mutex
}

func (h *fakeHost) Start(ctx context.Context, ref RuntimeRef) (HostObservation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	data, err := os.ReadFile(filepath.Join(h.m.instance(ref), "launch.json"))
	if err != nil {
		return HostObservation{}, err
	}
	launch, digest, err := DecodeLaunch(data)
	if err != nil {
		return HostObservation{}, err
	}
	config, hash, err := readRuntimeConfiguration(filepath.Join(h.m.instance(ref), "requests", ref.RequestID, "runtime-config.json"), launch, digest)
	if err != nil {
		return HostObservation{}, err
	}
	h.observation = HostObservation{LaunchRequest: launch, Digest: digest, Outcome: "running", Phase: "running", Unit: "fake-unit", BootID: "fake-boot", InvocationID: "fake-invocation", ControlGroup: "/fake-fixture", MainPID: os.Getpid(), PIDStart: "fake-start", MountNamespace: "fake-mnt", UserNamespace: "fake-user"}
	s, err := newSupervisor(launch, config, Handshake{HostObservation: h.observation, RuntimeConfigSHA256: hash}, h.metadata)
	if err != nil {
		return h.observation, err
	}
	s.childrenCheck = func() (bool, error) { return false, nil }
	h.s = s
	// Finite in-process fixture. No systemd, sudo, installed unit or credential.
	directory, err := os.Open(h.m.instance(ref))
	if err != nil {
		return h.observation, err
	}
	defer directory.Close()
	path := filepath.Join("/proc/self/fd", fmtInt(int(directory.Fd())), "control.sock")
	h.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return h.observation, err
	}
	h.listener.SetUnlinkOnClose(false) // TempDir owns the exact fixture pathname; the bind directory FD closes here.
	go func() {
		for {
			c, err := h.listener.AcceptUnix()
			if err != nil {
				return
			}
			go s.connection(h.ctx, c)
		}
	}()
	return h.observation, nil
}
func fmtInt(n int) string { return strconv.Itoa(n) }
func (h *fakeHost) Inspect(_ context.Context, ref RuntimeRef) (HostObservation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.observation.RequestID != ref.RequestID {
		return HostObservation{}, ErrScope
	}
	return h.observation, nil
}

func supervisorFixture(t *testing.T) (*Manager, *fakeHost, StartRequest, context.Context) {
	t.Helper()
	pi := os.Getenv("PI_PROBE_PACKAGE")
	if pi == "" {
		t.Skip("explicit pinned SDK package required")
	}
	data, err := os.ReadFile(filepath.Join(pi, "bin/pi"))
	if err != nil {
		t.Fatal(err)
	}
	node := regexp.MustCompile(`/nix/store/[a-z0-9]{32}-nodejs-[^/\s"']+/bin/node`).FindString(string(data))
	entry, err := filepath.Abs("../../../pi-runtime/test/fake-worker.mjs")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, name := range []string{"state", "host", "auth", "workspace"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	h := &fakeHost{t: t, ctx: ctx, metadata: workerMetadata{Node: node, Entrypoint: entry, PiPackage: pi}}
	m, err := NewManager(Config{UserStateRoot: filepath.Join(root, "state"), HostStateRoot: filepath.Join(root, "host"), AuthDirectory: filepath.Join(root, "auth"), AccountBindings: []AccountBinding{{Provider: "p3-fake", AccountRef: "fake-account"}}, Host: h, retainPackage: func(context.Context, string, string) error { return nil }, verifyProcess: func(HostObservation) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	h.m = m
	t.Cleanup(func() {
		if h.listener != nil {
			h.listener.Close()
		}
		if h.s != nil {
			for _, c := range h.s.members {
				if c.worker != nil {
					p, ok := c.worker.(*workerProcess)
					if !ok {
						continue
					}
					select {
					case <-p.closed:
					default:
						_ = p.command.Process.Kill()
						<-p.closed
					}
				}
			}
		}
		cancel()
	})
	start := StartRequest{Session: agentbackend.SessionRef{Workspace: filepath.Join(root, "workspace"), Slug: "2026-11-11-fixture", Incarnation: strings.Repeat("a", 64)}, BindingRevision: 1, RequestID: "11111111-1111-4111-8111-111111111111", AppStorePath: "/nix/store/00000000000000000000000000000000-fixture", ProfilePath: filepath.Join(root, "profile"), ProfileGeneration: "1"}
	if err := os.MkdirAll(filepath.Join(start.Session.Workspace, "work", start.Session.Slug), 0700); err != nil {
		t.Fatal(err)
	}
	return m, h, start, ctx
}
func settingsFixture() EffectiveSettings {
	return EffectiveSettings{Provider: "p3-fake", Model: "p3-model", Thinking: "off", AccountRef: "fake-account", PolicyRevision: "fake-policy", Revision: 1}
}
func waitIdle(t *testing.T, m *Manager, ctx context.Context, handle agentbackend.ExecutionHandle) {
	t.Helper()
	for {
		state, err := m.Load(ctx, handle)
		if err != nil {
			t.Fatal(err)
		}
		if state.State == "idle" {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}
func TestSupervisorNativeWorkersCreationWitnessAndReconnect(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil || runtime.Phase != "ready" {
		t.Fatal(runtime, err)
	}
	if _, err := m.Ensure(ctx, start); err != nil || h.count != 1 {
		t.Fatal("start replayed", err, h.count)
	}
	var handles []agentbackend.ExecutionHandle
	for i, member := range []string{"lead", "implementer1"} {
		create := CreateRequest{Session: start.Session, Member: member, Incarnation: strings.Repeat(string(rune('b'+i)), 64), CreationID: []string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"}[i], Settings: settingsFixture()}
		state, err := m.Create(ctx, runtime.Runtime, create)
		if err != nil || state.Persisted || state.HistoryRef == "" {
			t.Fatal(state, err)
		}
		conflictCreate := create
		conflictCreate.Settings.Model = "changed-model"
		if _, err := m.Create(ctx, runtime.Runtime, conflictCreate); !errors.Is(err, ErrConflict) {
			t.Fatal("changed creation reused", err)
		}
		handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
		handles = append(handles, handle)
		proof, err := m.VerifyCreation(ctx, handle, create.CreationID, state.HistoryRef, nil)
		if err != nil || proof.Persisted || proof.Bootstrap != nil {
			t.Fatal(proof, err)
		}
		ops, err := m.Bind(ctx, handle)
		if err != nil {
			t.Fatal(err)
		}
		if ops.QueueWrite != nil || ops.Settings != nil || ops.Respond != nil || ops.Activity != nil {
			t.Fatal("unproved slot exposed")
		}
		identity := BackendIdentity(handle)
		wrong := identity
		wrong.Generation = strings.Repeat("d", 64)
		if err := ops.Verify.Verify(ctx, wrong); !errors.Is(err, ErrScope) {
			t.Fatal(err)
		}
		id := "0123456789abcdef0123456789abcdef"
		request := backend.SendRequest{Message: "<script>identical real input</script>", ClientUserMessageID: id, ActionContext: []string{"initial", "team assignment"}[i]}
		result, err := ops.Send.Send(ctx, identity, request)
		if err != nil || result.Outcome != backend.Accepted || result.Receipt.ClientUserMessageID != id || result.Receipt.TurnID == "" {
			t.Fatal(result, err)
		}
		ref := SubmissionRef{Conversation: handle.Conversation, OperationID: id, Action: "send"}
		ref.Digest = SubmissionDigest(ref, SendInput{Message: request.Message, ActionContext: request.ActionContext}, state.Settings)
		proof, err = m.VerifyCreation(ctx, handle, create.CreationID, state.HistoryRef, &ref)
		if err != nil || !proof.Persisted || proof.Bootstrap.Phase != "Accepted" {
			t.Fatal(proof, err)
		}
		if _, err := m.VerifyCreation(ctx, handle, "44444444-4444-4444-8444-444444444444", state.HistoryRef, &ref); !errors.Is(err, ErrScope) {
			t.Fatal(err)
		}
		if _, err := m.VerifyCreation(ctx, handle, create.CreationID, state.HistoryRef, nil); !errors.Is(err, ErrHeld) {
			t.Fatal("persisted history returned initialized-only proof", err)
		}
		if _, err := m.VerifyCreation(ctx, handle, create.CreationID, strings.Repeat("0", 64), &ref); !errors.Is(err, ErrScope) {
			t.Fatal("wrong history locator", err)
		}
		memberState, _ := h.s.member(handle.Conversation)
		original := *proof.Bootstrap
		bad := original
		witness := *original.Witness
		witness.EntryID = "absent-native-entry"
		bad.Witness = &witness
		if err := memberState.receipts.save(bad); err != nil {
			t.Fatal(err)
		}
		if _, err := m.VerifyCreation(ctx, handle, create.CreationID, state.HistoryRef, &ref); !errors.Is(err, ErrHeld) {
			t.Fatal("wrong native entry accepted", err)
		}
		if err := memberState.receipts.save(original); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, m, ctx, handle)
		read, err := ops.Read.Read(ctx, identity)
		if err != nil || !strings.Contains(read.Entries[0].HTML, "&lt;script&gt;") {
			t.Fatal(read, err)
		}
		again, err := ops.Send.Send(ctx, identity, request)
		if err != nil || again != result {
			t.Fatal("lost ACK replay changed", again, result, err)
		}
		// Native readiness loss does not erase a committed host admission receipt.
		oldHash := h.s.handshake.RuntimeConfigSHA256
		h.s.handshake.RuntimeConfigSHA256 = strings.Repeat("0", 64)
		accepted, lookupErr := m.Lookup(ctx, ref)
		if lookupErr != nil || accepted.Phase != "Accepted" {
			t.Fatal(accepted, lookupErr)
		}
		h.s.handshake.RuntimeConfigSHA256 = oldHash
		request2 := request
		request2.ClientUserMessageID = "66666666-6666-4666-8666-666666666666"
		memberState.receipts.checkpoint = func(point string) error {
			if point == "receipt" {
				return errors.New("lost private ACK")
			}
			return nil
		}
		lost, sendErr := ops.Send.Send(ctx, identity, request2)
		if sendErr == nil || lost.Outcome != backend.Unknown {
			t.Fatal(lost, sendErr)
		}
		ref2 := ref
		ref2.OperationID = request2.ClientUserMessageID
		ref2.Digest = SubmissionDigest(ref2, SendInput{Message: request2.Message, ActionContext: request2.ActionContext}, state.Settings)
		retained, err := m.Lookup(ctx, ref2)
		if err != nil || retained.Phase != "Accepted" || retained.Witness.EntryID == proof.Bootstrap.Witness.EntryID {
			t.Fatal(retained, err)
		}
		memberState.receipts.checkpoint = nil
		replay, err := ops.Send.Send(ctx, identity, request2)
		if err != nil || replay.Outcome != backend.Accepted || replay.Receipt.TurnID != retained.Witness.EntryID {
			t.Fatal(replay, err)
		}
		waitIdle(t, m, ctx, handle)
		ref.Digest = strings.Repeat("0", 64)
		if _, err := m.Lookup(ctx, ref); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		stale := handle
		stale.Generation = strings.Repeat("d", 64)
		if _, err := m.Load(ctx, stale); !errors.Is(err, ErrScope) {
			t.Fatal(err)
		}
	}
	if BackendIdentity(handles[0]).Directory != filepath.Join(start.Session.Workspace, "work", start.Session.Slug) {
		t.Fatal("workspace root substituted for session cwd")
	}
	if handles[0].Conversation.NativeID == handles[1].Conversation.NativeID {
		t.Fatal("native identity shared")
	}
	first, _ := h.s.member(handles[0].Conversation)
	second, _ := h.s.member(handles[1].Conversation)
	if first.record.HistoryFile == second.record.HistoryFile || first.record.WorkerPID == second.record.WorkerPID {
		t.Fatal("workers/history shared")
	}

	result, err := m.Unload(ctx, handles[1])
	if err != nil || !result.VerifiedExit {
		t.Fatal(result, err)
	}
	if err := os.Remove(second.record.HistoryFile); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Load(ctx, handles[1]); !errors.Is(err, ErrHeld) {
		t.Fatal("missing history synthesized", err)
	}
	// Ambiguous detached descendants retain a durable hold, even if a later
	// observation no longer sees the process. No implicit recovery clears it.
	h.s.childrenCheck = func() (bool, error) { return true, nil }
	if _, err := m.Unload(ctx, handles[0]); !errors.Is(err, ErrHeld) {
		t.Fatal("survivor not held", err)
	}
	h.s.childrenCheck = func() (bool, error) { return false, nil }
	if _, err := m.Unload(ctx, handles[0]); !errors.Is(err, ErrHeld) {
		t.Fatal("survivor hold cleared", err)
	}
	// A native worker crash does not create a new recovery identity or turn.
	process := first.worker.(*workerProcess)
	_ = process.command.Process.Kill()
	<-process.closed
	replacement, err := newSupervisor(h.s.launch, h.s.config, h.s.handshake, h.s.metadata)
	if err != nil {
		t.Fatal(err)
	}
	replacement.childrenCheck = func() (bool, error) { return false, nil }
	if _, err := replacement.load(ctx, handles[0]); !errors.Is(err, ErrHeld) {
		t.Fatal("crashed worker silently resumed", err)
	}

}

func TestManagerPreparationAndHandshakeRefuseChangedInputs(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	m.config.retainPackage = func(context.Context, string, string) error { return errors.New("interrupted prepare") }
	if _, err := m.Ensure(ctx, start); err == nil {
		t.Fatal("injection absent")
	}
	reserved, err := m.record(start.Session)
	if err != nil || reserved.Phase != "preparing" || h.count != 0 {
		t.Fatal(reserved, err)
	}
	m.config.retainPackage = func(context.Context, string, string) error { return nil }
	ready, err := m.Ensure(ctx, start)
	if err != nil || ready.Runtime != reserved.Runtime || h.count != 1 {
		t.Fatal(ready, err)
	}
	changed := start
	changed.ProfileGeneration = "2"
	if _, err := m.Ensure(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	h.s.handshake.RuntimeConfigSHA256 = strings.Repeat("0", 64)
	if _, err := m.Inspect(ctx, ready.Runtime); !errors.Is(err, ErrScope) {
		t.Fatal("config hash accepted", err)
	}
	if len(h.s.members) != 0 {
		t.Fatal("worker loaded before readiness")
	}
	data, err := os.ReadFile(filepath.Join(m.instance(ready.Runtime), "requests", start.RequestID, "runtime-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{append(data, []byte(`{}`)...), []byte(strings.Replace(string(data), `"schema":1`, `"schema":1,"schema":1`, 1)), []byte(strings.Replace(string(data), `"schema":1`, `"schema":null`, 1)), []byte(strings.Replace(string(data), `"schema":1`, `"schema":2`, 1))} {
		path := filepath.Join(t.TempDir(), "runtime-config.json")
		if err := os.WriteFile(path, bad, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readRuntimeConfiguration(path, h.s.launch, ready.Runtime.Digest); err == nil {
			t.Fatal("invalid runtime config accepted")
		}
	}
}
