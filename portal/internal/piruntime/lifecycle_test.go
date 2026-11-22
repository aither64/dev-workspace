package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// Observe the existing fixture transport without replacing worker/unload logic.
type retirementRecordingInput struct {
	io.WriteCloser
	mu         *sync.Mutex
	operations *[]string
}

func (c retirementRecordingInput) Write(data []byte) (int, error) {
	var frame struct {
		Operation string `json:"op"`
	}
	if json.Unmarshal(data, &frame) == nil && frame.Operation != "" {
		c.mu.Lock()
		*c.operations = append(*c.operations, frame.Operation)
		c.mu.Unlock()
	}
	return c.WriteCloser.Write(data)
}

func TestRetireSharedRuntimeAndRetry(t *testing.T) {
	m, base, start, ctx := supervisorFixture(t)
	base.metadata.Protocol = 2
	host := newUpgradeFixtureHost(base)
	m.config.Host = host
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
		if sent, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "retained lifecycle fixture", ClientUserMessageID: strings.Repeat("d", 32)}); err != nil || sent.Outcome != backend.Accepted {
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
	directory, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	runtimeBefore, err := os.ReadFile(filepath.Join(directory, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	launchBefore, err := os.ReadFile(filepath.Join(m.instance(first.Runtime), "launch.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	operations := []string{}
	physical := base.s.sharedHost
	if physical == nil {
		t.Fatal("fixture lacks shared worker")
	}
	physical.input = retirementRecordingInput{physical.input, &mu, &operations}
	base.s.start = func(context.Context, workerMetadata, agentbackend.ExecutionHandle, workerConfig, func()) (worker, error) {
		t.Fatal("retirement tried to load/create")
		return nil, ErrScope
	}

	attachmentCalls := 0
	hook := func(_ context.Context, ref RuntimeRef, _ HostCallbackConnector) error {
		attachmentCalls++
		if ref != first.Runtime {
			t.Fatal("changed attachment owner")
		}
		return nil
	}
	retire := func() (RuntimeStatus, error) { return m.Retire(ctx, first.Runtime, members, hook) }
	wrong := first.Runtime
	wrong.Generation = strings.Repeat("f", 64)
	if _, err := m.Retire(ctx, wrong, members, hook); !errors.Is(err, ErrScope) {
		t.Fatal("wrong runtime admitted", err)
	}
	wrongMembers := append([]agentbackend.ConversationRef{}, members...)
	wrongMembers[1].NativeID = newUUID()
	if _, err := m.Retire(ctx, first.Runtime, wrongMembers, hook); err == nil {
		t.Fatal("changed member admitted")
	}
	target, _ := base.s.member(members[0])
	input := SendInput{Message: "unresolved callback input"}
	ref := SubmissionRef{Conversation: members[0], OperationID: "55555555-5555-4555-8555-555555555555", Action: "send"}
	ref.Digest = SubmissionDigest(ref, input, target.record.Settings)
	now := time.Now().UTC()
	path := filepath.Join(target.receipts.root, ref.OperationID+".json")
	if err := writePrivate(path, SubmissionStatus{Schema: 1, Ref: ref, Phase: "Unknown", Generation: first.Runtime.Generation, Settings: target.record.Settings, Input: input, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := retire(); !errors.Is(err, ErrHeld) {
		t.Fatal("Unknown admitted", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	} // Fixture-owned negative control only.
	target.record.HeldReason = "forwarded callback uncertainty"
	if err := target.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := retire(); !errors.Is(err, ErrHeld) {
		t.Fatal("callback hold admitted", err)
	}
	target.record.HeldReason = ""
	if err := target.save(); err != nil {
		t.Fatal(err)
	}
	target.activeMu.Lock()
	target.callbackUnknown = true
	target.activeMu.Unlock()
	if _, err := retire(); !errors.Is(err, ErrHeld) {
		t.Fatal("live callback uncertainty admitted", err)
	}
	target.activeMu.Lock()
	target.callbackUnknown = false
	target.activeMu.Unlock()
	resource := ResourceRequest{ResourceID: "66666666-6666-4666-8666-666666666666", OperationID: "77777777-7777-4777-8777-777777777777", Cwd: start.Session.Workspace, Argv: []string{"fixture"}, Environment: []string{}}
	resource.Digest = ResourceDigest(first.Runtime, resource)
	resourcePath := filepath.Join(directory, "resources", resource.ResourceID+".json")
	if err := writePrivate(resourcePath, resourceRecord{Schema: 1, Request: resource, Status: ResourceStatus{Handle: ResourceHandle{Runtime: first.Runtime, ResourceID: resource.ResourceID, OperationID: resource.OperationID}, Phase: "running"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := retire(); !errors.Is(err, ErrHeld) {
		t.Fatal("resource hold admitted", err)
	}
	if err := os.Remove(resourcePath); err != nil {
		t.Fatal(err)
	}
	if host.stops != 0 || host.starts != 1 {
		t.Fatal("preflight changed host", host.stops, host.starts)
	}
	drained := false
	m.config.verifyDrain = func(observed HostObservation) error {
		if !validTerminalReceipt(runtimeRecord{Start: start, Runtime: first.Runtime}, observed) {
			t.Fatal("invalid terminal receipt")
		}
		select {
		case <-physical.closed:
		default:
			t.Fatal("host drain preceded final physical exit")
		}
		for _, member := range members {
			var mapping conversationRecord
			path := filepath.Join(directory, "members", member.Member, member.Incarnation, "conversation.json")
			if readPrivate(path, &mapping) != nil || mapping.Loaded {
				t.Fatal("host drain preceded durable unload")
			}
		}
		if !drained {
			return ErrHeld
		}
		return nil
	}
	host.stopReplyErr = ErrUnavailable
	if result, err := retire(); !errors.Is(err, ErrUnavailable) || result.Runtime != first.Runtime {
		t.Fatal("lost Stop control", result, err)
	}
	if host.stops != 1 {
		t.Fatal("missing original Stop")
	}
	host.stopReplyErr = nil
	if _, err := retire(); !errors.Is(err, ErrHeld) {
		t.Fatal("nonpositive current drain admitted", err)
	}
	drained = true
	result, err := retire()
	if err != nil || result.Phase != "stopped" || result.Runtime != first.Runtime || host.stops != 1 || host.starts != 1 {
		t.Fatal(result, err, host.stops, host.starts)
	}
	if attachmentCalls < 1 {
		t.Fatal("live attachment omitted")
	}
	mu.Lock()
	defer mu.Unlock()
	unloads := 0
	for _, op := range operations {
		if op == "unload" {
			unloads++
		}
		if op == "create" || op == "load" || op == "send" || op == "configure" || op == "input_enqueue_v1" || op == "prompt" || op == "attach" {
			t.Fatal("retirement dispatched work", op)
		}
	}
	if unloads != 2 {
		t.Fatal("members detached again on retry", operations)
	}
	for path, before := range histories {
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatal("history changed", err)
		}
	}
	for path, before := range map[string][]byte{filepath.Join(directory, "runtime.json"): runtimeBefore, filepath.Join(m.instance(first.Runtime), "launch.json"): launchBefore} {
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatal("runtime replaced", err)
		}
	}
}

func TestPiLifecycleStoppedRevivalKeepsOriginalMembers(t *testing.T) {
	m, base, start, ctx := supervisorFixture(t)
	base.metadata.Protocol = 2
	host := newUpgradeFixtureHost(base)
	m.config.Host = host
	m.config.validatePackage = func(string) error { return nil }
	first, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	oldBoot, currentBoot := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	base.observation.BootID = oldBoot
	base.observation.InvocationID = strings.Repeat("a", 32)
	base.s.handshake.BootID = oldBoot
	base.s.handshake.InvocationID = base.observation.InvocationID
	bootProof := t.TempDir()
	bootPath := filepath.Join(bootProof, "boot")
	groups := filepath.Join(bootProof, "groups")
	if err := os.Mkdir(groups, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(groups, "cgroup.controllers"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootPath, []byte(oldBoot), 0600); err != nil {
		t.Fatal(err)
	}
	attachFixture(t, m, base.s, ctx, first.Runtime, &workflowFixtureHandler{invalidated: func(WorkflowInvalidation) {}})
	awaitWorkflowAttachment(t, base.s, ctx)
	policy := policyTarget("default", 1, []string{}, []string{})
	state, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: settingsFixture(), ToolPolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: first.Runtime.Generation}
	if result, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "original accepted input", ClientUserMessageID: strings.Repeat("d", 32)}); err != nil || result.Outcome != backend.Accepted {
		t.Fatal(result, err)
	}
	waitIdle(t, m, ctx, handle)
	member, _ := base.s.member(state.Conversation)
	before, err := os.ReadFile(member.record.HistoryFile)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: "implementer0", Incarnation: strings.Repeat("e", 64), CreationID: strings.Repeat("c", 64), Settings: settingsFixture(), ToolPolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	peerHandle := agentbackend.ExecutionHandle{Conversation: peer.Conversation, Generation: first.Runtime.Generation}
	if result, err := m.Deliver(ctx, peerHandle, backend.SendRequest{Message: "original creating member input", ClientUserMessageID: strings.Repeat("e", 32)}); err != nil || result.Outcome != backend.Accepted {
		t.Fatal(result, err)
	}
	waitIdle(t, m, ctx, peerHandle)
	slots := []LifecycleSlot{{State: "ready", Member: "lead", Incarnation: state.Conversation.Incarnation, CreationID: member.record.CreationID, NativeID: state.Conversation.NativeID, InitialSettings: member.record.InitialSettings}, {State: "creating", Member: "implementer0", Incarnation: peer.Conversation.Incarnation, CreationID: strings.Repeat("c", 64), InitialSettings: settingsFixture()}}
	captured, err := m.CaptureLifecycleMembers(ctx, first.Runtime, slots)
	if err != nil || len(captured) != 2 || captured[1].Conversation == nil {
		t.Fatal(captured, err)
	}
	attached := map[string]bool{first.Runtime.RequestID: true}
	hook := func(_ context.Context, r RuntimeRef, _ HostCallbackConnector) error {
		if !attached[r.RequestID] {
			attachFixture(t, m, base.s, ctx, r, &workflowFixtureHandler{invalidated: func(WorkflowInvalidation) {}})
			awaitWorkflowAttachment(t, base.s, ctx)
			attached[r.RequestID] = true
		}
		return nil
	}
	m.config.verifyDrain = func(observed HostObservation) error { return verifyCurrentDrainAt(observed, bootPath, groups) }
	if status, err := m.Retire(ctx, first.Runtime, []agentbackend.ConversationRef{state.Conversation, peer.Conversation}, hook); err != nil || status.Phase != "stopped" {
		t.Fatal(status, err)
	}
	oldReceipt := mustJSON(host.terminal[first.Runtime.RequestID])
	if err := os.WriteFile(bootPath, []byte(currentBoot), 0600); err != nil {
		t.Fatal(err)
	}
	peerDirectory, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	peerPath := filepath.Join(peerDirectory, "members", peer.Conversation.Member, peer.Conversation.Incarnation, "conversation.json")
	peerBefore, err := os.ReadFile(peerPath)
	if err != nil {
		t.Fatal(err)
	}
	next := nextStart(start, "22222222-2222-4222-8222-222222222222")
	next.BindingRevision = start.BindingRevision
	request := UpgradeRequest{Start: next, ExpectedGeneration: first.Runtime.Generation, Members: []agentbackend.ConversationRef{state.Conversation}}
	publications := 0
	hooks := UpgradeHooks{EnsureAttachment: hook, Publish: func(_ context.Context, p UpgradeProof) error {
		transition, err := p.Transition()
		if err != nil || transition.Previous != first.Runtime || len(transition.Members) != 1 || transition.Members[0] != state.Conversation {
			return ErrScope
		}
		publications++
		if publications == 1 {
			return ErrUnavailable
		}
		return nil
	}}
	host.onStart = func(s *supervisor) {
		base.observation.BootID = currentBoot
		base.observation.InvocationID = strings.Repeat("b", 32)
		s.handshake.BootID = currentBoot
		s.handshake.InvocationID = base.observation.InvocationID
		original := s.start
		s.start = func(ctx context.Context, md workerMetadata, h agentbackend.ExecutionHandle, c workerConfig, changed func()) (worker, error) {
			if c.EmptyProven {
				t.Error("revive created native conversation")
			}
			return original(ctx, md, h, c, changed)
		}
	}
	result, err := m.Upgrade(ctx, request, hooks)
	if !errors.Is(err, ErrUnavailable) || host.starts != 2 || host.stops != 1 {
		t.Fatal(result, err, host.starts, host.stops)
	}
	again, err := m.Upgrade(ctx, request, hooks)
	if err != nil || again.Runtime != result.Runtime || again.Phase != "ready" || host.starts != 2 || host.stops != 1 || publications != 2 {
		t.Fatal(again, err, host.starts, host.stops, publications)
	}
	after, err := os.ReadFile(member.record.HistoryFile)
	if err != nil || string(before) != string(after) {
		t.Fatal("revival changed native history", err)
	}
	root, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	var restored conversationRecord
	if err := readPrivate(filepath.Join(root, "members", "lead", state.Conversation.Incarnation, "conversation.json"), &restored); err != nil || restored.Conversation != state.Conversation || restored.Settings != member.record.Settings || !restored.Loaded {
		t.Fatal("restored identity/settings", restored, err)
	}
	receipts, err := os.ReadDir(filepath.Join(root, "members", "lead", state.Conversation.Incarnation, "submissions"))
	if err != nil || len(receipts) != 1 {
		t.Fatal("revival dispatched another input", receipts, err)
	}
	freshSlots := append([]LifecycleSlot{}, slots...)
	fresh, err := m.CaptureLifecycleMembers(ctx, again.Runtime, freshSlots)
	if err != nil || len(fresh) != 2 || fresh[1].Generation != first.Runtime.Generation {
		t.Fatal("creating ancestor capture", fresh, err)
	}
	var stale conversationRecord
	if err := readPrivate(peerPath, &stale); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"loaded", "foreign-generation"} {
		bad := stale
		if fault == "loaded" {
			bad.Loaded = true
		} else {
			bad.Generation = strings.Repeat("f", 64)
		}
		if err := writePrivate(peerPath, bad); err != nil {
			t.Fatal(err)
		}
		if _, err := m.CaptureLifecycleMembers(ctx, again.Runtime, freshSlots); !errors.Is(err, ErrScope) {
			t.Fatal(fault, err)
		}
		if err := os.WriteFile(peerPath, peerBefore, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if status, err := m.Retire(ctx, again.Runtime, []agentbackend.ConversationRef{state.Conversation}, hook); err != nil || status.Phase != "stopped" {
		t.Fatal("next archive", status, err)
	}
	peerAfter, err := os.ReadFile(peerPath)
	if err != nil || string(peerAfter) != string(peerBefore) {
		t.Fatal("revival materialized or changed creating slot", err)
	}

	if string(oldReceipt) != string(mustJSON(host.terminal[first.Runtime.RequestID])) {
		t.Fatal("revival rewrote old-boot terminal receipt")
	}

}

func TestPiLifecycleEmptyCreatingMemberProof(t *testing.T) {
	m, base, start, ctx := supervisorFixture(t)
	base.metadata.Protocol = 2
	first, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	handler := &workflowFixtureHandler{callbackFixtureHandler: callbackFixtureHandler{tools: []string{"report_to_lead"}}, invalidated: func(WorkflowInvalidation) {}}
	attachFixture(t, m, base.s, ctx, first.Runtime, handler)
	awaitWorkflowAttachment(t, base.s, ctx)
	policy := policyTarget("default", 1, []string{"report_to_lead"}, []string{"report_to_lead"})
	state, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: "implementer1", Incarnation: strings.Repeat("e", 64), CreationID: strings.Repeat("f", 64), Settings: settingsFixture(), Instructions: "Exact initialized member.", ToolPolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	member, err := base.s.member(state.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	member.mu.Lock()
	record := member.record
	member.mu.Unlock()
	if record.Persisted || !record.Loaded {
		t.Fatal("fixture did not preserve unpersisted native initialization")
	}
	if _, err := os.Stat(record.HistoryFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixture unexpectedly persisted history", err)
	}
	slot := LifecycleSlot{Member: state.Conversation.Member, Incarnation: state.Conversation.Incarnation, CreationID: record.CreationID, InitialInputID: "33333333-3333-4333-8333-333333333333", State: "creating", InitialSettings: settingsFixture()}
	saved := LifecycleMember{Generation: record.Generation, Slot: slot, Conversation: &state.Conversation, Settings: &state.Settings, HistoryRef: state.HistoryRef}
	before, err := os.ReadFile(filepath.Join(member.root, "conversation.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"exact", "wrong-slot", "wrong-generation", "wrong-native", "settings", "persisted", "hold", "pending", "fork", "unloaded", "receipt", "history", "missing-receipt-directory", "missing-live-proof", "unsettled-workflow"} {
		t.Run(kind, func(t *testing.T) {
			capture, current := saved, record
			defer func() {
				if after, err := os.ReadFile(filepath.Join(member.root, "conversation.json")); err != nil || !reflect.DeepEqual(after, before) {
					t.Fatal("empty proof changed mapping", err)
				}
			}()
			switch kind {
			case "wrong-slot":
				capture.Slot.State = "ready"
			case "wrong-generation":
				capture.Generation = strings.Repeat("a", 64)
			case "wrong-native":
				ref := *capture.Conversation
				ref.NativeID = "foreign"
				capture.Conversation = &ref
			case "settings":
				settings := *capture.Settings
				settings.Revision++
				capture.Settings = &settings
			case "persisted":
				current.Persisted = true
			case "hold":
				current.HeldReason = "unrelated hold"
			case "pending":
				pending := current.Settings
				current.PendingSettings = &pending
			case "fork":
				current.Fork = &ForkOrigin{}
			case "unloaded":
				current.Loaded = false
			case "receipt":
				p := filepath.Join(member.root, "submissions", "33333333-3333-4333-8333-333333333333.json")
				if err := os.WriteFile(p, []byte("uncertain receipt"), 0600); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(p)
			case "history":
				if err := os.WriteFile(record.HistoryFile, []byte("unexpected history"), 0600); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(record.HistoryFile)
			case "missing-receipt-directory":
				p := filepath.Join(member.root, "submissions")
				if err := os.Rename(p, p+".saved"); err != nil {
					t.Fatal(err)
				}
				defer os.Rename(p+".saved", p)
			case "missing-live-proof":
				previous := m.config.verifyProcess
				m.config.verifyProcess = func(HostObservation) error { return ErrUnavailable }
				defer func() { m.config.verifyProcess = previous }()
			case "unsettled-workflow":
				previous := base.s.childrenCheck
				base.s.childrenCheck = func() (bool, error) { return true, nil }
				defer func() { base.s.childrenCheck = previous }()
			}
			err := m.emptyLifecycleMember(ctx, first.Runtime, capture, current, member.root, true)
			if (err == nil) != (kind == "exact") {
				t.Fatal(kind, err)
			}
		})
	}
}

func TestPiLifecycleTerminalCallbackDeletionPreservesAudit(t *testing.T) {
	m, base, start, ctx := supervisorFixture(t)
	base.metadata.Protocol = 2
	host := newUpgradeFixtureHost(base)
	m.config.Host = host
	first, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	handler := &workflowFixtureHandler{callbackFixtureHandler: callbackFixtureHandler{tools: []string{"report_to_lead"}, check: func(context.Context, HostCall) error { return ErrUnavailable }}, invalidated: func(WorkflowInvalidation) {}}
	attachFixture(t, m, base.s, ctx, first.Runtime, handler)
	awaitWorkflowAttachment(t, base.s, ctx)
	policy := policyTarget("default", 1, []string{"report_to_lead"}, []string{"report_to_lead"})
	state, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: settingsFixture(), Instructions: "fixture:lifecycle-callbacks", ToolPolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: "implementer1", Incarnation: strings.Repeat("e", 64), CreationID: strings.Repeat("f", 64), Settings: settingsFixture(), Instructions: "Exact initialized member.", ToolPolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	emptyMember, err := base.s.member(empty.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	emptyMapping, err := os.ReadFile(filepath.Join(emptyMember.root, "conversation.json"))
	if err != nil || emptyMember.record.Persisted {
		t.Fatal("empty fixture mapping", err)
	}
	if _, err := os.Stat(emptyMember.record.HistoryFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("initialized member unexpectedly persisted history", err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	if result, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "known original input", ClientUserMessageID: strings.Repeat("d", 32)}); err != nil || result.Outcome != backend.Accepted {
		t.Fatal(result, err)
	}
	member, _ := base.s.member(state.Conversation)
	for {
		var snapshot workerSnapshot
		if err := m.handleCall(ctx, handle, "snapshot", conversationRequest{}, &snapshot); err != nil {
			t.Fatal(err)
		}
		member.mu.Lock()
		held := member.record.HeldReason != ""
		member.mu.Unlock()
		if !snapshot.Busy && held {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	slots := []LifecycleSlot{
		{Member: "lead", Incarnation: state.Conversation.Incarnation, CreationID: strings.Repeat("c", 64), InitialInputID: strings.Repeat("d", 32), NativeID: state.Conversation.NativeID, State: "ready", InitialSettings: settingsFixture()},
		{Member: "implementer1", Incarnation: empty.Conversation.Incarnation, CreationID: strings.Repeat("f", 64), InitialInputID: "33333333-3333-4333-8333-333333333333", State: "creating", InitialSettings: settingsFixture()},
	}
	captured, err := m.CaptureLifecycleMembers(ctx, first.Runtime, slots)
	if err != nil || len(captured) != 2 || captured[1].Conversation == nil {
		t.Fatal("complete creating member capture", captured, err)
	}
	counts := &upgradeDispatchCounts{}
	for _, c := range []*conversation{member, emptyMember} {
		c.mu.Lock()
		c.worker = &upgradeCountedWorker{worker: c.worker, counts: counts}
		c.mu.Unlock()
	}
	t.Cleanup(func() {
		for _, c := range []*conversation{member, emptyMember} {
			if w, ok := c.worker.(*upgradeCountedWorker); ok {
				c.worker = w.worker
			}
		}
	})
	originalStart := base.s.start
	base.s.start = func(ctx context.Context, metadata workerMetadata, handle agentbackend.ExecutionHandle, config workerConfig, changed func()) (worker, error) {
		counts.creates.Add(1)
		return originalStart(ctx, metadata, handle, config, changed)
	}
	callbacks, err := m.TerminalCallbacks(ctx, first.Runtime, captured, true)
	if err != nil || len(callbacks) != 3 {
		t.Fatal("complete possible callbacks", callbacks, err)
	}
	mapping, err := os.ReadFile(filepath.Join(member.root, "conversation.json"))
	if err != nil {
		t.Fatal(err)
	}
	history, err := os.ReadFile(member.record.HistoryFile)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := os.ReadFile(filepath.Join(member.root, "submissions", strings.Repeat("d", 32)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	hook := func(context.Context, RuntimeRef, HostCallbackConnector) error { return nil }
	if _, err := m.Retire(ctx, first.Runtime, []agentbackend.ConversationRef{state.Conversation}, hook); !errors.Is(err, ErrHeld) {
		t.Fatal("ordinary archive accepted callback hold", err)
	}
	host.stopReplyErr = ErrUnavailable
	status, err := m.RetireTerminalCallbacks(ctx, first.Runtime, captured, callbacks)
	if !errors.Is(err, ErrUnavailable) || host.stops != 1 {
		t.Fatal("owned Stop response was not lost", status, err, host.stops)
	}
	host.stopReplyErr = nil
	m.config.verifyDrain = func(HostObservation) error { return ErrHeld }
	status, err = m.RetireTerminalCallbacks(ctx, first.Runtime, captured, callbacks)
	if !errors.Is(err, ErrHeld) || host.stops != 1 {
		t.Fatal("missing drain was accepted", status, err, host.stops)
	}
	m.config.verifyDrain = func(HostObservation) error { return nil }
	status, err = m.RetireTerminalCallbacks(ctx, first.Runtime, captured, callbacks)
	if err != nil || status.Phase != "stopped" || host.stops != 1 || host.starts != 1 {
		t.Fatal(status, err, host.stops, host.starts)
	}
	for path, before := range map[string][]byte{filepath.Join(emptyMember.root, "conversation.json"): emptyMapping, filepath.Join(member.root, "conversation.json"): mapping, member.record.HistoryFile: history, filepath.Join(member.root, "submissions", strings.Repeat("d", 32)+".json"): receipt} {
		after, err := os.ReadFile(path)
		if err != nil || string(before) != string(after) {
			t.Fatal("terminal deletion changed audit", path, err)
		}
	}
	if _, err := m.Retire(ctx, first.Runtime, []agentbackend.ConversationRef{state.Conversation}, hook); !errors.Is(err, ErrHeld) {
		t.Fatal("terminal disposition cleared ordinary hold", err)
	}
	if _, err := os.Stat(emptyMember.record.HistoryFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("terminal disposition materialized empty history", err)
	}
	entries, err := os.ReadDir(filepath.Join(emptyMember.root, "submissions"))
	if err != nil || len(entries) != 0 || counts.creates.Load() != 0 || counts.sends.Load() != 0 || counts.configures.Load() != 0 {
		t.Fatal("terminal disposition created input/configuration effects", entries, err)
	}
}

func TestPiLifecycleExplicitArchiveAbortsAcceptedBusyInput(t *testing.T) {
	m, base, start, ctx := supervisorFixture(t)
	base.metadata.Protocol = 2
	host := newUpgradeFixtureHost(base)
	m.config.Host = host
	first, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	attachFixture(t, m, base.s, ctx, first.Runtime, &workflowFixtureHandler{invalidated: func(WorkflowInvalidation) {}})
	awaitWorkflowAttachment(t, base.s, ctx)
	policy := policyTarget("default", 1, []string{"bash"}, []string{"bash"})
	state, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: settingsFixture(), Instructions: "fixture:policy-child", ToolPolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: first.Runtime.Generation}
	cwd, _ := sessionDirectory(start.Session)
	t.Cleanup(func() {
		f, err := os.OpenFile(filepath.Join(cwd, "policy-child-release"), os.O_WRONLY|syscall.O_NONBLOCK, 0600)
		if err == nil {
			_, _ = f.WriteString("release\n")
			_ = f.Close()
		}
	})
	if result, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "accepted active archive fixture", ClientUserMessageID: strings.Repeat("d", 32)}); err != nil || result.Outcome != backend.Accepted {
		t.Fatal(result, err)
	}
	for {
		if _, err := os.Stat(filepath.Join(cwd, "policy-child-ready")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	members := []agentbackend.ConversationRef{state.Conversation}
	hook := func(context.Context, RuntimeRef, HostCallbackConnector) error { return nil }
	if err := m.CheckLifecycle(ctx, first.Runtime, members); err != nil {
		t.Fatal("known accepted busy admission", err)
	}
	if _, err := m.Retire(ctx, first.Runtime, members, hook); !errors.Is(err, ErrHeld) || host.stops != 0 {
		t.Fatal("strict idle gate", err, host.stops)
	}
	if err := m.InterruptLifecycle(ctx, first.Runtime, members, hook); err != nil {
		t.Fatal("abort request", err)
	}
	m.config.verifyDrain = func(HostObservation) error { return nil }
	for {
		status, err := m.Retire(ctx, first.Runtime, members, hook)
		if err == nil {
			if status.Phase != "stopped" || host.stops != 1 {
				t.Fatal(status, host.stops)
			}
			break
		}
		if !errors.Is(err, ErrHeld) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("actual post-abort settlement", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	memberRoot, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	receipts, err := os.ReadDir(filepath.Join(memberRoot, "members", "lead", state.Conversation.Incarnation, "submissions"))
	if err != nil || len(receipts) != 1 {
		t.Fatal("abort resubmitted input", receipts, err)
	}
}
