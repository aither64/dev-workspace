package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type checkedInputWorker struct {
	*snapshotWorker
	identities, enqueues, historyChecks int
	mutateIdentity                      func(*workerIdentityProof)
	enqueueError                        error
	verifyHistory                       bool
}

func (w *checkedInputWorker) Call(ctx context.Context, op string, input, result any) error {
	if op != "identity_v1" && op != "input_enqueue_v1" && op != "verify_history" {
		return w.snapshotWorker.Call(ctx, op, input, result)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var value any
	switch op {
	case "identity_v1":
		w.identities++
		s := w.snapshot
		proof := workerIdentityProof{Version: 1, NativeID: s.NativeID, HistoryFile: s.HistoryFile, Persisted: s.Persisted, Busy: s.Busy, Held: s.Held, Model: s.Model, Provider: s.Provider, Thinking: s.Thinking, InstructionsDigest: s.InstructionsDigest, InputDelivery: 1}
		if w.mutateIdentity != nil {
			w.mutateIdentity(&proof)
		}
		value = proof
	case "input_enqueue_v1":
		w.enqueues++
		if w.enqueueError != nil {
			return w.enqueueError
		}
		value = nativeResult{Outcome: "NotSubmitted", Refusal: "fixture native capacity refusal"}
	case "verify_history":
		w.historyChecks++
		value = struct{ Verified bool }{w.verifyHistory}
	}
	data, _ := json.Marshal(value)
	return json.Unmarshal(data, result)
}

func checkedInputFixture(t *testing.T) (*Manager, *fakeHost, *conversation, *checkedInputWorker, InputSelection, context.Context) {
	t.Helper()
	m, host, start, ctx := supervisorFixture(t)
	transcriptContractFixture(t, m, start, `{"piTranscriptSnapshot":1,"piInputDelivery":1}`)
	for _, target := range []*func(string) (bool, error){&m.config.conversationIdentityContract, &m.config.checkedInputContract} {
		*target = func(app string) (bool, error) {
			if app != start.AppStorePath {
				t.Fatal("mutable/new package selected", app)
			}
			return true, nil
		}
	}
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	host.s.verifiedInstanceDirectory = m.instance(runtime.Runtime)
	w := &checkedInputWorker{snapshotWorker: &snapshotWorker{}, verifyHistory: true}
	host.s.start = func(_ context.Context, _ workerMetadata, h agentbackend.ExecutionHandle, config workerConfig, _ func()) (worker, error) {
		w.snapshot = workerSnapshot{NativeID: h.Conversation.NativeID, HistoryFile: filepath.Join(config.HistoryDir, "owned.jsonl"), Model: config.Settings.Model, Provider: config.Settings.Provider, Thinking: config.Settings.Thinking, InstructionsDigest: instructionsDigest(config.Instructions)}
		return w, nil
	}
	state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: "22222222-2222-4222-8222-222222222222", Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	c, err := host.s.member(state.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := m.ReadInputSelection(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	return m, host, c, w, selection, ctx
}

func TestConversationIdentityStrictRequiredFieldsAndCapabilities(t *testing.T) {
	proof := workerIdentityProof{Version: 1, NativeID: "native", HistoryFile: "/owned/history", Model: "model", Provider: "provider", Thinking: "off", InputDelivery: 1}
	data, _ := json.Marshal(proof)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	for key := range fields {
		copy := map[string]json.RawMessage{}
		for name, value := range fields {
			if name != key {
				copy[name] = value
			}
		}
		missing, _ := json.Marshal(copy)
		var decoded workerIdentityProof
		if json.Unmarshal(missing, &decoded) == nil {
			t.Fatal("missing scalar accepted", key)
		}
	}
	for _, key := range []string{"piConversationIdentity", "piCheckedInput"} {
		for _, raw := range []string{`null`, `[]`, `{"` + key + `":null}`, `{"` + key + `":true}`, `{"` + key + `":2}`, `{"` + key + `":1.0}`, `{"` + key + `":1,"` + key + `":1}`} {
			if _, err := decodeConversationCapability([]byte(raw), key); err == nil {
				t.Fatal("bad capability accepted", raw)
			}
		}
		if supported, err := decodeConversationCapability([]byte(`{"other":1}`), key); err != nil || supported {
			t.Fatal("legacy declared absence", supported, err)
		}
	}
}

type countingSelectionHost struct {
	Host
	inspections atomic.Int32
}

func (h *countingSelectionHost) Inspect(ctx context.Context, ref RuntimeRef) (HostObservation, error) {
	h.inspections.Add(1)
	return h.Host.Inspect(ctx, ref)
}

func TestReadInputSelectionIsDiskOnlyAndKeepsRetainedCapabilityAbsent(t *testing.T) {
	m, host, c, _, selection, ctx := checkedInputFixture(t)
	counted := &countingSelectionHost{Host: host}
	m.config.Host = counted
	directory, _ := stateDirectory(m.config.UserStateRoot, selection.Runtime.Session)
	release, err := runtimeLock(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		budget, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		got, err := m.ReadInputSelection(budget, selection.Handle)
		if err != nil || got.Runtime != selection.Runtime || !got.CheckedInput || counted.inspections.Load() != 0 {
			t.Fatal("selection waited for native ownership or inspected host", got, err)
		}
		m.config.checkedInputContract = func(string) (bool, error) { return false, nil }
		got, err = m.ReadInputSelection(ctx, selection.Handle)
		if err != nil || got.CheckedInput || got.Policy != nil {
			t.Fatal("retained absence was upgraded", got, err)
		}
		c.record.PendingSettings = &EffectiveSettings{}
		if err := c.save(); err != nil {
			t.Fatal(err)
		}
		if _, err := m.ReadInputSelection(ctx, selection.Handle); err == nil {
			t.Fatal("malformed pending settings accepted")
		}
		c.record.PendingSettings = nil
		c.record.Loaded = false
		if err := c.save(); err != nil {
			t.Fatal(err)
		}
		if _, err := m.ReadInputSelection(ctx, selection.Handle); !errors.Is(err, ErrUnavailable) {
			t.Fatal("unloaded mapping implied readiness", err)
		}
	}()
}

func TestConversationIdentityBindVerifyAndCapabilitiesNeverSnapshot(t *testing.T) {
	m, _, _, w, selection, ctx := checkedInputFixture(t)
	snapshots := w.snapshots
	ops, err := m.Bind(ctx, selection.Handle)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ops.Verify.Verify(ctx, BackendIdentity(selection.Handle)); err != nil {
			t.Fatal(err)
		}
		if supported, err := m.InputDelivery(ctx, selection.Handle); err != nil || !supported {
			t.Fatal(supported, err)
		}
	}
	if w.snapshots != snapshots || w.identities != 5 {
		t.Fatal("identity/capability reconstructed history or cached proof", w.snapshots, w.identities)
	}
	for _, field := range []string{"native", "history", "model", "thinking", "instructions", "held", "version"} {
		w.mutateIdentity = func(proof *workerIdentityProof) {
			switch field {
			case "native":
				proof.NativeID = "foreign"
			case "history":
				proof.HistoryFile = "/foreign/history/file"
			case "model":
				proof.Model = "foreign"
			case "thinking":
				proof.Thinking = "foreign"
			case "instructions":
				proof.InstructionsDigest = strings.Repeat("f", 64)
			case "held":
				proof.Held = true
			case "version":
				proof.Version = 2
			}
		}
		if err := ops.Verify.Verify(ctx, BackendIdentity(selection.Handle)); err == nil {
			t.Fatal("changed live proof accepted", field)
		}
		if w.snapshots != snapshots {
			t.Fatal("failed advertised identity fell back", field)
		}
	}
	w.mutateIdentity = nil
	w.snapshot.Held = true
	readOps, delivery, err := m.BindRead(ctx, selection.Handle)
	if err != nil || delivery {
		t.Fatal("verified held worker lost readonly binding", delivery, err)
	}
	transcript, err := readOps.Read.Read(ctx, BackendIdentity(selection.Handle))
	if err != nil || transcript.Status != "held" || transcript.InputDelivery {
		t.Fatal("held history became unreadable or executable", transcript, err)
	}
	if err := ops.Verify.Verify(ctx, BackendIdentity(selection.Handle)); !errors.Is(err, ErrHeld) {
		t.Fatal("held effect verifier granted execution", err)
	}
	w.snapshot.Held = false
	snapshots = w.snapshots // the explicit readonly transcript still uses its original snapshot path
	m.config.verifyProcess = func(HostObservation) error { return ErrScope }
	beforeProcess := w.identities
	if err := ops.Verify.Verify(ctx, BackendIdentity(selection.Handle)); err == nil || w.identities != beforeProcess {
		t.Fatal("worker proof bypassed fresh process admission", err)
	}
	m.config.verifyProcess = func(HostObservation) error { return nil }
	m.config.conversationIdentityContract = func(string) (bool, error) { return false, ErrScope }
	if err := ops.Verify.Verify(ctx, BackendIdentity(selection.Handle)); err == nil || w.snapshots != snapshots {
		t.Fatal("malformed claimed capability fell back", err)
	}
	m.config.conversationIdentityContract = func(string) (bool, error) { return false, nil }
	m.config.checkedInputContract = func(string) (bool, error) { return false, nil }
	before := w.identities
	if _, err := m.Bind(ctx, selection.Handle); err != nil {
		t.Fatal(err)
	}
	if w.identities != before || w.snapshots != snapshots+1 {
		t.Fatal("retained worker did not use exact legacy path")
	}
}
