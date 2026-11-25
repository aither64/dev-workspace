package piruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// Existing real Unix supervisor framing with a finite worker fake. This fixture
// launches no SDK/model worker and rejects every new RPC against its predecessor.
type teamConflictWorker struct {
	record              conversationRecord
	generation          string
	arguments           json.RawMessage
	missingOrigin, busy bool
	calls               map[string]int
}

func (w *teamConflictWorker) Call(_ context.Context, op string, input, out any) error {
	w.calls[op]++
	p := *w.record.ToolPolicy
	proof := ToolPolicyProof{Policy: p, Generation: w.generation, Active: p.Active, Callable: p.Active, Registered: p.RegisteredCeiling, Applied: true}
	var value any
	switch op {
	case "snapshot":
		history := recoveryHistory{}
		if w.record.Persisted {
			var err error
			history, err = captureRecoveryHistory(w.record)
			if err != nil {
				return err
			}
		}
		value = workerSnapshot{NativeID: w.record.Conversation.NativeID, HistoryFile: w.record.HistoryFile, Persisted: w.record.Persisted, Model: w.record.Settings.Model, Provider: w.record.Settings.Provider, Thinking: w.record.Settings.Thinking, InstructionsDigest: instructionsDigest(w.record.Instructions), HistoryDigest: history.Digest, LeafID: history.LeafID, Held: w.record.HeldReason != "", Busy: w.busy}
	case "tool_policy_inspect":
		value = proof
	case "workflow_observe":
		value = workerWorkflowObservation{NativeID: w.record.Conversation.NativeID, Model: w.record.Settings.Model, Provider: w.record.Settings.Provider, Thinking: w.record.Settings.Thinking, Proof: &proof, Pending: workflowPointer(false), Streaming: workflowPointer(false), HostCalls: workflowPointer(uint64(0)), Running: workflowPointer(w.busy), ActiveCalls: workflowPointer(uint64(0)), Held: workflowPointer(w.record.HeldReason != ""), HostTools: []string{}}
	case "inspect_origin":
		if w.missingOrigin {
			return ErrScope
		}
		value = struct {
			RootEntryID string
			Arguments   json.RawMessage
		}{"user-entry", w.arguments}
	case "verify_history":
		value = struct{ Verified bool }{true}
	default:
		return fmt.Errorf("fixture forbids native operation %s", op)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
func (*teamConflictWorker) Wait(context.Context) error { return nil }
func (*teamConflictWorker) PID() (int, string)         { return 0, "" }

type teamConflictDomainFixture struct {
	slots     []LifecycleSlot
	target    agentbackend.ConversationRef
	kind      string
	fail      bool
	published *int
	pubErr    error
}
type teamConflictProofFixture struct{ e TeamConflictEvidence }

func (p teamConflictProofFixture) TeamConflictEvidence() TeamConflictEvidence { return p.e }
func (d *teamConflictDomainFixture) Published(context.Context, RuntimeRef) (bool, error) {
	return d.published != nil && *d.published > 0, d.pubErr
}
func (d *teamConflictDomainFixture) Slots(context.Context, RuntimeRef) ([]LifecycleSlot, error) {
	return append([]LifecycleSlot(nil), d.slots...), nil
}
func (d *teamConflictDomainFixture) Settle(_ context.Context, _ RuntimeRef, call TeamConflictCall, _ []LifecycleMember) (TeamConflictProof, error) {
	if d.fail {
		return nil, ErrHeld
	}
	return teamConflictProofFixture{TeamConflictEvidence{Kind: d.kind, Target: d.target, InitialInputID: d.slots[1].InitialInputID, RecordsSHA256: strings.Repeat("e", 64)}}, nil
}

type teamConflictTestFixture struct {
	m             *Manager
	host          *upgradeFixtureHost
	request       UpgradeRequest
	hooks         UpgradeHooks
	domain        *teamConflictDomainFixture
	source        RuntimeRef
	sender, empty *conversation
	worker        *teamConflictWorker
	publications  int
	ctx           context.Context
}

func newTeamConflictFixture(t *testing.T) *teamConflictTestFixture {
	t.Helper()
	m, base, start, ctx := supervisorFixture(t)
	host := newUpgradeFixtureHost(base)
	m.config.Host = host
	m.config.verifyDrain = func(HostObservation) error { return nil }
	m.config.validatePackage = func(string) error { return nil }
	m.config.teamConflictRecoveryContract = func(string) (bool, error) { return true, nil }
	m.config.teamConflictPredecessorContract = func(string) error { return nil }
	first, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	f := &teamConflictTestFixture{m: m, host: host, source: first.Runtime, ctx: ctx}
	domain := &teamConflictDomainFixture{kind: "initial_conflict", published: &f.publications}
	f.domain = domain
	args := json.RawMessage(`{"message_id":"55555555-5555-4555-8555-555555555555","to":"implementer0","message":"original conflicting assignment"}`)
	for i, address := range []string{"lead", "implementer0"} {
		ref := agentbackend.ConversationRef{Session: start.Session, Backend: agentbackend.Pi, Member: address, Incarnation: strings.Repeat(string(rune('b'+i)), 64), NativeID: fmt.Sprintf("33333333-3333-4333-8333-%012d", i+1)}
		root := filepath.Join(base.s.stateRoot, "members", address, ref.Incarnation)
		for _, name := range []string{"history", "submissions", "agent"} {
			if err := makePrivateDirectory(filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
		}
		policy := policyTarget("default", 1, []string{}, []string{})
		record := conversationRecord{Schema: 2, Conversation: ref, CreationID: strings.Repeat(string(rune('d'+i)), 64), Instructions: "retained instructions", HistoryRef: historyReference(ref), HistoryFile: filepath.Join(root, "history", ref.NativeID+".jsonl"), Generation: first.Runtime.Generation, Settings: settingsFixture(), InitialSettings: settingsFixture(), Loaded: true, Persisted: i == 0, ToolPolicy: &policy}
		if i == 0 {
			record.HeldReason = "forwarded callback outcome unknown"
			lines := []any{map[string]any{"type": "session", "id": ref.NativeID, "cwd": filepath.Join(start.Session.Workspace, "work", start.Session.Slug)}, map[string]any{"type": "message", "id": "user-entry", "parentId": "", "message": map[string]any{"role": "user", "content": []any{map[string]string{"type": "text", "text": "one original input"}}}}, map[string]any{"type": "message", "id": "anchor", "parentId": "user-entry", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "id": "native-team-call", "name": "team_message", "arguments": args}}}}}
			var raw bytes.Buffer
			for _, line := range lines {
				json.NewEncoder(&raw).Encode(line)
			}
			if err := os.WriteFile(record.HistoryFile, raw.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
		}
		worker := &teamConflictWorker{record: record, generation: first.Runtime.Generation, arguments: args, calls: map[string]int{}}
		c := &conversation{root: root, record: record, worker: worker, receipts: &receiptEngine{root: filepath.Join(root, "submissions")}, hostTools: []string{}}
		if i == 0 {
			c.callbackUnknown = true
		}
		base.s.members[root] = c
		if err := c.save(); err != nil {
			t.Fatal(err)
		}
		initialID := "44444444-4444-4444-8444-444444444444"
		state := "creating"
		if i == 0 {
			f.sender = c
			f.worker = worker
			state = "ready"
			initialID = "11111111-2222-4333-8444-555555555555"
			input := SendInput{Message: "one original input"}
			ref := SubmissionRef{Conversation: ref, OperationID: initialID, Action: "send"}
			ref.Digest = SubmissionDigest(ref, input, record.Settings)
			now := time.Now().UTC()
			status := SubmissionStatus{Schema: 1, Ref: ref, Phase: "Accepted", Generation: first.Runtime.Generation, Settings: record.Settings, Input: input, Witness: &NativeWitness{SessionID: ref.Conversation.NativeID, EntryID: "user-entry", EffectiveDigest: MessageDigest(input.Message)}, CreatedAt: now, UpdatedAt: now}
			if err := writePrivate(filepath.Join(c.receipts.root, initialID+".json"), status); err != nil {
				t.Fatal(err)
			}
		} else {
			f.empty = c
			domain.target = ref
		}
		domain.slots = append(domain.slots, LifecycleSlot{Member: address, Incarnation: ref.Incarnation, CreationID: record.CreationID, InitialInputID: initialID, NativeID: ref.NativeID, State: state, InitialSettings: record.InitialSettings})
	}
	workflowAttachmentFixture(t, base.s)
	next := nextStart(start, "22222222-2222-4222-8222-222222222222")
	next.BindingRevision = start.BindingRevision
	f.request = UpgradeRequest{Start: next, ExpectedGeneration: first.Runtime.Generation, Members: []agentbackend.ConversationRef{f.sender.record.Conversation}, RecoverTeamConflict: &TeamConflictRecovery{Member: "lead", OperationID: "55555555-5555-4555-8555-555555555555"}}
	host.onStart = func(s *supervisor) {
		s.start = func(_ context.Context, _ workerMetadata, h agentbackend.ExecutionHandle, cfg workerConfig, _ func()) (worker, error) {
			var record conversationRecord
			root := filepath.Join(s.stateRoot, "members", h.Conversation.Member, h.Conversation.Incarnation)
			if err := readPrivate(filepath.Join(root, "conversation.json"), &record); err != nil {
				return nil, err
			}
			if cfg.EmptyProven != (h.Conversation.Member == "implementer0") {
				t.Error("wrong empty reopening proof")
			}
			record.HeldReason = ""
			if cfg.EmptyProven {
				// Like the real SDK, reopening the same empty native ID replans
				// an unsaved filename rather than preserving the initial locator.
				record.HistoryFile = filepath.Join(root, "history", "reloaded-"+h.Generation+".jsonl")
			}
			return &teamConflictWorker{record: record, generation: h.Generation, arguments: args, calls: map[string]int{}}, nil
		}
	}
	f.hooks = UpgradeHooks{TeamConflict: domain, EnsureAttachment: func(_ context.Context, r RuntimeRef, _ HostCallbackConnector) error {
		if r == first.Runtime {
			return nil
		}
		attachFixture(t, m, base.s, ctx, r, &workflowFixtureHandler{invalidated: func(WorkflowInvalidation) {}})
		awaitWorkflowAttachment(t, base.s, ctx)
		return nil
	}, Publish: func(_ context.Context, p UpgradeProof) error {
		transition, err := p.Transition()
		if err != nil || len(transition.Members) != 1 || len(transition.RetainedEmpty) != 1 || transition.RetainedEmpty[0] != f.empty.record.Conversation {
			return ErrScope
		}
		f.publications++
		return nil
	}}
	return f
}
func TestTeamConflictOldProtocolPreflightAndNoEffectRefusals(t *testing.T) {
	for _, fault := range []string{"", "origin", "busy", "hold", "pending", "domain", "nonempty", "unknown", "generation"} {
		t.Run(fault, func(t *testing.T) {
			f := newTeamConflictFixture(t)
			if fault == "" {
				captured, err := f.m.CaptureLifecycleMembers(f.ctx, f.source, f.domain.slots)
				if err != nil {
					t.Fatal(err)
				}
				callbacks, err := f.m.TerminalCallbacks(f.ctx, f.source, captured, true, func(context.Context, RuntimeRef, HostCallbackConnector) error { return nil })
				if err != nil || len(callbacks) != 1 {
					t.Fatal("unchanged lifecycle read capture", callbacks, err)
				}
				ordinary := f.request
				ordinary.RecoverTeamConflict = nil
				if _, err := f.m.Upgrade(f.ctx, ordinary, f.hooks); err == nil || f.host.stops != 0 {
					t.Fatal("ordinary upgrade cleared hold", err)
				}
			}

			switch fault {
			case "origin":
				f.worker.missingOrigin = true
			case "busy":
				f.worker.busy = true
			case "hold":
				f.sender.record.HeldReason = "independent hold"
				f.sender.save()
			case "pending":
				v := settingsFixture()
				f.empty.record.PendingSettings = &v
				f.empty.save()
			case "domain":
				f.domain.fail = true
			case "nonempty":
				f.empty.record.Persisted = true
				f.empty.save()
			case "unknown":
				engine := f.sender.receipts
				entries, _ := os.ReadDir(engine.root)
				var status SubmissionStatus
				readPrivate(filepath.Join(engine.root, entries[0].Name()), &status)
				status.Phase = "Unknown"
				status.Witness = nil
				writePrivate(filepath.Join(engine.root, entries[0].Name()), status)
			case "generation":
				f.request.ExpectedGeneration = strings.Repeat("f", 64)
			}
			stop := errors.New("fixture checkpoint")
			f.hooks.recoveryCheckpoint = func(point string) error {
				if point == "team-preflight" {
					return stop
				}
				return nil
			}
			_, err := f.m.Upgrade(f.ctx, f.request, f.hooks)
			if fault == "" && !errors.Is(err, stop) || fault != "" && (err == nil || errors.Is(err, stop)) {
				t.Fatal(fault, err)
			}
			if f.host.stops != 0 || f.host.starts != 1 || f.publications != 0 {
				t.Fatal("preflight performed an effect")
			}
		})
	}
}
func TestTeamConflictRecoveryPreservesEmptyOwnerAndSameUUID(t *testing.T) {
	f := newTeamConflictFixture(t)
	history, _ := os.ReadFile(f.sender.record.HistoryFile)
	receipts, _ := os.ReadDir(f.sender.receipts.root)
	original, _ := os.ReadFile(filepath.Join(f.sender.receipts.root, receipts[0].Name()))
	result, err := f.m.Upgrade(f.ctx, f.request, f.hooks)
	if err != nil || result.Phase != "ready" || result.Disposition != "recovered-team-conflict" || result.TeamMessageID != f.request.RecoverTeamConflict.OperationID || f.host.stops != 1 {
		t.Fatal(result, err, f.host.stops)
	}
	again, err := f.m.Upgrade(f.ctx, f.request, f.hooks)
	if err != nil || again != result || f.host.stops != 1 || f.host.starts != 2 {
		t.Fatal("same UUID recovery", again, err)
	}
	current, _ := f.m.records(f.request.Start.Session)
	directory, _ := stateDirectory(f.m.config.UserStateRoot, f.request.Start.Session)
	for _, before := range []conversationRecord{f.sender.record, f.empty.record} {
		var got conversationRecord
		if err := readPrivate(filepath.Join(directory, "members", before.Conversation.Member, before.Conversation.Incarnation, "conversation.json"), &got); err != nil {
			t.Fatal(err)
		}
		expected := before
		if !before.Persisted {
			if got.HistoryFile == before.HistoryFile {
				t.Fatal("finite empty Load did not replan its locator")
			}
			expected.HistoryFile = got.HistoryFile
			if _, err := os.Lstat(got.HistoryFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("new planned locator materialized", err)
			}
		}
		if !sameUpgradeMember(expected, got) || got.HeldReason != "" || !got.Loaded || got.Generation != current.Requests[current.CurrentRequestID].Runtime.Generation {
			t.Fatal("retained owner changed", got)
		}
	}
	after, _ := os.ReadFile(f.sender.record.HistoryFile)
	saved, _ := os.ReadFile(filepath.Join(f.sender.receipts.root, receipts[0].Name()))
	if !bytes.Equal(history, after) || !bytes.Equal(original, saved) {
		t.Fatal("recovery rewrote history/receipt")
	}
	if _, err := os.Stat(f.empty.record.HistoryFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty history materialized", err)
	}
	if !reflect.DeepEqual(f.domain.slots[1].InitialInputID, "44444444-4444-4444-8444-444444444444") {
		t.Fatal("reserved original changed")
	}
}

func TestTeamConflictRecoveryCheckpoints(t *testing.T) {
	for _, point := range []string{"team-reservation", "team-prepared", "team-before-stop", "lost-stop-reply", "team-stop", "team-drain", "team-settled", "team-mapping:lead", "team-mapping:implementer0", "team-loaded:lead", "team-loaded:implementer0", "attachment", "publication", "team-publication"} {
		t.Run(point, func(t *testing.T) {
			f := newTeamConflictFixture(t)
			history, _ := os.ReadFile(f.sender.record.HistoryFile)
			entries, _ := os.ReadDir(f.sender.receipts.root)
			receiptPath := filepath.Join(f.sender.receipts.root, entries[0].Name())
			receipt, _ := os.ReadFile(receiptPath)
			interrupted := errors.New("fixture interruption")
			f.hooks.recoveryCheckpoint = func(at string) error {
				if at == point {
					return interrupted
				}
				return nil
			}
			originalAttach, originalPublish := f.hooks.EnsureAttachment, f.hooks.Publish
			if point == "lost-stop-reply" {
				f.host.stopReplyErr = interrupted
			}
			if point == "attachment" {
				f.hooks.EnsureAttachment = func(ctx context.Context, ref RuntimeRef, c HostCallbackConnector) error {
					if ref != f.source {
						return interrupted
					}
					return originalAttach(ctx, ref, c)
				}
			}
			if point == "publication" {
				f.hooks.Publish = func(context.Context, UpgradeProof) error { return interrupted }
			}
			result, err := f.m.Upgrade(f.ctx, f.request, f.hooks)
			if !errors.Is(err, interrupted) || result.Disposition != "" || result.TeamMessageID != "" {
				t.Fatal("missing original failure or premature completion", result, err)
			}
			if point == "team-reservation" || point == "team-prepared" || point == "team-before-stop" {
				if f.host.stops != 0 || f.sender.record.HeldReason == "" {
					t.Fatal("pre-stop effects")
				}
			}
			// A reserved operation cannot be replaced or changed, or bypassed as an ordinary upgrade.
			for _, change := range []string{"request", "selector", "package", "ordinary"} {
				changed := f.request
				selection := *changed.RecoverTeamConflict
				changed.RecoverTeamConflict = &selection
				switch change {
				case "request":
					changed.Start.RequestID = "66666666-6666-4666-8666-666666666666"
				case "selector":
					selection.OperationID = "77777777777747778777777777777777"
				case "package":
					changed.Start.AppStorePath += "-different"
				case "ordinary":
					changed.RecoverTeamConflict = nil
				}
				stops, starts := f.host.stops, f.host.starts
				if _, err := f.m.Upgrade(f.ctx, changed, f.hooks); err == nil || f.host.stops != stops || f.host.starts != starts {
					t.Fatal("changed operation admitted", change, err)
				}
			}
			f.host.stopReplyErr = nil
			f.hooks.recoveryCheckpoint = nil
			f.hooks.EnsureAttachment, f.hooks.Publish = originalAttach, originalPublish
			result, err = f.m.Upgrade(f.ctx, f.request, f.hooks)
			if err != nil || result.Disposition != "recovered-team-conflict" || f.host.stops != 1 || f.host.starts != 2 {
				t.Fatal("resume did not finish once", result, err, f.host.stops, f.host.starts)
			}
			savedHistory, _ := os.ReadFile(f.sender.record.HistoryFile)
			savedReceipt, _ := os.ReadFile(receiptPath)
			if !bytes.Equal(history, savedHistory) || !bytes.Equal(receipt, savedReceipt) {
				t.Fatal("rewrote original evidence")
			}
		})
	}
}

func TestTeamConflictRecoveryTargetContractBeforeReservation(t *testing.T) {
	for _, raw := range []string{`{}`, `{"piTeamConflictRecovery":null}`, `{"piTeamConflictRecovery":false}`, `{"piTeamConflictRecovery":"1"}`, `{"piTeamConflictRecovery":1.0}`, `{"piTeamConflictRecovery":2}`, `{"piTeamConflictRecovery":1}`} {
		t.Run(raw, func(t *testing.T) {
			f := newTeamConflictFixture(t)
			f.m.config.teamConflictRecoveryContract = func(string) (bool, error) { return teamConflictRecoveryContract([]byte(raw)) }
			stop := errors.New("proved preflight")
			f.hooks.recoveryCheckpoint = func(point string) error {
				if point == "team-preflight" {
					return stop
				}
				return nil
			}
			_, err := f.m.Upgrade(f.ctx, f.request, f.hooks)
			if raw == `{"piTeamConflictRecovery":1}` {
				if !errors.Is(err, stop) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			records, _ := f.m.records(f.source.Session)
			if records.CurrentRequestID != f.source.RequestID || f.host.stops != 0 || f.host.starts != 1 {
				t.Fatal("unsupported target mutated runtime")
			}
		})
	}
}
func TestTeamConflictRecoveryRetainedInventory(t *testing.T) {
	for _, point := range []string{"team-reservation", "team-prepared", "team-settled", "team-publication", "complete"} {
		t.Run(point, func(t *testing.T) {
			f := newTeamConflictFixture(t)
			stop := errors.New("inventory boundary")
			f.hooks.recoveryCheckpoint = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			_, err := f.m.Upgrade(f.ctx, f.request, f.hooks)
			if point == "complete" && err != nil || point != "complete" && !errors.Is(err, stop) {
				t.Fatal(err)
			}
			before := privateInventory(t, f.m.config.UserStateRoot)
			rows, err := InspectRetainedConversations(f.ctx, f.m.config.UserStateRoot, []string{f.source.Session.Workspace})
			if err != nil || len(rows) != 2 {
				t.Fatal(rows, err)
			}
			for _, row := range rows {
				if row.HasTeamConflictRecovery != (point != "team-reservation") {
					t.Fatal("journal dependency lost", point, row)
				}
			}
			if !reflect.DeepEqual(before, privateInventory(t, f.m.config.UserStateRoot)) {
				t.Fatal("inventory mutated retained state")
			}
			if point == "team-reservation" {
				return
			}
			directory, _ := stateDirectory(f.m.config.UserStateRoot, f.source.Session)
			path := teamConflictPath(directory, f.request.Start.RequestID)
			original, _ := os.ReadFile(path)
			for _, fault := range []string{"malformed", "orphan", "selector", "member"} {
				var j teamConflictJournal
				json.Unmarshal(original, &j)
				switch fault {
				case "malformed":
					os.WriteFile(path, []byte(`{"Schema":1,"unknown":true}`), 0600)
				case "orphan":
					j.Successor.RequestID = "77777777-7777-4777-8777-777777777777"
					writePrivate(path, j)
				case "selector":
					j.Selector.OperationID = "77777777777747778777777777777777"
					writePrivate(path, j)
				case "member":
					j.Members[0].Before = j.Members[1].Before
					j.Members[0].BeforeSHA256 = j.Members[1].BeforeSHA256
					writePrivate(path, j)
				}
				if _, err := InspectRetainedConversations(f.ctx, f.m.config.UserStateRoot, []string{f.source.Session.Workspace}); err == nil {
					t.Fatal("bad journal accepted", fault)
				}
				os.WriteFile(path, original, 0600)
			}
		})
	}
}

func TestTeamConflictRecoveryMarkedSourceEligibility(t *testing.T) {
	data, err := os.ReadFile("../session/runtime-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"missing", "null", "string", "version", "valid"} {
		t.Run(marker, func(t *testing.T) {
			f := newTeamConflictFixture(t)
			var contract map[string]json.RawMessage
			json.Unmarshal(data, &contract)
			switch marker {
			case "missing":
				delete(contract, "piTeamConflictRecovery")
			case "null":
				contract["piTeamConflictRecovery"] = json.RawMessage(`null`)
			case "string":
				contract["piTeamConflictRecovery"] = json.RawMessage(`"1"`)
			case "version":
				contract["piTeamConflictRecovery"] = json.RawMessage(`2`)
			}
			encoded, _ := json.Marshal(contract)
			f.m.config.teamConflictPredecessorContract = func(app string) error {
				return teamConflictPredecessorMetadata(app, "/nix/store/11111111111111111111111111111111-known-legacy", encoded)
			}
			stop := errors.New("source eligible")
			f.hooks.recoveryCheckpoint = func(point string) error {
				if point == "team-preflight" {
					return stop
				}
				return nil
			}
			_, err := f.m.Upgrade(f.ctx, f.request, f.hooks)
			if marker == "valid" {
				if !errors.Is(err, stop) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			if f.host.stops != 0 || f.host.starts != 1 {
				t.Fatal("source refusal had effects")
			}
		})
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(data, &fields)
	delete(fields, "piTeamConflictRecovery")
	unmarked, _ := json.Marshal(fields)
	legacy := "/nix/store/11111111111111111111111111111111-known-legacy"
	if err := teamConflictPredecessorMetadata(legacy, legacy, unmarked); err != nil {
		t.Fatal(err)
	}
	delete(fields, "workflowObservations")
	incomplete, _ := json.Marshal(fields)
	if err := teamConflictPredecessorMetadata(legacy, legacy, incomplete); err == nil {
		t.Fatal("legacy output bypassed complete read contract")
	}
}

func TestTeamConflictCompletedRetryPreservesLaterInputAndIndependentHold(t *testing.T) {
	for _, point := range []string{"complete", "team-publication"} {
		for _, owner := range []string{"persisted", "original-empty"} {
			t.Run(point+"/"+owner, func(t *testing.T) {
				f := newTeamConflictFixture(t)
				stop := errors.New("lost final response")
				f.hooks.recoveryCheckpoint = func(at string) error {
					if at == point {
						return stop
					}
					return nil
				}
				result, err := f.m.Upgrade(f.ctx, f.request, f.hooks)
				if point == "complete" && err != nil || point != "complete" && !errors.Is(err, stop) {
					t.Fatal(result, err)
				}
				f.hooks.recoveryCheckpoint = nil
				current := f.host.base.s
				ref := f.sender.record.Conversation
				if owner == "original-empty" {
					ref = f.empty.record.Conversation
					f.domain.slots[1].State = "ready"
				}
				member, err := current.member(ref)
				if err != nil {
					t.Fatal(err)
				}
				history, err := os.OpenFile(member.record.HistoryFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				input := SendInput{Message: "one legitimate later input", ActionContext: "pi-delivery-v1:steer"}
				parent := "anchor"
				if owner == "original-empty" {
					parent = ""
					if err := json.NewEncoder(history).Encode(map[string]any{"type": "session", "id": ref.NativeID, "cwd": filepath.Join(ref.Session.Workspace, "work", ref.Session.Slug)}); err != nil {
						t.Fatal(err)
					}
					member.record.Persisted = true
				}
				row := map[string]any{"type": "message", "id": "later-user", "parentId": parent, "message": map[string]any{"role": "user", "content": []any{map[string]string{"type": "text", "text": input.Message}}}}
				if err := json.NewEncoder(history).Encode(row); err != nil {
					t.Fatal(err)
				}
				if owner == "original-empty" {
					if err := json.NewEncoder(history).Encode(map[string]any{"type": "message", "id": "later-answer", "parentId": "later-user", "message": map[string]any{"role": "assistant", "content": []any{map[string]string{"type": "text", "text": "later input answered"}}}}); err != nil {
						t.Fatal(err)
					}
				}
				history.Close()
				if owner == "original-empty" {
					if captured, err := captureRecoveryHistory(member.record); err != nil || captured.Entries != 2 {
						t.Fatal("invalid later persisted empty-owner history", captured, err)
					}
				}
				submitted := SubmissionRef{Conversation: ref, OperationID: "99999999-9999-4999-8999-999999999999", Action: "send"}
				if owner == "original-empty" {
					submitted.OperationID = f.domain.slots[1].InitialInputID
				}
				submitted.Digest = SubmissionDigest(submitted, input, member.record.Settings)
				now := time.Now().UTC()
				status := SubmissionStatus{Schema: 1, Ref: submitted, Phase: "Accepted", Generation: current.ref.Generation, Settings: member.record.Settings, Input: input, Witness: &NativeWitness{SessionID: ref.NativeID, EntryID: "later-user", ParentID: parent, EffectiveDigest: MessageDigest(input.Message)}, CreatedAt: now, UpdatedAt: now}
				receiptPath := filepath.Join(member.receipts.root, submitted.OperationID+".json")
				if err := writePrivate(receiptPath, status); err != nil {
					t.Fatal(err)
				}
				member.record.HeldReason = "new independent callback hold"
				member.worker.(*teamConflictWorker).record = member.record
				if err := member.save(); err != nil {
					t.Fatal(err)
				}
				before := privateInventory(t, f.m.config.UserStateRoot)
				workerCalls := func() map[string]map[string]int {
					calls := map[string]map[string]int{}
					for _, c := range current.members {
						calls[c.record.Conversation.Member] = map[string]int{}
						for op, n := range c.worker.(*teamConflictWorker).calls {
							calls[c.record.Conversation.Member][op] = n
						}
					}
					return calls
				}
				calls := workerCalls()
				f.hooks.EnsureAttachment = func(context.Context, RuntimeRef, HostCallbackConnector) error {
					t.Error("completed retry attached/loaded again")
					return ErrScope
				}
				f.hooks.Publish = func(context.Context, UpgradeProof) error { t.Error("completed retry republished"); return ErrScope }
				replay, err := f.m.Upgrade(f.ctx, f.request, f.hooks)
				if err != nil || replay.Disposition != "recovered-team-conflict" || replay.TeamMessageID != f.request.RecoverTeamConflict.OperationID || f.host.stops != 1 || f.host.starts != 2 {
					t.Fatal(replay, err)
				}
				after := privateInventory(t, f.m.config.UserStateRoot)
				directory, _ := stateDirectory(f.m.config.UserStateRoot, ref.Session)
				// Only completing the previously settled journal is allowed after a lost reply.
				delete(before, teamConflictPath(directory, f.request.Start.RequestID))
				delete(after, teamConflictPath(directory, f.request.Start.RequestID))
				if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(calls, workerCalls()) {
					t.Fatal("replay changed later input/hold or called worker")
				}
				f.domain.pubErr = ErrUnavailable
				if _, err := f.m.Upgrade(f.ctx, f.request, f.hooks); !errors.Is(err, ErrUnavailable) {
					t.Fatal("publication read error resumed effects", err)
				}
				f.domain.pubErr = nil
				if owner == "original-empty" {
					member.record.HistoryRef = strings.Repeat("f", 64)
					if err := member.save(); err != nil {
						t.Fatal(err)
					}
				} else {
					raw, err := os.ReadFile(member.record.HistoryFile)
					if err != nil {
						t.Fatal(err)
					}
					raw[0] = ' '
					if err := os.WriteFile(member.record.HistoryFile, raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.m.Upgrade(f.ctx, f.request, f.hooks); !errors.Is(err, ErrConflict) {
					t.Fatal("changed original prefix or immutable history reference accepted", err)
				}

			})
		}
	}
}

func TestTeamConflictOldProtocolHistoryAndTargetRefusals(t *testing.T) {
	for _, fault := range []string{"target", "body", "ambiguous-history", "unfinished-tool", "utility-callback"} {
		t.Run(fault, func(t *testing.T) {
			f := newTeamConflictFixture(t)
			switch fault {
			case "target":
				f.domain.target = f.sender.record.Conversation
			case "body":
				f.worker.arguments = json.RawMessage(`{"message_id":"55555555-5555-4555-8555-555555555555","to":"implementer0","message":"changed body"}`)
			default:
				id := "another-anchor"
				name := "bash"
				if fault == "ambiguous-history" {
					id = "anchor"
				}
				if fault == "utility-callback" {
					name = "monitor_operation"
				}
				file, err := os.OpenFile(f.sender.record.HistoryFile, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				json.NewEncoder(file).Encode(map[string]any{"type": "message", "id": id, "parentId": "anchor", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "id": "unsettled-call", "name": name, "arguments": map[string]string{"command": "true"}}}}})
				file.Close()
			}
			if _, err := f.m.Upgrade(f.ctx, f.request, f.hooks); err == nil || f.host.stops != 0 || f.host.starts != 1 {
				t.Fatal("unproved callback had effects", fault, err)
			}
		})
	}
}

func TestTeamConflictRejectsPersistedCreatingOwnerBeforeReservation(t *testing.T) {
	f := newTeamConflictFixture(t)
	record := f.empty.record
	record.Persisted = true
	input := SendInput{Message: "original admitted initial assignment"}
	var history bytes.Buffer
	for _, row := range []any{
		map[string]any{"type": "session", "id": record.Conversation.NativeID, "cwd": filepath.Join(record.Conversation.Session.Workspace, "work", record.Conversation.Session.Slug)},
		map[string]any{"type": "message", "id": "creating-user", "parentId": "", "message": map[string]any{"role": "user", "content": []any{map[string]string{"type": "text", "text": input.Message}}}},
		map[string]any{"type": "message", "id": "creating-answer", "parentId": "creating-user", "message": map[string]any{"role": "assistant", "content": []any{map[string]string{"type": "text", "text": "initial assignment answered"}}}},
	} {
		if err := json.NewEncoder(&history).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(record.HistoryFile, history.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	f.empty.record = record
	f.empty.worker.(*teamConflictWorker).record = record
	if err := f.empty.save(); err != nil {
		t.Fatal(err)
	}
	ref := SubmissionRef{Conversation: record.Conversation, OperationID: f.domain.slots[1].InitialInputID, Action: "send"}
	ref.Digest = SubmissionDigest(ref, input, record.Settings)
	now := time.Now().UTC()
	accepted := SubmissionStatus{Schema: 1, Ref: ref, Phase: "Accepted", Generation: f.source.Generation, Settings: record.Settings, Input: input, Witness: &NativeWitness{SessionID: record.Conversation.NativeID, EntryID: "creating-user", EffectiveDigest: MessageDigest(input.Message)}, CreatedAt: now, UpdatedAt: now}
	if err := writePrivate(filepath.Join(f.empty.receipts.root, ref.OperationID+".json"), accepted); err != nil {
		t.Fatal(err)
	}
	// Establish a genuine complete persisted history and its exact Accepted initial
	// receipt. The roster intentionally still says creating; no malformed-bit shortcut.
	if captured, err := captureRecoveryHistory(record); err != nil || captured.Entries != 2 {
		t.Fatal(captured, err)
	}
	proof, err := f.m.VerifyCreation(f.ctx, agentbackend.ExecutionHandle{Conversation: record.Conversation, Generation: f.source.Generation}, record.CreationID, record.HistoryRef, &ref)
	if err != nil || !proof.Persisted || proof.Bootstrap == nil || proof.Bootstrap.Ref != ref {
		t.Fatal("invalid persisted initial fixture", proof, err)
	}
	before := privateInventory(t, f.m.config.UserStateRoot)
	records, err := f.m.records(f.source.Session)
	if err != nil {
		t.Fatal(err)
	}
	reached := errors.New("persisted creating owner passed preflight")
	f.hooks.recoveryCheckpoint = func(point string) error {
		if point == "team-preflight" {
			return reached
		}
		return nil
	}
	_, err = f.m.Upgrade(f.ctx, f.request, f.hooks)
	if !errors.Is(err, ErrHeld) {
		t.Errorf("persisted creating owner was not refused before reservation: %v", err)
	}
	after, readErr := f.m.records(f.source.Session)
	if readErr != nil || !reflect.DeepEqual(records, after) || !reflect.DeepEqual(before, privateInventory(t, f.m.config.UserStateRoot)) || f.host.stops != 0 || f.host.starts != 1 || f.publications != 0 {
		t.Fatal("refusal changed runtime, mapping, history, receipt or host effects", after, readErr, f.host.stops, f.host.starts, f.publications)
	}
}

func teamConflictLargeCaptureFixture(t *testing.T) (*teamConflictTestFixture, teamConflictJournal) {
	t.Helper()
	f := newTeamConflictFixture(t)
	// Each field and the history entry remain within the existing reader bounds;
	// repeating exact supported calls makes only the aggregate journal too large.
	anchor := strings.Repeat("a", 4096)
	blocks := make([]any, 128)
	for i := range blocks {
		blocks[i] = map[string]any{"type": "toolCall", "id": fmt.Sprintf("%04x", i) + strings.Repeat("b", 4092), "name": "team_message", "arguments": f.worker.arguments}
	}
	var history bytes.Buffer
	for _, row := range []any{
		map[string]any{"type": "session", "id": f.sender.record.Conversation.NativeID, "cwd": filepath.Join(f.source.Session.Workspace, "work", f.source.Session.Slug)},
		map[string]any{"type": "message", "id": "user-entry", "parentId": "", "message": map[string]any{"role": "user", "content": []any{map[string]string{"type": "text", "text": "one original input"}}}},
		map[string]any{"type": "message", "id": anchor, "parentId": "user-entry", "message": map[string]any{"role": "assistant", "content": blocks}},
	} {
		if err := json.NewEncoder(&history).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if history.Len() >= recordLimit {
		t.Fatal("individual history entry exceeded its limit")
	}
	if err := os.WriteFile(f.sender.record.HistoryFile, history.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	capture, err := f.m.captureTeamConflict(f.ctx, f.request, f.source, f.hooks, true, nil)
	if err != nil {
		t.Fatal("invalid aggregate-size fixture", err)
	}
	return f, capture
}
func TestTeamConflictOversizedCaptureRefusesBeforeReservation(t *testing.T) {
	f, capture := teamConflictLargeCaptureFixture(t)
	encoded, err := json.Marshal(capture)
	if err != nil || len(encoded)+1 <= recordLimit {
		t.Fatal("fixture did not exceed aggregate codec", len(encoded), err)
	}
	before := privateInventory(t, f.m.config.UserStateRoot)
	records, err := f.m.records(f.source.Session)
	if err != nil {
		t.Fatal(err)
	}
	retentions := 0
	f.m.config.retainPackage = func(context.Context, string, string) error { retentions++; return nil }
	_, err = f.m.Upgrade(f.ctx, f.request, f.hooks)
	if err == nil {
		t.Error("oversized recovery admitted")
	}
	after, readErr := f.m.records(f.source.Session)
	if readErr != nil || !reflect.DeepEqual(records, after) || !reflect.DeepEqual(before, privateInventory(t, f.m.config.UserStateRoot)) || retentions != 0 || f.host.stops != 0 || f.host.starts != 1 || f.publications != 0 {
		t.Errorf("oversized preflight changed current runtime/retention/data/effects: current=%s retentions=%d stops=%d starts=%d publications=%d err=%v", after.CurrentRequestID, retentions, f.host.stops, f.host.starts, f.publications, readErr)
	}
}

func TestTeamConflictJournalSizeIncludesNormalizedTerminalAndExactCandidate(t *testing.T) {
	f, j := teamConflictLargeCaptureFixture(t)
	identity := teamConflictHostIdentity(f.host.base.observation)
	j.SourceHost = &identity
	candidate, err := f.m.reserveRuntimeRecord(f.request.Start, f.source.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	for teamConflictJournalSize(j, candidate.Runtime) != nil {
		if len(j.Calls) <= 1 {
			t.Fatal("could not construct boundary capture")
		}
		j.Calls = j.Calls[:len(j.Calls)-1]
		j.Domains = j.Domains[:len(j.Domains)-1]
	}
	records, err := f.m.records(f.source.Session)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := readPrivateBytes(filepath.Join(f.m.instance(f.source), "launch.json"), 65536)
	if err != nil {
		t.Fatal(err)
	}
	var admitted runtimeRecord
	actual, err := f.m.reserveSuccessorRecord(f.request.Start, records, records.Requests[f.source.RequestID], descriptor, func(r runtimeRecord) error { admitted = r; return teamConflictJournalSize(j, r.Runtime) })
	if err != nil || actual.Runtime != admitted.Runtime {
		t.Fatal("admitted candidate was replaced", actual, admitted, err)
	}
	terminal := identity
	terminal.Phase, terminal.Outcome, terminal.Result = "terminal", "stopped", "stopped"
	terminal.LoadState, terminal.ActiveState, terminal.Job = "loaded", "inactive", "0"
	terminal.DrainProof = map[string]any{"empty": true, "boot_id": terminal.BootID, "invocation_id": terminal.InvocationID, "control_group": terminal.ControlGroup, "load_state": "loaded", "active_state": "inactive", "job": "0", "unused": strings.Repeat("z", 128<<10)}
	if !validTerminalReceipt(records.Requests[f.source.RequestID], terminal) {
		t.Fatal("invalid terminal boundary fixture")
	}
	normalized := teamConflictStopEvidence(terminal)
	if _, ok := normalized.DrainProof["unused"]; ok || !validTerminalReceipt(records.Requests[f.source.RequestID], normalized) {
		t.Fatal("normalization changed proof")
	}
	j.Successor = actual.Runtime
	j.Phase = "complete"
	j.Stop = &normalized
	encoded, err := json.Marshal(j)
	if err != nil || len(encoded)+1 > recordLimit || len(encoded)+1 < recordLimit-12*1024 {
		t.Fatal("terminal headroom boundary", len(encoded), err)
	}
	j.Stop = &terminal
	raw, err := json.Marshal(j)
	if err != nil || len(raw)+1 <= recordLimit {
		t.Fatal("unused extension did not exercise bound", len(raw), err)
	}
	for _, field := range []string{"boot", "invocation", "cgroup"} {
		changed := normalized
		changed.DrainProof = map[string]any{}
		for key, value := range normalized.DrainProof {
			changed.DrainProof[key] = value
		}
		switch field {
		case "boot":
			changed.BootID = "different"
			changed.DrainProof["boot_id"] = "different"
		case "invocation":
			changed.InvocationID = "different"
			changed.DrainProof["invocation_id"] = "different"
		case "cgroup":
			changed.ControlGroup = "/different"
			changed.DrainProof["control_group"] = "/different"
		}
		if sameTeamConflictHost(identity, changed) {
			t.Fatal("required identity drift admitted", field)
		}
	}
	delete(terminal.DrainProof, "invocation_id")
	absent := teamConflictStopEvidence(terminal)
	if _, ok := absent.DrainProof["invocation_id"]; ok || !validTerminalReceipt(records.Requests[f.source.RequestID], absent) {
		t.Fatal("optional invocation evidence was manufactured")
	}
	delete(terminal.DrainProof, "boot_id")
	if validTerminalReceipt(records.Requests[f.source.RequestID], teamConflictStopEvidence(terminal)) {
		t.Fatal("missing required drain proof admitted")
	}
}
func TestTeamConflictSavedJournalRequiresSourceHost(t *testing.T) {
	for _, phase := range []string{"prepared", "settled", "complete"} {
		for _, invalid := range []string{"missing", "null", "missing-identity", "not-identity-only"} {
			t.Run(phase+"/"+invalid, func(t *testing.T) {
				f := newTeamConflictFixture(t)
				interrupted := errors.New("saved journal checkpoint")
				f.hooks.recoveryCheckpoint = func(at string) error {
					if phase != "complete" && at == "team-"+phase {
						return interrupted
					}
					return nil
				}
				_, err := f.m.Upgrade(f.ctx, f.request, f.hooks)
				if phase == "complete" && err != nil || phase != "complete" && !errors.Is(err, interrupted) {
					t.Fatal("journal setup", err)
				}
				directory, _ := stateDirectory(f.m.config.UserStateRoot, f.source.Session)
				path := teamConflictPath(directory, f.request.Start.RequestID)
				var raw map[string]json.RawMessage
				if err := readPrivate(path, &raw); err != nil {
					t.Fatal(err)
				}
				switch invalid {
				case "missing":
					delete(raw, "SourceHost")
				case "null":
					raw["SourceHost"] = json.RawMessage("null")
				default:
					var identity HostObservation
					if err := json.Unmarshal(raw["SourceHost"], &identity); err != nil {
						t.Fatal(err)
					}
					if invalid == "missing-identity" {
						identity.InvocationID = ""
					} else {
						identity.ObservedAt = "unexpected nonidentity evidence"
					}
					raw["SourceHost"], _ = json.Marshal(identity)
				}
				if err := writePrivate(path, raw); err != nil {
					t.Fatal(err)
				}
				before := privateInventory(t, f.m.config.UserStateRoot)
				stops, starts, publications := f.host.stops, f.host.starts, f.publications
				f.hooks.recoveryCheckpoint = nil
				if _, err := f.m.Upgrade(f.ctx, f.request, f.hooks); err == nil {
					t.Fatal("invalid saved host identity was adopted")
				}
				if _, err := retainedTeamConflictMembers(f.ctx, f.m.config.UserStateRoot, directory, f.source.Session); err == nil {
					t.Fatal("inventory accepted invalid saved host identity")
				}
				if !reflect.DeepEqual(before, privateInventory(t, f.m.config.UserStateRoot)) || f.host.stops != stops || f.host.starts != starts || f.publications != publications {
					t.Fatal("invalid journal refusal changed data or effects")
				}
			})
		}
	}
}

func TestTeamConflictReplannedEmptyAfterimageRefusals(t *testing.T) {
	for _, fault := range []string{"persisted-file", "native-id", "creation-id", "history-ref", "foreign-dir", "noncanonical", "settings", "policy", "pending", "held", "persisted", "input", "old-file", "new-file", "reservation"} {
		t.Run(fault, func(t *testing.T) {
			f := newTeamConflictFixture(t)
			interrupted := errors.New("after empty Load before publication")
			f.hooks.recoveryCheckpoint = func(point string) error {
				if point == "team-loaded:implementer0" {
					return interrupted
				}
				return nil
			}
			if _, err := f.m.Upgrade(f.ctx, f.request, f.hooks); !errors.Is(err, interrupted) {
				t.Fatal(err)
			}
			directory, _ := stateDirectory(f.m.config.UserStateRoot, f.source.Session)
			j, err := readTeamConflictJournal(directory, f.request.Start.RequestID)
			if err != nil || j.Phase != "settled" {
				t.Fatal(j, err)
			}
			ref := f.empty.record.Conversation
			if fault == "persisted-file" {
				ref = f.sender.record.Conversation
			}
			root := filepath.Join(directory, "members", ref.Member, ref.Incarnation)
			path := filepath.Join(root, "conversation.json")
			var actual conversationRecord
			if err := readPrivate(path, &actual); err != nil {
				t.Fatal(err)
			}
			if fault != "persisted-file" && actual.HistoryFile == f.empty.record.HistoryFile {
				t.Fatal("not a replanned afterimage")
			}
			switch fault {
			case "persisted-file":
				actual.HistoryFile = filepath.Join(root, "history", "different-persisted.jsonl")
			case "native-id":
				actual.Conversation.NativeID = "88888888-8888-4888-8888-888888888888"
			case "creation-id":
				actual.CreationID = strings.Repeat("9", 64)
			case "history-ref":
				actual.HistoryRef = strings.Repeat("9", 64)
			case "foreign-dir":
				actual.HistoryFile = filepath.Join(directory, "foreign.jsonl")
			case "noncanonical":
				actual.HistoryFile = root + "/history/../history/new.jsonl"
			case "settings":
				actual.Settings.Model = "other"
			case "policy":
				p := policyTarget("plan", 2, []string{}, []string{})
				actual.ToolPolicy = &p
			case "pending":
				p := actual.Settings
				actual.PendingSettings = &p
			case "held":
				actual.HeldReason = "unexpected independent hold before publication"
			case "persisted":
				actual.Persisted = true
			case "input":
				input := SendInput{Message: "unexpected input"}
				submitted := SubmissionRef{Conversation: ref, OperationID: "99999999-9999-4999-8999-999999999999", Action: "send"}
				submitted.Digest = SubmissionDigest(submitted, input, actual.Settings)
				now := time.Now().UTC()
				status := SubmissionStatus{Schema: 1, Ref: submitted, Phase: "NotSubmitted", Generation: actual.Generation, Settings: actual.Settings, Input: input, Refusal: "unexpected admitted input", CreatedAt: now, UpdatedAt: now}
				if err := writePrivate(filepath.Join(root, "submissions", submitted.OperationID+".json"), status); err != nil {
					t.Fatal(err)
				}
			case "old-file", "new-file":
				file := actual.HistoryFile
				if fault == "old-file" {
					file = f.empty.record.HistoryFile
				}
				if err := os.WriteFile(file, []byte("unexpected history"), 0600); err != nil {
					t.Fatal(err)
				}
			case "reservation":
				f.domain.slots[1].InitialInputID = "99999999-9999-4999-8999-999999999999"
			}
			if err := writePrivate(path, actual); err != nil {
				t.Fatal(err)
			}
			f.hooks.recoveryCheckpoint = nil
			before := privateInventory(t, f.m.config.UserStateRoot)
			if _, err := f.m.Upgrade(f.ctx, f.request, f.hooks); err == nil {
				t.Fatal("invalid afterimage accepted", fault)
			}
			if f.publications != 0 || f.host.stops != 1 || f.host.starts != 2 || !reflect.DeepEqual(before, privateInventory(t, f.m.config.UserStateRoot)) {
				t.Fatal("refused afterimage changed data or repeated effects", fault, f.publications, f.host.stops, f.host.starts)
			}
		})
	}
}
