package piruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"html"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type conversationAdapter struct {
	manager *Manager
	handle  agentbackend.ExecutionHandle
}

func (a *conversationAdapter) Verify(ctx context.Context, identity backend.Identity) error {
	if identity != BackendIdentity(a.handle) {
		return ErrScope
	}
	var snapshot workerSnapshot
	if err := a.manager.handleCall(ctx, a.handle, "snapshot", conversationRequest{}, &snapshot); err != nil {
		return err
	}
	if snapshot.NativeID != a.handle.Conversation.NativeID {
		return ErrScope
	}
	return nil
}
func (a *conversationAdapter) readSnapshot(ctx context.Context, identity backend.Identity) (workerSnapshot, error) {
	if identity != BackendIdentity(a.handle) {
		return workerSnapshot{}, ErrScope
	}
	var snapshot workerSnapshot
	err := a.manager.handleCall(ctx, a.handle, "snapshot", conversationRequest{}, &snapshot)
	if err == nil && snapshot.NativeID != a.handle.Conversation.NativeID {
		return snapshot, ErrScope
	}
	return snapshot, err
}
func (a *conversationAdapter) Read(ctx context.Context, identity backend.Identity) (backend.Transcript, error) {
	snapshot, err := a.readSnapshot(ctx, identity)
	if err != nil {
		return backend.Transcript{}, err
	}
	return projectTranscript(snapshot), nil
}

type nativeEntry struct {
	ID, ParentID, Type, Timestamp string
	Message                       nativeMessage
}

type nativeMessage struct {
	Role         string
	Content      json.RawMessage
	StopReason   string
	ErrorMessage string
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

func projectTranscript(snapshot workerSnapshot) backend.Transcript {
	status := "idle"
	if snapshot.Busy {
		status = "active"
	}
	if snapshot.Held {
		status = "held"
	}
	result := backend.Transcript{ThreadID: snapshot.NativeID, Status: status, Model: snapshot.Model, ReasoningEffort: snapshot.Thinking, Entries: []backend.TranscriptEntry{}}
	turn := ""
	for _, raw := range snapshot.Entries {
		var entry nativeEntry
		if json.Unmarshal(raw, &entry) != nil || entry.Type != "message" {
			continue
		}
		if entry.Message.Role == "user" {
			turn = entry.ID
			result.LatestTurnID = turn
		}
		kind := entry.Message.Role
		if kind != "user" && kind != "assistant" && kind != "toolResult" {
			continue
		}
		text := messageText(entry.Message.Content)
		if kind == "user" {
			kind = "userMessage"
		}
		if kind == "assistant" {
			kind, text = assistantPresentation(entry.Message)
		}
		result.Entries = append(result.Entries, backend.TranscriptEntry{Kind: kind, ItemID: entry.ID, TurnID: turn, Text: text, HTML: html.EscapeString(text), Timestamp: entry.Timestamp})
	}
	if len(snapshot.LiveMessage) != 0 && string(snapshot.LiveMessage) != "null" {
		var live nativeMessage
		if json.Unmarshal(snapshot.LiveMessage, &live) == nil {
			kind, text := assistantPresentation(live)
			result.Entries = append(result.Entries, backend.TranscriptEntry{Kind: kind, ItemID: "pi-stream", TurnID: turn, TurnStatus: "inProgress", Text: text, HTML: html.EscapeString(text)})
		}
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
		if old.Snapshot != current.HistoryDigest || old.End > current.HistoryTotal {
			return backend.TranscriptPage{}, &backend.CursorError{Code: "transcript_reset_required"}
		}
		before = &old.End
	}
	var snapshot workerSnapshot
	if err := a.manager.handleCall(ctx, a.handle, "snapshot", conversationRequest{Before: before}, &snapshot); err != nil {
		return backend.TranscriptPage{}, err
	}
	if snapshot.NativeID != a.handle.Conversation.NativeID || snapshot.HistoryStart < 0 || snapshot.HistoryTotal < snapshot.HistoryStart || !hexPattern.MatchString(snapshot.HistoryDigest) {
		return backend.TranscriptPage{}, ErrScope
	}
	if before != nil && (old.Snapshot != snapshot.HistoryDigest || *before > snapshot.HistoryTotal) {
		return backend.TranscriptPage{}, &backend.CursorError{Code: "transcript_reset_required"}
	}
	transcript := projectTranscript(snapshot)
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
	err := a.manager.handleCall(ctx, a.handle, "send", conversationRequest{Send: request}, &status)
	if err != nil {
		return backend.SendResult{Outcome: backend.Unknown}, err
	}
	switch status.Phase {
	case "Accepted":
		if status.Witness == nil || status.Ref.OperationID != request.ClientUserMessageID || status.Ref.Conversation != a.handle.Conversation {
			return backend.SendResult{Outcome: backend.Unknown}, ErrScope
		}
		return backend.SendResult{Outcome: backend.Accepted, Receipt: backend.SendReceipt{ClientUserMessageID: status.Ref.OperationID, TurnID: status.Witness.EntryID}}, nil
	case "NotSubmitted":
		return backend.SendResult{Outcome: backend.NotSubmitted}, nil
	default:
		return backend.SendResult{Outcome: backend.Unknown}, nil
	}
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
