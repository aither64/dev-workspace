package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aither64/codex-web/conversation/backend"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func workflowPointer[T any](v T) *T { return &v }

type workflowWorker struct {
	base    *policyWorker
	native  workerWorkflowObservation
	observe func()
	calls   int
}

func (w *workflowWorker) Call(ctx context.Context, op string, input, out any) error {
	if op != "workflow_observe" {
		return w.base.Call(ctx, op, input, out)
	}
	w.calls++
	if w.observe != nil {
		w.observe()
	}
	b, _ := json.Marshal(w.native)
	return strictDecode(b, out, recordLimit)
}
func (w *workflowWorker) Wait(ctx context.Context) error { return w.base.Wait(ctx) }
func (w *workflowWorker) PID() (int, string)             { return w.base.PID() }
func workflowAttachmentFixture(t *testing.T, s *supervisor) *hostAttachment {
	t.Helper()
	left, right := net.Pipe()
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
	a := &hostAttachment{id: newUUID(), connection: left, closed: make(chan struct{}), workflow: true, workflowReady: true, workflowACKs: map[string]workflowACK{}, workflowEvents: make(chan hostFrame, workflowQueueLimit), tools: []string{}}
	s.workflow.mu.Lock()
	s.workflow.required = true
	s.workflow.sequence++
	s.workflow.attachment = a
	s.workflow.mu.Unlock()
	return a
}
func workflowSupervisorFixture(t *testing.T) (*supervisor, *conversation, *workflowWorker, agentbackend.ExecutionHandle, *hostAttachment) {
	t.Helper()
	s, c, base, h := policySupervisorFixture(t)
	w := &workflowWorker{base: base, native: workerWorkflowObservation{NativeID: h.Conversation.NativeID, Model: c.record.Settings.Model, Provider: c.record.Settings.Provider, Thinking: c.record.Settings.Thinking, Proof: workflowPointer(base.proof()), Pending: workflowPointer(false), Streaming: workflowPointer(false), HostCalls: workflowPointer(uint64(0)), Running: workflowPointer(false), ActiveCalls: workflowPointer(uint64(0)), Held: workflowPointer(false), HostTools: []string{}}}
	c.worker = w
	return s, c, w, h, workflowAttachmentFixture(t, s)
}
func workflowFrame(t *testing.T, a *hostAttachment) hostFrame {
	t.Helper()
	select {
	case frame := <-a.workflowEvents:
		return frame
	case <-time.After(time.Second):
		t.Fatal("missing owned workflow frame")
		return hostFrame{}
	}
}
func acknowledgeWorkflow(t *testing.T, s *supervisor, a *hostAttachment, frame hostFrame) {
	t.Helper()
	frame.Event = "workflow_invalidated_ack"
	frame.RequiresACK = false
	if !s.acceptWorkflowACK(a, frame) {
		t.Fatal("valid ACK refused")
	}
}
func TestWorkflowObservationCompleteReadOnlyAndNativeRace(t *testing.T) {
	s, c, w, h, a := workflowSupervisorFixture(t)
	ctx := context.Background()
	path := filepath.Join(c.root, "conversation.json")
	before, _ := os.ReadFile(path)
	seen, err := s.observeWorkflow(ctx, h)
	if err != nil || !seen.Idle || !seen.Loaded || seen.Pending || seen.Held || seen.Sequence == 0 || seen.AttachmentID != a.id || !sameToolPolicy(*seen.Retained, w.base.policy) {
		t.Fatal(seen, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || w.base.applies != 0 || w.base.holds != 0 {
		t.Fatal("observation wrote or applied")
	}
	for _, fault := range []string{"input", "streaming", "bridge", "running", "tool", "callback", "pending-policy", "held", "missing-count", "wrong-proof", "wrong-tools", "native-race"} {
		t.Run(fault, func(t *testing.T) {
			s, c, w, h, _ := workflowSupervisorFixture(t)
			switch fault {
			case "input":
				w.native.Pending = workflowPointer(true)
			case "streaming":
				w.native.Streaming = workflowPointer(true)
			case "bridge":
				w.native.HostCalls = workflowPointer(uint64(1))
			case "running":
				w.native.Running = workflowPointer(true)
			case "tool":
				w.native.ActiveCalls = workflowPointer(uint64(1))
			case "callback":
				c.callbackCount = 1
			case "pending-policy":
				target := policyTarget("default", 2, []string{"bash", "read"}, []string{"bash", "read"})
				c.record.PendingToolPolicy = &target
			case "held":
				w.native.Held = workflowPointer(true)
			case "missing-count":
				w.native.ActiveCalls = nil
			case "wrong-proof":
				w.native.Proof.Generation = "wrong"
			case "wrong-tools":
				w.native.HostTools = []string{"publish_plan"}
			case "native-race":
				w.observe = s.workflowChanged
			}
			result, err := s.observeWorkflow(ctx, h)
			if fault == "missing-count" || fault == "wrong-proof" || fault == "wrong-tools" || fault == "native-race" {
				if err == nil {
					t.Fatal("incomplete/racing proof published", result)
				}
			} else if err != nil || result.Idle {
				t.Fatal(result, err)
			}
		})
	}
}
func TestWorkflowBarrierACKWithoutEffectLocksAndPendingPublication(t *testing.T) {
	s, c, w, h, a := workflowSupervisorFixture(t)
	c.mu.Lock()
	c.receipts.mu.Lock()
	s.mu.Lock()
	type admitted struct {
		ctx    context.Context
		finish func()
		err    error
	}
	done := make(chan admitted, 1)
	go func() {
		ctx, finish, err := s.beginWorkflowMutation(context.Background())
		done <- admitted{ctx, finish, err}
	}()
	frame := workflowFrame(t, a)
	if !frame.RequiresACK {
		t.Fatal("missing pre-effect ACK")
	}
	// ACK processing must progress while every ordinary state/receipt lock is held.
	acknowledgeWorkflow(t, s, a, frame)
	result := <-done
	s.mu.Unlock()
	c.receipts.mu.Unlock()
	c.mu.Unlock()
	if result.err != nil || s.workflowCheck(result.ctx) != nil {
		t.Fatal(result.err)
	}
	if _, err := s.observeWorkflow(context.Background(), h); err == nil || w.calls != 0 {
		t.Fatal("pending mutation published proof", err, w.calls)
	}
	result.finish()
	completion := workflowFrame(t, a)
	if completion.RequiresACK || completion.Sequence <= frame.Sequence {
		t.Fatal(completion)
	}
	if s.workflowCheck(result.ctx) == nil {
		t.Fatal("completed permit retained authority")
	}
	if seen, err := s.observeWorkflow(context.Background(), h); err != nil || !seen.Idle {
		t.Fatal(seen, err)
	}
}
func TestWorkflowBarrierMissingACKAndChangedPermitHaveZeroPolicyEffects(t *testing.T) {
	for _, fault := range []string{"missing-ack", "replaced", "canceled"} {
		t.Run(fault, func(t *testing.T) {
			s, c, w, h, a := workflowSupervisorFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			request, _ := json.Marshal(toolPolicyRequest{Handle: h, ExpectedRevision: 1, Target: workflowPointer(policyTarget("default", 2, []string{"bash", "read"}, []string{"bash", "read"}))})
			if fault != "missing-ack" {
				c.mu.Lock()
			}
			done := make(chan error, 1)
			go func() { _, err := s.invoke(ctx, "tool_policy_apply", request); done <- err }()
			frame := workflowFrame(t, a)
			if fault != "missing-ack" {
				acknowledgeWorkflow(t, s, a, frame)
				// Wait for permit delivery, then revoke it before the effect can own c.mu.
				if fault == "replaced" {
					workflowAttachmentFixture(t, s)
				} else {
					cancel()
				}
				c.mu.Unlock()
			}
			if err := <-done; err == nil {
				t.Fatal("lost authority admitted")
			}
			if w.base.applies != 0 || c.record.PendingToolPolicy != nil {
				t.Fatal("effect before valid current ACK/permit")
			}
		})
	}
}
func TestWorkflowOverflowStaleACKAndNoLegacyDowngrade(t *testing.T) {
	s, _, _, h, a := workflowSupervisorFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := s.beginWorkflowMutation(ctx); done <- err }()
	frame := workflowFrame(t, a)
	frame.Event = "workflow_invalidated_ack"
	frame.RequiresACK = false
	frame.Sequence++
	if s.acceptWorkflowACK(a, frame) {
		t.Fatal("wrong sequence admitted")
	}
	if err := <-done; err == nil {
		t.Fatal("unacknowledged effect admitted")
	}
	s.workflow.mu.Lock()
	s.workflow.sequence = math.MaxUint64
	s.workflow.mu.Unlock()
	s.workflowChanged()
	if _, err := s.observeWorkflow(context.Background(), h); err == nil {
		t.Fatal("overflow published")
	}
	if _, _, err := s.beginWorkflowMutation(context.Background()); err == nil {
		t.Fatal("overflow admitted effect")
	}
	s.workflow.mu.Lock()
	s.workflow.overflow = false
	s.workflow.attachment = nil
	s.workflow.mu.Unlock()
	if s.workflowCheck(context.Background()) == nil {
		t.Fatal("disconnect downgraded to B1")
	}
	legacy := &supervisor{}
	ctx2, finish, err := legacy.beginWorkflowMutation(context.Background())
	if err != nil || legacy.workflowCheck(ctx2) != nil {
		t.Fatal("B1 changed", err)
	}
	finish()
}
func TestWorkflowNotificationOverflowCannotBlockWorkerReader(t *testing.T) {
	s, _, _, _, a := workflowSupervisorFixture(t)
	for i := 0; i <= workflowQueueLimit; i++ {
		s.workflowChanged()
	}
	s.workflow.mu.Lock()
	ready := a.workflowReady
	s.workflow.mu.Unlock()
	if ready {
		t.Fatal("overflow retained live observations")
	}
}
func TestWorkflowUtilityAndResourceRejectRevokedPermit(t *testing.T) {
	s, _, _, _, a := workflowSupervisorFixture(t)
	ctx := context.Background()
	type result struct {
		ctx    context.Context
		finish func()
		err    error
	}
	done := make(chan result, 1)
	go func() { ctx, finish, err := s.beginWorkflowMutation(ctx); done <- result{ctx, finish, err} }()
	acknowledgeWorkflow(t, s, a, workflowFrame(t, a))
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	workflowAttachmentFixture(t, s)
	request := ResourceRequest{ResourceID: "11111111111111111111111111111111", OperationID: "22222222222222222222222222222222", Argv: []string{"/cannot/execute"}, Cwd: t.TempDir()}
	request.Digest = ResourceDigest(s.ref, request)
	if _, err := s.startResource(r.ctx, request); err == nil {
		t.Fatal("revoked resource permit executed")
	}
	u := UtilityRef{Session: s.ref.Session, OperationID: "33333333333333333333333333333333", Incarnation: h64("e")}
	if _, err := s.createUtility(r.ctx, UtilityCreateRequest{Utility: u, CreationID: "44444444444444444444444444444444", Settings: settingsFixture()}); err == nil {
		t.Fatal("revoked utility permit created")
	}
	if _, err := os.Stat(filepath.Join(s.stateRoot, "resources")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("resource intent written", err)
	}
	if _, err := os.Stat(filepath.Join(s.stateRoot, "utilities")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("utility mapping written", err)
	}
	r.finish()
}
func h64(s string) string {
	var out string
	for i := 0; i < 64; i++ {
		out += s
	}
	return out
}
func TestWorkflowNaturalResourceCompletionInvalidatesWithoutACK(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	s, _, _, _, a := workflowSupervisorFixture(t)
	request := ResourceRequest{ResourceID: "55555555555555555555555555555555", OperationID: "66666666666666666666666666666666", Argv: []string{shell, "-c", "sleep 0.05"}, Environment: os.Environ(), Cwd: t.TempDir()}
	request.Digest = ResourceDigest(s.ref, request)
	data, _ := json.Marshal(request)
	done := make(chan error, 1)
	go func() { _, err := s.invoke(context.Background(), "start_resource", data); done <- err }()
	acknowledgeWorkflow(t, s, a, workflowFrame(t, a))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	completion := workflowFrame(t, a)
	if completion.RequiresACK {
		t.Fatal("launch completion required ACK")
	}
	// Natural command exit must persist even with no observer and no host ACK.
	s.workflow.mu.Lock()
	s.workflow.attachment = nil
	a.workflowReady = false
	s.workflow.mu.Unlock()
	deadline := time.After(time.Second)
	for {
		s.mu.Lock()
		var record resourceRecord
		err := readPrivate(filepath.Join(s.stateRoot, "resources", request.ResourceID+".json"), &record)
		s.mu.Unlock()
		if err == nil && record.Status.Phase == "stopped" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("resource reaping waited for observer", record, err)
		case <-time.After(time.Millisecond):
		}
	}
	s.workflow.mu.Lock()
	sequence := s.workflow.sequence
	s.workflow.mu.Unlock()
	if sequence <= completion.Sequence {
		t.Fatal("natural completion did not invalidate")
	}
}

type workflowFixtureHandler struct {
	callbackFixtureHandler
	invalidated func(WorkflowInvalidation)
}

func (h *workflowFixtureHandler) InvalidateWorkflow(event WorkflowInvalidation) { h.invalidated(event) }
func awaitWorkflowAttachment(t *testing.T, s *supervisor, ctx context.Context) {
	t.Helper()
	for {
		s.workflow.mu.Lock()
		ready := s.workflow.attachment != nil && s.workflow.attachment.workflowReady
		s.workflow.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}
func awaitIdleWorkflow(t *testing.T, m *Manager, ctx context.Context, h agentbackend.ExecutionHandle, events <-chan struct{}) WorkflowObservation {
	t.Helper()
	for {
		observed, err := m.ObserveWorkflow(ctx, h)
		if err == nil && observed.Idle {
			return observed
		}
		select {
		case <-ctx.Done():
			t.Fatal("workflow failed to settle", observed, err, ctx.Err())
		case <-events:
		}
	}
}
func TestActualWorkflowBarrierSendACKBeforeCallbackSharedAdmission(t *testing.T) {
	m, host, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan struct{}, 1)
	var eventMu sync.Mutex
	var last WorkflowInvalidation
	invalidations := 0
	handler := &workflowFixtureHandler{callbackFixtureHandler: callbackFixtureHandler{tools: []string{"report_to_lead"}}, invalidated: func(event WorkflowInvalidation) {
		// This is deliberately only the short host observation-map mutex. Taking
		// the simulated domain/runtime SH here would deadlock the EX-owned Send.
		eventMu.Lock()
		last = event
		invalidations++
		eventMu.Unlock()
		select {
		case events <- struct{}{}:
		default:
		}
	}}
	cancelAttachment := attachFixture(t, m, host.s, ctx, runtime.Runtime, handler)
	awaitWorkflowAttachment(t, host.s, ctx)
	var admission sync.RWMutex
	callbacks := make(chan HostCall, 2)
	message := "original input under creation EX with independent callback SH"
	handler.check = func(callCtx context.Context, call HostCall) error {
		callbacks <- call
		admission.RLock()
		defer admission.RUnlock()
		original := SubmissionRef{Conversation: call.Subject.Conversation.Conversation, OperationID: call.InputOperationID, Action: "send"}
		original.Digest = SubmissionDigest(original, SendInput{Message: message, ActionContext: "initial"}, settingsFixture())
		status, err := m.Lookup(callCtx, original)
		if err != nil || status.Phase != "Accepted" || status.Witness == nil {
			return ErrHeld
		}
		return nil
	}
	admission.Lock()
	// Always release the owned fixture lock on an assertion failure, before the
	// existing harness joins/cancels handlers and workers.
	locked := true
	defer func() {
		if locked {
			admission.Unlock()
		}
	}()
	policy := policyTarget("default", 1, []string{"report_to_lead"}, []string{"report_to_lead"})
	creation := "22222222-2222-4222-8222-222222222222"
	state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: creation, Settings: settingsFixture(), ToolPolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	initial := awaitIdleWorkflow(t, m, ctx, handle, events)
	request := backend.SendRequest{Message: message, ActionContext: "initial", ClientUserMessageID: "44444444-4444-4444-8444-444444444444"}
	result, err := m.Deliver(ctx, handle, request)
	if err != nil || result.Outcome != backend.Accepted {
		t.Fatal("EX-owned Send did not return persisted ACK", result, err)
	}
	original := SubmissionRef{Conversation: handle.Conversation, OperationID: request.ClientUserMessageID, Action: "send"}
	original.Digest = SubmissionDigest(original, SendInput{Message: message, ActionContext: "initial"}, settingsFixture())
	receipt, err := m.Lookup(ctx, original)
	if err != nil || receipt.Phase != "Accepted" || receipt.Witness == nil {
		t.Fatal(receipt, err)
	}
	select {
	case <-callbacks:
	case <-ctx.Done():
		t.Fatal("callback could not reach independent SH", ctx.Err())
	}
	proof, err := m.VerifyCreation(ctx, handle, creation, state.HistoryRef, &original)
	if err != nil || proof.Bootstrap == nil || proof.Bootstrap.Phase != "Accepted" {
		t.Fatal("ready proof waited for callback", proof, err)
	}
	if busy, err := m.ObserveWorkflow(ctx, handle); err == nil && busy.Idle {
		t.Fatal("SH-blocked callback appeared idle")
	}
	admission.Unlock()
	locked = false
	settled := awaitIdleWorkflow(t, m, ctx, handle, events)
	if settled.Sequence <= initial.Sequence || settled.AttachmentID != initial.AttachmentID || !sameToolPolicy(*settled.Retained, policy) {
		t.Fatal("old observation survived Send", initial, settled)
	}
	next := cloneToolPolicy(policy)
	next.Revision = 2
	applied, err := m.ApplyToolPolicy(ctx, handle, 1, next)
	if err != nil || applied.Outcome != "applied" {
		t.Fatal(applied, err)
	}
	afterApply := awaitIdleWorkflow(t, m, ctx, handle, events)
	if afterApply.Retained.Revision != 2 || afterApply.Sequence <= settled.Sequence {
		t.Fatal(afterApply)
	}
	settings := settingsFixture()
	settings.Revision = 2
	configured, err := m.Configure(ctx, handle, settings)
	if err != nil || configured.Outcome != "applied" {
		t.Fatal(configured, err)
	}
	afterConfigure := awaitIdleWorkflow(t, m, ctx, handle, events)
	if afterConfigure.Sequence <= afterApply.Sequence {
		t.Fatal("settings failed to invalidate")
	}
	if stopped, err := m.Unload(ctx, handle); err != nil || !stopped.VerifiedExit {
		t.Fatal(stopped, err)
	}
	c, err := host.s.member(handle.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ObserveWorkflow(ctx, handle); err == nil || c.worker != nil {
		t.Fatal("observation loaded cold worker", err)
	}
	cancelAttachment()
	for {
		eventMu.Lock()
		disconnected := !last.Connected
		eventMu.Unlock()
		if disconnected {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-events:
		}
	}
	if _, err := m.Load(ctx, handle); err == nil || c.worker != nil {
		t.Fatal("lost observer downgraded mutation to B1", err)
	}
	host.s.workflow.mu.Lock()
	detached := host.s.workflow.attachment == nil
	host.s.workflow.mu.Unlock()
	if !detached {
		t.Fatal("server did not retire lost attachment")
	}
	attachFixture(t, m, host.s, ctx, runtime.Runtime, handler)
	awaitWorkflowAttachment(t, host.s, ctx)
	if _, err := m.Load(ctx, handle); err != nil {
		t.Fatal(err)
	}
	reopened := awaitIdleWorkflow(t, m, ctx, handle, events)
	if reopened.AttachmentID == initial.AttachmentID || reopened.Sequence <= afterConfigure.Sequence || reopened.Retained.Revision != 2 {
		t.Fatal("reconnect reused old proof", reopened)
	}
	ops, err := m.Bind(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := ops.Read.Read(ctx, BackendIdentity(handle))
	if err != nil {
		t.Fatal(err)
	}
	inputs := 0
	for _, entry := range transcript.Entries {
		if entry.Kind == "userMessage" {
			inputs++
		}
	}
	if inputs != 1 {
		t.Fatal("workflow refresh/reconnect sent native input", inputs)
	}
	handler.mu.Lock()
	calls := len(handler.calls)
	handler.mu.Unlock()
	eventMu.Lock()
	count := invalidations
	eventMu.Unlock()
	if calls != 2 || count < 6 {
		t.Fatal("missing native callbacks/ordered invalidations", calls, count)
	}
	if stopped, err := m.Unload(ctx, handle); err != nil || !stopped.VerifiedExit {
		t.Fatal(stopped, err)
	}
}

// The existing one-frame control protocol, exercised through real Unix sockets
// and production connection/callRaw. The attachment queue alone is a pure fake.
func workflowControlFixture(t *testing.T, s *supervisor) (*Manager, <-chan struct{}) {
	t.Helper()
	root := t.TempDir()
	s.ref.InstanceID = newUUID()
	s.ref.RequestID = newUUID()
	m := &Manager{config: Config{HostStateRoot: root, Dial: func(ctx context.Context, path string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	instance := m.instance(s.ref)
	if err := makePrivateDirectory(instance); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(instance)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join("/proc/self/fd", fmt.Sprint(directory.Fd()), "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(done)
		connection, err := listener.AcceptUnix()
		if err == nil {
			s.connection(ctx, connection)
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		_ = directory.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("ordinary connection/EOF monitor did not join")
		}
	})
	return m, done
}
func TestWorkflowControlCancellationAndBufferedExtraFrameHaveZeroEffects(t *testing.T) {
	for _, operation := range []string{"tool_policy_apply", "utility_create", "start_resource"} {
		for _, fault := range []string{"cancel", "eof", "extra-frame"} {
			t.Run(operation+"/"+fault, func(t *testing.T) {
				s, c, w, h, a := workflowSupervisorFixture(t)
				s.config.AccountBindings = []AccountBinding{{Provider: "p3-fake", AccountRef: "fake-account"}}
				m, joined := workflowControlFixture(t, s)
				var body any
				switch operation {
				case "tool_policy_apply":
					body = toolPolicyRequest{Handle: h, ExpectedRevision: 1, Target: workflowPointer(policyTarget("default", 2, []string{"bash", "read"}, []string{"bash", "read"}))}
				case "utility_create":
					u := UtilityRef{Session: s.ref.Session, OperationID: strings.Repeat("d", 32), Incarnation: strings.Repeat("e", 64)}
					grant := UtilityGrant{Runtime: s.ref, Utility: u, CreationID: strings.Repeat("a", 32), LaunchOperationID: strings.Repeat("b", 32), Settings: settingsFixture(), Input: SendInput{Message: "original"}}
					body = utilityRequest{Grant: grant, Create: &UtilityCreateRequest{Utility: u, CreationID: grant.CreationID, Settings: grant.Settings}}
				case "start_resource":
					r := ResourceRequest{ResourceID: strings.Repeat("a", 32), OperationID: strings.Repeat("b", 32), Argv: []string{"/cannot/execute"}, Cwd: t.TempDir()}
					r.Digest = ResourceDigest(s.ref, r)
					body = r
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				clientDone := make(chan error, 1)
				var raw net.Conn
				var barrier hostFrame
				if fault == "cancel" {
					go func() { clientDone <- m.callRaw(ctx, s.ref, operation, body, nil) }()
				} else {
					directory, err := os.Open(m.instance(s.ref))
					if err != nil {
						t.Fatal(err)
					}
					defer directory.Close()
					raw, err = m.config.Dial(ctx, filepath.Join("/proc/self/fd", fmt.Sprint(directory.Fd()), "control.sock"))
					if err != nil {
						t.Fatal(err)
					}
					defer raw.Close()
					encoded, _ := json.Marshal(body)
					request := controlRequest{Schema: 1, ID: newUUID(), Runtime: s.ref, Operation: operation, Body: encoded}
					data, _ := marshalWire(request)
					if fault == "extra-frame" {
						data = append(data, []byte("{}\n")...)
					}
					if _, err := raw.Write(data); err != nil {
						t.Fatal(err)
					}
				}
				if fault == "extra-frame" {
					// The buffered monitor may cancel before a barrier is enqueued, which is
					// already a successful no-effect refusal; otherwise leave the ACK withheld.
					select {
					case frame := <-a.workflowEvents:
						barrier = frame
					case <-joined:
					}
				} else {
					frame := workflowFrame(t, a)
					barrier = frame
					if !frame.RequiresACK {
						t.Fatal(frame)
					}
					if fault == "cancel" {
						cancel()
						if err := <-clientDone; err == nil {
							t.Fatal("canceled call returned success")
						}
					} else {
						_ = raw.Close()
					}
				}
				select {
				case <-joined:
				case <-time.After(time.Second):
					t.Fatal("EOF monitor failed to cancel pre-ACK operation")
				}
				// A late ACK cannot revive the canceled operation or cross any effect.
				if barrier.RequiresACK {
					barrier.Event = "workflow_invalidated_ack"
					barrier.RequiresACK = false
					if s.acceptWorkflowACK(a, barrier) {
						t.Fatal("late ACK revived canceled operation")
					}
				}
				s.workflow.mu.Lock()
				remaining := len(a.workflowACKs)
				s.workflow.mu.Unlock()
				if remaining != 0 || w.base.applies != 0 || c.record.PendingToolPolicy != nil {
					t.Fatal("canceled permit retained effects", remaining, w.base.applies)
				}
				for _, name := range []string{"utilities", "resources"} {
					if _, err := os.Stat(filepath.Join(s.stateRoot, name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("canceled operation wrote", name, err)
					}
				}
			})
		}
	}
}

type workflowAcceptedWorker struct {
	base   *policyWorker
	active bool
}

func (w *workflowAcceptedWorker) Call(ctx context.Context, op string, input, out any) error {
	if op != "send" {
		return w.base.Call(ctx, op, input, out)
	}
	w.active = true
	request := input.(map[string]string)
	b, _ := json.Marshal(nativeResult{Outcome: "Accepted", Witness: &workerWitness{SessionID: w.base.snapshot.NativeID, EntryID: "original-native-entry", EffectiveMessage: request["message"]}})
	return json.Unmarshal(b, out)
}
func (w *workflowAcceptedWorker) Wait(ctx context.Context) error { return w.base.Wait(ctx) }
func (w *workflowAcceptedWorker) PID() (int, string)             { return w.base.PID() }
func TestWorkflowNormalControlClosePreservesAcceptedRunAndJoinsMonitor(t *testing.T) {
	s, c, w, h, a := workflowSupervisorFixture(t)
	m, joined := workflowControlFixture(t, s)
	w.base.snapshot.HistoryFile = filepath.Join(c.root, "history", "native.jsonl")
	w.base.snapshot.Persisted = true
	w.base.snapshot.Busy = true
	active := &workflowAcceptedWorker{base: w.base}
	c.worker = active
	request := conversationRequest{Handle: h, Send: backend.SendRequest{Message: "one accepted input", ClientUserMessageID: strings.Repeat("f", 32)}}
	var result SubmissionStatus
	done := make(chan error, 1)
	go func() { done <- m.callRaw(context.Background(), s.ref, "send", request, &result) }()
	acknowledgeWorkflow(t, s, a, workflowFrame(t, a))
	if err := <-done; err != nil {
		t.Fatal(result, err)
	}
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("normal post-ACK connection did not join monitor")
	}
	if result.Phase != "Accepted" || result.Witness == nil || !active.active || w.base.holds != 0 {
		t.Fatal("normal reply close canceled native run", result, active.active, w.base.holds)
	}
	stored, err := c.receipts.lookup(result.Ref)
	if err != nil || stored.Phase != "Accepted" {
		t.Fatal("post-ACK close rewrote receipt", stored, err)
	}
}

func TestWorkflowInitialACKPrecedesReadinessAndResourceProofIsComplete(t *testing.T) {
	s, _, _, h, a := workflowSupervisorFixture(t)
	s.workflow.mu.Lock()
	a.workflowReady = false
	a.setupID = newUUID()
	a.setupSequence = s.workflow.sequence
	s.workflow.mu.Unlock()
	if _, err := s.observeWorkflow(context.Background(), h); err == nil {
		t.Fatal("unacknowledged setup published")
	}
	if _, _, err := s.beginWorkflowMutation(context.Background()); err == nil {
		t.Fatal("unacknowledged setup admitted effect")
	}
	ack := hostFrame{Schema: 1, Runtime: s.ref, AttachmentID: a.id, Event: "workflow_invalidated_ack", ID: a.setupID, Sequence: a.setupSequence}
	wrong := ack
	wrong.Runtime.Generation = "wrong"
	if s.acceptWorkflowACK(a, wrong) {
		t.Fatal("wrong-runtime setup ACK accepted")
	}
	wrong = ack
	wrong.AttachmentID = newUUID()
	if s.acceptWorkflowACK(a, wrong) {
		t.Fatal("wrong-attachment setup ACK accepted")
	}
	if !s.acceptWorkflowACK(a, ack) {
		t.Fatal("exact setup ACK refused")
	}
	if s.acceptWorkflowACK(a, ack) {
		t.Fatal("duplicate setup ACK accepted")
	}
	if seen, err := s.observeWorkflow(context.Background(), h); err != nil || !seen.Idle {
		t.Fatal(seen, err)
	}
	request := ResourceRequest{ResourceID: strings.Repeat("a", 32), OperationID: strings.Repeat("b", 32)}
	request.Digest = ResourceDigest(s.ref, request)
	record := resourceRecord{Schema: 1, Request: request, Status: ResourceStatus{Handle: ResourceHandle{Runtime: s.ref, ResourceID: request.ResourceID, OperationID: request.OperationID}, Phase: "stopped"}}
	path := filepath.Join(s.stateRoot, "resources", request.ResourceID+".json")
	if err := writePrivate(path, record); err != nil {
		t.Fatal(err)
	}
	if seen, err := s.observeWorkflow(context.Background(), h); err == nil && seen.Idle {
		t.Fatal("missing wait result became positive idle")
	}
	record.Status.ExitCode = workflowPointer(0)
	if err := writePrivate(path, record); err != nil {
		t.Fatal(err)
	}
	if seen, err := s.observeWorkflow(context.Background(), h); err != nil || !seen.Idle {
		t.Fatal(seen, err)
	}
}

func workflowHistoricalResourceFixture(t *testing.T) (*supervisor, *Manager, runtimeRecords, resourceRecord, string) {
	t.Helper()
	s, _, _, _, _ := workflowSupervisorFixture(t)
	m := &Manager{config: Config{UserStateRoot: t.TempDir(), HostStateRoot: t.TempDir(), AuthDirectory: t.TempDir(), AccountBindings: []AccountBinding{{Provider: "p3-fake", AccountRef: "fake-account"}}}}
	first := StartRequest{Session: s.ref.Session, BindingRevision: 1, RequestID: newUUID(), AppStorePath: "/nix/store/00000000000000000000000000000000-fixture", ProfilePath: "/owned-profile", ProfileGeneration: "1"}
	previous, err := m.reserveRuntimeRecord(first, newUUID())
	if err != nil {
		t.Fatal(err)
	}
	previous.Phase = "starting"
	next := first
	next.RequestID = newUUID()
	next.ReplacesRequestID = first.RequestID
	next.BindingRevision = 2
	current, err := m.reserveRuntimeRecord(next, previous.Runtime.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	current.Phase = "starting"
	records := runtimeRecords{Schema: 2, Session: first.Session, InstanceID: previous.Runtime.InstanceID, CurrentRequestID: next.RequestID, Requests: map[string]runtimeRecord{first.RequestID: previous, next.RequestID: current}}
	directory, err := stateDirectory(m.config.UserStateRoot, first.Session)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(directory, "runtime.json"), records); err != nil {
		t.Fatal(err)
	}
	instance := m.instance(current.Runtime)
	launch, configuration, err := m.frozenRequestBytes(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := freezeBytes(filepath.Join(instance, "launch.json"), launch); err != nil {
		t.Fatal(err)
	}
	if err := freezeBytes(filepath.Join(instance, "requests", next.RequestID, "runtime-config.json"), configuration); err != nil {
		t.Fatal(err)
	}
	s.ref = current.Runtime
	s.launch = launchFor(next, current.Runtime)
	s.config = m.runtimeConfiguration(s.launch, s.ref)
	s.stateRoot = directory
	s.handshake = Handshake{HostObservation: HostObservation{LaunchRequest: s.launch, Digest: s.ref.Digest, BootID: "owned-current-boot", InvocationID: strings.Repeat("d", 32), MainPID: os.Getpid()}, RuntimeConfigSHA256: current.RuntimeConfigSHA256}
	s.verifiedInstanceDirectory = instance
	if err := s.validateSuccessorStartup(instance); err != nil {
		t.Fatal("strict owned map/startup proof invalid", err)
	}
	// Supervise sets this only after the same exact startup validation succeeds.
	s.reclaimControl = true
	request := ResourceRequest{ResourceID: strings.Repeat("a", 32), OperationID: strings.Repeat("b", 32), Argv: []string{"/original-owned-command"}, Cwd: first.Session.Workspace}
	request.Digest = ResourceDigest(previous.Runtime, request)
	resource := resourceRecord{Schema: 1, Request: request, Status: ResourceStatus{Handle: ResourceHandle{Runtime: previous.Runtime, ResourceID: request.ResourceID, OperationID: request.OperationID, PID: 42, ProcessGroup: 42}, Phase: "stopped", ExitCode: workflowPointer(0)}}
	path := filepath.Join(directory, "resources", request.ResourceID+".json")
	return s, m, records, resource, path
}
func TestWorkflowHistoricalResourceUsesStrictStartupAndOriginalChainOnly(t *testing.T) {
	for _, fault := range []string{"valid", "foreign-runtime", "foreign-chain", "disconnected-chain", "digest", "filename", "operation", "missing-exit", "running", "unknown", "rejected", "missing-startup", "missing-directory", "wrong-directory", "wrong-config", "wrong-current", "current-rejected"} {
		t.Run(fault, func(t *testing.T) {
			s, m, records, resource, path := workflowHistoricalResourceFixture(t)
			switch fault {
			case "foreign-runtime":
				resource.Status.Handle.Runtime.Generation = strings.Repeat("f", 64)
				resource.Request.Digest = ResourceDigest(resource.Status.Handle.Runtime, resource.Request)
			case "foreign-chain":
				resource.Status.Handle.Runtime.RequestID = newUUID()
				resource.Request.Digest = ResourceDigest(resource.Status.Handle.Runtime, resource.Request)
			case "disconnected-chain":
				current := records.Requests[records.CurrentRequestID]
				start := current.Start
				start.ReplacesRequestID = ""
				isolated, err := m.reserveRuntimeRecord(start, records.InstanceID)
				if err != nil {
					t.Fatal(err)
				}
				isolated.Phase = "starting"
				records.Requests[records.CurrentRequestID] = isolated
			case "digest":
				resource.Request.Digest = strings.Repeat("0", 64)
			case "filename":
				path = filepath.Join(filepath.Dir(path), strings.Repeat("c", 32)+".json")
			case "operation":
				resource.Request.OperationID = strings.Repeat("c", 32)
				resource.Request.Digest = ResourceDigest(resource.Status.Handle.Runtime, resource.Request)
			case "missing-exit":
				resource.Status.ExitCode = nil
			case "running", "unknown", "rejected":
				resource.Status.Phase = fault
			case "missing-startup":
				s.reclaimControl = false
			case "missing-directory":
				s.verifiedInstanceDirectory = ""
			case "wrong-directory":
				s.verifiedInstanceDirectory = t.TempDir()
			case "wrong-config":
				s.handshake.RuntimeConfigSHA256 = strings.Repeat("f", 64)
			case "wrong-current":
				s.ref.RequestID = newUUID()
			case "current-rejected":
				resource.Status.Handle.Runtime = s.ref
				resource.Request.Digest = ResourceDigest(s.ref, resource.Request)
				resource.Status.Phase = "rejected"
				resource.Status.ExitCode = nil
			}
			directory, _ := stateDirectory(m.config.UserStateRoot, records.Session)
			if err := writePrivate(filepath.Join(directory, "runtime.json"), records); err != nil {
				t.Fatal(err)
			}
			if err := writePrivate(path, resource); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			idle, err := s.workflowResourcesIdle(context.Background())
			if fault == "valid" || fault == "current-rejected" {
				if err != nil || !idle {
					t.Fatal(idle, err)
				}
			} else if err == nil && idle {
				t.Fatal("unproved historical resource accepted", fault)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("historical observation repaired row")
			}
		})
	}
}
