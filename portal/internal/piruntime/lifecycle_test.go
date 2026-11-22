package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
