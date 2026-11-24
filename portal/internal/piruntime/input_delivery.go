package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

const inputDeliveryPrefix = "pi-delivery-v1:"

type liveInputReceipt struct {
	ref    receiptReference
	worker worker
}

func deliveryContext(mode string) (string, error) {
	if mode != "steer" && mode != "followUp" {
		return "", ErrScope
	}
	return inputDeliveryPrefix + mode, nil
}

// Only a positive, same-process enqueue acknowledgement may project Pending.
// Persisted dispatching alone remains Unknown after every process replacement.
func (e *receiptEngine) projectInput(record ownedReceipt) ownedReceipt {
	if record.Phase == "dispatching" {
		if live := e.liveInputs[record.ref.id()]; live != nil && live.ref.equal(record.ref) {
			record.Phase = "Pending"
		} else {
			record.Phase = "Unknown"
		}
	} else if record.Phase == "prepared" {
		record.Phase = "Unknown"
	}
	return record
}

func (s *supervisor) inputDelivery(ctx context.Context, handle agentbackend.ExecutionHandle, request backend.SendRequest) (SubmissionStatus, error) {
	c, err := s.member(handle.Conversation)
	if err != nil {
		return SubmissionStatus{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return s.inputDeliveryLocked(ctx, c, handle, request, nil)
}

// Caller owns c.mu throughout the frozen selection check and native enqueue.
// expected is set only by the new checked port; legacy retry bytes/semantics
// remain unchanged, including lookup of the original settings after a change.
func (s *supervisor) inputDeliveryLocked(ctx context.Context, c *conversation, handle agentbackend.ExecutionHandle, request backend.SendRequest, expected *SubmissionRef) (SubmissionStatus, error) {
	action, err := deliveryContext(request.Delivery)
	if err != nil || request.ActionContext != "" && request.ActionContext != action {
		return SubmissionStatus{}, ErrScope
	}
	e := c.receipts
	e.mu.Lock()
	old, loadErr := e.loadOwned(request.ClientUserMessageID)
	if loadErr == nil {
		if old.ref.kind() != "retained" || old.ref.retained.Conversation != handle.Conversation || old.Input.Message != request.Message || old.Input.ActionContext != action || expected != nil && *old.ref.retained != *expected {
			e.mu.Unlock()
			return SubmissionStatus{}, ErrConflict
		}
		result := e.projectInput(old).retained()
		e.mu.Unlock()
		return result, nil
	}
	if !errors.Is(loadErr, os.ErrNotExist) {
		e.mu.Unlock()
		return SubmissionStatus{}, loadErr
	}
	if request.Retry {
		e.mu.Unlock()
		return SubmissionStatus{Phase: "Unknown"}, nil
	}
	if err = s.checkInputAdmission(ctx, c); err != nil {
		e.mu.Unlock()
		return SubmissionStatus{}, err
	}

	// Other unknown operations fence admission, while positively owned native
	// pending inputs allow a later steer to pass an earlier follow-up.
	entries, err := os.ReadDir(e.root)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil || len(entries) > 4096 {
		e.mu.Unlock()
		return SubmissionStatus{}, ErrHeld
	}
	recoveries, recoveryErr := e.inputRecoveries()
	if recoveryErr != nil {
		e.mu.Unlock()
		return SubmissionStatus{}, recoveryErr
	}
	for _, entry := range entries {
		if entry.IsDir() || len(entry.Name()) < 6 || entry.Name()[len(entry.Name())-5:] != ".json" {
			e.mu.Unlock()
			return SubmissionStatus{}, ErrHeld
		}
		record, err := e.loadOwned(entry.Name()[:len(entry.Name())-5])
		if err != nil || record.Phase != "Accepted" && record.Phase != "NotSubmitted" && e.projectInput(record).Phase != "Pending" && !retiredInput(record, recoveries) {
			e.mu.Unlock()
			return SubmissionStatus{}, ErrHeld
		}
	}
	input := SendInput{Message: request.Message, ActionContext: action, Retry: false}
	ref := SubmissionRef{Conversation: handle.Conversation, OperationID: request.ClientUserMessageID, Action: "send"}
	ref.Digest = SubmissionDigest(ref, input, c.record.Settings)
	if expected != nil && ref != *expected {
		e.mu.Unlock()
		return SubmissionStatus{}, ErrConflict
	}
	reference := receiptReference{retained: &ref}
	if !reference.valid(input, c.record.Settings) {
		e.mu.Unlock()
		return SubmissionStatus{}, ErrScope
	}
	now := time.Now().UTC()
	record := ownedReceipt{reference, receiptData{Phase: "prepared", Generation: s.ref.Generation, Settings: c.record.Settings, Input: input, CreatedAt: now, UpdatedAt: now}}
	if len(e.liveInputs) >= 20 {
		record.Phase = "NotSubmitted"
		record.Refusal = "native input capacity reached"
		err = e.saveOwned(record)
		e.mu.Unlock()
		return record.retained(), err
	}
	if err = e.saveOwned(record); err == nil {
		err = e.point("intent")
	}
	if err == nil {
		record.Phase = "dispatching"
		err = e.saveOwned(record)
	}
	if err == nil {
		err = e.point("dispatch")
	}
	if err != nil {
		e.mu.Unlock()
		return record.retained(), err
	}
	w := c.worker
	var ack nativeResult
	err = w.Call(ctx, "input_enqueue_v1", map[string]string{"message": input.Message, "inputOperationID": ref.OperationID, "delivery": request.Delivery}, &ack)
	if err == nil && ack.Outcome == "Pending" {
		if e.liveInputs == nil {
			e.liveInputs = map[string]*liveInputReceipt{}
		}
		live := &liveInputReceipt{reference, w}
		e.liveInputs[ref.OperationID] = live
		e.mu.Unlock()
		s.inputTasks.Add(1)
		go func() { defer s.inputTasks.Done(); s.finishInput(c, live) }()
		record.Phase = "Pending"
		return record.retained(), nil
	}
	record.Phase = "Unknown"
	if err == nil && ack.Outcome == "NotSubmitted" && ack.Refusal != "" {
		record.Phase = "NotSubmitted"
		record.Refusal = ack.Refusal
	}
	record.UpdatedAt = time.Now().UTC()
	saveErr := e.saveOwned(record)
	e.mu.Unlock()
	if record.Phase == "Unknown" || saveErr != nil {
		s.holdInput(c, w)
	}
	return record.retained(), errors.Join(err, saveErr)
}

func (s *supervisor) holdInput(c *conversation, w worker) {
	c.activeMu.Lock()
	c.callbackUnknown = true
	c.activeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = w.Call(ctx, "input_hold_v1", struct{}{}, nil)
}

func (s *supervisor) finishInput(c *conversation, live *liveInputReceipt) {
	// Receipt persistence may outlive the original Pending response or precede a
	// failed native commit. Wake connected readers after every terminal path.
	defer s.workflowChanged()
	// The worker settles this operation on consumption, hold, abort or EOF. No
	// admission, conversation or receipt mutex is held while awaiting it.
	var result nativeResult
	waitCtx := s.inputContext
	if waitCtx == nil {
		waitCtx = context.Background()
	}
	err := live.worker.Call(waitCtx, "input_wait_v1", map[string]string{"inputOperationID": live.ref.id()}, &result)
	e := c.receipts
	e.mu.Lock()
	if e.liveInputs[live.ref.id()] != live {
		e.mu.Unlock()
		return
	}
	record, loadErr := e.loadOwned(live.ref.id())
	if loadErr != nil || !record.ref.equal(live.ref) || record.Phase != "dispatching" {
		delete(e.liveInputs, live.ref.id())
		e.mu.Unlock()
		s.holdInput(c, live.worker)
		return
	}
	record.Phase = "Unknown"
	if err == nil && e.point("native") == nil && result.Outcome == "Accepted" && result.Witness != nil {
		w := result.Witness
		if w.SessionID == live.ref.nativeID() && w.EntryID != "" && w.EffectiveMessage == record.Input.Message {
			record.Phase = "Accepted"
			record.Witness = &NativeWitness{SessionID: w.SessionID, EntryID: w.EntryID, ParentID: w.ParentID, PreviousLeafID: w.PreviousLeafID, EffectiveDigest: MessageDigest(w.EffectiveMessage)}
		}
	}
	record.UpdatedAt = time.Now().UTC()
	saveErr := e.point("witness")
	if saveErr == nil {
		saveErr = e.saveOwned(record)
	}
	if saveErr == nil {
		saveErr = e.point("receipt")
	}
	delete(e.liveInputs, live.ref.id())
	e.mu.Unlock()
	if saveErr != nil || record.Phase != "Accepted" {
		s.holdInput(c, live.worker)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var committed struct {
		Committed bool `json:"committed"`
	}
	if err := live.worker.Call(ctx, "input_commit_v1", map[string]string{"inputOperationID": live.ref.id()}, &committed); err != nil || !committed.Committed {
		s.holdInput(c, live.worker)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.worker == live.worker {
		if _, err := s.observeState(ctx, c); err != nil {
			s.holdInput(c, live.worker)
		}
	}
}

// InputDelivery is selected from the retained CURRENT runtime package, never
// from the newly installed portal that happens to perform this read.
func (m *Manager) InputDelivery(ctx context.Context, handle agentbackend.ExecutionHandle) (bool, error) {
	if err := validateHandle(handle); err != nil {
		return false, err
	}
	record, err := m.record(handle.Conversation.Session)
	if err != nil {
		return false, err
	}
	if record.Runtime.Generation != handle.Generation {
		return false, ErrScope
	}
	supported, err := m.config.inputDeliveryContract(record.Start.AppStorePath)
	if err != nil || !supported {
		return supported, err
	}
	if proof, light, err := m.readConversationIdentity(ctx, handle); err != nil || light {
		return err == nil && proof.InputDelivery == 1 && !proof.Held, err
	}
	var capabilities struct {
		Version int `json:"version"`
	}
	if err := m.handleCall(ctx, handle, "input_capabilities_v1", conversationRequest{}, &capabilities); err != nil {
		return false, err
	}
	if capabilities.Version != 1 {
		return false, ErrUnavailable
	}
	return true, nil
}

func readInputDeliveryContract(app string) (bool, error) {
	if !storePattern.MatchString(app) {
		return false, ErrScope
	}
	file, err := os.Open(filepath.Join(app, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return false, err
	}
	return decodeInputDeliveryContract(data)
}
func decodeInputDeliveryContract(data []byte) (bool, error) {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields, 1<<20); err != nil || fields == nil {
		return false, ErrScope
	}
	raw, ok := fields["piInputDelivery"]
	if !ok {
		return false, nil
	}
	var version int
	if err := strictDecode(raw, &version, 32); err != nil || version != 1 {
		return false, ErrScope
	}
	return true, nil
}

// InputDeliveryPackage inspects only the frozen immutable package contract.
// It does not grant execution authority or inspect a selected mutable profile.
func InputDeliveryPackage(app string) (bool, error) { return readInputDeliveryContract(app) }
