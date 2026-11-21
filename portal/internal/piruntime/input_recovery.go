package piruntime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// InputRetirement identifies an original uncertain input, never a new input.
type InputRetirement struct{ OperationID, Digest, Member string }

func (r InputRetirement) Valid() bool {
	return operationPattern.MatchString(r.OperationID) && hexPattern.MatchString(r.Digest) && memberPattern.MatchString(r.Member)
}

type recoveryHistory struct {
	File, Ref, LeafID, Digest, SHA256 string
	Bytes                             int64
	Entries                           int
}
type recoveryMember struct {
	Before       string
	BeforeSHA256 string
	History      recoveryHistory
}
type inputRecoveryRecord struct {
	Schema                        int
	Phase, Reason                 string
	Ref                           SubmissionRef
	Source, Successor             RuntimeRef
	Start                         StartRequest
	Members                       []recoveryMember
	ReceiptSHA256, SettingsDigest string
	PreparedAt                    time.Time
	RetiredAt                     *time.Time       `json:",omitempty"`
	Stop                          *HostObservation `json:",omitempty"`
}

func bytesDigest(data []byte) string      { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func recoveryPath(root, id string) string { return filepath.Join(root, "input-recoveries", id+".json") }
func recoveryUnloaded(before conversationRecord) conversationRecord {
	before.Loaded = false
	before.WorkerPID = 0
	before.WorkerStart = ""
	return before
}

// Read the existing runtime codec with its retained configuration. No execution,
// host or authentication path is opened by this metadata-only lineage read.
func recoveryRuntimeRecords(root string, session agentbackend.SessionRef) (runtimeRecords, error) {
	directory := filepath.Clean(filepath.Join(root, "../../.."))
	state := filepath.Clean(filepath.Join(directory, "../../../.."))
	expected, err := stateDirectory(state, session)
	if err != nil || expected != directory {
		return runtimeRecords{}, ErrScope
	}
	var raw runtimeRecords
	if err := readPrivate(filepath.Join(directory, "runtime.json"), &raw); err != nil {
		return raw, err
	}
	current := raw.Requests[raw.CurrentRequestID]
	m := Manager{config: Config{UserStateRoot: state, HostStateRoot: current.HostStateRoot, AuthDirectory: current.AuthDirectory, AccountBindings: current.AccountBindings}}
	return m.records(session)
}
func recoveryDescendant(records runtimeRecords, successor RuntimeRef) bool {
	seen := map[string]bool{}
	for id := records.CurrentRequestID; id != ""; {
		if seen[id] {
			return false
		}
		seen[id] = true
		r, ok := records.Requests[id]
		if !ok {
			return false
		}
		if r.Runtime == successor {
			return true
		}
		id = r.Start.ReplacesRequestID
	}
	return false
}

// The sole strict decoder validates the complete directory and every linked
// original receipt, including prepared records. A partial record grants nothing.
func readInputRecoveries(root string) (map[string]inputRecoveryRecord, error) {
	result := map[string]inputRecoveryRecord{}
	directory := filepath.Join(root, "input-recoveries")
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || owner.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("input recovery owner: %w", ErrScope)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) == 0 || len(entries) > 4096 {
		return nil, ErrConflict
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || entry.Name() != id+".json" || !operationPattern.MatchString(id) {
			return nil, ErrConflict
		}
		var record inputRecoveryRecord
		if err := readPrivate(filepath.Join(directory, entry.Name()), &record); err != nil {
			return nil, err
		}
		if record.Schema != 1 || record.Reason != "retire-with-uncertainty" || (record.Phase != "prepared" && record.Phase != "retired") || record.Ref.OperationID != id || record.Ref.Action != "send" || !hexPattern.MatchString(record.Ref.Digest) || !hexPattern.MatchString(record.ReceiptSHA256) || !hexPattern.MatchString(record.SettingsDigest) || record.PreparedAt.IsZero() || len(record.Members) == 0 || len(record.Members) > 64 {
			return nil, ErrConflict
		}
		ref := record.Ref.Conversation
		if filepath.Base(root) != ref.Incarnation || filepath.Base(filepath.Dir(root)) != ref.Member || filepath.Base(filepath.Dir(filepath.Dir(root))) != "members" {
			return nil, ErrScope
		}
		records, err := recoveryRuntimeRecords(root, ref.Session)
		if err != nil {
			return nil, fmt.Errorf("input recovery runtime: %w", err)
		}
		source, ok := records.Requests[record.Source.RequestID]
		if !ok || source.Runtime != record.Source {
			return nil, fmt.Errorf("input recovery source: %w", ErrScope)
		}
		next, ok := records.Requests[record.Successor.RequestID]
		if !ok || next.Runtime != record.Successor || next.Start != record.Start || next.Start.ReplacesRequestID != source.Runtime.RequestID || !recoveryDescendant(records, record.Successor) {
			return nil, fmt.Errorf("input recovery successor: %w", ErrScope)
		}
		raw, err := readPrivateBytes(filepath.Join(root, "submissions", id+".json"), recordLimit)
		if err != nil || bytesDigest(raw) != record.ReceiptSHA256 {
			return nil, ErrConflict
		}
		status, err := (&receiptEngine{root: filepath.Join(root, "submissions")}).loadOwned(id)
		if err != nil || status.ref.retained == nil || *status.ref.retained != record.Ref || status.Phase != "Unknown" || status.Witness != nil || status.Refusal != "" || status.Generation != source.Runtime.Generation || digestValue(status.Settings) != record.SettingsDigest || (status.Input.ActionContext != inputDeliveryPrefix+"steer" && status.Input.ActionContext != inputDeliveryPrefix+"followUp") {
			return nil, ErrConflict
		}
		found := false
		seen := map[agentbackend.ConversationRef]bool{}
		for _, member := range record.Members {
			var before conversationRecord
			if strictDecode([]byte(member.Before), &before, recordLimit) != nil || bytesDigest([]byte(member.Before)) != member.BeforeSHA256 || seen[before.Conversation] || before.Conversation.Session != ref.Session || before.Generation != source.Runtime.Generation || before.HeldReason != "" || before.PendingSettings != nil || before.PendingToolPolicy != nil {
				return nil, fmt.Errorf("input recovery member snapshot: %w", ErrScope)
			}
			seen[before.Conversation] = true
			memberRoot := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(root))), "members", before.Conversation.Member, before.Conversation.Incarnation)
			if validateConversationRecord(before, before.Conversation, memberRoot) != nil || member.History.File != before.HistoryFile || member.History.Ref != before.HistoryRef {
				return nil, fmt.Errorf("input recovery history identity: %w", ErrScope)
			}
			// The selected execution can still append while Stop drains it.
			// Only retired freezes its final history; idle siblings stay frozen.
			if (record.Phase == "retired" || before.Conversation != ref) && (!hexPattern.MatchString(member.History.SHA256) || !hexPattern.MatchString(member.History.Digest) || member.History.Bytes <= 0 || member.History.Entries < 1 || member.History.LeafID == "") {
				return nil, fmt.Errorf("input recovery final history: %w", ErrScope)
			}
			if before.Conversation == ref {
				found = true
				if before.Settings != status.Settings {
					return nil, ErrScope
				}
			}
		}
		if !found {
			return nil, ErrScope
		}
		if record.Phase == "prepared" {
			if record.Stop != nil || record.RetiredAt != nil {
				return nil, ErrConflict
			}
		} else if record.Stop == nil || record.RetiredAt == nil || record.RetiredAt.Before(record.PreparedAt) || !validTerminalReceipt(source, *record.Stop) {
			return nil, ErrScope
		}
		result[id] = record
	}
	return result, nil
}
func retiredInput(status ownedReceipt, records map[string]inputRecoveryRecord) bool {
	r, ok := records[status.ref.id()]
	return ok && r.Phase == "retired" && status.Phase == "Unknown" && status.ref.retained != nil && *status.ref.retained == r.Ref
}
func (e *receiptEngine) inputRecoveries() (map[string]inputRecoveryRecord, error) {
	records, err := readInputRecoveries(filepath.Dir(e.root))
	if err == nil && e.expectedOwner() != "retained" && len(records) != 0 {
		return nil, ErrScope
	}
	return records, err
}

// Hash the unmodified SDK JSONL and its complete parent chain without retaining
// text in the recovery record. History is preserved, never an acceptance witness.
func captureRecoveryHistory(record conversationRecord) (recoveryHistory, error) {
	result := recoveryHistory{File: record.HistoryFile, Ref: record.HistoryRef}
	file, err := os.Open(record.HistoryFile)
	if err != nil {
		return result, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return result, ErrHeld
	}
	rawHash := sha256.New()
	branchHash := sha256.New()
	branchHash.Write([]byte("["))
	scan := bufio.NewScanner(io.TeeReader(file, rawHash))
	scan.Buffer(make([]byte, 4096), recordLimit)
	ids := map[string]bool{}
	parent := ""
	header := false
	for scan.Scan() {
		line := scan.Bytes()
		if len(line) == 0 {
			return result, ErrHeld
		}
		var entry struct {
			Type, ID, ParentID, Cwd string
			Message                 struct {
				Role, StopReason string
				Content          json.RawMessage
			}
		}
		// Native entries have SDK-owned fields. Validate JSON syntax/duplicate keys
		// before reading only this operation's fixed eligibility fields.
		var fields map[string]json.RawMessage
		if strictDecode(line, &fields, recordLimit) != nil || json.Unmarshal(line, &entry) != nil {
			return result, ErrHeld
		}
		if !header {
			header = true
			if entry.Type != "session" || entry.ID != record.Conversation.NativeID || entry.Cwd != filepath.Join(record.Conversation.Session.Workspace, "work", record.Conversation.Session.Slug) {
				return result, ErrScope
			}
			continue
		}
		if entry.ID == "" || ids[entry.ID] || entry.ParentID != parent || entry.Type == "branch_summary" || entry.Type == "context_edit" {
			return result, ErrHeld
		}
		ids[entry.ID] = true
		parent = entry.ID
		compact := new(bytes.Buffer)
		if json.Compact(compact, line) != nil {
			return result, ErrHeld
		}
		if result.Entries != 0 {
			branchHash.Write([]byte(","))
		}
		branchHash.Write(compact.Bytes())
		result.Entries++
	}
	if scan.Err() != nil || result.Entries == 0 {
		return result, ErrHeld
	}
	branchHash.Write([]byte("]"))
	result.LeafID = parent
	result.Digest = hex.EncodeToString(branchHash.Sum(nil))
	result.SHA256 = hex.EncodeToString(rawHash.Sum(nil))
	result.Bytes = info.Size()
	after, err := file.Stat()
	if err != nil || after.Size() != info.Size() || after.ModTime() != info.ModTime() {
		return result, ErrHeld
	}
	return result, nil
}
func verifyRecoveryHistory(member recoveryMember) error {
	var before conversationRecord
	if strictDecode([]byte(member.Before), &before, recordLimit) != nil {
		return ErrScope
	}
	now, err := captureRecoveryHistory(before)
	if err != nil {
		return err
	}
	if now != member.History {
		return ErrConflict
	}
	return nil
}
func (m *Manager) recoveryPoint(hooks UpgradeHooks, name string) error {
	if hooks.recoveryCheckpoint != nil {
		return hooks.recoveryCheckpoint(name)
	}
	return nil
}

func (m *Manager) upgradeInputRecovery(ctx context.Context, request UpgradeRequest, hooks UpgradeHooks) (UpgradeResult, error) {
	var result UpgradeResult
	tuple := request.RetireUnknown
	if tuple == nil || !tuple.Valid() {
		return result, ErrScope
	}
	contract := m.config.inputRecoveryContract
	if contract == nil {
		contract = InputRecoveryPackage
	}
	capable, err := contract(request.Start.AppStorePath)
	if err != nil || !capable {
		return result, ErrUnavailable
	}
	directory, err := stateDirectory(m.config.UserStateRoot, request.Start.Session)
	if err != nil {
		return result, err
	}
	unlock, err := runtimeLock(directory)
	if err != nil {
		return result, err
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	records, err := m.records(request.Start.Session)
	if err != nil {
		return result, err
	}
	if err := m.config.validatePackage(request.Start.AppStorePath); err != nil {
		return result, err
	}
	if _, ok := m.config.Host.(hostStopper); !ok {
		return result, ErrUnavailable
	}
	previous, ok := records.Requests[request.Start.ReplacesRequestID]
	if !ok || previous.Phase != "starting" || previous.Runtime.Generation != request.ExpectedGeneration {
		return result, ErrScope
	}
	current, reserved := records.Requests[request.Start.RequestID]
	if reserved {
		if records.CurrentRequestID != current.Runtime.RequestID || current.Start != request.Start {
			return result, ErrConflict
		}
	} else if records.CurrentRequestID != previous.Runtime.RequestID {
		return result, ErrConflict
	}
	root := ""
	var target agentbackend.ConversationRef
	for _, ref := range request.Members {
		if ref.Member == tuple.Member {
			if root != "" {
				return result, ErrScope
			}
			target = ref
			root = filepath.Join(directory, "members", ref.Member, ref.Incarnation)
		}
	}
	if root == "" {
		return result, ErrScope
	}
	recoveries, err := readInputRecoveries(root)
	if err != nil {
		return result, err
	}
	recovery, saved := recoveries[tuple.OperationID]
	if saved && (recovery.Ref.Conversation != target || recovery.Ref.Digest != tuple.Digest || recovery.Start != request.Start || recovery.Source != previous.Runtime || !reflect.DeepEqual(recoveryMembers(recovery), request.Members)) {
		return result, ErrConflict
	}
	if !saved {
		for _, old := range recoveries {
			if old.Phase != "retired" {
				return result, ErrHeld
			}
		}
	}
	_, members, err := m.upgradeMembers(ctx, request, records)
	if err != nil {
		return result, err
	}
	if !saved {
		if reserved && current.Phase != "preparing" {
			return result, ErrConflict
		}
		var connector HostCallbackConnector = m
		if reserved {
			lifetime, cancel := context.WithCancel(ctx)
			predecessor := &upgradePredecessorConnector{manager: m, current: current.Runtime, previous: previous.Runtime, lifetime: lifetime, cancel: cancel}
			defer predecessor.close()
			connector = predecessor
		}
		if err := hooks.EnsureAttachment(ctx, previous.Runtime, connector); err != nil {
			return result, err
		}
		original, err := (&receiptEngine{root: filepath.Join(root, "submissions")}).loadOwned(tuple.OperationID)
		if err != nil || original.ref.retained == nil || original.ref.retained.Conversation != target || original.ref.retained.Digest != tuple.Digest || original.Phase != "Unknown" || original.Witness != nil || original.Refusal != "" || original.Generation != previous.Runtime.Generation || (original.Input.ActionContext != inputDeliveryPrefix+"steer" && original.Input.ActionContext != inputDeliveryPrefix+"followUp") {
			return result, ErrHeld
		}
		raw, err := readPrivateBytes(filepath.Join(root, "submissions", tuple.OperationID+".json"), recordLimit)
		if err != nil {
			return result, err
		}
		recovery = inputRecoveryRecord{Schema: 1, Phase: "prepared", Reason: "retire-with-uncertainty", Ref: *original.ref.retained, Source: previous.Runtime, Start: request.Start, ReceiptSHA256: bytesDigest(raw), SettingsDigest: digestValue(original.Settings), PreparedAt: time.Now().UTC()}
		for _, member := range members {
			record := member.record
			if record.Generation != previous.Runtime.Generation {
				return result, ErrScope
			}
			before, err := readPrivateBytes(filepath.Join(directory, "members", record.Conversation.Member, record.Conversation.Incarnation, "conversation.json"), recordLimit)
			if err != nil {
				return result, err
			}
			var selected *ownedReceipt
			if record.Conversation == target {
				selected = &original
				if !record.Loaded || record.Settings != original.Settings {
					return result, ErrScope
				}
			}
			history := recoveryHistory{File: record.HistoryFile, Ref: record.HistoryRef}
			if selected == nil {
				history, err = captureRecoveryHistory(record)
				if err != nil {
					return result, err
				}
			}
			if record.Loaded {
				handle := agentbackend.ExecutionHandle{Conversation: record.Conversation, Generation: previous.Runtime.Generation}
				var snapshot workerSnapshot
				if err := m.callRaw(ctx, previous.Runtime, "snapshot", conversationRequest{Handle: handle}, &snapshot); err != nil || snapshot.NativeID != record.Conversation.NativeID || snapshot.HistoryFile != record.HistoryFile || selected == nil && (snapshot.HistoryDigest != history.Digest || snapshot.LeafID != history.LeafID) || !snapshot.Persisted || snapshot.Model != record.Settings.Model || snapshot.Provider != record.Settings.Provider || snapshot.Thinking != record.Settings.Thinking || snapshot.InstructionsDigest != instructionsDigest(record.Instructions) || selected == nil && (snapshot.Busy || snapshot.Held) {
					return result, ErrHeld
				}
				var observation WorkflowObservation
				if err := m.callRaw(ctx, previous.Runtime, "workflow_observe", toolPolicyRequest{Handle: handle}, &observation); err != nil || observation.Handle != handle || observation.Settings != record.Settings || !observation.Loaded || observation.Retained == nil || record.ToolPolicy == nil || !sameToolPolicy(*observation.Retained, *record.ToolPolicy) || ValidateToolPolicyProof(observation.Proof, *record.ToolPolicy, previous.Runtime.Generation) != nil || !uuidPattern.MatchString(observation.AttachmentID) || observation.Sequence == 0 || selected == nil && (!observation.Idle || observation.Pending || observation.Held) {
					return result, ErrHeld
				}
			}
			recovery.Members = append(recovery.Members, recoveryMember{Before: string(before), BeforeSHA256: bytesDigest(before), History: history})
		}
		if !reserved {
			descriptor, err := readPrivateBytes(filepath.Join(m.instance(previous.Runtime), "launch.json"), 65536)
			if err != nil {
				return result, err
			}
			current, err = m.reserveSuccessorRecord(request.Start, records, previous, descriptor)
			if err != nil {
				return result, err
			}
		}
		result.Runtime = current.Runtime
		if err := m.recoveryPoint(hooks, "reservation"); err != nil {
			return result, err
		}
		if err := m.retainRuntimeRequest(ctx, previous); err != nil {
			return result, err
		}
		if err := m.retainRuntimeRequest(ctx, current); err != nil {
			return result, err
		}
		recovery.Successor = current.Runtime
		if err := writePrivate(recoveryPath(root, tuple.OperationID), recovery); err != nil {
			return result, err
		}
		if err := m.recoveryPoint(hooks, "prepared"); err != nil {
			return result, err
		}
	}
	result.Runtime = current.Runtime
	result.Previous = previous.Runtime
	result.AppStorePath = request.Start.AppStorePath
	result.Phase = current.Phase
	if recovery.Phase == "prepared" {
		for _, member := range recovery.Members {
			var before conversationRecord
			if strictDecode([]byte(member.Before), &before, recordLimit) != nil {
				return result, ErrScope
			}
			if before.Conversation != target {
				if err := verifyRecoveryHistory(member); err != nil {
					return result, err
				}
			}
			if err := checkRecoveryMapping(directory, member, recovery, false); err != nil {
				return result, err
			}
		}
		status, err := m.upgradePredecessorStatus(ctx, current.Runtime, previous.Runtime)
		if err != nil {
			return result, err
		}
		observed := status.Observation
		if status.Phase != "stopped" {
			if status.Phase != "ready" {
				return result, ErrHeld
			}
			stopper, ok := m.config.Host.(hostStopper)
			if !ok {
				return result, ErrUnavailable
			}
			observed, err = stopper.Stop(ctx, previous.Runtime, observed)
			if err != nil {
				return result, err
			}
		}
		if !validTerminalReceipt(previous, observed) || m.config.verifyDrain(observed) != nil {
			return result, ErrHeld
		}
		if _, err := readInputRecoveries(root); err != nil {
			return result, err
		}
		for i, member := range recovery.Members {
			if err := checkRecoveryMapping(directory, member, recovery, false); err != nil {
				return result, err
			}
			var before conversationRecord
			if strictDecode([]byte(member.Before), &before, recordLimit) != nil {
				return result, ErrScope
			}
			if before.Conversation == target {
				// Stop and a fresh drain proof make this the owner's final
				// history. A lost Stop reply retries this capture in place.
				history, err := captureRecoveryHistory(before)
				if err != nil {
					return result, err
				}
				recovery.Members[i].History = history
			} else if err := verifyRecoveryHistory(member); err != nil {
				return result, err
			}
		}
		fresh, err := m.records(request.Start.Session)
		if err != nil {
			return result, err
		}
		if _, _, err := m.upgradeMembers(ctx, request, fresh); err != nil {
			return result, err
		}
		now := time.Now().UTC()
		recovery.Phase = "retired"
		recovery.RetiredAt = &now
		recovery.Stop = &observed
		if err := writePrivate(recoveryPath(root, tuple.OperationID), recovery); err != nil {
			return result, err
		}
		if err := m.recoveryPoint(hooks, "retired"); err != nil {
			return result, err
		}
	}
	if current.Phase == "preparing" {
		status, err := m.upgradePredecessorStatus(ctx, current.Runtime, previous.Runtime)
		if err != nil || status.Phase != "stopped" {
			return result, ErrHeld
		}
	}
	for _, member := range recovery.Members {
		if err := checkRecoveryMapping(directory, member, recovery, true); err != nil {
			return result, err
		}
		var before, actual conversationRecord
		if strictDecode([]byte(member.Before), &before, recordLimit) != nil {
			return result, ErrScope
		}
		path := filepath.Join(directory, "members", before.Conversation.Member, before.Conversation.Incarnation, "conversation.json")
		if err := readPrivate(path, &actual); err != nil {
			return result, err
		}
		if actual.Generation == previous.Runtime.Generation {
			if err := verifyRecoveryHistory(member); err != nil {
				return result, err
			}
			if err := writePrivate(path, recoveryUnloaded(before)); err != nil {
				return result, err
			}
		}
		if err := m.recoveryPoint(hooks, "mapping:"+before.Conversation.Member); err != nil {
			return result, err
		}
	}
	unlock()
	unlock = nil
	ordinary := request
	ordinary.RetireUnknown = nil
	result, err = m.Upgrade(ctx, ordinary, hooks)
	if err == nil {
		result.Disposition = "retired-unknown"
		result.InputID = tuple.OperationID
		result.InputDigest = tuple.Digest
	}
	return result, err
}
func recoveryMembers(record inputRecoveryRecord) []agentbackend.ConversationRef {
	refs := make([]agentbackend.ConversationRef, 0, len(record.Members))
	for _, member := range record.Members {
		var before conversationRecord
		if json.Unmarshal([]byte(member.Before), &before) == nil {
			refs = append(refs, before.Conversation)
		}
	}
	return refs
}
func checkRecoveryMapping(directory string, member recoveryMember, recovery inputRecoveryRecord, after bool) error {
	var before, actual conversationRecord
	if strictDecode([]byte(member.Before), &before, recordLimit) != nil {
		return ErrScope
	}
	root := filepath.Join(directory, "members", before.Conversation.Member, before.Conversation.Incarnation)
	raw, err := readPrivateBytes(filepath.Join(root, "conversation.json"), recordLimit)
	if err != nil {
		return err
	}
	if bytes.Equal(raw, []byte(member.Before)) {
		return nil
	}
	if !after || strictDecode(raw, &actual, recordLimit) != nil || validateConversationRecord(actual, before.Conversation, root) != nil {
		return ErrConflict
	}
	if equalJSON(actual, recoveryUnloaded(before)) {
		return nil
	}
	// A replay after first Load may observe its same saved successor bookkeeping.
	if actual.Generation != recovery.Successor.Generation || !actual.Loaded {
		return ErrConflict
	}
	actual.Generation = before.Generation
	actual.Loaded = before.Loaded
	actual.WorkerPID = before.WorkerPID
	actual.WorkerStart = before.WorkerStart
	actual.WorkerTopology = before.WorkerTopology
	if !equalJSON(actual, before) {
		return ErrConflict
	}
	return nil
}
func InputRecoveryPackage(app string) (bool, error) {
	if !storePattern.MatchString(app) {
		return false, ErrScope
	}
	data, err := os.ReadFile(filepath.Join(app, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return false, err
	}
	var fields map[string]json.RawMessage
	if strictDecode(data, &fields, 1<<20) != nil || fields == nil {
		return false, ErrScope
	}
	raw, ok := fields["piInputRecovery"]
	if !ok {
		return false, nil
	}
	var version int
	if strictDecode(raw, &version, 32) != nil || version != 1 {
		return false, ErrScope
	}
	return true, nil
}

// The first successor Load must consume exactly the captured durable history.
// Later loads retain the audit but may see ordinary append-only conversation work.
func verifyRecoveryLoad(root string, record conversationRecord, runtime RuntimeRef) error {
	recoveries, err := readInputRecoveries(root)
	if err != nil {
		return err
	}
	for _, recovery := range recoveries {
		if recovery.Phase != "retired" || recovery.Successor != runtime || record.Generation != recovery.Source.Generation {
			continue
		}
		for _, member := range recovery.Members {
			var before conversationRecord
			if strictDecode([]byte(member.Before), &before, recordLimit) != nil {
				return ErrScope
			}
			if before.Conversation == record.Conversation {
				if !equalJSON(record, recoveryUnloaded(before)) {
					return ErrConflict
				}
				if err := verifyRecoveryHistory(member); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
