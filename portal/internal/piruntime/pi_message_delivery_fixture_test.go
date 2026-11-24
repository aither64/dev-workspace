//go:build pi_native_fixture

package piruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime/trace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// Native counters observe attempted and complete serialized pipe writes, not
// arbitrary Call results. The original endpoint, bytes and Close are preserved.
// Outcome/witness checks and hold writes retain later failure evidence.
type PiMessageDeliveryCounts struct {
	HostInspects, HostStarts                                          int
	AdmissionHostInspects                                             int
	AdmissionRuntimeDials                                             int
	Calls                                                             map[string]int
	FullWrites                                                        map[string]int
	FailedWrites                                                      map[string]int
	InjectedReadUncertainty                                           int
	Enqueues                                                          map[string]int
	ByMember                                                          map[string]map[string]int
	ReceiptObserverCalls, ReceiptObserverReads, ReceiptObserverErrors int
	ReceiptObserverNS, ReceiptObserverMaxNS                           int64
	ReceiptObserverMaxLive, ReceiptObserverMaxPending                 int
	ReceiptDelegateCalls, ReceiptDelegateErrors                       int
	ReceiptDelegateNS, ReceiptDelegateMaxNS                           int64
}

type piDeliveryAdmissionKey struct{}

// A test-only context marker attributes calls to the synchronous POST handler,
// separating that path from concurrent, already-admitted native continuations.
func PiMessageDeliveryAdmissionContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, piDeliveryAdmissionKey{}, true)
}

type piDeliveryCounts struct {
	mu            sync.Mutex
	value         PiMessageDeliveryCounts
	failing       string
	commits       map[piDeliveryInputKey]PiMessageDeliveryAcceptance
	expected      map[piDeliveryInputKey]*piDeliveryExpectedReceipt
	receiptActive map[*receiptEngine]*piDeliveryExpectedReceipt
	receiptArmed  map[*receiptEngine]bool
}

type piDeliveryInputKey struct {
	Conversation agentbackend.ConversationRef
	ID           string
}
type PiMessageDeliveryAcceptance struct {
	Handle     agentbackend.ExecutionHandle
	ObservedAt time.Time
}

// This time is a POST-SYNC CHECKPOINT UPPER, not fsync return, SDK
// consumption, or worker commit completion. Keep its monotonic clock in memory.
type PiMessageDeliveryCheckpoint struct {
	Ref                  SubmissionRef
	Runtime              RuntimeRef
	Handle               agentbackend.ExecutionHandle
	Witness              NativeWitness
	ObservedAt           time.Time
	VerificationNS       int64
	Error, DelegateError string
}
type piDeliveryExpectedReceipt struct {
	handle           agentbackend.ExecutionHandle
	request          QueuedInputRequest
	registered       bool
	invalid, missing string
	event            PiMessageDeliveryCheckpoint
}

// Declare only actual timed samples. Setup, questions and helper turns are not
// silently added to the latency set. No engine lock is taken with counts.mu held.
func (f *PiMessageDeliveryFixture) ExpectCheckpoint(h agentbackend.ExecutionHandle, id string) {
	f.counts.mu.Lock()
	defer f.counts.mu.Unlock()
	if f.counts.expected == nil {
		f.counts.expected = map[piDeliveryInputKey]*piDeliveryExpectedReceipt{}
	}
	key := piDeliveryInputKey{h.Conversation, id}
	if old := f.counts.expected[key]; old != nil {
		if old.handle != h {
			old.invalid = "timed execution identity changed"
		}
		return
	}
	f.counts.expected[key] = &piDeliveryExpectedReceipt{handle: h}
}

// Register after existing dispatch fault setup, before the one Manager call.
// Errors affect measurement evidence only; they never change native admission.
func (f *PiMessageDeliveryFixture) RegisterCheckpoint(q QueuedInputRequest) {
	key := piDeliveryInputKey{q.Ref.Conversation, q.Ref.OperationID}
	f.counts.mu.Lock()
	target := f.counts.expected[key]
	f.counts.mu.Unlock()
	if target == nil {
		return
	}
	fail := func(reason string) { f.counts.mu.Lock(); target.invalid = reason; f.counts.mu.Unlock() }
	owner := f.base.s
	if owner == nil || owner.ref != q.Selection.Runtime || validateQueuedInput(q) != nil || target.handle != q.Selection.Handle {
		fail("timed request scope or full execution runtime changed")
		return
	}
	c, err := owner.member(q.Ref.Conversation)
	if err != nil {
		fail("timed receipt owner unavailable")
		return
	}
	copy := q
	if q.Selection.Policy != nil {
		policy := cloneToolPolicy(*q.Selection.Policy)
		copy.Selection.Policy = &policy
	}
	e := c.receipts
	e.mu.Lock()
	defer e.mu.Unlock()
	f.counts.mu.Lock()
	if f.counts.receiptActive == nil {
		f.counts.receiptActive = map[*receiptEngine]*piDeliveryExpectedReceipt{}
		f.counts.receiptArmed = map[*receiptEngine]bool{}
	}
	if target.registered && !reflect.DeepEqual(target.request, copy) {
		target.invalid = "timed original request changed"
	}
	if old := f.counts.receiptActive[e]; old != nil && old != target && old.event.ObservedAt.IsZero() && old.invalid == "" {
		old.invalid = "overlapping timed requests in one conversation"
		target.invalid = old.invalid
		f.counts.value.ReceiptObserverMaxPending = max(f.counts.value.ReceiptObserverMaxPending, 2)
	} else {
		f.counts.value.ReceiptObserverMaxPending = max(f.counts.value.ReceiptObserverMaxPending, 1)
	}
	target.request = copy
	target.registered = true
	f.counts.receiptActive[e] = target
	armed := f.counts.receiptArmed[e]
	if !armed {
		f.counts.receiptArmed[e] = true
	}
	f.counts.mu.Unlock()
	if armed {
		return
	}
	previous := e.checkpoint
	runtime := owner.ref
	e.checkpoint = func(name string) error {
		entered := time.Now()
		if name == "receipt" {
			f.observeReceiptLocked(e, runtime, entered)
		}
		if previous == nil {
			return nil
		}
		start := time.Now()
		err := previous(name)
		elapsed := time.Since(start).Nanoseconds()
		f.counts.mu.Lock()
		f.counts.value.ReceiptDelegateCalls++
		f.counts.value.ReceiptDelegateNS += elapsed
		f.counts.value.ReceiptDelegateMaxNS = max(f.counts.value.ReceiptDelegateMaxNS, elapsed)
		if name == "receipt" && err != nil {
			f.counts.value.ReceiptDelegateErrors++
			if current := f.counts.receiptActive[e]; current != nil && current.event.ObservedAt == entered {
				current.event.DelegateError = err.Error()
			}
		}
		f.counts.mu.Unlock()
		return err
	}
}

// The caller already owns e.mu. Never take c.mu or call Manager/native ports.
// loadOwned is the owning strict, nonlocking local reader. Its I/O and this
// mutex occupancy are real observer overhead, separately reported below.
func (f *PiMessageDeliveryFixture) observeReceiptLocked(e *receiptEngine, owner RuntimeRef, entered time.Time) {
	f.counts.mu.Lock()
	target := f.counts.receiptActive[e]
	if target == nil || target.invalid != "" || !target.event.ObservedAt.IsZero() {
		f.counts.mu.Unlock()
		return
	}
	q := target.request
	f.counts.mu.Unlock()
	reads := 0
	var event *PiMessageDeliveryCheckpoint
	invalid, missing := "", ""
	defer func() {
		f.counts.mu.Lock()
		defer f.counts.mu.Unlock()
		// Includes strict readback and wait for publication's short counter lock;
		// excludes the final counter assignments and delegated injector below.
		elapsed := time.Since(entered).Nanoseconds()
		value := &f.counts.value
		value.ReceiptObserverCalls++
		value.ReceiptObserverReads += reads
		value.ReceiptObserverNS += elapsed
		value.ReceiptObserverMaxNS = max(value.ReceiptObserverMaxNS, elapsed)
		value.ReceiptObserverMaxLive = max(value.ReceiptObserverMaxLive, len(e.liveInputs))
		if invalid != "" {
			target.invalid = invalid
			value.ReceiptObserverErrors++
		}
		if missing != "" {
			target.missing = missing
		}
		if event != nil && target.invalid == "" && target.event.ObservedAt.IsZero() {
			event.VerificationNS = elapsed
			target.event = *event
		}
	}()
	if !target.registered || q.Selection.Runtime != owner || q.Selection.Handle != target.handle || q.Ref.Conversation != target.handle.Conversation || q.Selection.Handle.Generation != owner.Generation {
		invalid = "receipt checkpoint execution mismatch"
		return
	}
	live := e.liveInputs[q.Ref.OperationID]
	if live == nil {
		missing = "no still-live owner at receipt checkpoint"
		return
	}
	if live.worker == nil || live.ref.retained == nil || *live.ref.retained != q.Ref {
		invalid = "receipt checkpoint live original mismatch"
		return
	}
	record, err := e.loadOwned(q.Ref.OperationID)
	reads++
	if err != nil {
		invalid = "receipt checkpoint strict record unavailable"
		return
	}
	if record.Phase != "Accepted" {
		missing = "no Accepted record at receipt checkpoint"
		return
	}
	if record.ref.retained == nil || *record.ref.retained != q.Ref || record.Input != q.Input || record.Settings != q.Selection.Settings || record.Generation != owner.Generation || SubmissionDigest(q.Ref, record.Input, record.Settings) != q.Ref.Digest || record.Witness == nil || record.Witness.SessionID != q.Ref.Conversation.NativeID || record.Witness.EntryID == "" || record.Witness.EffectiveDigest != MessageDigest(q.Input.Message) {
		invalid = "receipt checkpoint original or witness mismatch"
		return
	}
	// A successful finishInput still owns its live entry until this callback
	// returns. Earlier completions delete theirs before releasing the same mutex.
	if len(e.liveInputs) > 20 {
		invalid = "receipt checkpoint live-owner bound exceeded"
		return
	}
	for id, other := range e.liveInputs {
		if id == q.Ref.OperationID {
			continue
		}
		retained, err := e.loadOwned(id)
		reads++
		if err != nil || other == nil || !retained.ref.equal(other.ref) {
			invalid = "receipt checkpoint cannot prove unique owner"
			return
		}
		if retained.Phase == "Accepted" {
			invalid = "ambiguous Accepted live owners at receipt checkpoint"
			return
		}
	}
	event = &PiMessageDeliveryCheckpoint{Ref: q.Ref, Runtime: owner, Handle: q.Selection.Handle, Witness: *record.Witness, ObservedAt: entered}
}

func (f *PiMessageDeliveryFixture) AcceptedCheckpoint(h agentbackend.ExecutionHandle, id string) (PiMessageDeliveryCheckpoint, bool) {
	f.counts.mu.Lock()
	defer f.counts.mu.Unlock()
	target := f.counts.expected[piDeliveryInputKey{h.Conversation, id}]
	if target == nil {
		return PiMessageDeliveryCheckpoint{Error: "timed checkpoint was not declared"}, false
	}
	if target.handle != h {
		return PiMessageDeliveryCheckpoint{Error: "checkpoint requested for different full execution handle"}, false
	}
	event := target.event
	if target.invalid != "" {
		event.Error = target.invalid
	}
	if event.ObservedAt.IsZero() && event.Error == "" {
		event.Error = target.missing
		if event.Error == "" {
			event.Error = "identified post-sync receipt checkpoint missing"
		}
	}
	return event, event.Error == "" && !event.ObservedAt.IsZero()
}

func (c *piDeliveryCounts) operation(member, operation, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value.Calls[operation]++
	if c.value.ByMember[member] == nil {
		c.value.ByMember[member] = map[string]int{}
	}
	c.value.ByMember[member][operation]++
	if operation == "input_enqueue_v1" {
		c.value.Enqueues[id]++
	}
	return c.failing == member && operation == "workflow_observe"
}

type piDeliveryInput struct {
	io.WriteCloser
	counts *piDeliveryCounts
}

func (w *piDeliveryInput) Write(data []byte) (int, error) {
	// workerHost.write already owns writeMu. Never reacquire it or call
	// host.write here. Callback/result frames pass through untouched.
	entered := time.Now()
	var frame struct {
		ID        string          `json:"id"`
		Operation string          `json:"op"`
		Identity  workerIdentity  `json:"identity"`
		Body      json.RawMessage `json:"body"`
	}
	var input struct {
		ID string `json:"inputOperationID"`
	}
	observe := json.Unmarshal(data, &frame) == nil && frame.Identity.Conversation.NativeID != "" && frame.Identity.OwnerKind == "" && frame.Identity.Utility == nil && frame.Operation != "host_result" && frame.Operation != "host_cancel" && json.Unmarshal(frame.Body, &input) == nil
	inject := false
	if observe {
		inject = w.counts.operation(frame.Identity.Conversation.Member, frame.Operation, input.ID)
		trace.Logf(context.Background(), "pi.fixture.frame.enter", "frame=%s op=%s handle=%+v originalID=%s writeEntryUnixNS=%d", frame.ID, frame.Operation, agentbackend.ExecutionHandle{Conversation: frame.Identity.Conversation, Generation: frame.Identity.Generation}, input.ID, entered.UnixNano())
	}
	n, err := w.WriteCloser.Write(data)
	completed := time.Now()
	if !observe {
		return n, err
	}
	trace.Logf(context.Background(), "pi.fixture.frame.return", "frame=%s op=%s full=%t written=%d expected=%d error=%v writeReturnUnixNS=%d elapsedNS=%d", frame.ID, frame.Operation, n == len(data) && err == nil, n, len(data), err, completed.UnixNano(), completed.Sub(entered).Nanoseconds())
	w.counts.mu.Lock()
	if n == len(data) && err == nil {
		w.counts.value.FullWrites[frame.Operation]++
		if frame.Operation == "input_commit_v1" && operationPattern.MatchString(input.ID) {
			handle := agentbackend.ExecutionHandle{Conversation: frame.Identity.Conversation, Generation: frame.Identity.Generation}
			key := piDeliveryInputKey{handle.Conversation, input.ID}
			if _, exists := w.counts.commits[key]; !exists {
				w.counts.commits[key] = PiMessageDeliveryAcceptance{handle, entered}
			}
		}
		if inject {
			w.counts.value.InjectedReadUncertainty++
		}
	} else {
		w.counts.value.FailedWrites[frame.Operation]++
		inject = false
	}
	w.counts.mu.Unlock()
	// The real read request was fully written. Only its caller observes this
	// injected write-return uncertainty; no response or witness is invented.
	if inject {
		return n, ErrUnavailable
	}
	return n, err
}

type piDeliveryTestInput struct {
	data    []byte
	short   bool
	failure error
	closed  bool
}

func (w *piDeliveryTestInput) Write(data []byte) (int, error) {
	w.data = append([]byte{}, data...)
	if w.short {
		return len(data) - 1, w.failure
	}
	return len(data), w.failure
}
func (w *piDeliveryTestInput) Close() error { w.closed = true; return w.failure }

// No SDK/process is used here. These failure boundaries guard the timing proof:
// an attempted or partial write must never publish successful native evidence.
func TestPiMessageDeliveryPipeObservation(t *testing.T) {
	handle := agentbackend.ExecutionHandle{Conversation: agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: "/fixture", Slug: "2026-11-11-fixture", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"}, Generation: strings.Repeat("c", 64)}
	for _, kind := range []string{"full", "partial", "write_error", "injected_read", "callback"} {
		t.Run(kind, func(t *testing.T) {
			counts := &piDeliveryCounts{value: PiMessageDeliveryCounts{Calls: map[string]int{}, FullWrites: map[string]int{}, FailedWrites: map[string]int{}, Enqueues: map[string]int{}, ByMember: map[string]map[string]int{}}, commits: map[piDeliveryInputKey]PiMessageDeliveryAcceptance{}}
			original := &piDeliveryTestInput{short: kind == "partial"}
			if kind == "write_error" {
				original.failure = io.ErrClosedPipe
			}
			op := "input_commit_v1"
			if kind == "injected_read" {
				op = "workflow_observe"
				counts.failing = "lead"
			}
			if kind == "callback" {
				op = "host_result"
			}
			id := strings.Repeat("1", 32)
			raw, err := json.Marshal(map[string]any{"schema": 1, "id": "reply-id", "op": op, "identity": retainedWorkerIdentity(handle), "body": map[string]string{"inputOperationID": id}})
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, '\n')
			observer := &piDeliveryInput{WriteCloser: original, counts: counts}
			n, err := observer.Write(raw)
			if !bytes.Equal(raw, original.data) {
				t.Fatal("observer changed exact native frame bytes")
			}
			_, published := counts.commits[piDeliveryInputKey{handle.Conversation, id}]
			if published != (kind == "full") {
				t.Fatal("partial/failed/unrelated frame published native acceptance", kind, published)
			}
			if kind == "partial" {
				if n != len(raw)-1 || err != nil || counts.value.FailedWrites[op] != 1 {
					t.Fatal("partial write result changed", n, err)
				}
			} else if kind == "write_error" {
				if !errors.Is(err, io.ErrClosedPipe) || counts.value.FailedWrites[op] != 1 {
					t.Fatal("underlying write error changed", err)
				}
			} else if kind == "injected_read" {
				if n != len(raw) || !errors.Is(err, ErrUnavailable) || counts.value.InjectedReadUncertainty != 1 || counts.value.FullWrites[op] != 1 || counts.value.FailedWrites[op] != 0 {
					t.Fatal("injection was not a fully delegated read with separate uncertainty", n, err, counts.value)
				}
			} else if err != nil || n != len(raw) {
				t.Fatal("complete frame changed", n, err)
			}
			if kind == "callback" && len(counts.value.Calls) != 0 {
				t.Fatal("callback was interpreted as a native request")
			}
			if err := observer.Close(); !original.closed || !errors.Is(err, original.failure) {
				t.Fatal("Close not delegated unchanged", err)
			}
		})
	}
}

type piDeliveryHost struct {
	*upgradeFixtureHost
	counts *piDeliveryCounts
}

func (h *piDeliveryHost) Inspect(ctx context.Context, ref RuntimeRef) (HostObservation, error) {
	h.counts.mu.Lock()
	h.counts.value.HostInspects++
	if ctx.Value(piDeliveryAdmissionKey{}) == true {
		h.counts.value.AdmissionHostInspects++
	}
	h.counts.mu.Unlock()
	return h.upgradeFixtureHost.Inspect(ctx, ref)
}
func (h *piDeliveryHost) Start(ctx context.Context, ref RuntimeRef) (HostObservation, error) {
	h.counts.mu.Lock()
	h.counts.value.HostStarts++
	h.counts.mu.Unlock()
	return h.upgradeFixtureHost.Start(ctx, ref)
}
func (h *piDeliveryHost) Stop(ctx context.Context, ref RuntimeRef, observed HostObservation) (HostObservation, error) {
	// This kills only the fixture's actual Node child. The host's subsequent
	// terminal receipt/cgroup drain is synthetic, as in the existing P6 fixture.
	if h.stopErr == nil {
		piDeliveryKillWorkers(h.base.s)
	}
	return h.upgradeFixtureHost.Stop(ctx, ref, observed)
}

func piDeliveryKillWorkers(s *supervisor) {
	seen := map[*workerHost]bool{}
	for _, c := range s.members {
		if p, ok := c.worker.(*workerProcess); ok && !seen[p.host] {
			seen[p.host] = true
			select {
			case <-p.host.closed:
			default:
				_ = p.host.command.Process.Kill()
				<-p.host.closed
			}
		}
	}
}

// PiMessageDeliveryFixture composes the existing P6 Unix/SDK bootstrap. Reopen
// means a Manager only; portal Close/restart and Node process loss are named
// separately in the external tests. It is never a production fixture API.
type PiMessageDeliveryFixture struct {
	P6NativeFixture
	Start             StartRequest
	WorkerMetadata    workerMetadata
	Entrypoint, Gates string
	base              *fakeHost
	counts            *piDeliveryCounts
}

// Prepare transport/configuration only. The external suite freezes and validates
// its catalog-derived intent before calling Ensure or creating native effects.
func NewPiMessageDeliveryFixture(t *testing.T, parent context.Context, modelIDs []string) *PiMessageDeliveryFixture {
	t.Helper()
	for _, key := range []string{"PI_PROBE_PACKAGE", "PI_CENTRAL_APP_ROOT", "PI_CENTRAL_CATALOG_FIXTURE"} {
		if os.Getenv(key) == "" {
			t.Fatalf("%s is required; compilation is not campaign execution", key)
		}
	}
	m, base, start, ctx := supervisorFixtureWithContext(t, parent)
	app := os.Getenv("PI_CENTRAL_APP_ROOT")
	canonical, err := filepath.EvalSymlinks(app)
	info, statErr := os.Stat(app)
	if err != nil || statErr != nil || canonical != app || !storePattern.MatchString(app) || !info.IsDir() || info.Mode().Perm()&0222 != 0 {
		t.Fatal("immutable candidate application required", app, err, statErr)
	}
	if err := ValidatePackage(app); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(app, "share/dev-workspace/pi-worker.json"))
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := DecodeWorkerMetadata(data)
	if err != nil || metadata.Protocol != 2 || metadata.PiPackage != os.Getenv("PI_PROBE_PACKAGE") {
		t.Fatal("candidate worker/SDK provenance", metadata, err)
	}
	for _, path := range []string{metadata.Node, metadata.Entrypoint, filepath.Join(filepath.Dir(metadata.Entrypoint), "protocol.mjs"), filepath.Join(filepath.Dir(metadata.Entrypoint), "persistence.mjs")} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0222 != 0 {
			t.Fatal("immutable worker input", path, err)
		}
	}
	// Match the exact productionSource filter in nix/pi-worker.nix. A capable
	// but different retained worker is not evidence for this candidate's source.
	sourceRoot := filepath.Join("..", "..", "..", "pi-runtime", "src")
	modules, err := os.ReadDir(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, module := range modules {
		if module.IsDir() || !strings.HasSuffix(module.Name(), ".mjs") || strings.HasSuffix(module.Name(), ".test.mjs") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(sourceRoot, module.Name()))
		if err != nil {
			t.Fatal(err)
		}
		packaged, err := os.ReadFile(filepath.Join(filepath.Dir(metadata.Entrypoint), module.Name()))
		if err != nil || !bytes.Equal(source, packaged) {
			t.Fatal("candidate worker differs from compiled fixture source", module.Name(), err)
		}
	}
	for _, read := range []func(string) (bool, error){readConversationIdentityContract, readCheckedInputContract, InputDeliveryPackage} {
		if supported, err := read(app); err != nil || !supported {
			t.Fatal("candidate lacks required native contract", supported, err)
		}
	}
	start.AppStorePath = app
	if err := os.Symlink(app, start.ProfilePath); err != nil {
		t.Fatal(err)
	}
	m.config.transcriptSnapshotContract = readTranscriptSnapshotContract
	m.config.inputDeliveryContract = readInputDeliveryContract
	m.config.activityContract = readActivityContract
	m.config.conversationIdentityContract = readConversationIdentityContract
	m.config.checkedInputContract = readCheckedInputContract
	models, err := LoadInstalledModelConfiguration(app)
	if err != nil {
		t.Fatal(err)
	}
	if models.Path != "" {
		m.config.AccountBindings = append([]AccountBinding{{Provider: "openai", AccountRef: "fixture-unused-openai"}}, m.config.AccountBindings...)
	}
	gates := t.TempDir()
	entry := filepath.Join(t.TempDir(), "delivery-worker.mjs")
	quoted := func(s string) string { data, _ := json.Marshal(s); return string(data) }
	if len(modelIDs) == 0 {
		t.Fatal("exact catalog-derived faux model IDs required")
	}
	modelJSON, err := json.Marshal(modelIDs)
	if err != nil {
		t.Fatal(err)
	}
	script := strings.NewReplacer("PROTOCOL", quoted(filepath.Join(filepath.Dir(metadata.Entrypoint), "protocol.mjs")), "PERSISTENCE", quoted(filepath.Join(filepath.Dir(metadata.Entrypoint), "persistence.mjs")), "GATES", quoted(gates), "MODELS", string(modelJSON), "MODEL_RUNTIME", piDeliveryModelRuntimeSource).Replace(piDeliveryWorkerSource)
	if err := os.WriteFile(entry, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	base.metadata = workerMetadata{Node: metadata.Node, Entrypoint: entry, PiPackage: metadata.PiPackage, Protocol: metadata.Protocol}
	counts := &piDeliveryCounts{value: PiMessageDeliveryCounts{Calls: map[string]int{}, FullWrites: map[string]int{}, FailedWrites: map[string]int{}, Enqueues: map[string]int{}, ByMember: map[string]map[string]int{}}, commits: map[piDeliveryInputKey]PiMessageDeliveryAcceptance{}}
	dial := m.config.Dial
	m.config.Dial = func(ctx context.Context, path string) (net.Conn, error) {
		if ctx.Value(piDeliveryAdmissionKey{}) == true {
			counts.mu.Lock()
			counts.value.AdmissionRuntimeDials++
			counts.mu.Unlock()
		}
		return dial(ctx, path)
	}
	host := &piDeliveryHost{upgradeFixtureHost: newUpgradeFixtureHost(base), counts: counts}
	host.onStart = func(s *supervisor) {
		s.verifiedInstanceDirectory = m.instance(s.ref)
		original := s.start
		s.start = func(ctx context.Context, meta workerMetadata, h agentbackend.ExecutionHandle, cfg workerConfig, changed func()) (worker, error) {
			w, err := original(ctx, meta, h, cfg, func() { counts.operation(h.Conversation.Member, "invalidate-event", ""); changed() })
			if err != nil {
				return nil, err
			}
			endpoint, ok := w.(*workerProcess)
			if !ok || !endpoint.host.shared {
				return nil, ErrScope
			}
			endpoint.host.writeMu.Lock()
			if _, installed := endpoint.host.input.(*piDeliveryInput); !installed {
				endpoint.host.input = &piDeliveryInput{WriteCloser: endpoint.host.input, counts: counts}
			}
			endpoint.host.writeMu.Unlock()
			// Production owns topology and detach through this concrete endpoint.
			return w, nil
		}
	}
	m.config.Host = host
	m.config.verifyDrain = func(HostObservation) error { return nil } // synthetic host only
	f := &PiMessageDeliveryFixture{P6NativeFixture: P6NativeFixture{Manager: m, Context: ctx}, Start: start, WorkerMetadata: metadata, Entrypoint: entry, Gates: gates, base: base, counts: counts}
	f.Attach = func(handler HostCallbackHandler) { attachFixture(t, m, base.s, ctx, base.s.ref, handler) }
	f.Idle = func(handle agentbackend.ExecutionHandle) { waitIdle(t, m, ctx, handle) }
	f.Reopen = func() (*Manager, error) { return NewManager(m.config) }
	t.Cleanup(func() {
		if base.s != nil {
			piDeliveryKillWorkers(base.s)
		}
	})
	return f
}

func (f *PiMessageDeliveryFixture) Ensure(t *testing.T) {
	t.Helper()
	status, err := f.Manager.Ensure(f.Context, f.Start)
	if err != nil {
		t.Fatal("existing Unix bootstrap", err)
	}
	f.Runtime = status.Runtime
}

// Check the actual generated worker's model-runtime factory with its pinned
// Node/SDK. This creates no AgentSession, supervisor, history, receipt or input.
func CheckPiMessageDeliveryModels(t *testing.T, selections []EffectiveSettings) {
	t.Helper()
	pi := os.Getenv("PI_PROBE_PACKAGE")
	wrapper, err := os.ReadFile(filepath.Join(pi, "bin/pi"))
	if err != nil || !storePattern.MatchString(pi) {
		t.Fatal("explicit immutable SDK model preflight input required", err)
	}
	// As in P6, use the SDK wrapper's exact Node, never the ambient executable.
	// This check runs before a candidate app is built; native setup separately
	// validates that candidate's complete metadata, worker bytes and SDK input.
	node := regexp.MustCompile(`/nix/store/[a-z0-9]{32}-nodejs-[^/\s"']+/bin/node`).FindString(string(wrapper))
	if !storeFile(node) {
		t.Fatal("SDK wrapper has no exact Node input")
	}
	data, err := json.Marshal(selections)
	if err != nil || len(selections) == 0 {
		t.Fatal("saved selections required", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	script := strings.ReplaceAll(piDeliveryModelCheckSource, "MODEL_RUNTIME", piDeliveryModelRuntimeSource)
	command := exec.CommandContext(ctx, node, "--input-type=module", "-e", script, pi, t.TempDir(), string(data))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("actual SDK model/effort preflight: %v %s", err, output)
	}
	t.Logf("SDK model preflight node=%s sdk=%s\n%s", node, pi, strings.TrimSpace(string(output)))
}

type PiMessageDeliveryCounterSnapshot struct {
	ObservedAt time.Time
	Counts     PiMessageDeliveryCounts
}

func (f *PiMessageDeliveryFixture) CounterSnapshot() PiMessageDeliveryCounterSnapshot {
	f.counts.mu.Lock()
	defer f.counts.mu.Unlock()
	data, _ := json.Marshal(f.counts.value)
	var copy PiMessageDeliveryCounts
	_ = json.Unmarshal(data, &copy)
	return PiMessageDeliveryCounterSnapshot{ObservedAt: time.Now(), Counts: copy}
}
func (f *PiMessageDeliveryFixture) Counts() PiMessageDeliveryCounts {
	return f.CounterSnapshot().Counts
}

// Capture one immutable count/time pair. Later native events cannot mutate a
// retained warm or measured endpoint through nested map aliases.
func TestPiMessageDeliveryCounterSnapshot(t *testing.T) {
	counts := &piDeliveryCounts{value: PiMessageDeliveryCounts{Calls: map[string]int{"snapshot": 20}, Enqueues: map[string]int{"warm": 1}, ByMember: map[string]map[string]int{"lead": {"snapshot": 20}}}}
	f := &PiMessageDeliveryFixture{counts: counts}
	warm := f.CounterSnapshot()
	counts.mu.Lock()
	counts.value.Calls["snapshot"] += 4
	counts.value.Enqueues["measured"] = 1
	counts.value.ByMember["lead"]["snapshot"] += 4
	counts.mu.Unlock()
	measured := f.CounterSnapshot()
	counts.mu.Lock()
	counts.value.Calls["snapshot"]++
	counts.value.ByMember["lead"]["snapshot"]++
	counts.mu.Unlock()
	if measured.ObservedAt.Before(warm.ObservedAt) || warm.Counts.Calls["snapshot"] != 20 || measured.Counts.Calls["snapshot"] != 24 || measured.Counts.ByMember["lead"]["snapshot"] != 24 || warm.Counts.Enqueues["measured"] != 0 {
		t.Fatal("count interval mutated after capture", warm, measured)
	}
}

// Preserve only disposable conversation/receipt and worker error evidence before
// TempDir cleanup. No auth files, history mutation, RPC or acceptance is added.
func (f *PiMessageDeliveryFixture) SetupEvidence() map[string]any {
	read := func(path string) map[string]any {
		data, err := os.ReadFile(path)
		info, statErr := os.Stat(path)
		mode := ""
		if info != nil {
			mode = info.Mode().String()
		}
		return map[string]any{"bytes": string(data), "mode": mode, "readError": fmt.Sprint(err), "statError": fmt.Sprint(statErr)}
	}
	f.base.s.mu.Lock()
	roots := []string{}
	for root := range f.base.s.members {
		roots = append(roots, root)
	}
	f.base.s.mu.Unlock()
	records := map[string]any{}
	for _, root := range roots {
		relative, _ := filepath.Rel(f.base.s.stateRoot, root)
		files := map[string]any{"conversation.json": read(filepath.Join(root, "conversation.json"))}
		entries, err := os.ReadDir(filepath.Join(root, "submissions"))
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				files["submissions/"+entry.Name()] = read(filepath.Join(root, "submissions", entry.Name()))
			}
		}
		records[relative] = map[string]any{"files": files, "submissionsError": fmt.Sprint(err)}
	}
	return map[string]any{"records": records, "workerErrors": read(filepath.Join(f.Gates, "worker-errors.log"))}
}

// finishInput writes input_commit_v1 only after native witness validation and
// durable Accepted receipt persistence. The serialized write-entry timestamp is
// published only after a complete successful write, and bounds native acceptance
// from above. It is neither portal queue commit time nor worker commit completion.
func (f *PiMessageDeliveryFixture) AcceptedUpper(handle agentbackend.ExecutionHandle, id string) (PiMessageDeliveryAcceptance, bool) {
	f.counts.mu.Lock()
	defer f.counts.mu.Unlock()
	value, ok := f.counts.commits[piDeliveryInputKey{handle.Conversation, id}]
	return value, ok
}
func (f *PiMessageDeliveryFixture) FailObservation(member string) {
	f.counts.mu.Lock()
	f.counts.failing = member
	f.counts.mu.Unlock()
}

func (f *PiMessageDeliveryFixture) History(t *testing.T, h agentbackend.ExecutionHandle) (recoveryHistory, []string) {
	t.Helper()
	c, err := f.base.s.member(h.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	record := c.record
	c.mu.Unlock()
	inventory, err := captureRecoveryHistory(record)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(record.HistoryFile)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 4096), recordLimit)
	users := []string{}
	for scan.Scan() {
		var row struct {
			Message struct {
				Role    string
				Content json.RawMessage
			}
		}
		if json.Unmarshal(scan.Bytes(), &row) != nil {
			t.Fatal("native JSONL")
		}
		if row.Message.Role == "user" {
			var blocks []struct{ Type, Text string }
			if json.Unmarshal(row.Message.Content, &blocks) != nil {
				t.Fatal("native user content")
			}
			text := ""
			for _, b := range blocks {
				if b.Type == "text" {
					text += b.Text
				}
			}
			users = append(users, text)
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return inventory, users
}

func (f *PiMessageDeliveryFixture) SeedHistory(t *testing.T, h agentbackend.ExecutionHandle, entries int) {
	t.Helper()
	if entries == 0 {
		return
	}
	if _, err := f.Manager.Unload(f.Context, h); err != nil {
		t.Fatal("ordinary unload before SDK seed", err)
	}
	c, err := f.base.s.member(h.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	path := c.record.HistoryFile
	model := c.record.Settings.Model
	c.mu.Unlock()
	command := exec.CommandContext(f.Context, f.WorkerMetadata.Node, "--input-type=module", "-e", piDeliverySeedSource, f.WorkerMetadata.PiPackage, path, fmt.Sprint(entries), model)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("SDK history seed: %v %s", err, output)
	}
	if _, err := f.Manager.Load(f.Context, h); err != nil {
		t.Fatal("ordinary seeded history load", err)
	}
	inventory, _ := f.History(t, h)
	if inventory.Entries != entries {
		t.Fatalf("partial history cell: wanted%d actual%d", entries, inventory.Entries)
	}
}

// Existing receipt checkpoints inject real persistence-boundary failures. They
// never manufacture an Accepted or Unknown receipt or edit its saved bytes.
func (f *PiMessageDeliveryFixture) FailReceipt(t *testing.T, h agentbackend.ExecutionHandle, point string) func() {
	t.Helper()
	c, err := f.base.s.member(h.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	c.receipts.mu.Lock()
	original := c.receipts.checkpoint
	c.receipts.checkpoint = func(name string) error {
		if name == point {
			return ErrUnavailable
		}
		if original != nil {
			return original(name)
		}
		return nil
	}
	f.counts.mu.Lock()
	if f.counts.receiptArmed != nil {
		f.counts.receiptArmed[c.receipts] = false
	}
	f.counts.mu.Unlock()
	c.receipts.mu.Unlock()
	return func() {
		c.receipts.mu.Lock()
		c.receipts.checkpoint = original
		f.counts.mu.Lock()
		if f.counts.receiptArmed != nil {
			f.counts.receiptArmed[c.receipts] = false
		}
		f.counts.mu.Unlock()
		c.receipts.mu.Unlock()
	}
}
func (f *PiMessageDeliveryFixture) KillNative() { piDeliveryKillWorkers(f.base.s) }
func (f *PiMessageDeliveryFixture) RefuseRetirementStop(refuse bool) {
	host := f.Manager.config.Host.(*piDeliveryHost)
	if refuse {
		host.stopErr = ErrUnavailable
	} else {
		host.stopErr = nil
	}
}

// Local mapping fault only: no native identity is replaced and no receipt or
// loaded transcript is edited. Ordinary checked admission must reject it.
func (f *PiMessageDeliveryFixture) ChangeInstructions(t *testing.T, h agentbackend.ExecutionHandle) {
	t.Helper()
	c, err := f.base.s.member(h.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record.Instructions += "\nchanged disposable instruction selection"
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
}

func (f *PiMessageDeliveryFixture) ReceiptBytes(t *testing.T, ref SubmissionRef) []byte {
	t.Helper()
	c, err := f.base.s.member(ref.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(c.receipts.root, ref.OperationID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Call only after portal writers join, with no independent client/new admission
// able to add input tasks. This joins receipt continuations, not assistant turns.
// The untouched original is outside all strict member/submission roots; the
// malformed placeholder is negative storage-fault evidence, never a repair.
func (f *PiMessageDeliveryFixture) QuarantineReceipt(t *testing.T, ref SubmissionRef) (original []byte, malformed func(), restore func()) {
	t.Helper()
	f.base.s.inputTasks.Wait()
	c, err := f.base.s.member(ref.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(c.receipts.root, ref.OperationID+".json")
	original, err = os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	var status SubmissionStatus
	if strictDecode(original, &status, recordLimit) != nil || status.Ref != ref || status.Phase != "Accepted" || status.Witness == nil {
		t.Fatal("storage fault needs exact completed original")
	}
	saved := filepath.Join(t.TempDir(), "untouched-receipt")
	if err := os.Rename(source, saved); err != nil {
		t.Fatal(err)
	}
	malformed = func() {
		f.base.s.inputTasks.Wait()
		if err := os.WriteFile(source, []byte("{"), info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
	var once sync.Once
	restore = func() {
		once.Do(func() {
			f.base.s.inputTasks.Wait()
			if err := os.Remove(source); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Error(err)
				return
			}
			if err := os.Rename(saved, source); err != nil {
				t.Error(err)
				return
			}
			data, err := os.ReadFile(source)
			after, statErr := os.Stat(source)
			if err != nil || statErr != nil || bytesDigest(data) != bytesDigest(original) || after.Mode() != info.Mode() {
				t.Error("original receipt bytes/mode changed", err, statErr)
			}
		})
	}
	return original, malformed, restore
}

const piDeliveryModelRuntimeSource = `
async function deliveryModelRuntime(sdk,config,models){
 const root=join(config.piPackage,'lib/pi/node_modules/@earendil-works/pi-ai');
 const manifest=JSON.parse(readFileSync(join(root,'package.json'),'utf8'));
 const ai=await import(pathToFileURL(join(root,manifest.exports?.['.']?.import??manifest.main)).href);
 const runtime=await sdk.ModelRuntime.create({authPath:join(config.authDirectory,'auth.json'),modelsPath:null,allowModelNetwork:false});
 const fake=ai.fauxProvider({provider:'p3-fake',models:models.map(id=>({id,reasoning:true,contextWindow:100000000})),tokensPerSecond:1000000});
 // Pi1.5.2 needs an explicit map for xhigh/max; reasoning:true alone clamps
 // xhigh to high. Declare synthetic support before registering these objects.
 for(const model of fake.models)model.thinkingLevelMap={off:'off',minimal:'minimal',low:'low',medium:'medium',high:'high',xhigh:'xhigh',max:'max'};
 runtime.registerNativeProvider(fake.provider);await runtime.refresh({allowNetwork:false});
 return {ai,runtime,fake};
}
`

const piDeliveryModelCheckSource = `
import {readFileSync} from 'node:fs';import {join} from 'node:path';import {pathToFileURL} from 'node:url';
const [pi,auth,raw]=process.argv.slice(1);
const sdk=await import(pathToFileURL(join(pi,'lib/pi/node_modules/@earendil-works/pi-coding-agent/dist/index.js')).href);
const selections=JSON.parse(raw);
MODEL_RUNTIME
const {ai,runtime}=await deliveryModelRuntime(sdk,{piPackage:pi,authDirectory:auth},[...new Set(selections.map(s=>s.Model))]);
for(const selected of selections){
 const model=runtime.getModel(selected.Provider,selected.Model);
 const supported=model?ai.getSupportedThinkingLevels(model):[];
 const actual=model?ai.clampThinkingLevel(model,selected.Thinking):undefined;
 if(!model||model.id!==selected.Model||model.provider!==selected.Provider||!supported.includes(selected.Thinking)||actual!==selected.Thinking)throw new Error(JSON.stringify({selected,actual,supported}));
 console.log(JSON.stringify({provider:model.provider,model:model.id,requested:selected.Thinking,actual,supported}));
}
`

const piDeliveryWorkerSource = `
import {readFileSync,writeFileSync,existsSync,appendFileSync} from 'node:fs';
import {join} from 'node:path'; import {pathToFileURL} from 'node:url';
import {runWorker} from PROTOCOL; import {syncHistoryPrefix} from PERSISTENCE;
const gates=GATES;
MODEL_RUNTIME
await runWorker({sessionOptions:async(sdk,config)=>{
 const {ai,runtime,fake}=await deliveryModelRuntime(sdk,config,MODELS);
 const response=async(context,options)=>{
  const last=context.messages.at(-1); const text=last?.role==='user'?(typeof last.content==='string'?last.content:last.content.filter(x=>x.type==='text').map(x=>x.text).join('')):'';
  const delayed=/fixture-delayed-turn:([a-f0-9-]+)/.exec(text);
  if(delayed){writeFileSync(join(gates,delayed[1]+'.ready'),'ready');while(!existsSync(join(gates,delayed[1]+'.release'))){if(options?.signal?.aborted)throw new Error('fixture turn aborted');await new Promise(r=>setTimeout(r,5));}}
  if(text.includes('fixture-real-question'))return ai.fauxAssistantMessage([ai.fauxToolCall('request_user_input_async',{questions:[{title:'Choose the disposable fixture path',options:['One','Two']}]})],{stopReason:'toolUse'});
  return ai.fauxAssistantMessage('deterministic native delivery answer');
 };
 fake.setResponses(Array.from({length:20000},()=>response));
 return {modelRuntime:runtime,sessionOptions:{tools:[]},syncHistory:syncHistoryPrefix};
},onError:error=>appendFileSync(join(gates,'worker-errors.log'),String(error.stack)+'\n')});
`

const piDeliverySeedSource = `
import {readFileSync} from 'node:fs';import {join,dirname} from 'node:path';import {pathToFileURL} from 'node:url';
const [pi,file,raw,modelID]=process.argv.slice(1);const sdk=await import(pathToFileURL(join(pi,'lib/pi/node_modules/@earendil-works/pi-coding-agent/dist/index.js')).href);
const ai=await import(pathToFileURL(join(pi,'lib/pi/node_modules/@earendil-works/pi-ai/dist/index.js')).href);
const manager=sdk.SessionManager.open(file,dirname(file));const target=Number(raw);let count=manager.getBranch().length;
if(!modelID)throw new Error('saved member model required');
const model=ai.fauxProvider({provider:'p3-fake',models:[{id:modelID,reasoning:true,contextWindow:100000000}]}).getModel();
const assistant=(content,options={})=>({...ai.fauxAssistantMessage(content,options),api:model.api,provider:model.provider,model:model.id});
while(count<target){const left=target-count;const id='fixture-tool-'+count;
 if(left>=4){manager.appendMessage({role:'user',content:[{type:'text',text:'representative fixture history '+count}],timestamp:Date.now()});manager.appendMessage(assistant([ai.fauxToolCall('read',{path:'fixture.txt'},{id})],{stopReason:'toolUse'}));manager.appendMessage({role:'toolResult',toolCallId:id,toolName:'read',content:[{type:'text',text:'representative tool output'}],isError:false,timestamp:Date.now()});manager.appendMessage(assistant('representative assistant history'));count+=4;}
 else {manager.appendMessage(assistant('history tail'));count++;}
}
if(manager.getBranch().length!==target)throw new Error('partial SDK history');
`

// These tests use the owning atomic receipt writer/strict reader and finishInput,
// with the existing native-free checked fixture. They do not claim SDK latency.
func piCheckpointFixture(t *testing.T) (*PiMessageDeliveryFixture, *conversation, *checkedInputWorker, QueuedInputRequest) {
	t.Helper()
	m, host, c, w, selected, ctx := checkedInputFixture(t)
	f := &PiMessageDeliveryFixture{P6NativeFixture: P6NativeFixture{Manager: m, Runtime: selected.Runtime, Context: ctx}, base: host, counts: &piDeliveryCounts{}}
	return f, c, w, checkedRequest(selected, strings.Repeat("7", 32))
}
func piCheckpointRecord(q QueuedInputRequest, phase string) ownedReceipt {
	record := fromRetained(SubmissionStatus{Schema: 1, Ref: q.Ref, Phase: phase, Generation: q.Selection.Runtime.Generation, Settings: q.Selection.Settings, Input: q.Input, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if phase == "Accepted" {
		record.Witness = &NativeWitness{SessionID: q.Ref.Conversation.NativeID, EntryID: "exact-entry", EffectiveDigest: MessageDigest(q.Input.Message)}
	}
	return record
}
func piCheckpointLive(e *receiptEngine, q QueuedInputRequest, w worker) *liveInputReceipt {
	ref := q.Ref
	live := &liveInputReceipt{ref: receiptReference{retained: &ref}, worker: w}
	if e.liveInputs == nil {
		e.liveInputs = map[string]*liveInputReceipt{}
	}
	e.liveInputs[q.Ref.OperationID] = live
	return live
}
func piCheckpointWrite(t *testing.T, e *receiptEngine, record ownedReceipt) {
	t.Helper()
	if err := e.saveOwned(record); err != nil {
		t.Fatal(err)
	}
}

func TestPiMessageDeliveryCheckpointAttribution(t *testing.T) {
	for _, change := range []string{"exact", "old-without-live", "missing", "malformed", "unknown", "wrong-generation", "wrong-input", "wrong-settings", "wrong-scope", "wrong-runtime", "wrong-handle", "wrong-execution", "wrong-witness", "ambiguous", "other-pending"} {
		t.Run(change, func(t *testing.T) {
			f, c, w, q := piCheckpointFixture(t)
			h := q.Selection.Handle
			f.ExpectCheckpoint(h, q.Ref.OperationID)
			registered := q
			switch change {
			case "wrong-runtime":
				registered.Selection.Runtime.RequestID = "33333333-3333-4333-8333-333333333333"
			case "wrong-handle":
				registered.Selection.Handle.Generation = strings.Repeat("d", 64)
			}
			f.RegisterCheckpoint(registered)
			if change == "wrong-execution" {
				registered.OriginalRuntime.RequestID = "33333333-3333-4333-8333-333333333333"
				f.RegisterCheckpoint(registered)
			}
			e := c.receipts
			e.mu.Lock()
			record := piCheckpointRecord(q, "Accepted")
			if change != "old-without-live" {
				piCheckpointLive(e, q, w)
			}
			switch change {
			case "unknown":
				record.Phase = "Unknown"
				record.Witness = nil
			case "wrong-generation":
				record.Generation = strings.Repeat("e", 64)
			case "wrong-input":
				record.Input.Message = "another literal"
			case "wrong-settings":
				record.Settings.Revision++
			case "wrong-scope":
				record.ref.retained.Conversation.NativeID = "foreign"
			case "wrong-witness":
				record.Witness.EffectiveDigest = strings.Repeat("0", 64)
			}
			if change != "missing" {
				piCheckpointWrite(t, e, record)
			}
			if change == "malformed" {
				if err := writePrivateBytes(filepath.Join(e.root, q.Ref.OperationID+".json"), []byte("{malformed")); err != nil {
					t.Fatal(err)
				}
			}
			if change == "ambiguous" || change == "other-pending" {
				other := checkedRequest(q.Selection, strings.Repeat("8", 32))
				piCheckpointLive(e, other, w)
				phase := "dispatching"
				if change == "ambiguous" {
					phase = "Accepted"
				}
				piCheckpointWrite(t, e, piCheckpointRecord(other, phase))
			}
			afterSave := time.Now()
			if err := e.point("receipt"); err != nil {
				t.Fatal(err)
			}
			end := time.Now()
			e.mu.Unlock()
			event, ok := f.AcceptedCheckpoint(h, q.Ref.OperationID)
			valid := change == "exact" || change == "other-pending"
			if ok != valid {
				t.Fatalf("checkpoint validity %t want%t: %+v", ok, valid, event)
			}
			if valid {
				if event.Ref != q.Ref || event.Runtime != q.Selection.Runtime || event.Handle != h || event.Witness != *record.Witness || event.ObservedAt.Before(afterSave) || event.ObservedAt.After(end) || event.VerificationNS <= 0 {
					t.Fatal("unbound post-sync endpoint", event)
				}
				// No later duplicate observation can substitute a later timestamp.
				e.mu.Lock()
				delete(e.liveInputs, q.Ref.OperationID)
				_ = e.point("receipt")
				e.mu.Unlock()
				again, _ := f.AcceptedCheckpoint(h, q.Ref.OperationID)
				if again.ObservedAt != event.ObservedAt {
					t.Fatal("first endpoint replaced", event, again)
				}
				wrongScope := h
				wrongScope.Conversation.NativeID = "another-native"
				if _, ok := f.AcceptedCheckpoint(wrongScope, q.Ref.OperationID); ok {
					t.Fatal("borrowed another scope's time")
				}
				wrong := h
				wrong.Generation = strings.Repeat("e", 64)
				if _, ok := f.AcceptedCheckpoint(wrong, q.Ref.OperationID); ok {
					t.Fatal("borrowed another execution's time")
				}
			}
		})
	}
}

func TestPiMessageDeliveryCheckpointConcurrentExpectations(t *testing.T) {
	f, c, w, q := piCheckpointFixture(t)
	other := checkedRequest(q.Selection, strings.Repeat("8", 32))
	var wg sync.WaitGroup
	for _, r := range []QueuedInputRequest{q, other} {
		wg.Add(1)
		go func(r QueuedInputRequest) {
			defer wg.Done()
			f.ExpectCheckpoint(r.Selection.Handle, r.Ref.OperationID)
			f.RegisterCheckpoint(r)
		}(r)
	}
	wg.Wait()
	c.receipts.mu.Lock()
	piCheckpointLive(c.receipts, q, w)
	piCheckpointWrite(t, c.receipts, piCheckpointRecord(q, "Accepted"))
	_ = c.receipts.point("receipt")
	c.receipts.mu.Unlock()
	for _, r := range []QueuedInputRequest{q, other} {
		if event, ok := f.AcceptedCheckpoint(r.Selection.Handle, r.Ref.OperationID); ok || event.Error == "" {
			t.Fatal("overlap guessed an ID", event)
		}
	}
	if f.Counts().ReceiptObserverMaxPending != 2 {
		t.Fatal("pending occupancy not retained")
	}
}

func TestPiMessageDeliveryCheckpointNoConversationLockOrRPC(t *testing.T) {
	f, c, w, q := piCheckpointFixture(t)
	f.ExpectCheckpoint(q.Selection.Handle, q.Ref.OperationID)
	f.RegisterCheckpoint(q)
	// Real setup has completed. Both Manager RPC and implicit worker load are
	// forbidden below; holding c.mu also proves the callback does not reverse locks.
	f.Manager.config.Dial = func(context.Context, string) (net.Conn, error) {
		t.Error("checkpoint attempted RPC")
		return nil, ErrUnavailable
	}
	inspections, identities, enqueues := f.base.count, w.identities, w.enqueues
	c.receipts.mu.Lock()
	piCheckpointLive(c.receipts, q, w)
	piCheckpointWrite(t, c.receipts, piCheckpointRecord(q, "Accepted"))
	c.receipts.mu.Unlock()
	c.mu.Lock()
	done := make(chan error, 1)
	go func() { c.receipts.mu.Lock(); defer c.receipts.mu.Unlock(); done <- c.receipts.point("receipt") }()
	select {
	case err := <-done:
		c.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		c.mu.Unlock()
		t.Fatal("receipt hook relocked engine or conversation")
	}
	if _, ok := f.AcceptedCheckpoint(q.Selection.Handle, q.Ref.OperationID); !ok || f.base.count != inspections || w.identities != identities || w.enqueues != enqueues {
		t.Fatal("observer changed runtime or missed endpoint")
	}
}

// Delegate only the unit fixture's native responses; the real finishInput state
// machine still owns witness validation, durable persistence, commit and hold.
type piCheckpointFinishWorker struct {
	worker
	request                    QueuedInputRequest
	commitError, snapshotError error
	calls                      map[string]int
}

func (w *piCheckpointFinishWorker) Call(ctx context.Context, op string, input, result any) error {
	w.calls[op]++
	switch op {
	case "input_wait_v1":
		data, _ := json.Marshal(nativeResult{Outcome: "Accepted", Witness: &workerWitness{SessionID: w.request.Ref.Conversation.NativeID, EntryID: "exact-entry", EffectiveMessage: w.request.Input.Message}})
		return json.Unmarshal(data, result)
	case "input_commit_v1":
		if w.commitError != nil {
			return w.commitError
		}
		data := []byte(`{"committed":true}`)
		return json.Unmarshal(data, result)
	case "input_hold_v1":
		return nil
	case "snapshot":
		if w.snapshotError != nil {
			return w.snapshotError
		}
	}
	return w.worker.Call(ctx, op, input, result)
}

func TestPiMessageDeliveryCheckpointFinishFaultComposition(t *testing.T) {
	for _, fault := range []string{"none", "native", "witness", "receipt", "commit", "snapshot"} {
		t.Run(fault, func(t *testing.T) {
			f, c, base, q := piCheckpointFixture(t)
			if fault == "snapshot" {
				// Retained legacy metadata refresh still proves snapshot-error hold.
				f.base.s.conversationIdentityContract = func(string) (bool, error) { return false, nil }
			}
			w := &piCheckpointFinishWorker{worker: base, request: q, calls: map[string]int{}}
			c.worker = w
			sentinel := errors.New("exact delegated receipt fault")
			delegated := map[string]int{}
			c.receipts.checkpoint = func(point string) error {
				delegated[point]++
				if point == fault {
					return sentinel
				}
				return nil
			}
			if fault == "commit" {
				w.commitError = sentinel
			}
			if fault == "snapshot" {
				w.snapshotError = sentinel
			}
			f.ExpectCheckpoint(q.Selection.Handle, q.Ref.OperationID)
			f.RegisterCheckpoint(q)
			e := c.receipts
			e.mu.Lock()
			live := piCheckpointLive(e, q, w)
			piCheckpointWrite(t, e, piCheckpointRecord(q, "dispatching"))
			e.mu.Unlock()
			f.base.s.finishInput(c, live)
			event, ok := f.AcceptedCheckpoint(q.Selection.Handle, q.Ref.OperationID)
			want := fault != "native" && fault != "witness"
			if ok != want {
				t.Fatal("checkpoint concealed a fault or was missed", fault, event)
			}
			if (w.calls["input_hold_v1"] > 0) != (fault != "none") {
				t.Fatal("later health changed", fault, w.calls)
			}
			if delegated["native"] != 1 || delegated["witness"] != 1 || delegated["receipt"] > 1 {
				t.Fatal("fault delegate repeated", delegated)
			}
			if fault == "receipt" && (event.DelegateError != sentinel.Error() || w.calls["input_commit_v1"] != 0 || f.Counts().ReceiptDelegateErrors != 1) {
				t.Fatal("receipt fault swallowed", event, w.calls)
			}
			e.mu.Lock()
			saved, err := e.loadOwned(q.Ref.OperationID)
			left := len(e.liveInputs)
			e.mu.Unlock()
			if err != nil || left != 0 {
				t.Fatal(saved, err, left)
			}
			if want && (saved.Phase != "Accepted" || saved.Witness == nil) {
				t.Fatal("durable proof lost", saved)
			}
			if fault == "native" && saved.Phase != "Unknown" {
				t.Fatal("native fault fabricated Accepted", saved)
			}
		})
	}
}

func TestPiMessageDeliveryCheckpointFaultRestoreRearm(t *testing.T) {
	f, c, w, q := piCheckpointFixture(t)
	sentinel := errors.New("prior callback result")
	calls := 0
	c.receipts.checkpoint = func(string) error { calls++; return sentinel }
	restore := f.FailReceipt(t, q.Selection.Handle, "native")
	f.ExpectCheckpoint(q.Selection.Handle, q.Ref.OperationID)
	f.RegisterCheckpoint(q)
	c.receipts.mu.Lock()
	piCheckpointLive(c.receipts, q, w)
	piCheckpointWrite(t, c.receipts, piCheckpointRecord(q, "Accepted"))
	if err := c.receipts.point("receipt"); err != sentinel {
		t.Fatal("delegate error replaced", err)
	}
	if err := c.receipts.point("native"); err != ErrUnavailable {
		t.Fatal("native injector changed", err)
	}
	delete(c.receipts.liveInputs, q.Ref.OperationID)
	c.receipts.mu.Unlock()
	if calls != 1 {
		t.Fatal("prior callback not called exactly once", calls)
	}
	restore()
	other := checkedRequest(q.Selection, strings.Repeat("8", 32))
	f.ExpectCheckpoint(other.Selection.Handle, other.Ref.OperationID)
	f.RegisterCheckpoint(other)
	c.receipts.mu.Lock()
	piCheckpointLive(c.receipts, other, w)
	piCheckpointWrite(t, c.receipts, piCheckpointRecord(other, "Accepted"))
	if err := c.receipts.point("receipt"); err != sentinel {
		t.Fatal(err)
	}
	c.receipts.mu.Unlock()
	if _, ok := f.AcceptedCheckpoint(other.Selection.Handle, other.Ref.OperationID); !ok || calls != 2 {
		t.Fatal("observer not rearmed after restore", calls)
	}
	// A fault installed later is outer and suppresses observation on its matching
	// point. Missing instrumentation must not be replaced by a commit/public time.
	third := checkedRequest(q.Selection, strings.Repeat("9", 32))
	f.ExpectCheckpoint(third.Selection.Handle, third.Ref.OperationID)
	f.RegisterCheckpoint(third)
	restoreLate := f.FailReceipt(t, third.Selection.Handle, "receipt")
	defer restoreLate()
	c.receipts.mu.Lock()
	delete(c.receipts.liveInputs, other.Ref.OperationID)
	piCheckpointLive(c.receipts, third, w)
	piCheckpointWrite(t, c.receipts, piCheckpointRecord(third, "Accepted"))
	err := c.receipts.point("receipt")
	c.receipts.mu.Unlock()
	if err != ErrUnavailable {
		t.Fatal(err)
	}
	if _, ok := f.AcceptedCheckpoint(third.Selection.Handle, third.Ref.OperationID); ok {
		t.Fatal("suppressed callback fabricated time")
	}
}
