package piruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// Shared read-only complete linear-history capture. Terminal lifecycle keeps its
// historical domain restrictions; team recovery admits only proved publications.
func (m *Manager) captureHistoryCallbacks(ctx context.Context, ref RuntimeRef, record conversationRecord, records runtimeRecords, inspect, publications bool, reader NativeOriginInspector) ([]TeamConflictCall, error) {
	member := record.Conversation
	directory, err := stateDirectory(m.config.UserStateRoot, member.Session)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(directory, "members", member.Member, member.Incarnation)
	engine := receiptEngine{root: root + "/submissions"}
	entries, err := settledDirectory(engine.root)
	if err != nil {
		return nil, err
	}
	witnesses := map[string]ownedReceipt{}
	for _, entry := range entries {
		status, err := engine.loadOwned(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if status.ref.retained == nil || status.ref.retained.Conversation != member {
			return nil, ErrScope
		}
		known := false
		for _, runtime := range records.Requests {
			if runtime.Runtime.Generation == status.Generation {
				known = true
			}
		}
		if !known {
			return nil, ErrScope
		}
		if status.Phase == "Accepted" && status.Witness != nil {
			if _, exists := witnesses[status.Witness.EntryID]; exists {
				return nil, ErrScope
			}
			witnesses[status.Witness.EntryID] = status
		}
	}
	result := []TeamConflictCall{}
	beforeHistory, err := captureRecoveryHistory(record)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(record.HistoryFile)
	if err != nil {
		return nil, err
	}
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 4096), recordLimit)
	native := []nativeEntry{}
	header := true
	for scan.Scan() {
		if header {
			header = false
			continue
		}
		var entry nativeEntry
		if json.Unmarshal(scan.Bytes(), &entry) != nil {
			file.Close()
			return nil, ErrScope
		}
		native = append(native, entry)
		if len(native) > 65536 {
			file.Close()
			return nil, ErrHeld
		}
	}
	err = scan.Err()
	file.Close()
	if err != nil || len(native) == 0 || len(native) != beforeHistory.Entries {
		return nil, ErrHeld
	}
	handle := agentbackend.ExecutionHandle{Conversation: member, Generation: ref.Generation}
	token, err := LinearSelectionToken(CallbackSubject{Kind: "conversation", Conversation: &handle}, native[0].ID)
	if err != nil {
		return nil, err
	}
	if publications {
		tools := map[string]string{}
		for _, entry := range native {
			if entry.Message.Role == "assistant" {
				for _, block := range nativeBlocks(entry.Message.Content) {
					if block.Type == "toolCall" {
						if block.ID == "" || tools[block.ID] != "" {
							return nil, ErrScope
						}
						tools[block.ID] = block.Name
					}
				}
			} else if entry.Message.Role == "toolResult" {
				name, ok := tools[entry.Message.ToolCallID]
				if !ok || entry.Message.ToolName != name {
					return nil, ErrHeld
				}
			}
		}
	}
	input := SubmissionRef{}
	seen := map[string]bool{}
	completed := map[string]bool{}
	for _, entry := range native {
		if entry.Message.Role == "toolResult" {
			if entry.Message.ToolCallID == "" || completed[entry.Message.ToolCallID] {
				return nil, ErrScope
			}
			completed[entry.Message.ToolCallID] = true
		}
	}
	count := 0
	for _, entry := range native {
		if entry.Message.Role == "user" {
			status, ok := witnesses[entry.ID]
			if !ok || status.Witness.SessionID != member.NativeID || status.Witness.ParentID != entry.ParentID || status.Witness.EffectiveDigest != MessageDigest(messageText(entry.Message.Content)) {
				return nil, ErrHeld
			}
			input = *status.ref.retained
		}
		if entry.Message.Role != "assistant" {
			continue
		}
		for _, block := range nativeBlocks(entry.Message.Content) {
			if block.Type != "toolCall" {
				continue
			}
			if block.ID == "" || seen[block.ID] {
				return nil, ErrScope
			}
			seen[block.ID] = true
			if !slices.Contains(shippedHostTools, block.Name) {
				if !completed[block.ID] {
					return nil, ErrHeld
				}
				continue
			}
			if input.OperationID == "" {
				return nil, ErrHeld
			}
			if block.Name != "team_message" && block.Name != "report_to_lead" {
				if !publications || (block.Name != "request_user_input_async" && block.Name != "publish_plan") {
					return nil, ErrHeld
				}
				origin := NativeOriginRef{AnchorEntryID: entry.ID, SelectionToken: token, NativeToolCallID: block.ID, Tool: block.Name}
				if inspect {
					observed, err := reader.InspectNativeOrigin(ctx, handle, origin)
					if err != nil || observed.Handle != handle || observed.Origin != origin || !sameCallbackArguments(observed.Arguments, block.Arguments) {
						return nil, errors.Join(ErrConflict, err)
					}
				}
				result = append(result, TeamConflictCall{LifecycleCallback: LifecycleCallback{Conversation: member, Input: input, Origin: origin, ArgumentsSHA256: bytesDigest(block.Arguments)}, Arguments: append(json.RawMessage(nil), block.Arguments...)})
				count++
				continue
			}
			var args map[string]string
			if strictDecode(block.Arguments, &args, callbackLimit) != nil {
				return nil, ErrScope
			}
			to := args["to"]
			if block.Name == "report_to_lead" {
				to = "lead"
			}
			expected := 3
			if block.Name == "report_to_lead" {
				expected = 2
			}
			if len(args) != expected || !operationPattern.MatchString(args["message_id"]) || !memberPattern.MatchString(to) || args["message"] == "" {
				return nil, ErrScope
			}
			origin := NativeOriginRef{AnchorEntryID: entry.ID, SelectionToken: token, NativeToolCallID: block.ID, Tool: block.Name}
			if inspect {
				observed, err := reader.InspectNativeOrigin(ctx, handle, origin)
				if err != nil {
					return nil, err
				}
				var actual map[string]string
				if json.Unmarshal(observed.Arguments, &actual) != nil || !reflect.DeepEqual(actual, args) {
					return nil, ErrScope
				}
			}
			result = append(result, TeamConflictCall{LifecycleCallback: LifecycleCallback{Conversation: member, Input: input, Origin: origin, ArgumentsSHA256: bytesDigest(block.Arguments), MessageID: args["message_id"], To: to}, Arguments: append(json.RawMessage(nil), block.Arguments...)})
			count++
		}
	}
	afterHistory, err := captureRecoveryHistory(record)
	if err != nil || afterHistory.SHA256 != beforeHistory.SHA256 {
		return nil, errors.Join(ErrHeld, err)
	}
	if record.HeldReason != "" && count == 0 {
		return nil, ErrHeld
	}
	return result, nil
}

func sameCallbackArguments(a, b json.RawMessage) bool {
	var left, right any
	return strictDecode(a, &left, callbackLimit) == nil && strictDecode(b, &right, callbackLimit) == nil && reflect.DeepEqual(left, right)
}
