package piruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type conversationAdapter struct {
	manager       *Manager
	handle        agentbackend.ExecutionHandle
	readRuntime   *RuntimeRef
	readResult    *transcriptSnapshot
	inputDelivery *bool
}

func (a *conversationAdapter) verifyReadRuntime() error {
	record, err := a.manager.record(a.handle.Conversation.Session)
	if err != nil {
		return err
	}
	if a.readRuntime == nil || record.Runtime != *a.readRuntime {
		return ErrScope
	}
	return nil
}

func (a *conversationAdapter) inputCapability(ctx context.Context) (bool, error) {
	if a.readRuntime != nil {
		if err := a.verifyReadRuntime(); err != nil {
			return false, err
		}
		return *a.inputDelivery, nil
	}
	return a.manager.InputDelivery(ctx, a.handle)
}

func (a *conversationAdapter) Verify(ctx context.Context, identity backend.Identity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if identity != BackendIdentity(a.handle) {
		return ErrScope
	}
	if a.readRuntime != nil {
		return a.verifyReadRuntime()
	}
	return a.manager.verifyConversation(ctx, a.handle)
}
func (a *conversationAdapter) readSnapshot(ctx context.Context, identity backend.Identity) (transcriptSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return transcriptSnapshot{}, err
	}
	if identity != BackendIdentity(a.handle) {
		return transcriptSnapshot{}, ErrScope
	}
	if a.readRuntime != nil {
		if err := a.verifyReadRuntime(); err != nil {
			return transcriptSnapshot{}, err
		}
		if a.readResult != nil {
			return *a.readResult, nil
		}
	}
	result, err := a.manager.readTranscriptSnapshot(ctx, a.handle, nil)
	if err == nil && a.readRuntime != nil {
		a.readResult = &result
	}
	return result, err
}
func (a *conversationAdapter) Read(ctx context.Context, identity backend.Identity) (backend.Transcript, error) {
	snapshot, err := a.readSnapshot(ctx, identity)
	if err != nil {
		return backend.Transcript{}, err
	}
	result := snapshot.transcript()
	result.InputDelivery, err = a.inputCapability(ctx)
	return result, err
}

type nativeEntry struct {
	ID, ParentID, Type string
	Timestamp          json.RawMessage
	Message            nativeMessage
}

type nativeMessage struct {
	Role         string
	Content      json.RawMessage
	StopReason   string
	ErrorMessage string
	Timestamp    json.RawMessage
	ToolCallID   string `json:"toolCallId"`
	ToolName     string
	IsError      *bool
}

func assistantPresentation(message nativeMessage) (string, string) {
	text := messageText(message.Content)
	if strings.TrimSpace(text) != "" || (message.StopReason != "error" && message.StopReason != "aborted") {
		return "agentMessage", text
	}
	text = message.StopReason
	if message.ErrorMessage != "" {
		text += ": " + message.ErrorMessage
	}
	const limit = 4096
	const indicator = "…"
	if len(text) > limit {
		end := limit - len(indicator)
		for !utf8.RuneStart(text[end]) {
			end--
		}
		text = text[:end] + indicator
	}
	return "error", text
}

type nativeContentBlock struct {
	Type, Text, Thinking, ID, Name string
	Arguments                      json.RawMessage
}

func nativeBlocks(raw json.RawMessage) []nativeContentBlock {
	var blocks []nativeContentBlock
	_ = json.Unmarshal(raw, &blocks)
	return blocks
}

func nativeTimestamp(entry json.RawMessage, message json.RawMessage) string {
	var value string
	if json.Unmarshal(entry, &value) == nil {
		if _, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return value
		}
	}
	var millis int64
	if len(message) != 0 && string(message) != "null" && json.Unmarshal(message, &millis) == nil {
		when := time.UnixMilli(millis).UTC()
		if when.Year() >= 1 && when.Year() <= 9999 {
			return when.Format(time.RFC3339Nano)
		}
	}
	return ""
}

func transcriptSummary(value string) string {
	const limit = 256
	if len(value) <= limit {
		return value
	}
	end := limit - len("…")
	for !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "…"
}

func toolSummary(name string, arguments json.RawMessage) string {
	if name == "" {
		name = "Tool"
	}
	var values map[string]json.RawMessage
	_ = json.Unmarshal(arguments, &values)
	key, verb := "path", name
	switch name {
	case "read":
		verb = "Read"
	case "write":
		verb = "Write"
	case "edit":
		verb = "Edit"
	case "bash":
		key, verb = "command", "Run"
	}
	var value string
	_ = json.Unmarshal(values[key], &value)
	if value != "" {
		verb += " " + value
	}
	return transcriptSummary(verb)
}

func toolCommunication(name string, arguments json.RawMessage) *backend.TranscriptCommunication {
	switch name {
	case "report_to_lead":
		return &backend.TranscriptCommunication{Kind: "team", To: "lead"}
	case "team_message":
		var input struct{ To string }
		_ = json.Unmarshal(arguments, &input)
		return &backend.TranscriptCommunication{Kind: "team", To: input.To}
	}
	return nil
}

func argumentDetails(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var formatted bytes.Buffer
	if json.Indent(&formatted, raw, "", "  ") == nil {
		return formatted.String()
	}
	return string(raw)
}

type transcriptCall struct {
	block nativeContentBlock
	count int
}

func projectTranscript(snapshot workerSnapshot) backend.Transcript {
	status := "idle"
	if snapshot.Busy {
		status = "active"
	}
	if snapshot.Held {
		status = "held"
	}
	result := backend.Transcript{ThreadID: snapshot.NativeID, Status: status, Model: snapshot.Model, ReasoningEffort: snapshot.Thinking, Entries: []backend.TranscriptEntry{}}
	entries := []nativeEntry{}
	for _, raw := range snapshot.Entries {
		var entry nativeEntry
		if json.Unmarshal(raw, &entry) == nil && entry.Type == "message" {
			entries = append(entries, entry)
		}
	}
	var live nativeMessage
	hasLive := len(snapshot.LiveMessage) != 0 && string(snapshot.LiveMessage) != "null" && json.Unmarshal(snapshot.LiveMessage, &live) == nil
	calls := map[string]transcriptCall{}
	collect := func(message nativeMessage) {
		if message.Role != "assistant" {
			return
		}
		for _, block := range nativeBlocks(message.Content) {
			if block.Type == "toolCall" && block.ID != "" {
				call := calls[block.ID]
				call.block = block
				call.count++
				calls[block.ID] = call
			}
		}
	}
	for _, entry := range entries {
		collect(entry.Message)
	}
	if hasLive {
		collect(live)
	}
	turn := ""
	project := func(entry nativeEntry, transient bool) {
		message := entry.Message
		base := backend.TranscriptEntry{ItemID: entry.ID, TurnID: turn, Timestamp: nativeTimestamp(entry.Timestamp, message.Timestamp)}
		if transient {
			base.TurnStatus = "inProgress"
		}
		appendText := func(kind, text string) {
			row := base
			row.Kind, row.Text, row.HTML = kind, text, html.EscapeString(text)
			result.Entries = append(result.Entries, row)
		}
		appendActivity := func(id, kind, summary, status, details string) *backend.TranscriptEntry {
			row := base
			row.ItemID, row.Kind, row.Summary, row.Details = id, kind, transcriptSummary(summary), details
			row.Activity = &backend.TranscriptActivity{Action: kind, Status: status}
			result.Entries = append(result.Entries, row)
			return &result.Entries[len(result.Entries)-1]
		}
		switch message.Role {
		case "user":
			appendText("userMessage", messageText(message.Content))
		case "toolResult":
			name, summary, details := message.ToolName, "", messageText(message.Content)
			var arguments json.RawMessage
			if call := calls[message.ToolCallID]; message.ToolCallID != "" && call.count == 1 && (name == "" || name == call.block.Name) {
				name, arguments = call.block.Name, call.block.Arguments
				summary = toolSummary(call.block.Name, call.block.Arguments)
				if args := argumentDetails(call.block.Arguments); args != "" {
					details = "Arguments:\n" + args + "\n\nOutput:\n" + details
				}
			}
			if summary == "" {
				summary = toolSummary(name, nil)
			}
			status := "Result"
			if message.IsError != nil {
				if *message.IsError {
					status = "failed"
				} else {
					status = "completed"
				}
			}
			appendActivity(entry.ID, "toolResult", summary+" · Result", status, details).Communication = toolCommunication(name, arguments)
		case "assistant":
			kind, text := assistantPresentation(message)
			if kind == "error" || strings.TrimSpace(text) != "" {
				appendText(kind, text)
			}
			activities := 0
			for index, block := range nativeBlocks(message.Content) {
				id := fmt.Sprintf("%s:pi-block:%d", entry.ID, index)
				switch block.Type {
				case "text":
					continue
				case "toolCall":
					state := "Requested"
					if transient && (block.ID == "" || block.Name == "") {
						state = "Preparing"
					}
					appendActivity(id, "toolCall", toolSummary(block.Name, block.Arguments), state, argumentDetails(block.Arguments)).Communication = toolCommunication(block.Name, block.Arguments)
				case "thinking":
					appendActivity(id, "thinking", "Thinking", "", block.Thinking)
				default:
					summary := "Assistant activity"
					if block.Type != "" {
						summary += " · " + block.Type
					}
					appendActivity(id, "assistantActivity", summary, "", "")
				}
				activities++
			}
			if kind != "error" && strings.TrimSpace(text) == "" && activities == 0 {
				summary := "Assistant activity"
				if message.StopReason != "" {
					summary += " · " + message.StopReason
				}
				appendActivity(entry.ID, "assistantActivity", summary, "", "")
			}
		}
	}
	for _, entry := range entries {
		if entry.Message.Role == "user" {
			turn = entry.ID
			result.LatestTurnID = turn
		}
		project(entry, false)
	}
	if hasLive {
		project(nativeEntry{ID: "pi-stream", Message: live}, true)
	}
	return result
}
func messageText(raw json.RawMessage) string {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}
	var blocks []struct{ Type, Text string }
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	text := ""
	for _, block := range blocks {
		if block.Type == "text" {
			text += block.Text
		}
	}
	return text
}

type historyCursor struct {
	Version                     int
	Scope, Generation, Snapshot string
	End                         int
}

func validHistoryToken(value string) bool {
	if len(value) == 0 || len(value) > 5462 {
		return false
	}
	for _, char := range []byte(value) {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func (a *conversationAdapter) ReadPage(ctx context.Context, identity backend.Identity, cursor backend.HistoryCursor) (backend.TranscriptPage, error) {

	if identity != BackendIdentity(a.handle) {
		return backend.TranscriptPage{}, ErrScope
	}
	var old historyCursor
	var before *int
	if cursor != "" {
		if !validHistoryToken(string(cursor)) {
			return backend.TranscriptPage{}, &backend.CursorError{Code: "transcript_cursor_invalid"}
		}
		data, err := base64.RawURLEncoding.Strict().DecodeString(string(cursor))
		if err != nil || len(data) > 4096 {
			return backend.TranscriptPage{}, &backend.CursorError{Code: "transcript_cursor_invalid"}
		}
		if strictDecode(data, &old, 4096) != nil || old.Version != 1 || !hexPattern.MatchString(old.Scope) || !hexPattern.MatchString(old.Generation) || !hexPattern.MatchString(old.Snapshot) || old.End <= 0 {
			return backend.TranscriptPage{}, &backend.CursorError{Code: "transcript_cursor_invalid"}
		}
		if old.Scope != identity.ScopeKey || old.Generation != identity.Generation {
			return backend.TranscriptPage{}, &backend.CursorError{Code: "transcript_reset_required"}
		}
		current, err := a.readSnapshot(ctx, identity)
		if err != nil {
			return backend.TranscriptPage{}, err
		}
		if old.Snapshot != current.Snapshot.HistoryDigest || old.End > current.Snapshot.HistoryTotal {
			return backend.TranscriptPage{}, &backend.CursorError{Code: "transcript_reset_required"}
		}
		before = &old.End
	}
	var resultSnapshot transcriptSnapshot
	var err error
	if before == nil {
		resultSnapshot, err = a.readSnapshot(ctx, identity)
	} else {
		resultSnapshot, err = a.manager.readTranscriptSnapshot(ctx, a.handle, before)
	}
	if err != nil {
		return backend.TranscriptPage{}, err
	}
	snapshot := resultSnapshot.Snapshot
	if snapshot.NativeID != a.handle.Conversation.NativeID || snapshot.HistoryStart < 0 || snapshot.HistoryTotal < snapshot.HistoryStart || !hexPattern.MatchString(snapshot.HistoryDigest) {
		return backend.TranscriptPage{}, ErrScope
	}
	if before != nil && (old.Snapshot != snapshot.HistoryDigest || *before > snapshot.HistoryTotal) {
		return backend.TranscriptPage{}, &backend.CursorError{Code: "transcript_reset_required"}
	}
	transcript := resultSnapshot.transcript()
	transcript.InputDelivery, err = a.inputCapability(ctx)
	if err != nil {
		return backend.TranscriptPage{}, err
	}
	result := backend.TranscriptPage{Transcript: transcript, HasOlder: snapshot.HistoryStart > 0}
	if result.HasOlder {
		position := historyCursor{Version: 1, Scope: identity.ScopeKey, Generation: identity.Generation, Snapshot: snapshot.HistoryDigest, End: snapshot.HistoryStart}
		data, err := json.Marshal(position)
		if err != nil || len(data) > 4096 {
			return backend.TranscriptPage{}, ErrScope
		}
		value := backend.HistoryCursor(base64.RawURLEncoding.EncodeToString(data))
		if !validHistoryToken(string(value)) {
			return backend.TranscriptPage{}, ErrScope
		}
		result.OlderCursor = &value
	}

	return result, nil
}
func (a *conversationAdapter) Send(ctx context.Context, identity backend.Identity, request backend.SendRequest) (backend.SendResult, error) {
	if identity != BackendIdentity(a.handle) {
		return backend.SendResult{Outcome: backend.NotSubmitted}, ErrScope
	}
	var status SubmissionStatus
	operation := "send"
	if request.Delivery != "" {
		supported, err := a.manager.InputDelivery(ctx, a.handle)
		if err != nil || !supported {
			return backend.SendResult{Outcome: backend.NotSubmitted}, ErrUnavailable
		}
		operation = "input_delivery_v1"
	}
	var err error
	if request.Retry && request.Delivery == "" {
		status, err = a.manager.lookupOrdinaryRetry(ctx, a.handle, request)
	} else {
		err = a.manager.handleCall(ctx, a.handle, operation, conversationRequest{Send: request}, &status)
	}
	if err != nil {
		return backend.SendResult{Outcome: backend.Unknown}, err
	}
	switch status.Phase {
	case "Accepted":
		if status.Witness == nil || status.Ref.OperationID != request.ClientUserMessageID || status.Ref.Conversation != a.handle.Conversation {
			return backend.SendResult{Outcome: backend.Unknown}, ErrScope
		}
		return backend.SendResult{Outcome: backend.Accepted, Receipt: backend.SendReceipt{ClientUserMessageID: status.Ref.OperationID, TurnID: status.Witness.EntryID}}, nil
	case "Pending":
		if request.Delivery == "" || status.Ref.OperationID != request.ClientUserMessageID {
			return backend.SendResult{Outcome: backend.Unknown}, ErrScope
		}
		return backend.SendResult{Outcome: backend.Pending}, nil
	case "NotSubmitted":
		return backend.SendResult{Outcome: backend.NotSubmitted}, nil
	default:
		return backend.SendResult{Outcome: backend.Unknown}, nil
	}
}

// The HTTP retry flag selects an inquiry; the original native input is immutable.
func (m *Manager) lookupOrdinaryRetry(ctx context.Context, handle agentbackend.ExecutionHandle, request backend.SendRequest) (SubmissionStatus, error) {
	unknown := SubmissionStatus{Phase: "Unknown"}
	if err := ctx.Err(); err != nil {
		return unknown, err
	}
	if err := validateHandle(handle); err != nil {
		return unknown, err
	}
	current, err := m.record(handle.Conversation.Session)
	if err != nil {
		return unknown, err
	}
	if current.Runtime.Generation != handle.Generation {
		return unknown, ErrScope
	}
	if state, err := m.Inspect(ctx, current.Runtime); err != nil || state.Phase != "ready" {
		return unknown, errors.Join(ErrHeld, err)
	}
	directory, err := stateDirectory(m.config.UserStateRoot, handle.Conversation.Session)
	if err != nil {
		return unknown, err
	}
	root := filepath.Join(directory, "members", handle.Conversation.Member, handle.Conversation.Incarnation)
	var mapping conversationRecord
	if err := readPrivate(filepath.Join(root, "conversation.json"), &mapping); err != nil {
		return unknown, err
	}
	if err := validateConversationRecord(mapping, handle.Conversation, root); err != nil {
		return unknown, err
	}
	if mapping.Generation != handle.Generation || !mapping.Loaded || mapping.HeldReason != "" || mapping.PendingSettings != nil || mapping.PendingToolPolicy != nil {
		return unknown, ErrUnavailable
	}
	original, err := (&receiptEngine{root: filepath.Join(root, "submissions")}).loadOwned(request.ClientUserMessageID)
	if errors.Is(err, os.ErrNotExist) {
		return unknown, nil
	}
	if err != nil {
		return unknown, err
	}
	if original.ref.kind() != "retained" || original.ref.retained.Conversation != handle.Conversation ||
		original.ref.retained.Action != "send" || original.Input.Message != request.Message ||
		original.Input.ActionContext != request.ActionContext || strings.HasPrefix(original.Input.ActionContext, inputDeliveryPrefix) {
		return unknown, ErrConflict
	}
	result, err := m.LookupQueuedInput(ctx, *original.ref.retained)
	if err != nil {
		return unknown, err
	}
	latest, err := m.record(handle.Conversation.Session)
	if err != nil {
		return unknown, err
	}
	if latest.Runtime != current.Runtime {
		return unknown, ErrScope
	}
	return result.Status, nil
}

func (a *conversationAdapter) Interrupt(ctx context.Context, identity backend.Identity) error {
	if identity != BackendIdentity(a.handle) {
		return ErrScope
	}
	var result StopResult
	return a.manager.handleCall(ctx, a.handle, "abort", conversationRequest{}, &result)
}
func (a *conversationAdapter) Subscribe(ctx context.Context, identity backend.Identity) (<-chan struct{}, func(), error) {
	initial, err := a.readSnapshot(ctx, identity)
	if err != nil {
		return nil, nil, err
	}
	watchCtx, cancel := context.WithCancel(ctx)
	updates := make(chan struct{}, 1)
	go func() {
		defer close(updates)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		last := digestValue(initial)
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				snapshot, err := a.readSnapshot(watchCtx, identity)
				next := digestValue(snapshot)
				if err != nil || next != last {
					select {
					case updates <- struct{}{}:
					default:
					}
					last = next
				}
				if err != nil {
					return
				}
			}
		}
	}()
	return updates, cancel, nil
}
