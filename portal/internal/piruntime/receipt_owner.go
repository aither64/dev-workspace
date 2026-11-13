package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// One receipt algorithm serves disjoint owned roots. The tagged reference never
// converts a utility into a retained conversation or changes schema-1 bytes.
type receiptReference struct {
	retained *SubmissionRef
	utility  *UtilitySubmissionRef
}

func (r receiptReference) kind() string {
	if r.utility != nil && r.retained == nil {
		return "utility"
	}
	if r.retained != nil && r.utility == nil {
		return "retained"
	}
	return ""
}
func (r receiptReference) id() string {
	if r.utility != nil {
		return r.utility.OperationID
	}
	if r.retained != nil {
		return r.retained.OperationID
	}
	return ""
}
func (r receiptReference) nativeID() string {
	if r.utility != nil {
		return r.utility.Utility.NativeID
	}
	if r.retained != nil {
		return r.retained.Conversation.NativeID
	}
	return ""
}
func (r receiptReference) valid(input SendInput, settings EffectiveSettings) bool {
	if !operationPattern.MatchString(r.id()) {
		return false
	}
	if r.kind() == "utility" {
		return validateUtility(r.utility.Utility, true) == nil && r.utility.Action == "send" && r.utility.Digest == UtilitySubmissionDigest(*r.utility, input, settings)
	}
	return r.kind() == "retained" && r.retained.Action == "send" && r.retained.Digest == SubmissionDigest(*r.retained, input, settings)
}
func (r receiptReference) equal(other receiptReference) bool {
	if r.kind() != other.kind() {
		return false
	}
	if r.kind() == "utility" {
		return *r.utility == *other.utility
	}
	return r.kind() == "retained" && *r.retained == *other.retained
}

type receiptData struct {
	Phase, Generation    string
	Settings             EffectiveSettings
	Input                SendInput
	Witness              *NativeWitness
	Refusal              string
	CreatedAt, UpdatedAt time.Time
}
type ownedReceipt struct {
	ref receiptReference
	receiptData
}

func fromRetained(status SubmissionStatus) ownedReceipt {
	ref := status.Ref
	return ownedReceipt{receiptReference{retained: &ref}, receiptData{status.Phase, status.Generation, status.Settings, status.Input, status.Witness, status.Refusal, status.CreatedAt, status.UpdatedAt}}
}
func fromUtility(status UtilitySubmissionStatus) ownedReceipt {
	ref := status.Ref
	return ownedReceipt{receiptReference{utility: &ref}, receiptData{status.Phase, status.Generation, status.Settings, status.Input, status.Witness, status.Refusal, status.CreatedAt, status.UpdatedAt}}
}
func (r ownedReceipt) retained() SubmissionStatus {
	if r.ref.kind() != "retained" {
		return SubmissionStatus{}
	}
	return SubmissionStatus{Schema: 1, Ref: *r.ref.retained, Phase: r.Phase, Generation: r.Generation, Settings: r.Settings, Input: r.Input, Witness: r.Witness, Refusal: r.Refusal, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}
func (r ownedReceipt) utility() UtilitySubmissionStatus {
	if r.ref.kind() != "utility" {
		return UtilitySubmissionStatus{}
	}
	return UtilitySubmissionStatus{Schema: 2, OwnerKind: "utility", Ref: *r.ref.utility, Phase: r.Phase, Generation: r.Generation, Settings: r.Settings, Input: r.Input, Witness: r.Witness, Refusal: r.Refusal, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}
func (e *receiptEngine) expectedOwner() string {
	if e.ownerKind == "utility" {
		return "utility"
	}
	return "retained"
}
func (e *receiptEngine) loadOwned(id string) (ownedReceipt, error) {
	if !operationPattern.MatchString(id) {
		return ownedReceipt{}, ErrScope
	}
	var raw json.RawMessage
	if err := readPrivate(filepath.Join(e.root, id+".json"), &raw); err != nil {
		return ownedReceipt{}, err
	}
	var header struct {
		Schema    int
		OwnerKind string
	}
	if json.Unmarshal(raw, &header) != nil {
		return ownedReceipt{}, ErrConflict
	}
	var record ownedReceipt
	if e.expectedOwner() == "utility" {
		if header.Schema != 2 || header.OwnerKind != "utility" {
			return record, ErrConflict
		}
		var status UtilitySubmissionStatus
		if err := strictDecode(raw, &status, recordLimit); err != nil {
			return record, err
		}
		record = fromUtility(status)
	} else {
		if header.Schema != 1 || header.OwnerKind != "" {
			return record, ErrConflict
		}
		var status SubmissionStatus
		if err := strictDecode(raw, &status, recordLimit); err != nil {
			return record, err
		}
		record = fromRetained(status)
	}
	if record.ref.id() != id || !record.ref.valid(record.Input, record.Settings) {
		return record, ErrConflict
	}
	switch record.Phase {
	case "Accepted":
		if record.Witness == nil || record.Witness.SessionID != record.ref.nativeID() || record.Witness.EntryID == "" || record.Witness.EffectiveDigest != MessageDigest(record.Input.Message) {
			return record, ErrConflict
		}
	case "NotSubmitted":
		if record.Refusal == "" {
			return record, ErrConflict
		}
	case "prepared", "dispatching", "Unknown":
	default:
		return record, ErrConflict
	}
	return record, nil
}
func (e *receiptEngine) saveOwned(record ownedReceipt) error {
	if record.ref.kind() != e.expectedOwner() {
		return ErrConflict
	}
	if !operationPattern.MatchString(record.ref.id()) {
		return ErrScope
	}
	path := filepath.Join(e.root, record.ref.id()+".json")
	if record.ref.kind() == "utility" {
		return writePrivate(path, record.utility())
	}
	return writePrivate(path, record.retained())
}
func (e *receiptEngine) lookupOwned(ref receiptReference) (ownedReceipt, error) {
	if ref.kind() != e.expectedOwner() {
		return ownedReceipt{}, ErrConflict
	}
	record, err := e.loadOwned(ref.id())
	if err != nil {
		return record, err
	}
	if !record.ref.equal(ref) {
		return record, ErrConflict
	}
	if record.Phase == "prepared" || record.Phase == "dispatching" {
		record.Phase = "Unknown"
	}
	return record, nil
}
func (e *receiptEngine) held() (bool, error) {
	entries, err := os.ReadDir(e.root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if len(entries) > 4096 {
		return true, errors.New("Pi receipt capacity exceeded")
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return true, ErrConflict
		}
		status, err := e.loadOwned(entry.Name()[:len(entry.Name())-5])
		if err != nil {
			return true, err
		}
		if status.Phase != "Accepted" && status.Phase != "NotSubmitted" {
			return true, nil
		}
	}
	return false, nil
}
func (e *receiptEngine) sendOwned(ctx context.Context, ref receiptReference, input SendInput, settings EffectiveSettings, generation string, dispatch func(context.Context) (nativeResult, error), abort func()) (ownedReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ref.kind() != e.expectedOwner() {
		return ownedReceipt{}, ErrConflict
	}
	if !ref.valid(input, settings) {
		return ownedReceipt{}, ErrScope
	}
	old, err := e.loadOwned(ref.id())
	if err == nil {
		if !old.ref.equal(ref) || old.Input != input || old.Settings != settings {
			return old, ErrConflict
		}
		if old.Phase == "prepared" || old.Phase == "dispatching" {
			old.Phase = "Unknown"
		}
		return old, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return old, err
	}
	held, err := e.held()
	if err != nil {
		return old, err
	}
	if held {
		return old, ErrHeld
	}
	now := time.Now().UTC()
	status := ownedReceipt{ref, receiptData{Phase: "prepared", Generation: generation, Settings: settings, Input: input, CreatedAt: now, UpdatedAt: now}}
	if err := e.saveOwned(status); err != nil {
		return status, err
	}
	if err := e.point("intent"); err != nil {
		return status, err
	}
	status.Phase = "dispatching"
	if err := e.saveOwned(status); err != nil {
		return status, err
	}
	if err := e.point("dispatch"); err != nil {
		return status, err
	}
	result, dispatchErr := dispatch(ctx)
	if err := e.point("native"); err != nil {
		abort()
		return status, err
	}
	status.Phase = "Unknown"
	if dispatchErr == nil {
		switch result.Outcome {
		case "Accepted":
			w := result.Witness
			if w != nil && w.SessionID == ref.nativeID() && w.EntryID != "" && w.EffectiveMessage == input.Message {
				status.Phase = "Accepted"
				status.Witness = &NativeWitness{SessionID: w.SessionID, EntryID: w.EntryID, ParentID: w.ParentID, PreviousLeafID: w.PreviousLeafID, EffectiveDigest: MessageDigest(w.EffectiveMessage)}
			}
		case "NotSubmitted":
			if result.Refusal != "" {
				status.Phase = "NotSubmitted"
				status.Refusal = result.Refusal
			}
		}
	}
	status.UpdatedAt = time.Now().UTC()
	if status.Phase == "Unknown" {
		abort()
	}
	if err := e.point("witness"); err != nil {
		abort()
		return status, err
	}
	if err := e.saveOwned(status); err != nil {
		abort()
		return status, fmt.Errorf("Pi receipt commit unresolved: %w", err)
	}
	if err := e.point("receipt"); err != nil {
		return status, err
	}
	return status, nil
}
func (e *receiptEngine) lookupUtility(ref UtilitySubmissionRef) (UtilitySubmissionStatus, error) {
	record, err := e.lookupOwned(receiptReference{utility: &ref})
	return record.utility(), err
}
func (e *receiptEngine) sendUtility(ctx context.Context, ref UtilitySubmissionRef, input SendInput, settings EffectiveSettings, generation string, dispatch func(context.Context) (nativeResult, error), abort func()) (UtilitySubmissionStatus, error) {
	record, err := e.sendOwned(ctx, receiptReference{utility: &ref}, input, settings, generation, dispatch, abort)
	return record.utility(), err
}
