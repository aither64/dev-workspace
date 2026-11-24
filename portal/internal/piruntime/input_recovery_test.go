package piruntime

import (
	"bytes"
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

// Reuse the real protocol2 SDK/control fixture. Stop models the host owner's
// whole-runtime exit, including the shared physical worker, before its receipt.
type recoveryFixtureHost struct {
	*upgradeFixtureHost
	beforeStop func()
}

type recoveryLookupWorker struct {
	worker
	inputs *atomic.Int32
}

func (w *recoveryLookupWorker) Call(ctx context.Context, operation string, input, output any) error {
	if operation == "send" || operation == "input_enqueue_v1" {
		w.inputs.Add(1)
	}
	return w.worker.Call(ctx, operation, input, output)
}

func (h *recoveryFixtureHost) Stop(ctx context.Context, ref RuntimeRef, observed HostObservation) (HostObservation, error) {
	if h.beforeStop != nil {
		h.beforeStop()
	}
	return h.upgradeFixtureHost.Stop(ctx, ref, observed)
}

func TestInputRecoveryOwnedDrainRestoresSharedMembersWithoutResend(t *testing.T) {
	m, base, start, ctx := supervisorFixture(t)
	base.metadata.Protocol = 2
	host := &recoveryFixtureHost{upgradeFixtureHost: newUpgradeFixtureHost(base)}
	m.config.Host = host
	m.config.validatePackage = func(string) error { return nil }
	m.config.inputRecoveryContract = func(string) (bool, error) { return true, nil }
	first, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	attachFixture(t, m, base.s, ctx, first.Runtime, &workflowFixtureHandler{invalidated: func(WorkflowInvalidation) {}})
	awaitWorkflowAttachment(t, base.s, ctx)
	policy := policyTarget("default", 1, []string{}, []string{})
	members := []agentbackend.ConversationRef{}
	histories := map[string][]byte{}
	for i, address := range []string{"lead", "implementer0"} {
		state, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: address, Incarnation: strings.Repeat(string(rune('b'+i)), 64), CreationID: []string{"33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"}[i], Settings: settingsFixture(), ToolPolicy: &policy})
		if err != nil {
			t.Fatal(err)
		}
		handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: first.Runtime.Generation}
		if sent, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "fixture retained input", ClientUserMessageID: strings.Repeat("d", 32)}); err != nil || sent.Outcome != backend.Accepted {
			t.Fatal(sent, err)
		}
		waitIdle(t, m, ctx, handle)
		c, _ := base.s.member(state.Conversation)
		histories[c.record.HistoryFile], err = os.ReadFile(c.record.HistoryFile)
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, state.Conversation)
	}
	target, _ := base.s.member(members[0])
	sibling, _ := base.s.member(members[1])
	// A genuinely encoded Unknown receipt is deliberately disposable; the fixture
	// does not reconstruct an acceptance witness from retained user text.
	input := SendInput{Message: "disposable interrupted input", ActionContext: inputDeliveryPrefix + "steer"}
	ref := SubmissionRef{Conversation: members[0], OperationID: "55555555-5555-4555-8555-555555555555", Action: "send"}
	ref.Digest = SubmissionDigest(ref, input, target.record.Settings)
	now := time.Now().UTC()
	unknown := SubmissionStatus{Schema: 1, Ref: ref, Phase: "Unknown", Generation: first.Runtime.Generation, Settings: target.record.Settings, Input: input, CreatedAt: now, UpdatedAt: now}
	receiptPath := filepath.Join(target.receipts.root, ref.OperationID+".json")
	if err := writePrivate(receiptPath, unknown); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	next := nextStart(start, "22222222-2222-4222-8222-222222222222")
	next.BindingRevision = start.BindingRevision
	request := UpgradeRequest{Start: next, ExpectedGeneration: first.Runtime.Generation, Members: members, RetireUnknown: &InputRetirement{OperationID: ref.OperationID, Digest: ref.Digest, Member: "lead"}}
	publishFailures := 1
	hooks := UpgradeHooks{EnsureAttachment: func(_ context.Context, r RuntimeRef, _ HostCallbackConnector) error {
		if r.RequestID != first.Runtime.RequestID {
			attachFixture(t, m, base.s, ctx, r, &workflowFixtureHandler{invalidated: func(WorkflowInvalidation) {}})
			awaitWorkflowAttachment(t, base.s, ctx)
		}
		return nil
	}, Publish: func(_ context.Context, proof UpgradeProof) error {
		transition, err := proof.Transition()
		if err != nil || len(transition.Members) != 2 {
			t.Fatal(transition, err)
		}
		if publishFailures > 0 {
			publishFailures--
			return ErrUnavailable
		}
		return nil
	}}
	// The target exception must not excuse an unrelated member's hold.
	sibling.record.HeldReason = "independent callback uncertainty"
	if err := sibling.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upgrade(ctx, request, hooks); !errors.Is(err, ErrHeld) {
		t.Fatal("unrelated hold admitted", err)
	}
	sibling.record.HeldReason = ""
	if err := sibling.save(); err != nil {
		t.Fatal(err)
	}
	m.config.inputRecoveryContract = func(string) (bool, error) { return false, nil }
	if _, err := m.Upgrade(ctx, request, hooks); !errors.Is(err, ErrUnavailable) {
		t.Fatal("incapable target admitted", err)
	}
	m.config.inputRecoveryContract = func(string) (bool, error) { return true, nil }
	if host.stops != 0 {
		t.Fatal("preflight stopped runtime")
	}
	oldSupervisor := base.s
	physical := oldSupervisor.sharedHost
	if physical == nil {
		t.Fatal("fixture did not use a shared worker")
	}
	counts := &upgradeDispatchCounts{}
	t.Cleanup(func() {
		if base.s != nil {
			for _, c := range base.s.members {
				if w, ok := c.worker.(*upgradeCountedWorker); ok {
					c.worker = w.worker
				}
			}
		}
	})
	host.onStart = func(s *supervisor) {
		originalStart := s.start
		s.start = func(ctx context.Context, meta workerMetadata, h agentbackend.ExecutionHandle, cfg workerConfig, changed func()) (worker, error) {
			if cfg.HistoryFile == "" {
				counts.creates.Add(1)
			}
			w, err := originalStart(ctx, meta, h, cfg, changed)
			if err != nil {
				return w, err
			}
			return &upgradeCountedWorker{worker: w, counts: counts}, nil
		}
	}
	// Retain cleanup of the physical worker even if a correctness assertion fails.
	t.Cleanup(func() {
		select {
		case <-physical.closed:
		default:
			_ = physical.command.Process.Kill()
			<-physical.closed
		}
	})
	host.beforeStop = func() {
		for _, c := range []*conversation{target, sibling} {
			var saved conversationRecord
			if readPrivate(filepath.Join(c.root, "conversation.json"), &saved) != nil || !saved.Loaded {
				t.Fatal("bookkeeping changed before host Stop")
			}
		}
		if err := physical.command.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-physical.closed:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		// Model the SDK's last native append while the owned Stop completes.
		// It must survive both a lost Stop reply and the saved upgrade retry.
		path := target.record.HistoryFile
		previous := histories[path]
		lines := bytes.Split(bytes.TrimSpace(previous), []byte("\n"))
		var entry map[string]any
		if err := json.Unmarshal(lines[len(lines)-1], &entry); err != nil {
			t.Fatal(err)
		}
		entry["parentId"] = entry["id"]
		entry["id"] = "ef123abc"
		entry["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
		message, ok := entry["message"].(map[string]any)
		if !ok || message["role"] != "assistant" {
			t.Fatal("fixture lacks terminal native assistant")
		}
		message["stopReason"] = "aborted"
		message["content"] = []any{}
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, '\n')
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.Write(data)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			t.Fatal(writeErr, syncErr, closeErr)
		}
		histories[path] = append(append([]byte{}, previous...), data...)
	}
	drains := 0
	m.config.verifyDrain = func(observed HostObservation) error {
		drains++
		select {
		case <-physical.closed:
		default:
			t.Fatal("drain proof preceded worker exit")
		}
		if !validTerminalReceipt(runtimeRecord{Start: start, Runtime: first.Runtime}, observed) {
			t.Fatal("missing exact terminal proof")
		}
		return nil
	}
	host.stopReplyErr = ErrUnavailable
	if result, err := m.Upgrade(ctx, request, hooks); !errors.Is(err, ErrUnavailable) || result.Runtime.RequestID != next.RequestID || host.stops != 1 || host.starts != 1 {
		t.Fatal("lost Stop reply setup", result, err, host.stops, host.starts)
	}
	host.stopReplyErr = nil
	host.beforeStop = func() { t.Fatal("same operation stopped twice") }
	if result, err := m.Upgrade(ctx, request, hooks); !errors.Is(err, ErrUnavailable) || result.Runtime.RequestID != next.RequestID {
		t.Fatal("lost publication setup", result, err, "stops/starts/drains", host.stops, host.starts, drains)
	}
	if host.stops != 1 || host.starts != 2 || drains == 0 {
		t.Fatal("incorrect owned replacement", host.stops, host.starts, drains)
	}
	host.beforeStop = func() { t.Fatal("same operation stopped twice") }
	result, err := m.Upgrade(ctx, request, hooks)
	if err != nil || result.Phase != "ready" || result.Disposition != "retired-unknown" || result.InputID != ref.OperationID || result.InputDigest != ref.Digest || host.stops != 1 || host.starts != 2 {
		t.Fatal(result, err, host.stops, host.starts)
	}
	if counts.creates.Load() != 0 || counts.sends.Load() != 0 || counts.configures.Load() != 0 {
		t.Fatal("recovery dispatched new work", counts)
	}
	after, err := os.ReadFile(receiptPath)
	if err != nil || string(after) != string(original) {
		t.Fatal("Unknown receipt changed", err)
	}
	if status, err := m.Lookup(ctx, ref); err != nil || status.Phase != "Unknown" {
		t.Fatal("lookup rewrote uncertainty", status, err)
	}
	var nativeInputs atomic.Int32
	for _, member := range members {
		c, err := base.s.member(member)
		if err != nil {
			t.Fatal(err)
		}
		originalWorker := c.worker
		c.worker = &recoveryLookupWorker{worker: originalWorker, inputs: &nativeInputs}
		t.Cleanup(func() { c.worker = originalWorker })
	}
	// Both the retained-owner fallback and the new wire project authoritative
	// retirement without changing the original Unknown or dispatching input.
	for _, checked := range []bool{false, true} {
		m.config.checkedInputContract = func(string) (bool, error) { return checked, nil }
		for range 2 {
			restarted := &Manager{config: m.config}
			lookup, err := restarted.LookupQueuedInput(ctx, ref)
			if err != nil || !lookup.RetiredUnknown || lookup.Status.Phase != "Unknown" || lookup.Status.Ref != ref {
				t.Fatal("retirement proof missing after replacement/restart", checked, lookup, err)
			}
		}
	}
	recoveryFile := recoveryPath(target.root, ref.OperationID)
	recoveryBytes, err := os.ReadFile(recoveryFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"prepared", "ref", "source", "lineage", "receipt", "reason", "malformed"} {
		var record inputRecoveryRecord
		if err := strictDecode(recoveryBytes, &record, recordLimit); err != nil {
			t.Fatal(err)
		}
		switch fault {
		case "prepared":
			record.Phase = "prepared"
			record.Stop = nil
			record.RetiredAt = nil
		case "ref":
			record.Ref.Digest = strings.Repeat("e", 64)
		case "source":
			record.Source.Generation = strings.Repeat("e", 64)
		case "lineage":
			record.Successor.RequestID = "99999999-9999-4999-8999-999999999999"
		case "receipt":
			record.ReceiptSHA256 = strings.Repeat("e", 64)
		case "reason":
			record.Reason = "an arbitrary retirement reason"
		}
		if err := writePrivate(recoveryFile, record); err != nil {
			t.Fatal(err)
		}
		if fault == "malformed" {
			if err := os.WriteFile(recoveryFile, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		lookup, err := m.LookupQueuedInput(ctx, ref)
		if lookup.RetiredUnknown || fault != "prepared" && err == nil {
			t.Fatal("unproved retirement released original", fault, lookup, err)
		}
	}
	if err := os.WriteFile(recoveryFile, recoveryBytes, 0600); err != nil {
		t.Fatal(err)
	}
	recoveryDirectory := filepath.Dir(recoveryFile)
	if err := os.Rename(recoveryDirectory, recoveryDirectory+"-saved"); err != nil {
		t.Fatal(err)
	}
	lookup, lookupErr := m.LookupQueuedInput(ctx, ref)
	if err := os.Rename(recoveryDirectory+"-saved", recoveryDirectory); err != nil {
		t.Fatal(err)
	}
	if lookupErr != nil || lookup.RetiredUnknown || lookup.Status.Phase != "Unknown" {
		t.Fatal("missing recovery changed original uncertainty", lookup, lookupErr)
	}
	unrelated := filepath.Join(recoveryDirectory, "77777777-7777-4777-8777-777777777777.json")
	if err := os.WriteFile(unrelated, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	lookup, lookupErr = m.LookupQueuedInput(ctx, ref)
	if err := os.Remove(unrelated); err != nil {
		t.Fatal(err)
	}
	if lookupErr == nil || lookup.RetiredUnknown {
		t.Fatal("partial recovery directory granted retirement", lookup, lookupErr)
	}
	if got, err := os.ReadFile(receiptPath); err != nil || !bytes.Equal(got, original) {
		t.Fatal("lookup changed original Unknown bytes", err)
	}
	if nativeInputs.Load() != 0 {
		t.Fatal("retirement lookup dispatched native input", nativeInputs.Load())
	}
	for _, member := range members {
		c, err := base.s.member(member)
		if err != nil || c.record.Settings != settingsFixture() || !c.record.Loaded || c.record.Generation != result.Runtime.Generation || c.record.WorkerTopology != "shared-session-v1" {
			t.Fatal("member restoration", member, err)
		}
		data, err := os.ReadFile(c.record.HistoryFile)
		if err != nil || string(data) != string(histories[c.record.HistoryFile]) {
			t.Fatal("history changed", err)
		}
	}
	retained, err := InspectRetainedConversations(ctx, m.config.UserStateRoot, []string{start.Session.Workspace})
	if err != nil || len(retained) != 2 {
		t.Fatal(retained, err)
	}
	recoverySeen := 0
	for _, r := range retained {
		if r.HasInputRecovery {
			recoverySeen++
		}
	}
	if recoverySeen != 1 {
		t.Fatal("retained capability missing", retained)
	}
	// The original operation remains query-only, even after worker replacement.
	handle := agentbackend.ExecutionHandle{Conversation: members[0], Generation: result.Runtime.Generation}
	retry, err := base.s.inputDelivery(ctx, handle, backend.SendRequest{Message: input.Message, ClientUserMessageID: ref.OperationID, Delivery: "steer", Retry: true})
	if err != nil || retry.Phase != "Unknown" {
		t.Fatal(retry, err)
	}
	m.config.conversationIdentityContract = func(string) (bool, error) { return true, nil }
	m.config.inputDeliveryContract = func(string) (bool, error) { return true, nil }
	selection, err := m.ReadInputSelection(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	distinct := checkedRequest(selection, "66666666-6666-4666-8666-666666666666")
	accepted, err := m.DispatchQueuedInput(ctx, distinct)
	if err != nil || accepted.Status.Phase != "Pending" {
		t.Fatal("retirement blocked distinct checked input", accepted, err)
	}
	for {
		lookup, err := m.LookupQueuedInput(ctx, distinct.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if lookup.Status.Phase == "Accepted" {
			break
		}
		if lookup.Status.Phase != "Pending" {
			t.Fatal(lookup)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	base.s.inputTasks.Wait()
	if nativeInputs.Load() != 1 {
		t.Fatal("distinct input count or original replay", nativeInputs.Load())
	}
	if got, err := os.ReadFile(receiptPath); err != nil || !bytes.Equal(got, original) {
		t.Fatal("distinct send rewrote retired original", err)
	}
}
