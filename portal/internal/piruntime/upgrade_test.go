package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// Reuse the existing native worker/control-socket fixture and terminal receipt
// authority. Only this fixture host implements the optional Stop port.
type upgradeFixtureHost struct {
	*successorFixtureHost
	stops        int
	stopErr      error
	stopReplyErr error
	onStart      func(*supervisor)
}

type upgradeDispatchCounts struct{ creates, sends, configures atomic.Int64 }
type upgradeCountedWorker struct {
	worker
	counts *upgradeDispatchCounts
}

func (w *upgradeCountedWorker) Call(ctx context.Context, operation string, input, output any) error {
	if operation == "send" {
		w.counts.sends.Add(1)
	}
	if operation == "configure" {
		w.counts.configures.Add(1)
	}
	return w.worker.Call(ctx, operation, input, output)
}

func (h *upgradeFixtureHost) Start(ctx context.Context, ref RuntimeRef) (HostObservation, error) {
	observed, err := h.successorFixtureHost.Start(ctx, ref)
	if err == nil && h.onStart != nil {
		h.onStart(h.base.s)
		observed = h.base.observation
	}
	return observed, err
}

func newUpgradeFixtureHost(base *fakeHost) *upgradeFixtureHost {
	return &upgradeFixtureHost{successorFixtureHost: &successorFixtureHost{base: base, terminal: make(map[string]HostObservation)}}
}

func (h *upgradeFixtureHost) Stop(_ context.Context, ref RuntimeRef, observed HostObservation) (HostObservation, error) {
	h.stops++
	if h.stopErr != nil {
		return HostObservation{}, h.stopErr
	}
	current, err := h.base.Inspect(context.Background(), ref)
	if err != nil || !sameExecution(current, observed) {
		return HostObservation{}, ErrScope
	}
	h.finish(h.base.t, ref)
	if h.stopReplyErr != nil {
		return HostObservation{}, h.stopReplyErr
	}
	return h.terminal[ref.RequestID], nil
}

// Lose one observation after a genuine worker was loaded and its PID saved.
// The next Load must inspect that same worker rather than start another one.
type upgradeLostSnapshotWorker struct {
	worker
	lost bool
}

func (w *upgradeLostSnapshotWorker) Call(ctx context.Context, operation string, input, output any) error {
	err := w.worker.Call(ctx, operation, input, output)
	if err == nil && operation == "snapshot" && !w.lost {
		w.lost = true
		return ErrUnavailable
	}
	return err
}

func TestUpgradeProofCannotBeFabricated(t *testing.T) {
	if _, err := (UpgradeProof{}).Transition(); !errors.Is(err, ErrScope) {
		t.Fatal(err)
	}
}

func TestUpgradePredecessorConnectorKeepsPublicGateAndRevokesItsStream(t *testing.T) {
	m, host, start, first, ctx := successorFixture(t)
	records, err := m.records(start.Session)
	if err != nil {
		t.Fatal(err)
	}
	next := nextStart(start, "22222222-2222-4222-8222-222222222222")
	descriptor, err := os.ReadFile(filepath.Join(m.instance(first.Runtime), "launch.json"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := m.reserveSuccessorRecord(next, records, records.Requests[start.RequestID], descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AttachHost(ctx, first.Runtime, &callbackFixtureHandler{}); !errors.Is(err, ErrHeld) {
		t.Fatal("public historical attachment admitted", err)
	}
	lifetime, cancel := context.WithCancel(ctx)
	connector := &upgradePredecessorConnector{manager: m, current: current.Runtime, previous: first.Runtime, lifetime: lifetime, cancel: cancel}
	defer connector.close()
	wrong := first.Runtime
	wrong.Generation = strings.Repeat("f", 64)
	if err := connector.AttachHost(ctx, wrong, &callbackFixtureHandler{}); !errors.Is(err, ErrScope) {
		t.Fatal("foreign predecessor admitted", err)
	}
	done := make(chan error, 1)
	handler := &workflowFixtureHandler{callbackFixtureHandler: callbackFixtureHandler{}, invalidated: func(WorkflowInvalidation) {}}
	go func() { done <- connector.AttachHost(ctx, first.Runtime, handler) }()
	for {
		host.base.s.workflow.mu.Lock()
		ready := host.base.s.workflow.attachment != nil && host.base.s.workflow.attachment.workflowReady
		host.base.s.workflow.mu.Unlock()
		if ready {
			break
		}
		select {
		case err := <-done:
			t.Fatal("owned predecessor attachment failed", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	connector.close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("predecessor stream not joined", ctx.Err())
	}
	if err := connector.AttachHost(ctx, first.Runtime, handler); !errors.Is(err, ErrScope) {
		t.Fatal("revoked predecessor connector reused", err)
	}
}

func TestUpgradeReservationPreservesIdentityAndFencesOrdinaryOldHandles(t *testing.T) {
	m, base, start, ctx := supervisorFixture(t)
	host := newUpgradeFixtureHost(base)
	m.config.Host = host
	m.config.validatePackage = func(string) error { return nil }
	m.config.verifyDrain = func(HostObservation) error { return nil }
	first, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	// Existing fixture setup owns the only original input; Upgrade cannot send.
	input := backend.SendRequest{Message: "retained original input", ClientUserMessageID: strings.Repeat("d", 32)}
	if sent, err := m.Deliver(ctx, handle, input); err != nil || sent.Outcome != backend.Accepted {
		t.Fatal(sent, err)
	}
	waitIdle(t, m, ctx, handle)
	root, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	memberRoot := filepath.Join(root, "members", state.Conversation.Member, state.Conversation.Incarnation)
	var original conversationRecord
	if err := readPrivate(filepath.Join(memberRoot, "conversation.json"), &original); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(original.HistoryFile)
	if err != nil {
		t.Fatal(err)
	}
	next := nextStart(start, "22222222-2222-4222-8222-222222222222")
	next.BindingRevision = start.BindingRevision
	request := UpgradeRequest{Start: next, ExpectedGeneration: first.Runtime.Generation, Members: []agentbackend.ConversationRef{state.Conversation}}
	publications := 0
	failAttachment, failPublication := false, false
	loseLoadReply, loadHookInstalled, loads := false, false, 0
	t.Cleanup(func() {
		if base.s != nil {
			for _, member := range base.s.members {
				if w, ok := member.worker.(*upgradeLostSnapshotWorker); ok {
					member.worker = w.worker
				}
			}
		}
	})
	hooks := UpgradeHooks{EnsureAttachment: func(_ context.Context, ref RuntimeRef, _ HostCallbackConnector) error {
		if failAttachment && ref.RequestID == next.RequestID {
			return ErrUnavailable
		}
		if loseLoadReply && !loadHookInstalled && ref.RequestID == next.RequestID {
			loadHookInstalled = true
			originalStart := base.s.start
			base.s.start = func(ctx context.Context, metadata workerMetadata, handle agentbackend.ExecutionHandle, config workerConfig, changed func()) (worker, error) {
				loads++
				w, err := originalStart(ctx, metadata, handle, config, changed)
				if err != nil {
					return w, err
				}
				return &upgradeLostSnapshotWorker{worker: w}, nil
			}
		}
		return nil
	}, Publish: func(_ context.Context, proof UpgradeProof) error {
		publications++
		transition, err := proof.Transition()
		if err != nil || transition.Previous != first.Runtime || transition.Current.Generation == first.Runtime.Generation || len(transition.Members) != 1 || transition.Members[0] != state.Conversation {
			return ErrScope
		}
		if failPublication {
			return ErrUnavailable
		}
		return nil
	}}
	// Saved/live settings drift refuses before reservation or any drain.
	member, err := base.s.member(state.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	member.mu.Lock()
	member.record.Settings.Thinking = "low"
	err = member.save()
	member.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upgrade(ctx, request, hooks); !errors.Is(err, ErrHeld) {
		t.Fatal("native settings drift admitted", err)
	}
	records, err := m.records(start.Session)
	if err != nil || records.CurrentRequestID != start.RequestID || host.stops != 0 || host.starts != 1 || publications != 0 {
		t.Fatal("drift changed runtime", records, err, host.stops, host.starts, publications)
	}
	member.mu.Lock()
	member.record.Settings = original.Settings
	err = member.save()
	member.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// A previously completed ordinary Unload has positive durable evidence;
	// Upgrade must not require a vanished live worker for that member.
	if stopped, err := m.Unload(ctx, handle); err != nil || !stopped.VerifiedExit {
		t.Fatal(stopped, err)
	}
	m.config.retainPackage = func(_ context.Context, path, _ string) error {
		if strings.Contains(path, next.RequestID) {
			return ErrUnavailable
		}
		return nil
	}
	interrupted, err := m.Upgrade(ctx, request, hooks)
	if !errors.Is(err, ErrUnavailable) || interrupted.Runtime.Generation == "" || host.stops != 0 || host.starts != 1 || publications != 0 {
		t.Fatal(interrupted, err, host.stops, host.starts, publications)
	}
	records, err = m.records(start.Session)
	if err != nil || records.Schema != 2 || records.CurrentRequestID != next.RequestID || records.Requests[start.RequestID].Start != start {
		t.Fatal(records, err)
	}
	if _, err := m.Load(ctx, handle); !errors.Is(err, ErrScope) {
		t.Fatal("old handle dispatched after reservation", err)
	}
	changed := request
	changed.Start.ProfileGeneration = "changed"
	if _, err := m.Upgrade(ctx, changed, hooks); !errors.Is(err, ErrConflict) {
		t.Fatal("changed intent admitted", err)
	}
	m.config.retainPackage = func(context.Context, string, string) error { return nil }
	host.stopErr = ErrUnavailable
	if partial, err := m.Upgrade(ctx, request, hooks); !errors.Is(err, ErrUnavailable) || partial.Runtime != interrupted.Runtime || host.starts != 1 || host.stops != 1 || publications != 0 {
		t.Fatal("uncertain Stop progressed", partial, err, host.starts, host.stops, publications)
	}
	host.stopErr = nil
	host.stopReplyErr = ErrUnavailable
	if partial, err := m.Upgrade(ctx, request, hooks); !errors.Is(err, ErrUnavailable) || partial.Runtime != interrupted.Runtime || host.starts != 1 || host.stops != 2 || publications != 0 {
		t.Fatal("lost terminal Stop reply progressed", partial, err, host.starts, host.stops, publications)
	}
	host.stopReplyErr = nil
	failAttachment = true
	if partial, err := m.Upgrade(ctx, request, hooks); !errors.Is(err, ErrUnavailable) || partial.Runtime != interrupted.Runtime || host.starts != 2 || host.stops != 2 || publications != 0 {
		t.Fatal("missing successor attachment published", partial, err, host.starts, host.stops, publications)
	}
	failAttachment, loseLoadReply = false, true
	if partial, err := m.Upgrade(ctx, request, hooks); !errors.Is(err, ErrHeld) || partial.Runtime != interrupted.Runtime || host.starts != 2 || host.stops != 2 || publications != 0 || loads != 1 {
		t.Fatal("lost Load observation progressed", partial, err, host.starts, host.stops, publications, loads)
	}
	failPublication = true
	if partial, err := m.Upgrade(ctx, request, hooks); !errors.Is(err, ErrUnavailable) || partial.Runtime != interrupted.Runtime || host.starts != 2 || host.stops != 2 || publications != 1 {
		t.Fatal("publication uncertainty repeated effects", partial, err, host.starts, host.stops, publications)
	}
	failPublication = false
	result, err := m.Upgrade(ctx, request, hooks)
	if err != nil || result.Phase != "ready" || result.Runtime != interrupted.Runtime || result.Runtime.InstanceID != first.Runtime.InstanceID || host.stops != 2 || host.starts != 2 || publications != 2 || loads != 1 {
		t.Fatal(result, err, host.stops, host.starts, publications)
	}
	after, err := os.ReadFile(original.HistoryFile)
	if err != nil || string(after) != string(bytes) {
		beforeIDs := make(map[string]bool)
		for _, line := range strings.Split(strings.TrimSpace(string(bytes)), "\n") {
			var entry struct{ ID, Type string }
			if json.Unmarshal([]byte(line), &entry) == nil {
				beforeIDs[entry.ID] = true
			}
		}
		for _, line := range strings.Split(strings.TrimSpace(string(after)), "\n") {
			var entry struct{ ID, Type string }
			if json.Unmarshal([]byte(line), &entry) == nil && !beforeIDs[entry.ID] {
				t.Logf("new native entry kind=%s id=%s", entry.Type, entry.ID)
			}
		}
		t.Fatal("upgrade changed canonical history", err)
	}
	if _, err := m.Bind(ctx, handle); !errors.Is(err, ErrScope) {
		t.Fatal("stale handle survived upgrade", err)
	}
	currentHandle := handle
	currentHandle.Generation = result.Runtime.Generation
	operations, err := m.Bind(ctx, currentHandle)
	if err != nil {
		t.Fatal(err)
	}
	transcriptContractFixture(t, m, next, `{"piCreationSchema":4}`)
	transcript, err := operations.Read.Read(ctx, BackendIdentity(currentHandle))
	if err != nil || transcript.ThreadID != handle.Conversation.NativeID {
		t.Fatal(transcript, err)
	}
	if replay, err := m.Upgrade(ctx, request, hooks); err != nil || replay != result || host.starts != 2 || host.stops != 2 {
		t.Fatal(replay, err, host.starts, host.stops)
	}
}

func TestLegacyWorkersForwardUpgradeToSharedHost(t *testing.T) {
	m, base, start, ctx := supervisorFixture(t)
	host := newUpgradeFixtureHost(base)
	m.config.Host = host
	m.config.validatePackage = func(string) error { return nil }
	m.config.verifyDrain = func(HostObservation) error { return nil }
	first, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	members := []agentbackend.ConversationRef{}
	oldPIDs := []int{}
	histories := map[string][]byte{}
	for i, member := range []string{"lead", "implementer0"} {
		state, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: member, Incarnation: strings.Repeat(string(rune('b'+i)), 64), CreationID: []string{"33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"}[i], Settings: settingsFixture()})
		if err != nil {
			t.Fatal(err)
		}
		handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
		if sent, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "retained member", ClientUserMessageID: strings.Repeat("d", 32)}); err != nil || sent.Outcome != backend.Accepted {
			t.Fatal(sent, err)
		}
		waitIdle(t, m, ctx, handle)
		c, err := base.s.member(state.Conversation)
		if err != nil {
			t.Fatal(err)
		}
		oldPIDs = append(oldPIDs, c.record.WorkerPID)
		members = append(members, state.Conversation)
		histories[c.record.HistoryFile], err = os.ReadFile(c.record.HistoryFile)
		if err != nil {
			t.Fatal(err)
		}
	}
	if oldPIDs[0] == oldPIDs[1] {
		t.Fatal("predecessor fixture was not unique-process")
	}
	base.metadata.Protocol = 2
	next := nextStart(start, "22222222-2222-4222-8222-222222222222")
	next.BindingRevision = start.BindingRevision
	result, err := m.Upgrade(ctx, UpgradeRequest{Start: next, ExpectedGeneration: first.Runtime.Generation, Members: members}, UpgradeHooks{EnsureAttachment: func(context.Context, RuntimeRef, HostCallbackConnector) error { return nil }, Publish: func(context.Context, UpgradeProof) error { return nil }})
	if err != nil || result.Phase != "ready" || host.stops != 1 {
		t.Fatal(result, err, host.stops)
	}
	for _, pid := range oldPIDs {
		if !processGroupEmpty(pid) {
			t.Fatal("predecessor worker survived forward drain")
		}
	}
	pid := 0
	for _, member := range members {
		c, err := base.s.member(member)
		if err != nil {
			t.Fatal(err)
		}
		if c.record.WorkerTopology != "shared-session-v1" {
			t.Fatal("successor lacks shared marker")
		}
		if pid != 0 && pid != c.record.WorkerPID {
			t.Fatal("successor members are separate processes")
		}
		pid = c.record.WorkerPID
	}
	for path, before := range histories {
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatal("forward upgrade rewrote native history", err)
		}
	}
}
