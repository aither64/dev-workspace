package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type QueuedInputRequest struct {
	OriginalRuntime RuntimeRef
	Selection       InputSelection
	Ref             SubmissionRef
	Input           SendInput
	Delivery        string
}

type QueuedInputResult struct {
	Status SubmissionStatus
	// NotSubmitted proves a refusal before the native delivery path was called.
	// Errors and post-dispatch ambiguity never establish this separate outcome.
	NotSubmitted bool
	Refusal      string
}

// Retirement preserves the original Unknown receipt. It only proves that the
// existing explicit owner recovery permits a distinct later input.
type QueuedInputLookupResult struct {
	Status         SubmissionStatus
	RetiredUnknown bool
}

func (result *QueuedInputLookupResult) UnmarshalJSON(data []byte) error {
	type plain QueuedInputLookupResult
	var value plain
	if err := strictDecode(data, &value, recordLimit); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 2 {
		return ErrScope
	}
	if value.RetiredUnknown && (value.Status.Phase != "Unknown" || value.Status.Witness != nil || value.Status.Refusal != "" || validateQueuedRef(value.Status.Ref) != nil || validQueuedStatus(value.Status, value.Status.Ref) != nil) {
		return ErrScope
	}
	*result = QueuedInputLookupResult(value)
	return nil
}

func (result *QueuedInputResult) UnmarshalJSON(data []byte) error {
	type plain QueuedInputResult
	var value plain
	if err := strictDecode(data, &value, recordLimit); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 3 {
		return ErrScope
	}
	*result = QueuedInputResult(value)
	return nil
}

// QueuedInputController is optional. Legacy consumers retain their native
// acceptance contract; a missing port cannot fall back to an ordinary Send.
type QueuedInputController interface {
	ReadInputSelection(context.Context, agentbackend.ExecutionHandle) (InputSelection, error)
	ValidateInputRuntimeContinuation(context.Context, RuntimeRef, RuntimeRef) error
	DispatchQueuedInput(context.Context, QueuedInputRequest) (QueuedInputResult, error)
	LookupQueuedInput(context.Context, SubmissionRef) (QueuedInputLookupResult, error)
}

// ValidateInputRuntimeContinuation proves only full-ref storage ancestry to
// exact CURRENT. Readiness, logical selection and effect admission are separate.
func (m *Manager) ValidateInputRuntimeContinuation(ctx context.Context, original, current RuntimeRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if original.Session != current.Session {
		return ErrScope
	}
	records, err := m.records(current.Session)
	if err != nil {
		return err
	}
	if records.Requests[records.CurrentRequestID].Runtime != current || records.Requests[original.RequestID].Runtime != original {
		return ErrScope
	}
	for id := records.CurrentRequestID; id != ""; id = records.Requests[id].Start.ReplacesRequestID {
		if err := ctx.Err(); err != nil {
			return err
		}
		if records.Requests[id].Runtime == original {
			return nil
		}
	}
	return ErrScope
}

func (s *supervisor) validateInputRuntimeContinuation(ctx context.Context, original RuntimeRef) error {
	if original.Session != s.ref.Session || s.verifiedInstanceDirectory == "" {
		return ErrUnavailable
	}
	m := Manager{config: Config{UserStateRoot: s.config.UserStateRoot, HostStateRoot: filepath.Dir(filepath.Dir(s.verifiedInstanceDirectory)), AuthDirectory: s.config.AuthDirectory, AccountBindings: s.config.AccountBindings}}
	if m.instance(s.ref) != s.verifiedInstanceDirectory {
		return ErrScope
	}
	if original != s.ref {
		// Existing successor startup proof joins the actual native owner to its
		// complete predecessor records before any new checked input can run.
		if _, err := s.workflowRuntimeHistory(); err != nil {
			return err
		}
	}
	return m.ValidateInputRuntimeContinuation(ctx, original, s.ref)
}

func validateQueuedInput(request QueuedInputRequest) error {
	selection, ref, input := request.Selection, request.Ref, request.Input
	if validateHandle(selection.Handle) != nil || request.OriginalRuntime.Session != selection.Runtime.Session || selection.Runtime.Session != selection.Handle.Conversation.Session || selection.Runtime.Generation != selection.Handle.Generation || ref.Conversation != selection.Handle.Conversation || ref.Action != "send" || !operationPattern.MatchString(ref.OperationID) || !hexPattern.MatchString(ref.Digest) || validateSettings(selection.Settings) != nil || !utf8.ValidString(input.Message) || input.Message == "" || len(input.Message) > 65536 || !utf8.ValidString(input.ActionContext) || input.Retry || SubmissionDigest(ref, input, selection.Settings) != ref.Digest || selection.InstructionsDigest != "" && !hexPattern.MatchString(selection.InstructionsDigest) || selection.Policy != nil && ValidateToolPolicy(*selection.Policy) != nil {
		return ErrScope
	}
	if request.Delivery == "" {
		if strings.HasPrefix(input.ActionContext, inputDeliveryPrefix) {
			return ErrScope
		}
	} else if action, err := deliveryContext(request.Delivery); err != nil || input.ActionContext != action {
		return ErrScope
	}
	return nil
}

func queuedRefusal(reason string) QueuedInputResult {
	return QueuedInputResult{NotSubmitted: true, Refusal: reason}
}

func (s *supervisor) dispatchQueuedInput(ctx context.Context, request QueuedInputRequest) (QueuedInputResult, error) {
	if err := validateQueuedInput(request); err != nil {
		return QueuedInputResult{Status: SubmissionStatus{Phase: "Unknown"}}, err
	}
	if request.Selection.Runtime != s.ref || s.verifyHandle(request.Selection.Handle) != nil {
		return QueuedInputResult{Status: SubmissionStatus{Phase: "Unknown"}}, ErrScope
	}
	c, err := s.member(request.Ref.Conversation)
	if err != nil {
		return QueuedInputResult{Status: SubmissionStatus{Phase: "Unknown"}}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := s.validateInputRuntimeContinuation(ctx, request.OriginalRuntime); err != nil {
		return QueuedInputResult{Status: SubmissionStatus{Phase: "Unknown"}}, err
	}
	original, err := s.lookupQueuedInputLocked(ctx, c, request.Ref)
	if err != nil {
		return QueuedInputResult{Status: SubmissionStatus{Phase: "Unknown"}}, err
	}
	if original.Ref == request.Ref {
		return QueuedInputResult{Status: original}, nil
	}
	selection := request.Selection
	if !selection.CheckedInput || selection.Held || selection.PendingSettings || selection.PendingPolicy || c.record.Settings != selection.Settings || !reflect.DeepEqual(c.record.ToolPolicy, selection.Policy) || instructionsDigest(c.record.Instructions) != selection.InstructionsDigest || c.record.Generation != selection.Handle.Generation {
		return queuedRefusal("Settings or mode changed. Review them before sending a new message."), ErrConflict
	}
	proof, err := s.identityProofLocked(ctx, c)
	if err != nil || proof.Held {
		return queuedRefusal("The worker could not be verified. Check the conversation before sending a new message."), errors.Join(ErrHeld, err)
	}
	if err := s.checkInputAdmission(ctx, c); err != nil {
		return queuedRefusal("The worker could not accept this message. Check the conversation before sending a new message."), err
	}
	// Every return after entering the owning native path keeps its original
	// ambiguity. No transport/persistence error can become a positive refusal.
	var status SubmissionStatus
	if request.Delivery == "" {
		owned, err := s.sendOwned(ctx, c, receiptReference{retained: &request.Ref}, request.Input)
		return QueuedInputResult{Status: owned.retained()}, err
	}
	status, err = s.inputDeliveryLocked(ctx, c, selection.Handle, backend.SendRequest{Message: request.Input.Message, ClientUserMessageID: request.Ref.OperationID, ActionContext: request.Input.ActionContext, Delivery: request.Delivery}, &request.Ref)
	return QueuedInputResult{Status: status}, err
}

func validateQueuedRef(ref SubmissionRef) error {
	if validateHandle(agentbackend.ExecutionHandle{Conversation: ref.Conversation, Generation: strings.Repeat("0", 64)}) != nil || ref.Action != "send" || !operationPattern.MatchString(ref.OperationID) || !hexPattern.MatchString(ref.Digest) {
		return ErrScope
	}
	return nil
}

func (s *supervisor) lookupQueuedInput(ctx context.Context, ref SubmissionRef) (QueuedInputLookupResult, error) {
	if err := validateQueuedRef(ref); err != nil {
		return QueuedInputLookupResult{}, err
	}
	c, err := s.member(ref.Conversation)
	if err != nil {
		return QueuedInputLookupResult{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	status, err := s.lookupQueuedInputLocked(ctx, c, ref)
	if err != nil {
		return QueuedInputLookupResult{Status: status}, err
	}
	retired, err := c.receipts.queuedRetiredUnknown(ctx, ref, status)
	return QueuedInputLookupResult{Status: status, RetiredUnknown: retired}, err
}

func (e *receiptEngine) queuedRetiredUnknown(ctx context.Context, ref SubmissionRef, status SubmissionStatus) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if status.Phase != "Unknown" || status.Ref != ref {
		return false, nil
	}
	if e.expectedOwner() != "retained" {
		return false, ErrScope
	}
	raw, err := e.loadOwned(ref.OperationID)
	if err != nil {
		return false, err
	}
	// A projected Unknown from prepared/dispatching is not the raw terminal
	// Unknown receipt that the explicit retirement operation may own.
	if raw.Phase != "Unknown" {
		return false, nil
	}
	if !reflect.DeepEqual(raw.retained(), status) {
		return false, ErrConflict
	}
	recoveries, err := e.inputRecoveries()
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return retiredInput(raw, recoveries), nil
}

func (m *Manager) proveQueuedRetirement(ctx context.Context, current RuntimeRef, ref SubmissionRef, status SubmissionStatus) (bool, error) {
	directory, err := stateDirectory(m.config.UserStateRoot, ref.Conversation.Session)
	if err != nil {
		return false, err
	}
	root := filepath.Join(directory, "members", ref.Conversation.Member, ref.Conversation.Incarnation, "submissions")
	retired, err := (&receiptEngine{root: root}).queuedRetiredUnknown(ctx, ref, status)
	if err != nil {
		return false, err
	}
	latest, err := m.record(ref.Conversation.Session)
	if err != nil || latest.Runtime != current {
		return false, errors.Join(ErrConflict, err)
	}
	return retired, nil
}

func (s *supervisor) lookupQueuedInputLocked(ctx context.Context, c *conversation, ref SubmissionRef) (SubmissionStatus, error) {
	e := c.receipts
	e.mu.Lock()
	record, err := e.loadOwned(ref.OperationID)
	if errors.Is(err, os.ErrNotExist) {
		e.mu.Unlock()
		return SubmissionStatus{Phase: "Unknown"}, nil
	}
	if err != nil || record.ref.retained == nil || *record.ref.retained != ref {
		e.mu.Unlock()
		return SubmissionStatus{}, errors.Join(ErrConflict, err)
	}
	if record.Phase == "prepared" || record.Phase == "dispatching" {
		record.Phase = "Unknown"
		if live := e.liveInputs[ref.OperationID]; live != nil && live.ref.equal(record.ref) && live.worker == c.worker && record.Generation == s.ref.Generation {
			record.Phase = "Pending"
		}
	}
	e.mu.Unlock()
	if err := s.admitReceiptGeneration(record.Generation); err != nil {
		return SubmissionStatus{Phase: "Unknown"}, err
	}
	if record.Phase == "Accepted" {
		if c.worker == nil || c.record.Generation != s.ref.Generation {
			return SubmissionStatus{Phase: "Unknown"}, ErrUnavailable
		}
		var verified struct{ Verified bool }
		if err := c.worker.Call(ctx, "verify_history", struct {
			Witness *NativeWitness
			Message string
		}{record.Witness, record.Input.Message}, &verified); err != nil || !verified.Verified {
			return SubmissionStatus{Phase: "Unknown"}, errors.Join(ErrHeld, err)
		}
	}
	return record.retained(), nil
}

func validQueuedStatus(status SubmissionStatus, ref SubmissionRef) error {
	if reflect.DeepEqual(status, SubmissionStatus{Phase: "Unknown"}) {
		return nil
	}
	if status.Schema != 1 || status.Ref != ref || !hexPattern.MatchString(status.Generation) || validateSettings(status.Settings) != nil || SubmissionDigest(ref, status.Input, status.Settings) != ref.Digest || status.CreatedAt.IsZero() || status.UpdatedAt.IsZero() {
		return ErrScope
	}
	switch status.Phase {
	case "Accepted":
		if status.Witness == nil || status.Witness.SessionID != ref.Conversation.NativeID || status.Witness.EntryID == "" || status.Witness.EffectiveDigest != MessageDigest(status.Input.Message) {
			return ErrHeld
		}
	case "NotSubmitted":
		if status.Refusal == "" {
			return ErrHeld
		}
	case "Pending", "Unknown":
	default:
		return ErrScope
	}
	return nil
}

func (m *Manager) DispatchQueuedInput(ctx context.Context, request QueuedInputRequest) (QueuedInputResult, error) {
	if err := validateQueuedInput(request); err != nil {
		return QueuedInputResult{Status: SubmissionStatus{Phase: "Unknown"}}, err
	}
	current, err := m.record(request.Ref.Conversation.Session)
	if err != nil || current.Runtime != request.Selection.Runtime {
		return QueuedInputResult{Status: SubmissionStatus{Phase: "Unknown"}}, errors.Join(ErrScope, err)
	}
	if err := m.ValidateInputRuntimeContinuation(ctx, request.OriginalRuntime, current.Runtime); err != nil {
		return QueuedInputResult{Status: SubmissionStatus{Phase: "Unknown"}}, err
	}
	supported, err := m.config.checkedInputContract(current.Start.AppStorePath)
	if err != nil || !supported {
		return QueuedInputResult{Status: SubmissionStatus{Phase: "Unknown"}}, errors.Join(ErrUnavailable, err)
	}
	var result QueuedInputResult
	err = m.call(ctx, current.Runtime, "input_delivery_checked_v1", request, &result)
	if result.NotSubmitted {
		if result.Refusal == "" || len(result.Refusal) > 4096 || !validString(result.Refusal) || !reflect.DeepEqual(result.Status, SubmissionStatus{}) {
			return QueuedInputResult{Status: SubmissionStatus{Phase: "Unknown"}}, ErrScope
		}
		return result, err
	}
	if validErr := validQueuedStatus(result.Status, request.Ref); validErr != nil {
		return QueuedInputResult{Status: SubmissionStatus{Phase: "Unknown"}}, errors.Join(validErr, err)
	}
	return result, err
}

func (m *Manager) LookupQueuedInput(ctx context.Context, ref SubmissionRef) (QueuedInputLookupResult, error) {
	unknown := QueuedInputLookupResult{Status: SubmissionStatus{Phase: "Unknown"}}
	if err := validateQueuedRef(ref); err != nil {
		return unknown, err
	}
	current, err := m.record(ref.Conversation.Session)
	if err != nil {
		return unknown, err
	}
	supported, err := m.config.checkedInputContract(current.Start.AppStorePath)
	if err != nil {
		return unknown, err
	}
	if supported {
		var result QueuedInputLookupResult
		if err := m.call(ctx, current.Runtime, "input_lookup_v1", ref, &result); err != nil {
			return unknown, err
		}
		if err := validQueuedStatus(result.Status, ref); err != nil {
			return unknown, err
		}
		if result.Status.Ref == ref {
			if err := m.receiptGeneration(ref.Conversation.Session, result.Status.Generation); err != nil {
				return unknown, err
			}
			if result.Status.Phase == "Pending" && result.Status.Generation != current.Runtime.Generation {
				return unknown, ErrScope
			}
		}
		if result.RetiredUnknown {
			retired, err := m.proveQueuedRetirement(ctx, current.Runtime, ref, result.Status)
			if err != nil || !retired {
				return unknown, errors.Join(ErrScope, err)
			}
		}
		return result, nil
	}
	// Retained runtimes keep their original strict operations. Disk metadata
	// alone cannot establish Accepted; reprove it through the existing native
	// creation verifier only where that owning non-fork contract applies.
	result, err := m.Lookup(ctx, ref)
	if errors.Is(err, os.ErrNotExist) {
		return unknown, nil
	}
	if err != nil {
		return unknown, err
	}
	if err := validQueuedStatus(result, ref); err != nil {
		return unknown, err
	}
	if err := m.receiptGeneration(ref.Conversation.Session, result.Generation); err != nil {
		return unknown, err
	}
	if result.Phase != "Accepted" {
		retired, err := m.proveQueuedRetirement(ctx, current.Runtime, ref, result)
		if err != nil {
			return unknown, err
		}
		return QueuedInputLookupResult{Status: result, RetiredUnknown: retired}, nil
	}
	directory, err := stateDirectory(m.config.UserStateRoot, ref.Conversation.Session)
	if err != nil {
		return unknown, err
	}
	root := filepath.Join(directory, "members", ref.Conversation.Member, ref.Conversation.Incarnation)
	var mapping conversationRecord
	if err := readPrivate(filepath.Join(root, "conversation.json"), &mapping); err != nil {
		return unknown, err
	}
	if validateConversationRecord(mapping, ref.Conversation, root) != nil || mapping.Fork != nil || !mapping.Loaded || mapping.Generation != current.Runtime.Generation {
		return unknown, ErrUnavailable
	}
	proof, err := m.VerifyCreation(ctx, agentbackend.ExecutionHandle{Conversation: ref.Conversation, Generation: current.Runtime.Generation}, mapping.CreationID, mapping.HistoryRef, &ref)
	if err != nil || proof.Conversation != ref.Conversation || proof.Generation != current.Runtime.Generation || proof.CreationID != mapping.CreationID || proof.HistoryRef != mapping.HistoryRef || proof.InstructionsDigest != instructionsDigest(mapping.Instructions) || !proof.Persisted || proof.Bootstrap == nil || !reflect.DeepEqual(*proof.Bootstrap, result) {
		return unknown, errors.Join(ErrHeld, err)
	}
	return QueuedInputLookupResult{Status: result}, nil
}

var _ QueuedInputController = (*Manager)(nil)
