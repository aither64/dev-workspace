package piruntime

import (
	"bufio"
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
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// TeamConflictRecovery selects an original callback, never a replacement input.
type TeamConflictRecovery struct{ OperationID, Member string }

func (r TeamConflictRecovery) Valid() bool {
	return operationPattern.MatchString(r.OperationID) && memberPattern.MatchString(r.Member)
}

// Arguments are transient and read from the original history, never journaled.
type TeamConflictCall struct {
	LifecycleCallback
	Arguments       json.RawMessage
	TerminalRefusal *TeamConflictTerminalRefusal `json:",omitempty"`
}

// TeamConflictTerminalRefusal is transient evidence from the owning raw-history
// capture. Its digest is sealed with the domain proof; its body is not journaled.
type TeamConflictTerminalRefusal struct {
	ResultEntryID, NativeToolCallID, Tool, MessageSHA256 string
}

func teamConflictTerminalPolicyTool(tool string) bool {
	return tool == "team_message" || tool == "report_to_lead"
}

func (c TeamConflictCall) HasTerminalPolicyRefusal() bool {
	p := c.TerminalRefusal
	return p != nil && p.ResultEntryID != "" && p.NativeToolCallID == c.Origin.NativeToolCallID && teamConflictTerminalPolicyTool(p.Tool) && c.Origin.Tool == p.Tool && hexPattern.MatchString(p.MessageSHA256)
}

type TeamConflictEvidence struct {
	Kind, RecordsSHA256 string
	Target              agentbackend.ConversationRef
	InitialInputID      string
	Input               SendInput
	Receipts            []TeamConflictSubmission
	TerminalRefusal     *TeamConflictTerminalRefusal `json:",omitempty"`
}

type TeamConflictSubmission struct {
	Ref   SubmissionRef
	Input SendInput
}

// The owning domain constructs this short-lived proof from its validated stores.
// Serialized HTTP/operator values never implement this interface.
type TeamConflictProof interface{ TeamConflictEvidence() TeamConflictEvidence }
type TeamConflictDomain interface {
	Published(context.Context, RuntimeRef) (bool, error)
	Slots(context.Context, RuntimeRef) ([]LifecycleSlot, error)
	Settle(context.Context, RuntimeRef, TeamConflictCall, []LifecycleMember) (TeamConflictProof, error)
}

type teamConflictReceipt struct {
	Ref    SubmissionRef
	SHA256 string
}
type teamConflictSettlement struct {
	Kind, RecordsSHA256, EvidenceSHA256 string
	Target                              agentbackend.ConversationRef
	InitialInputID                      string
}
type teamConflictJournal struct {
	Schema            int
	Phase             string
	Selector          TeamConflictRecovery
	Start             StartRequest
	Source, Successor RuntimeRef
	Slots             []LifecycleSlot
	Captured          []LifecycleMember
	Members           []recoveryMember
	Calls             []LifecycleCallback
	Domains           []teamConflictSettlement
	Receipts          []teamConflictReceipt
	PreparedAt        time.Time
	SourceHost        *HostObservation
	Stop              *HostObservation `json:",omitempty"`
}

// teamConflictTerminalPolicyReply recognizes only the fixed producer's terminal
// pre-dispatch reply. Native call/history and producer eligibility remain the
// caller's separate proofs; this parser grants no recovery authority.
func teamConflictTerminalPolicyReply(content, details json.RawMessage) error {
	var blocks []json.RawMessage
	if err := strictDecode(content, &blocks, callbackLimit); err != nil || len(blocks) != 1 {
		return ErrHeld
	}
	var block map[string]json.RawMessage
	if err := strictDecode(blocks[0], &block, callbackLimit); err != nil || len(block) != 2 {
		return ErrHeld
	}
	var kind, text string
	if strictDecode(block["type"], &kind, callbackLimit) != nil || kind != "text" || strictDecode(block["text"], &text, callbackLimit) != nil {
		return ErrHeld
	}
	expected := map[string]string{"disposition": "held", "reason": "held", "materialization": "unattempted"}
	matchedStage := ""
	for _, raw := range []json.RawMessage{json.RawMessage(text), details} {
		var fields map[string]json.RawMessage
		if strictDecode(raw, &fields, callbackLimit) != nil || len(fields) != 4 {
			return ErrHeld
		}
		for key, want := range expected {
			var value string
			if strictDecode(fields[key], &value, callbackLimit) != nil || value != want {
				return ErrHeld
			}
		}
		var stage string
		if strictDecode(fields["stage"], &stage, callbackLimit) != nil || (stage != "caller_policy" && stage != "recipient_policy") || (matchedStage != "" && matchedStage != stage) {
			return ErrHeld
		}
		matchedStage = stage
	}
	return nil
}

func parseTeamConflictTerminalPolicyResult(raw json.RawMessage, nativeCallID, tool string) (*TeamConflictTerminalRefusal, error) {
	if nativeCallID == "" || !teamConflictTerminalPolicyTool(tool) {
		return nil, ErrHeld
	}
	var message map[string]json.RawMessage
	if strictDecode(raw, &message, recordLimit) != nil {
		return nil, ErrHeld
	}
	for key := range message {
		switch key {
		case "role", "toolCallId", "toolName", "isError", "content", "details", "durationMs", "timestamp":
		default:
			return nil, ErrHeld
		}
	}
	for key, want := range map[string]string{"role": "toolResult", "toolCallId": nativeCallID, "toolName": tool} {
		var value string
		if strictDecode(message[key], &value, recordLimit) != nil || value != want {
			return nil, ErrHeld
		}
	}
	var isError *bool
	if strictDecode(message["isError"], &isError, 32) != nil || isError == nil || *isError {
		return nil, ErrHeld
	}
	if err := teamConflictTerminalPolicyReply(message["content"], message["details"]); err != nil {
		return nil, err
	}
	return &TeamConflictTerminalRefusal{NativeToolCallID: nativeCallID, Tool: tool, MessageSHA256: bytesDigest(raw)}, nil
}

// Shared callback capture has already proved the complete linear history,
// Accepted input and native origin. This R-only projection additionally joins
// one following terminal result to the exact original call, never a selector.
func teamConflictTerminalResults(record conversationRecord, calls []TeamConflictCall, selector TeamConflictRecovery) error {
	file, err := os.Open(record.HistoryFile)
	if err != nil {
		return err
	}
	defer file.Close()
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 4096), recordLimit)
	type match struct {
		call, result int
		anchor       string
		message      json.RawMessage
		entryID      string
	}
	matched := map[string]*match{}
	for _, call := range calls {
		if teamConflictTerminalPolicyTool(call.Origin.Tool) {
			matched[call.Origin.NativeToolCallID] = &match{}
		}
	}
	for scan.Scan() {
		var fields map[string]json.RawMessage
		if strictDecode(scan.Bytes(), &fields, recordLimit) != nil {
			return ErrScope
		}
		var entry nativeEntry
		if json.Unmarshal(scan.Bytes(), &entry) != nil {
			return ErrScope
		}
		if entry.Message.Role == "assistant" {
			for _, block := range nativeBlocks(entry.Message.Content) {
				if block.Type == "toolCall" {
					if m := matched[block.ID]; m != nil {
						m.call++
						m.anchor = entry.ID
						if m.result != 0 {
							return ErrHeld
						}
					}
				}
			}
		}
		if entry.Message.Role == "toolResult" {
			if m := matched[entry.Message.ToolCallID]; m != nil {
				m.result++
				if m.call != 1 {
					return ErrHeld
				}
				m.message = append(json.RawMessage(nil), fields["message"]...)
				m.entryID = entry.ID
			}
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	for i := range calls {
		call := &calls[i]
		if !teamConflictTerminalPolicyTool(call.Origin.Tool) || call.Conversation.Member == selector.Member && call.MessageID == selector.OperationID {
			continue
		}
		m := matched[call.Origin.NativeToolCallID]
		if m == nil || m.call != 1 || m.result > 1 || m.anchor != call.Origin.AnchorEntryID {
			return ErrHeld
		}
		if m.result == 0 {
			continue
		}
		proof, err := parseTeamConflictTerminalPolicyResult(m.message, call.Origin.NativeToolCallID, call.Origin.Tool)
		if err != nil {
			continue
		} // Other outcomes still require their existing domain proof.
		proof.ResultEntryID = m.entryID
		call.TerminalRefusal = proof
	}
	return nil
}

func teamConflictPath(directory, id string) string {
	return filepath.Join(directory, "team-conflict-recoveries", id+".json")
}
func teamConflictUnloaded(before conversationRecord) conversationRecord {
	before = recoveryUnloaded(before)
	if before.HeldReason == "forwarded callback outcome unknown" {
		before.HeldReason = ""
	}
	return before
}

// The exact legacy output was reviewed with e61d3b7e and the retained composed
// deployment graph. Split the literal so it does not retain an old Nix closure.
func teamConflictLegacyImage() string {
	return strings.Join([]string{"/nix", "store", "g7wvpp2nc9cvsa8pdf1azfdib80kkcjm" + "-dev-workspace-0.2.0"}, "/")
}

func teamConflictPredecessorPackage(app string) error {
	known := teamConflictLegacyImage()
	data, err := os.ReadFile(filepath.Join(app, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return err
	}
	return teamConflictPredecessorMetadata(app, known, data)
}
func teamConflictPredecessorMetadata(app, legacy string, data []byte) error {
	if !storePattern.MatchString(app) {
		return ErrScope
	}
	marked, markerErr := teamConflictRecoveryContract(data)
	if app != legacy && (markerErr != nil || !marked) {
		return errors.Join(ErrUnavailable, markerErr)
	}
	var fields map[string]json.RawMessage
	if strictDecode(data, &fields, 1<<20) != nil {
		return ErrScope
	}
	for _, key := range []string{"hostCallbacks", "linearLineage", "workflowObservations", "piRoleInstructions", "piQuestionsSchema", "piPlanSchema"} {
		var n int
		if strictDecode(fields[key], &n, 32) != nil || n != 1 {
			return ErrScope
		}
	}
	var team map[string]int
	if strictDecode(fields["piTeamRecords"], &team, 4096) != nil || !reflect.DeepEqual(team, map[string]int{"policy": 1, "roster": 5, "monitorOperation": 2, "communication": 2, "progressObserver": 2, "utilityReceipt": 2}) {
		return ErrScope
	}
	return nil
}

type teamConflictReader struct {
	manager *Manager
	source  RuntimeRef
}

func (r teamConflictReader) call(ctx context.Context, op string, input, out any) error {
	records, err := r.manager.records(r.source.Session)
	if err != nil {
		return err
	}
	current := records.Requests[records.CurrentRequestID]
	if current.Runtime != r.source && (current.Phase != "preparing" || current.Start.ReplacesRequestID != r.source.RequestID || records.Requests[r.source.RequestID].Runtime != r.source) {
		return ErrScope
	}
	return r.manager.callRaw(ctx, r.source, op, input, out)
}
func (r teamConflictReader) InspectNativeOrigin(ctx context.Context, h agentbackend.ExecutionHandle, o NativeOriginRef) (NativeOriginObservation, error) {
	var result NativeOriginObservation
	if h.Generation != r.source.Generation || h.Conversation.Session != r.source.Session || validateNativeOrigin(o) != nil {
		return result, ErrScope
	}
	err := r.call(ctx, "inspect_origin", conversationRequest{Handle: h, Origin: &o}, &result)
	if err == nil && (result.Handle != h || result.Origin != o || len(result.Arguments) > callbackLimit || !json.Valid(result.Arguments)) {
		err = ErrScope
	}
	return result, err
}
func (r teamConflictReader) creation(ctx context.Context, record conversationRecord, input *SubmissionRef) (CreationProof, error) {
	var result CreationProof
	h := agentbackend.ExecutionHandle{Conversation: record.Conversation, Generation: r.source.Generation}
	err := r.call(ctx, "verify_creation", conversationRequest{Handle: h, CreationID: record.CreationID, HistoryRef: record.HistoryRef, Bootstrap: input}, &result)
	if err == nil && (result.Conversation != h.Conversation || result.Generation != h.Generation || result.CreationID != record.CreationID || result.HistoryRef != record.HistoryRef || result.InstructionsDigest != instructionsDigest(record.Instructions)) {
		err = ErrScope
	}
	return result, err
}
func (m *Manager) teamConflictLive(ctx context.Context, reader teamConflictReader, member LifecycleMember, record conversationRecord) error {
	if !record.Loaded {
		return ErrHeld
	}
	h := agentbackend.ExecutionHandle{Conversation: record.Conversation, Generation: reader.source.Generation}
	var snapshot workerSnapshot
	if err := reader.call(ctx, "snapshot", conversationRequest{Handle: h}, &snapshot); err != nil {
		return fmt.Errorf("snapshot read: %w", err)
	}
	held := record.HeldReason != ""
	if snapshot.NativeID != record.Conversation.NativeID || snapshot.HistoryFile != record.HistoryFile || snapshot.Persisted != record.Persisted || snapshot.Busy || snapshot.Held != held || snapshot.Model != record.Settings.Model || snapshot.Provider != record.Settings.Provider || snapshot.Thinking != record.Settings.Thinking || snapshot.InstructionsDigest != instructionsDigest(record.Instructions) {
		return ErrHeld
	}
	var observation WorkflowObservation
	if err := reader.call(ctx, "workflow_observe", toolPolicyRequest{Handle: h}, &observation); err != nil {
		return fmt.Errorf("workflow read: %w", err)
	}
	if observation.Handle != h || observation.Settings != record.Settings || !observation.Loaded || observation.Pending || observation.Held != held || held && observation.Idle || !held && !observation.Idle || record.ToolPolicy == nil || observation.Retained == nil || !sameToolPolicy(*observation.Retained, *record.ToolPolicy) || ValidateToolPolicyProof(observation.Proof, *record.ToolPolicy, h.Generation) != nil || !uuidPattern.MatchString(observation.AttachmentID) || observation.Sequence == 0 {
		return ErrHeld
	}
	if !record.Persisted {
		directory, err := stateDirectory(m.config.UserStateRoot, h.Conversation.Session)
		if err != nil {
			return err
		}
		root := filepath.Join(directory, "members", h.Conversation.Member, h.Conversation.Incarnation)
		if err := m.emptyLifecycleMember(ctx, reader.source, member, record, root, false); err != nil {
			return err
		}
		proof, err := reader.creation(ctx, record, nil)
		if err != nil || proof.Persisted || proof.Bootstrap != nil {
			return fmt.Errorf("empty creation read: %w", errors.Join(ErrHeld, err))
		}
	}
	return nil
}

func (m *Manager) captureTeamConflict(ctx context.Context, request UpgradeRequest, source RuntimeRef, hooks UpgradeHooks, live bool, previous *teamConflictJournal) (teamConflictJournal, error) {
	j := teamConflictJournal{Schema: 1, Phase: "prepared", Selector: *request.RecoverTeamConflict, Start: request.Start, Source: source, PreparedAt: time.Now().UTC()}
	if hooks.TeamConflict == nil {
		return j, ErrUnavailable
	}
	slots, err := hooks.TeamConflict.Slots(ctx, source)
	if err != nil {
		return j, err
	}
	j.Slots = slots
	records, err := m.records(source.Session)
	if err != nil {
		return j, err
	}
	j.Captured, err = m.captureLifecycleMembers(ctx, source, slots, records)
	if err != nil {
		return j, err
	}
	if len(j.Captured) == 0 || len(j.Captured) > 64 {
		return j, ErrScope
	}
	directory, err := stateDirectory(m.config.UserStateRoot, source.Session)
	if err != nil {
		return j, err
	}
	bound := map[string]agentbackend.ConversationRef{}
	reader := teamConflictReader{m, source}
	calls := []TeamConflictCall{}
	selectedHeld := false
	for _, member := range j.Captured {
		if member.Conversation == nil {
			if member.Slot.InitialInputID != "" || member.Slot.State != "creating" {
				return j, ErrHeld
			}
			continue
		}
		ref := *member.Conversation
		root := filepath.Join(directory, "members", ref.Member, ref.Incarnation)
		bound[root] = ref
		raw, err := readPrivateBytes(filepath.Join(root, "conversation.json"), recordLimit)
		if err != nil {
			return j, err
		}
		var record conversationRecord
		if strictDecode(raw, &record, recordLimit) != nil || record.Generation != source.Generation || record.PendingSettings != nil || record.PendingToolPolicy != nil || (record.HeldReason != "" && record.HeldReason != "forwarded callback outcome unknown") {
			return j, ErrHeld
		}
		if ref.Member == j.Selector.Member {
			selectedHeld = record.HeldReason == "forwarded callback outcome unknown"
		}
		held, err := (&receiptEngine{root: filepath.Join(root, "submissions")}).held()
		if err != nil || held {
			return j, ErrHeld
		}
		receiptEntries, err := settledDirectory(filepath.Join(root, "submissions"))
		if err != nil {
			return j, err
		}
		for _, entry := range receiptEntries {
			engine := receiptEngine{root: filepath.Join(root, "submissions")}
			status, err := engine.loadOwned(strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil || status.ref.retained == nil || status.ref.retained.Conversation != ref {
				return j, errors.Join(ErrScope, err)
			}
			raw, err := readPrivateBytes(filepath.Join(engine.root, entry.Name()), recordLimit)
			if err != nil {
				return j, err
			}
			j.Receipts = append(j.Receipts, teamConflictReceipt{*status.ref.retained, bytesDigest(raw)})
		}

		if member.Slot.State == "creating" || !record.Persisted {
			if err := m.emptyLifecycleMember(ctx, source, member, record, root, false); err != nil {
				return j, err
			}
		}

		if live {
			if err := m.teamConflictLive(ctx, reader, member, record); err != nil {
				return j, fmt.Errorf("team conflict member observation: %w", err)
			}
		}
		history := recoveryHistory{File: record.HistoryFile, Ref: record.HistoryRef}
		if record.Persisted {
			history, err = captureRecoveryHistory(record)
			if err != nil {
				return j, err
			}
			if live {
				var snapshot workerSnapshot
				if err := reader.call(ctx, "snapshot", conversationRequest{Handle: agentbackend.ExecutionHandle{Conversation: ref, Generation: source.Generation}}, &snapshot); err != nil || snapshot.HistoryDigest != history.Digest || snapshot.LeafID != history.LeafID {
					return j, errors.Join(ErrHeld, err)
				}
			}
			captured, err := m.captureHistoryCallbacks(ctx, source, record, records, live, true, reader)
			if err != nil {
				return j, err
			}
			if m.teamConflictTerminalProducer(records.Requests[source.RequestID].Start.AppStorePath) == nil {
				// A terminal reply describes one invocation. A matching prior operation
				// receipt still owns settlement, including after a same-ID refused retry.
				candidates := []TeamConflictCall{}
				indexes := []int{}
				for i, call := range captured {
					if !teamConflictTerminalPolicyTool(call.Origin.Tool) {
						continue
					}
					for _, target := range j.Captured {
						if target.Conversation == nil || target.Slot.Member != call.To {
							continue
						}
						ref := *target.Conversation
						engine := receiptEngine{root: filepath.Join(directory, "members", ref.Member, ref.Incarnation, "submissions")}
						if _, err := engine.loadOwned(call.MessageID); errors.Is(err, os.ErrNotExist) {
							candidates = append(candidates, call)
							indexes = append(indexes, i)
						}
						break
					}
				}
				if len(candidates) != 0 {
					if err := teamConflictTerminalResults(record, candidates, *request.RecoverTeamConflict); err != nil {
						return j, err
					}
					for i, index := range indexes {
						captured[index].TerminalRefusal = candidates[i].TerminalRefusal
					}
				}
			}
			calls = append(calls, captured...)
		}
		j.Members = append(j.Members, recoveryMember{Before: string(raw), BeforeSHA256: bytesDigest(raw), History: history})
	}
	if !selectedHeld {
		return j, ErrHeld
	}
	if err := m.terminalLifecycleOtherOwners(ctx, records, bound); err != nil {
		return j, err
	}
	matched := false
	type messageKey struct {
		Sender agentbackend.ConversationRef
		ID     string
	}
	seenMessages := map[messageKey]TeamConflictCall{}
	for _, call := range calls {
		proof, err := hooks.TeamConflict.Settle(ctx, source, call, j.Captured)
		if err != nil || proof == nil {
			return j, errors.Join(ErrHeld, err)
		}
		evidence := proof.TeamConflictEvidence()
		if !hexPattern.MatchString(evidence.RecordsSHA256) {
			return j, ErrScope
		}
		if call.Origin.Tool == "team_message" || call.Origin.Tool == "report_to_lead" {
			key := messageKey{call.Conversation, call.MessageID}
			if prior, ok := seenMessages[key]; ok && (prior.To != call.To || !sameCallbackArguments(prior.Arguments, call.Arguments)) {
				return j, ErrConflict
			}
			seenMessages[key] = call
			target, ok := teamConflictMember(j, evidence.Target)
			if !ok || evidence.Target.Member != call.To {
				return j, ErrScope
			}
			receipt := SubmissionRef{Conversation: evidence.Target, OperationID: call.MessageID, Action: "send"}
			engine := receiptEngine{root: filepath.Join(directory, "members", evidence.Target.Member, evidence.Target.Incarnation, "submissions")}
			original, readErr := engine.loadOwned(call.MessageID)
			switch evidence.Kind {
			case "terminal_policy_refused":
				if m.teamConflictTerminalProducer(records.Requests[source.RequestID].Start.AppStorePath) != nil || !call.HasTerminalPolicyRefusal() || call.Conversation.Member == j.Selector.Member && call.MessageID == j.Selector.OperationID || !reflect.DeepEqual(call.TerminalRefusal, evidence.TerminalRefusal) || !errors.Is(readErr, os.ErrNotExist) || evidence.InitialInputID != target.Slot.InitialInputID {
					return j, ErrHeld
				}
			case "initial_conflict", "created_empty":
				if !errors.Is(readErr, os.ErrNotExist) || target.Slot.State != "creating" || target.Slot.InitialInputID == "" || evidence.InitialInputID != target.Slot.InitialInputID {
					return j, ErrHeld
				}
				if evidence.Kind == "initial_conflict" {
					if evidence.InitialInputID == call.MessageID {
						return j, ErrHeld
					}
					if call.Conversation.Member == j.Selector.Member && call.MessageID == j.Selector.OperationID {
						matched = true
					}
				} else if evidence.InitialInputID != call.MessageID || target.Conversation == nil || teamConflictPersisted(j, evidence.Target) {
					return j, ErrHeld
				}
			case "accepted", "not_submitted":
				if readErr != nil || original.ref.retained == nil || original.ref.retained.Conversation != evidence.Target || original.Input != evidence.Input {
					return j, ErrHeld
				}
				receipt.Digest = SubmissionDigest(receipt, evidence.Input, original.Settings)
				if *original.ref.retained != receipt || evidence.Kind == "accepted" && (original.Phase != "Accepted" || original.Witness == nil) || evidence.Kind == "not_submitted" && (original.Phase != "NotSubmitted" || original.Witness != nil) {
					return j, ErrHeld
				}
				evidence.Receipts = append(evidence.Receipts, TeamConflictSubmission{Ref: receipt, Input: evidence.Input})
			default:
				return j, ErrHeld
			}
		} else if (call.Origin.Tool != "publish_plan" || evidence.Kind != "plan_publication") && (call.Origin.Tool != "request_user_input_async" || evidence.Kind != "question_publication") {
			return j, ErrHeld
		}
		for _, expected := range evidence.Receipts {
			ref := expected.Ref
			member, ok := teamConflictMember(j, ref.Conversation)
			if !ok || member.Conversation == nil {
				return j, ErrScope
			}
			path := filepath.Join(directory, "members", ref.Conversation.Member, ref.Conversation.Incarnation, "submissions", ref.OperationID+".json")
			engine := receiptEngine{root: filepath.Dir(path)}
			status, err := engine.loadOwned(ref.OperationID)
			if err != nil || status.ref.retained == nil || *status.ref.retained != ref || status.Input != expected.Input || (status.Phase != "Accepted" && status.Phase != "NotSubmitted") || status.Phase == "Accepted" && status.Witness == nil || status.Phase == "NotSubmitted" && status.Witness != nil {
				return j, ErrHeld
			}
			if live && status.Phase == "Accepted" {
				var mapping conversationRecord
				readPrivate(filepath.Join(filepath.Dir(engine.root), "conversation.json"), &mapping)
				proof, err := reader.creation(ctx, mapping, &ref)
				if err != nil || proof.Bootstrap == nil || !reflect.DeepEqual(*proof.Bootstrap, status.retained()) {
					return j, errors.Join(ErrHeld, err)
				}
			}
			raw, err := readPrivateBytes(path, recordLimit)
			if err != nil {
				return j, err
			}
			j.Receipts = append(j.Receipts, teamConflictReceipt{ref, bytesDigest(raw)})
		}
		j.Calls = append(j.Calls, call.LifecycleCallback)
		j.Domains = append(j.Domains, teamConflictSettlement{Kind: evidence.Kind, RecordsSHA256: evidence.RecordsSHA256, EvidenceSHA256: digestValue(evidence), Target: evidence.Target, InitialInputID: evidence.InitialInputID})
	}
	if !matched {
		return j, ErrHeld
	}
	for _, member := range j.Members {
		path := filepath.Join(directory, "members", teamConflictRef(member).Member, teamConflictRef(member).Incarnation, "conversation.json")
		raw, err := readPrivateBytes(path, recordLimit)
		if err != nil || bytesDigest(raw) != member.BeforeSHA256 {
			return j, errors.Join(ErrConflict, err)
		}
		if teamConflictPersisted(j, teamConflictRef(member)) {
			if err := verifyRecoveryHistory(member); err != nil {
				return j, err
			}
		}
	}
	for _, receipt := range j.Receipts {
		raw, err := readPrivateBytes(filepath.Join(directory, "members", receipt.Ref.Conversation.Member, receipt.Ref.Conversation.Incarnation, "submissions", receipt.Ref.OperationID+".json"), recordLimit)
		if err != nil || bytesDigest(raw) != receipt.SHA256 {
			return j, errors.Join(ErrConflict, err)
		}
	}
	finalSlots, err := hooks.TeamConflict.Slots(ctx, source)
	if err != nil || !reflect.DeepEqual(finalSlots, j.Slots) {
		return j, errors.Join(ErrConflict, err)
	}
	finalRecords, err := m.records(source.Session)
	if err != nil || !reflect.DeepEqual(finalRecords, records) {
		return j, errors.Join(ErrConflict, err)
	}
	if previous != nil {
		j.PreparedAt = previous.PreparedAt
		j.Successor = previous.Successor
		j.SourceHost = previous.SourceHost
		if !reflect.DeepEqual(j, *previous) {
			return j, ErrConflict
		}
	}
	return j, nil
}
func teamConflictRef(member recoveryMember) agentbackend.ConversationRef {
	var r conversationRecord
	json.Unmarshal([]byte(member.Before), &r)
	return r.Conversation
}
func teamConflictMember(j teamConflictJournal, ref agentbackend.ConversationRef) (LifecycleMember, bool) {
	for _, m := range j.Captured {
		if m.Conversation != nil && *m.Conversation == ref {
			return m, true
		}
	}
	return LifecycleMember{}, false
}
func teamConflictPersisted(j teamConflictJournal, ref agentbackend.ConversationRef) bool {
	for _, m := range j.Members {
		if teamConflictRef(m) == ref {
			var r conversationRecord
			json.Unmarshal([]byte(m.Before), &r)
			return r.Persisted
		}
	}
	return false
}

func (m *Manager) upgradeTeamConflict(ctx context.Context, request UpgradeRequest, hooks UpgradeHooks) (UpgradeResult, error) {
	return m.runTeamConflictRecovery(ctx, request, hooks)
}

func teamConflictEmpty(j *teamConflictJournal, ref agentbackend.ConversationRef) bool {
	if j == nil || (j.Phase != "settled" && j.Phase != "complete") {
		return false
	}
	member, ok := teamConflictMember(*j, ref)
	return ok && member.Slot.State == "creating" && !teamConflictPersisted(*j, ref)
}

// An originally empty locator names no persisted history. Only its sealed
// successor may replan it; persisted owners keep their exact file identity.
func teamConflictHistoryMatches(j *teamConflictJournal, before, actual conversationRecord, root string) bool {
	if !teamConflictEmpty(j, before.Conversation) {
		return before.HistoryFile == actual.HistoryFile
	}
	owned := func(path string) bool {
		return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Dir(path) == filepath.Join(root, "history")
	}
	return actual.Generation == j.Successor.Generation && owned(before.HistoryFile) && owned(actual.HistoryFile) && validateConversationRecord(actual, before.Conversation, root) == nil
}
func sameTeamConflictUpgradeMember(j *teamConflictJournal, before, actual conversationRecord, root string) bool {
	if !teamConflictHistoryMatches(j, before, actual, root) {
		return false
	}
	actual.HistoryFile = before.HistoryFile
	return sameUpgradeMember(before, actual)
}

func (m *Manager) verifyTeamConflictEmpty(ctx context.Context, j *teamConflictJournal, ref agentbackend.ConversationRef, actual conversationRecord, root string, live bool) error {
	if !teamConflictEmpty(j, ref) {
		return ErrScope
	}
	saved, ok := teamConflictMember(*j, ref)
	if !ok {
		return ErrScope
	}
	for _, member := range j.Members {
		if teamConflictRef(member) != ref {
			continue
		}
		var before conversationRecord
		if strictDecode([]byte(member.Before), &before, recordLimit) != nil || !sameTeamConflictUpgradeMember(j, before, actual, root) {
			return ErrConflict
		}
		if _, err := os.Lstat(before.HistoryFile); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(ErrHeld, err)
		}
		// Rebind only the proof's generation, keeping its original reservation,
		// native identity, settings and history reference sealed in the journal.
		saved.Generation = j.Successor.Generation
		return m.emptyLifecycleMember(ctx, j.Successor, saved, actual, root, live)
	}
	return ErrScope
}

func (m *Manager) checkTeamConflictMapping(ctx context.Context, directory string, member recoveryMember, j teamConflictJournal, after bool) error {
	var before, actual conversationRecord
	if strictDecode([]byte(member.Before), &before, recordLimit) != nil || bytesDigest([]byte(member.Before)) != member.BeforeSHA256 {
		return ErrScope
	}
	root := filepath.Join(directory, "members", before.Conversation.Member, before.Conversation.Incarnation)
	raw, err := readPrivateBytes(filepath.Join(root, "conversation.json"), recordLimit)
	if err != nil {
		return err
	}
	if string(raw) == member.Before {
		return nil
	}
	if !after || strictDecode(raw, &actual, recordLimit) != nil || validateConversationRecord(actual, before.Conversation, root) != nil {
		return ErrConflict
	}
	expected := teamConflictUnloaded(before)
	if equalJSON(actual, expected) {
		return nil
	}
	if actual.Generation != j.Successor.Generation || !actual.Loaded || !teamConflictHistoryMatches(&j, before, actual, root) {
		return ErrConflict
	}
	if teamConflictEmpty(&j, before.Conversation) {
		if err := m.verifyTeamConflictEmpty(ctx, &j, before.Conversation, actual, root, false); err != nil {
			return err
		}
	}
	actual.HistoryFile = expected.HistoryFile
	actual.Generation = expected.Generation
	actual.Loaded = expected.Loaded
	actual.WorkerPID = expected.WorkerPID
	actual.WorkerStart = expected.WorkerStart
	actual.WorkerTopology = expected.WorkerTopology
	if !equalJSON(actual, expected) {
		return ErrConflict
	}
	return nil
}
func readTeamConflictJournal(directory, id string) (teamConflictJournal, error) {
	var j teamConflictJournal
	if !uuidPattern.MatchString(id) {
		return j, ErrScope
	}
	if err := readPrivate(teamConflictPath(directory, id), &j); err != nil {
		return j, err
	}
	if j.Schema != 1 || !j.Selector.Valid() || j.Start.RequestID != id || j.Start.ReplacesRequestID != j.Source.RequestID || j.Source.Session != j.Start.Session || j.Successor.Session != j.Source.Session || j.Successor.RequestID != id || j.Source.Generation == j.Successor.Generation || j.PreparedAt.IsZero() || len(j.Members) == 0 || len(j.Members) > 64 || len(j.Calls) == 0 || len(j.Calls) != len(j.Domains) || (j.Phase != "prepared" && j.Phase != "settled" && j.Phase != "complete") || j.Phase == "prepared" && j.Stop != nil || j.Phase != "prepared" && j.Stop == nil {
		return j, ErrConflict
	}
	if j.SourceHost == nil || !sameTeamConflictHost(*j.SourceHost, *j.SourceHost) || j.SourceHost.Unit == "" || !reflect.DeepEqual(*j.SourceHost, teamConflictHostIdentity(*j.SourceHost)) {
		return j, ErrScope
	}
	seen := map[agentbackend.ConversationRef]bool{}
	for _, member := range j.Members {
		var r conversationRecord
		if strictDecode([]byte(member.Before), &r, recordLimit) != nil || bytesDigest([]byte(member.Before)) != member.BeforeSHA256 || r.Conversation.Session != j.Source.Session || r.Generation != j.Source.Generation || seen[r.Conversation] || r.PendingSettings != nil || r.PendingToolPolicy != nil || (r.HeldReason != "" && r.HeldReason != "forwarded callback outcome unknown") {
			return j, ErrConflict
		}
		seen[r.Conversation] = true
	}
	selected := false
	if len(j.Captured) != len(j.Slots) || len(j.Captured) == 0 || len(j.Captured) > 64 {
		return j, ErrScope
	}
	capturedRefs := map[agentbackend.ConversationRef]bool{}
	for i, captured := range j.Captured {
		if !reflect.DeepEqual(captured.Slot, j.Slots[i]) {
			return j, ErrScope
		}
		if captured.Conversation != nil {
			ref := *captured.Conversation
			if !seen[ref] || capturedRefs[ref] || captured.Generation != j.Source.Generation || captured.Settings == nil || validateHandle(agentbackend.ExecutionHandle{Conversation: ref, Generation: j.Source.Generation}) != nil {
				return j, ErrScope
			}
			capturedRefs[ref] = true
		}
	}
	if len(capturedRefs) != len(seen) {
		return j, ErrScope
	}
	for i, call := range j.Calls {
		evidence := j.Domains[i]
		if !seen[call.Conversation] || call.Input.Conversation != call.Conversation || call.Input.Action != "send" || !operationPattern.MatchString(call.Input.OperationID) || !hexPattern.MatchString(call.Input.Digest) || validateNativeOrigin(call.Origin) != nil || !hexPattern.MatchString(call.ArgumentsSHA256) || !hexPattern.MatchString(evidence.RecordsSHA256) || !hexPattern.MatchString(evidence.EvidenceSHA256) {
			return j, ErrScope
		}
		if call.Origin.Tool == "team_message" || call.Origin.Tool == "report_to_lead" {
			if !seen[evidence.Target] || evidence.Target.Member != call.To || !operationPattern.MatchString(call.MessageID) {
				return j, ErrScope
			}
			switch evidence.Kind {
			case "terminal_policy_refused":
				if !teamConflictTerminalPolicyTool(call.Origin.Tool) || call.Conversation.Member == j.Selector.Member && call.MessageID == j.Selector.OperationID {
					return j, ErrScope
				}
			case "initial_conflict", "created_empty", "accepted", "not_submitted":
			default:
				return j, ErrScope
			}
			if call.Conversation.Member == j.Selector.Member && call.MessageID == j.Selector.OperationID && evidence.Kind == "initial_conflict" {
				selected = true
			}
		} else if (call.Origin.Tool != "publish_plan" || evidence.Kind != "plan_publication") && (call.Origin.Tool != "request_user_input_async" || evidence.Kind != "question_publication") {
			return j, ErrScope
		}
	}
	if !selected {
		return j, ErrScope
	}
	for _, r := range j.Receipts {
		if !hexPattern.MatchString(r.SHA256) || r.Ref.Conversation.Session != j.Source.Session || !operationPattern.MatchString(r.Ref.OperationID) || !hexPattern.MatchString(r.Ref.Digest) {
			return j, ErrScope
		}
	}
	return j, nil
}
func (m *Manager) runTeamConflictRecovery(ctx context.Context, request UpgradeRequest, hooks UpgradeHooks) (UpgradeResult, error) {
	result := UpgradeResult{AppStorePath: request.Start.AppStorePath, Phase: "preparing"}
	if request.RecoverTeamConflict == nil || !request.RecoverTeamConflict.Valid() || request.RetireUnknown != nil || hooks.TeamConflict == nil {
		return result, ErrScope
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
	previous, ok := records.Requests[request.Start.ReplacesRequestID]
	if !ok || previous.Runtime.Generation != request.ExpectedGeneration || previous.Phase != "starting" {
		return result, ErrScope
	}
	contract := m.config.teamConflictPredecessorContract
	if contract == nil {
		contract = teamConflictPredecessorPackage
	}
	if err := contract(previous.Start.AppStorePath); err != nil {
		return result, fmt.Errorf("team conflict predecessor contract: %w", err)
	}
	if err := m.config.validatePackage(request.Start.AppStorePath); err != nil {
		return result, err
	}
	targetContract := m.config.teamConflictRecoveryContract
	if targetContract == nil {
		targetContract = TeamConflictRecoveryPackage
	}
	if supported, err := targetContract(request.Start.AppStorePath); err != nil || !supported {
		return result, errors.Join(ErrUnavailable, err)
	}
	stopper, ok := m.config.Host.(hostStopper)
	if !ok {
		return result, ErrUnavailable
	}
	current, reserved := records.Requests[request.Start.RequestID]
	if reserved {
		if records.CurrentRequestID != current.Runtime.RequestID || current.Start != request.Start {
			return result, ErrConflict
		}
	} else if records.CurrentRequestID != previous.Runtime.RequestID {
		return result, ErrConflict
	}
	j, savedErr := readTeamConflictJournal(directory, request.Start.RequestID)
	saved := savedErr == nil
	if savedErr != nil && !errors.Is(savedErr, os.ErrNotExist) {
		return result, savedErr
	}
	if saved && (j.Start != request.Start || j.Selector != *request.RecoverTeamConflict || j.Source != previous.Runtime || !reserved || j.Successor != current.Runtime || j.SourceHost.LaunchRequest != launchFor(previous.Start, previous.Runtime) || j.SourceHost.Digest != previous.Runtime.Digest) {
		return result, ErrConflict
	}
	if saved && teamConflictUsesTerminalRefusal(j) {
		if m.teamConflictTerminalProducer(previous.Start.AppStorePath) != nil {
			return result, ErrHeld
		}
		if err := m.requireTeamConflictTerminalReader(request.Start.AppStorePath); err != nil {
			return result, err
		}
	}
	if !saved {
		pending, err := settledDirectory(filepath.Join(directory, "team-conflict-recoveries"))
		if err != nil {
			return result, err
		}
		for _, entry := range pending {
			old, err := readTeamConflictJournal(directory, strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil || old.Phase != "complete" {
				return result, errors.Join(ErrHeld, err)
			}
		}
		if reserved && current.Phase != "preparing" {
			return result, ErrConflict
		}
		var connector HostCallbackConnector = m
		if reserved {
			lifetime, cancel := context.WithCancel(ctx)
			old := &upgradePredecessorConnector{manager: m, current: current.Runtime, previous: previous.Runtime, lifetime: lifetime, cancel: cancel}
			defer old.close()
			connector = old
		}
		if err := hooks.EnsureAttachment(ctx, previous.Runtime, connector); err != nil {
			return result, err
		}
		var sourceStatus RuntimeStatus
		if reserved {
			sourceStatus, err = m.upgradePredecessorStatus(ctx, current.Runtime, previous.Runtime)
		} else {
			sourceStatus, err = m.Inspect(ctx, previous.Runtime)
		}
		if err != nil || sourceStatus.Phase != "ready" {
			return result, errors.Join(ErrHeld, err)
		}
		j, err = m.captureTeamConflict(ctx, request, previous.Runtime, hooks, true, nil)
		if err != nil {
			return result, fmt.Errorf("team conflict preflight: %w", err)
		}
		if teamConflictUsesTerminalRefusal(j) {
			if err := m.requireTeamConflictTerminalReader(request.Start.AppStorePath); err != nil {
				return result, err
			}
		}
		identity := teamConflictHostIdentity(sourceStatus.Observation)
		j.SourceHost = &identity
		refs := []agentbackend.ConversationRef{}
		for _, member := range j.Captured {
			if member.Conversation != nil && member.Slot.State != "creating" {
				refs = append(refs, *member.Conversation)
			}
		}
		if !reflect.DeepEqual(refs, request.Members) {
			return result, ErrScope
		}
		if err := m.recoveryPoint(hooks, "team-preflight"); err != nil {
			return result, err
		}
		if !reserved {
			descriptor, err := readPrivateBytes(filepath.Join(m.instance(previous.Runtime), "launch.json"), 65536)
			if err != nil {
				return result, err
			}
			current, err = m.reserveSuccessorRecord(request.Start, records, previous, descriptor, func(candidate runtimeRecord) error { return teamConflictJournalSize(j, candidate.Runtime) })
			if err != nil {
				return result, err
			}
		}
		if err := teamConflictJournalSize(j, current.Runtime); err != nil {
			return result, err
		}
		result.Runtime = current.Runtime
		if err := m.recoveryPoint(hooks, "team-reservation"); err != nil {
			return result, err
		}
		if err := m.retainRuntimeRequest(ctx, previous); err != nil {
			return result, err
		}
		if err := m.retainRuntimeRequest(ctx, current); err != nil {
			return result, err
		}
		j.Successor = current.Runtime
		if err := writePrivate(teamConflictPath(directory, request.Start.RequestID), j); err != nil {
			return result, err
		}
		if err := m.recoveryPoint(hooks, "team-prepared"); err != nil {
			return result, err
		}
	}
	result.Runtime = current.Runtime
	result.Previous = previous.Runtime
	if j.Phase == "prepared" {
		status, err := m.upgradePredecessorStatus(ctx, current.Runtime, previous.Runtime)
		if err != nil {
			return result, err
		}
		if !sameTeamConflictHost(*j.SourceHost, status.Observation) || j.SourceHost.LaunchRequest != launchFor(previous.Start, previous.Runtime) || j.SourceHost.Digest != previous.Runtime.Digest {
			return result, ErrHeld
		}
		if err := teamConflictJournalSize(j, current.Runtime); err != nil {
			return result, err
		}
		if status.Phase != "stopped" {
			if status.Phase != "ready" {
				return result, ErrHeld
			}
			if _, err := m.captureTeamConflict(ctx, request, previous.Runtime, hooks, true, &j); err != nil {
				return result, err
			}
			if err := m.recoveryPoint(hooks, "team-before-stop"); err != nil {
				return result, err
			}
			observed, err := stopper.Stop(ctx, previous.Runtime, status.Observation)
			if err != nil {
				return result, err
			}
			status.Observation = observed
			if err := m.recoveryPoint(hooks, "team-stop"); err != nil {
				return result, err
			}
		}
		if !validTerminalReceipt(previous, status.Observation) || !sameTeamConflictHost(*j.SourceHost, status.Observation) {
			return result, ErrHeld
		}
		status.Observation = teamConflictStopEvidence(status.Observation)
		if !validTerminalReceipt(previous, status.Observation) {
			return result, ErrHeld
		}
		if err := m.config.verifyDrain(status.Observation); err != nil {
			return result, err
		}
		if err := m.recoveryPoint(hooks, "team-drain"); err != nil {
			return result, err
		}
		if _, err := m.captureTeamConflict(ctx, request, previous.Runtime, hooks, false, &j); err != nil {
			return result, fmt.Errorf("team conflict final domain/history proof: %w", err)
		}
		j.Phase = "settled"
		j.Stop = &status.Observation
		if err := writePrivate(teamConflictPath(directory, request.Start.RequestID), j); err != nil {
			return result, err
		}
		if err := m.recoveryPoint(hooks, "team-settled"); err != nil {
			return result, err
		}
	}
	if j.Stop == nil || !sameTeamConflictHost(*j.SourceHost, *j.Stop) {
		return result, ErrHeld
	}
	if j.Stop == nil || !validTerminalReceipt(previous, *j.Stop) || m.config.verifyDrain(*j.Stop) != nil {
		return result, ErrHeld
	}
	published, err := hooks.TeamConflict.Published(ctx, j.Successor)
	if err != nil {
		return result, err
	}
	if published {
		if err := m.verifyTeamConflictCompletion(ctx, directory, j); err != nil {
			return result, err
		}
		if j.Phase != "complete" {
			j.Phase = "complete"
			if err := writePrivate(teamConflictPath(directory, request.Start.RequestID), j); err != nil {
				return result, err
			}
		}
		return UpgradeResult{Runtime: j.Successor, Previous: j.Source, AppStorePath: j.Start.AppStorePath, Phase: "ready", Disposition: "recovered-team-conflict", TeamMessageID: j.Selector.OperationID}, nil
	}
	if j.Phase == "complete" {
		return result, ErrConflict
	}

	for _, member := range j.Members {
		if err := m.checkTeamConflictMapping(ctx, directory, member, j, true); err != nil {
			return result, err
		}
		var before, actual conversationRecord
		strictDecode([]byte(member.Before), &before, recordLimit)
		path := filepath.Join(directory, "members", before.Conversation.Member, before.Conversation.Incarnation, "conversation.json")
		if err := readPrivate(path, &actual); err != nil {
			return result, err
		}
		if actual.Generation == previous.Runtime.Generation {
			if before.Persisted {
				if err := verifyRecoveryHistory(member); err != nil {
					return result, err
				}
			}
			if err := writePrivate(path, teamConflictUnloaded(before)); err != nil {
				return result, err
			}
		}
		if err := m.recoveryPoint(hooks, "team-mapping:"+before.Conversation.Member); err != nil {
			return result, err
		}
	}
	for _, receipt := range j.Receipts {
		raw, err := readPrivateBytes(filepath.Join(directory, "members", receipt.Ref.Conversation.Member, receipt.Ref.Conversation.Incarnation, "submissions", receipt.Ref.OperationID+".json"), recordLimit)
		if err != nil || bytesDigest(raw) != receipt.SHA256 {
			return result, errors.Join(ErrConflict, err)
		}
	}
	unlock()
	unlock = nil
	ordinary := request
	ordinary.RecoverTeamConflict = nil
	ordinary.teamConflict = &j
	ordinary.Members = nil
	for _, member := range j.Captured {
		if member.Conversation != nil {
			ordinary.Members = append(ordinary.Members, *member.Conversation)
		}
	}
	result, err = m.Upgrade(ctx, ordinary, hooks)
	if err != nil {
		return result, err
	}
	if err := m.recoveryPoint(hooks, "team-publication"); err != nil {
		return result, err
	}
	j.Phase = "complete"
	if err := writePrivate(teamConflictPath(directory, request.Start.RequestID), j); err != nil {
		return result, err
	}
	result.Disposition = "recovered-team-conflict"
	result.TeamMessageID = request.RecoverTeamConflict.OperationID
	return result, nil
}

// TeamConflictRecoveryPackage is independent of input retirement's older codec.
func TeamConflictRecoveryPackage(app string) (bool, error) {
	if !storePattern.MatchString(app) {
		return false, ErrScope
	}
	data, err := os.ReadFile(filepath.Join(app, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return false, err
	}
	return teamConflictRecoveryContract(data)
}
func teamConflictRecoveryContract(data []byte) (bool, error) {
	var fields map[string]json.RawMessage
	if strictDecode(data, &fields, 1<<20) != nil || fields == nil {
		return false, ErrScope
	}
	raw, ok := fields["piTeamConflictRecovery"]
	if !ok {
		return false, nil
	}
	var version int
	if strictDecode(raw, &version, 32) != nil || version != 1 {
		return false, ErrScope
	}
	return true, nil
}

func teamConflictTerminalProducerPackage(app string) error {
	if app != teamConflictLegacyImage() {
		return ErrHeld
	}
	return nil
}
func (m *Manager) teamConflictTerminalProducer(app string) error {
	if m.config.teamConflictTerminalProducerContract != nil {
		return m.config.teamConflictTerminalProducerContract(app)
	}
	return teamConflictTerminalProducerPackage(app)
}

func teamConflictUsesTerminalRefusal(j teamConflictJournal) bool {
	for _, domain := range j.Domains {
		if domain.Kind == "terminal_policy_refused" {
			return true
		}
	}
	return false
}
func (m *Manager) requireTeamConflictTerminalReader(app string) error {
	contract := m.config.teamConflictTerminalRefusalContract
	if contract == nil {
		contract = TeamConflictTerminalRefusalPackage
	}
	supported, err := contract(app)
	if err != nil || !supported {
		return errors.Join(ErrUnavailable, err)
	}
	return nil
}

// TeamConflictTerminalRefusalPackage is a reader capability, never producer proof.
func TeamConflictTerminalRefusalPackage(app string) (bool, error) {
	if !storePattern.MatchString(app) {
		return false, ErrScope
	}
	data, err := os.ReadFile(filepath.Join(app, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return false, err
	}
	supported, err := teamConflictTerminalRefusalContract(data)
	if err != nil || !supported {
		return supported, err
	}
	base, err := teamConflictRecoveryContract(data)
	if err != nil || !base {
		return false, errors.Join(ErrScope, err)
	}
	return true, nil
}
func teamConflictTerminalRefusalContract(data []byte) (bool, error) {
	var fields map[string]json.RawMessage
	if strictDecode(data, &fields, 1<<20) != nil || fields == nil {
		return false, ErrScope
	}
	raw, ok := fields["piTeamConflictTerminalRefusal"]
	if !ok {
		return false, nil
	}
	var version int
	if strictDecode(raw, &version, 32) != nil || version != 1 {
		return false, ErrScope
	}
	return true, nil
}

// Retained inspection never consults a supervisor or infers settlement. It
// validates every session journal and joins it to its original runtime records.
type teamConflictRetention struct{ Recovery, TerminalRefusal bool }

func retainedTeamConflictMembers(ctx context.Context, stateRoot, directory string, session agentbackend.SessionRef) (map[agentbackend.ConversationRef]teamConflictRetention, error) {
	result := map[agentbackend.ConversationRef]teamConflictRetention{}
	entries, err := settledDirectory(filepath.Join(directory, "team-conflict-recoveries"))
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return result, nil
	}
	if _, err := InspectRetainedCurrentRuntime(ctx, stateRoot, session); err != nil {
		return nil, err
	}
	var records runtimeRecords
	if err := readPrivate(filepath.Join(directory, "runtime.json"), &records); err != nil || records.Schema != 2 {
		return nil, errors.Join(ErrScope, err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return nil, ErrScope
		}
		j, err := readTeamConflictJournal(directory, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		source, sourceOK := records.Requests[j.Source.RequestID]
		successor, successorOK := records.Requests[j.Successor.RequestID]
		if !sourceOK || !successorOK || source.Runtime != j.Source || successor.Runtime != j.Successor || successor.Start != j.Start || j.Source.Session != session {
			return nil, ErrScope
		}
		if j.SourceHost.LaunchRequest != launchFor(source.Start, source.Runtime) || j.SourceHost.Digest != source.Runtime.Digest || j.Stop != nil && !sameTeamConflictHost(*j.SourceHost, *j.Stop) {
			return nil, ErrScope
		}
		if j.Stop != nil && !validTerminalReceipt(source, *j.Stop) {
			return nil, ErrScope
		}
		for _, member := range j.Members {
			var before, actual conversationRecord
			if strictDecode([]byte(member.Before), &before, recordLimit) != nil {
				return nil, ErrScope
			}
			root := filepath.Join(directory, "members", before.Conversation.Member, before.Conversation.Incarnation)
			if validateConversationRecord(before, before.Conversation, root) != nil {
				return nil, ErrScope
			}
			if err := readPrivate(filepath.Join(root, "conversation.json"), &actual); err != nil {
				return nil, err
			}
			if actual.Conversation != before.Conversation || actual.CreationID != before.CreationID || actual.HistoryRef != before.HistoryRef {
				return nil, ErrConflict
			}

			dependency := result[before.Conversation]
			dependency.Recovery = true
			dependency.TerminalRefusal = dependency.TerminalRefusal || teamConflictUsesTerminalRefusal(j)
			result[before.Conversation] = dependency
		}
	}
	return result, nil
}

// Completed replay is observational. Legitimate later work may append history or
// hold the conversation again; it cannot invalidate the original saved prefix.
func (m *Manager) verifyTeamConflictCompletion(ctx context.Context, directory string, j teamConflictJournal) error {
	for _, member := range j.Members {
		var before, actual conversationRecord
		if strictDecode([]byte(member.Before), &before, recordLimit) != nil {
			return ErrScope
		}
		root := filepath.Join(directory, "members", before.Conversation.Member, before.Conversation.Incarnation)
		if err := readPrivate(filepath.Join(root, "conversation.json"), &actual); err != nil {
			return err
		}
		if validateConversationRecord(actual, before.Conversation, root) != nil || actual.Conversation != before.Conversation || actual.CreationID != before.CreationID || actual.HistoryRef != before.HistoryRef || !teamConflictHistoryMatches(&j, before, actual, root) || actual.Instructions != before.Instructions || actual.InitialSettings != before.InitialSettings || actual.Generation != j.Successor.Generation {
			return ErrConflict
		}
		if before.Persisted {
			file, err := os.Open(before.HistoryFile)
			if err != nil {
				return err
			}
			hash := sha256.New()
			n, readErr := io.CopyN(hash, file, member.History.Bytes)
			file.Close()
			if readErr != nil || n != member.History.Bytes || hex.EncodeToString(hash.Sum(nil)) != member.History.SHA256 {
				return ErrConflict
			}
		}
	}
	for _, receipt := range j.Receipts {
		raw, err := readPrivateBytes(filepath.Join(directory, "members", receipt.Ref.Conversation.Member, receipt.Ref.Conversation.Incarnation, "submissions", receipt.Ref.OperationID+".json"), recordLimit)
		if err != nil || bytesDigest(raw) != receipt.SHA256 {
			return errors.Join(ErrConflict, err)
		}
	}
	return ctx.Err()
}

// Only identity and proof fields consumed by the existing terminal/drain readers
// enter newly saved Stop evidence. Optional invocation evidence stays optional.
func teamConflictHostIdentity(o HostObservation) HostObservation {
	return HostObservation{LaunchRequest: o.LaunchRequest, Digest: o.Digest, Unit: o.Unit, BootID: o.BootID, InvocationID: o.InvocationID, ControlGroup: o.ControlGroup}
}
func sameTeamConflictHost(a, b HostObservation) bool {
	return a.BootID != "" && a.InvocationID != "" && a.ControlGroup != "" && a.LaunchRequest == b.LaunchRequest && a.Digest == b.Digest && a.Unit == b.Unit && a.BootID == b.BootID && a.InvocationID == b.InvocationID && a.ControlGroup == b.ControlGroup
}
func teamConflictStopEvidence(o HostObservation) HostObservation {
	n := teamConflictHostIdentity(o)
	n.Phase, n.Outcome, n.Result = o.Phase, o.Outcome, o.Result
	n.LoadState, n.ActiveState, n.Job = o.LoadState, o.ActiveState, o.Job
	if o.DrainProof != nil {
		n.DrainProof = map[string]any{}
		for _, key := range []string{"empty", "boot_id", "invocation_id", "control_group", "load_state", "active_state", "job"} {
			if value, ok := o.DrainProof[key]; ok {
				n.DrainProof[key] = value
			}
		}
	}
	return n
}

// Check the same candidate that reserveSuccessorRecord will publish. Projection
// is size admission only; no projected value is persisted or used as drain proof.
func teamConflictJournalSize(j teamConflictJournal, successor RuntimeRef) error {
	if j.SourceHost == nil {
		return ErrScope
	}
	j.Successor = successor
	j.Phase = "prepared"
	j.Stop = nil
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if len(raw)+1 > recordLimit {
		return fmt.Errorf("team conflict recovery record too large: %w", ErrScope)
	}
	stop := teamConflictHostIdentity(*j.SourceHost)
	stop.Phase, stop.Outcome, stop.Result = "terminal", "stopped", "stopped"
	stop.LoadState, stop.ActiveState, stop.Job = "loaded", "inactive", "0"
	stop.DrainProof = map[string]any{"empty": true, "boot_id": stop.BootID, "invocation_id": stop.InvocationID, "control_group": stop.ControlGroup, "load_state": "loaded", "active_state": "inactive", "job": "0"}
	// complete/stopped/inactive/0 and present invocation are the largest accepted
	// finite spellings; settled/exited/failed/empty or absent invocation are smaller.
	j.Phase = "complete"
	j.Stop = &stop
	raw, err = json.Marshal(j)
	if err != nil {
		return err
	}
	if len(raw)+1 > recordLimit {
		return fmt.Errorf("team conflict recovery record too large: %w", ErrScope)
	}
	return nil
}
