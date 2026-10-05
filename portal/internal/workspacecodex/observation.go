package workspacecodex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"time"
	"unicode"
	"unicode/utf8"
)

// ObservationDiagnostic carries policy meaning independently of human text.
type ObservationDiagnostic struct {
	Code     string `json:"code"`
	Category string `json:"category"`
	Message  string `json:"message"`
}

type ObservationError struct{ Code, Message string }

func (e *ObservationError) Error() string { return e.Message }

type ThreadObservation struct {
	Schema         int                     `json:"schema"`
	ThreadID       string                  `json:"threadId"`
	Cwd            string                  `json:"cwd"`
	UpdatedAt      int64                   `json:"updatedAt"` // Administrative only; never an inactivity boundary.
	ActivityKnown  bool                    `json:"activityKnown"`
	ActivityToken  string                  `json:"activityToken"`
	LastActivityAt *int64                  `json:"lastActivityAt"`
	Idle           bool                    `json:"idle"`
	Blockers       []string                `json:"blockers"`
	Diagnostics    []ObservationDiagnostic `json:"diagnostics"`
}

type latestTurn struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	StartedAt   *int64 `json:"startedAt"`
	CompletedAt *int64 `json:"completedAt"`
}

func observationFailure(code, message string) error {
	return &ObservationError{Code: code, Message: message}
}

func observationID(value string, required bool) bool {
	if !utf8.ValidString(value) || len(value) > 256 || (required && value == "") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// The selected protocol returns Unix seconds. A missing historical boundary is
// unknown as a date, while a verified ID/status still supplies a semantic token.
func decodeLatestTurn(data []byte, now time.Time) (*latestTurn, error) {
	fields, err := uniqueArchiveObject(data)
	if err != nil {
		return nil, err
	}
	raw, ok := fields["data"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("turn history has no data array")
	}
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil || len(rows) > 1 {
		return nil, errors.New("turn history is not a bounded latest-turn array")
	}
	if len(rows) == 0 {
		for _, name := range []string{"nextCursor", "backwardsCursor"} {
			if value, present := fields[name]; present && string(bytes.TrimSpace(value)) != "null" {
				return nil, errors.New("empty latest-turn history has a cursor")
			}
		}
		return nil, nil
	}
	row, err := uniqueArchiveObject(rows[0])
	if err != nil {
		return nil, err
	}
	var turn latestTurn
	if json.Unmarshal(row["id"], &turn.ID) != nil || !observationID(turn.ID, true) ||
		json.Unmarshal(row["status"], &turn.Status) != nil {
		return nil, errors.New("turn history has an invalid identity/status")
	}
	switch turn.Status {
	case "completed", "failed", "interrupted", "inProgress":
	default:
		return nil, errors.New("turn history has an unknown status")
	}
	for key, dest := range map[string]**int64{"startedAt": &turn.StartedAt, "completedAt": &turn.CompletedAt} {
		if value, present := row[key]; present {
			if json.Unmarshal(value, dest) != nil || (*dest != nil && (**dest <= 0 || **dest > now.Unix())) {
				return nil, errors.New("turn history has an invalid or future boundary")
			}
		}
	}
	if (turn.StartedAt != nil && turn.CompletedAt != nil && *turn.CompletedAt < *turn.StartedAt) ||
		(turn.Status == "inProgress" && turn.CompletedAt != nil) {
		return nil, errors.New("turn history has contradictory boundaries")
	}
	return &turn, nil
}

func (c *Client) latestTurn(ctx context.Context, threadID string) (*latestTurn, error) {
	var raw json.RawMessage
	err := c.Request(ctx, "thread/turns/list", map[string]any{
		"threadId": threadID, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded",
	}, &raw)
	if err != nil {
		return nil, observationFailure("history_unavailable", "Latest conversation history is unavailable.")
	}
	turn, err := decodeLatestTurn(raw, time.Now())
	if err != nil {
		return nil, observationFailure("history_invalid", "Latest conversation history is invalid or in the future.")
	}
	return turn, nil
}

func (c *Client) ObserveThread(ctx context.Context, threadID, cwd string) (ThreadObservation, error) {
	state, err := c.ProveArchivedRootThread(ctx, threadID, cwd)
	if err != nil {
		return ThreadObservation{}, observationFailure("identity_unverified", "Conversation identity or persistence cannot be verified.")
	}
	result, err := c.ObserveThreadIdentity(ctx, threadID, cwd, "", state)
	if err != nil {
		return ThreadObservation{}, err
	}
	again, err := c.ProveArchivedRootThread(ctx, threadID, cwd)
	if err != nil || again != state {
		return ThreadObservation{}, observationFailure("identity_changed", "Conversation persistence changed during observation.")
	}
	return result, nil
}

// ObserveThreadIdentity is bounded and read-only. A caller with a retained team
// must bracket this primitive with its owning project/materialization proof.
func (c *Client) ObserveThreadIdentity(ctx context.Context, threadID, cwd, projectID string, state ArchiveState) (ThreadObservation, error) {
	if !observationID(threadID, true) || !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return ThreadObservation{}, observationFailure("identity_invalid", "Conversation observation requires an exact identity and directory.")
	}
	before, err := c.ReadThreadMetadata(ctx, threadID, false)
	expected := ArchivedThreadIdentity{ThreadID: threadID, Cwd: cwd, ProjectID: projectID, SourceKind: threadSourceKind}
	if err != nil || validateArchiveMetadata(before, expected) != nil {
		return ThreadObservation{}, observationFailure("identity_unverified", "Conversation identity cannot be verified.")
	}
	turn, err := c.latestTurn(ctx, threadID)
	if err != nil {
		return ThreadObservation{}, err
	}
	result := ThreadObservation{Schema: 1, ThreadID: threadID, Cwd: cwd, UpdatedAt: before.UpdatedAt,
		ActivityKnown: true, Idle: true, Blockers: []string{}, Diagnostics: []ObservationDiagnostic{}}
	busy := func(code, message string) {
		result.Idle = false
		result.Blockers = append(result.Blockers, message)
		result.Diagnostics = append(result.Diagnostics, ObservationDiagnostic{Code: code, Category: "busy", Message: message})
	}
	if turn != nil && turn.Status == "inProgress" {
		busy("active_turn", "Codex has an active turn.")
	}
	requests, queueIDs := [][4]string{}, [][2]string{}
	if state == ArchiveArchived {
		// Reuse the positive vscode/notLoaded/file/loaded-list/turn/submission
		// owner. Cold archived queue/list is unsupported and must never be used.
		if err := c.RequireArchiveThreadIdle(ctx, threadID, cwd, state); err != nil {
			var typed *ObservationError
			if errors.As(err, &typed) && typed.Code == "submission_unverified" {
				return ThreadObservation{}, observationFailure("submission_unverified", "Conversation submissions cannot be proved resolved.")
			}
			return ThreadObservation{}, observationFailure("archived_idle_unverified", "Archived conversation idle state cannot be verified.")
		}
	} else if state == ArchiveActive || state == ArchiveFresh {
		prompts, err := c.PromptsWithItems(ctx, threadID)
		if err != nil {
			return ThreadObservation{}, observationFailure("requests_unverified", "Pending conversation requests cannot be verified.")
		}
		if len(prompts) > 256 {
			return ThreadObservation{}, observationFailure("requests_unbounded", "Pending conversation requests exceed the observation bound.")
		}
		for _, prompt := range prompts {
			if prompt.ThreadID != threadID || !observationID(prompt.ID, true) || !observationID(prompt.TurnID, false) || !observationID(prompt.ItemID, false) || !observationID(prompt.Method, true) {
				return ThreadObservation{}, observationFailure("requests_invalid", "Pending conversation request identity is invalid.")
			}
			requests = append(requests, [4]string{prompt.ID, prompt.TurnID, prompt.ItemID, prompt.Method})
		}
		if len(requests) > 0 {
			busy("pending_request", "Codex has pending requests.")
		}
		queue, err := c.ListQueue(ctx, threadID)
		if err != nil {
			return ThreadObservation{}, observationFailure("queue_unverified", "Queued conversation input cannot be verified.")
		}
		if len(queue) > 256 {
			return ThreadObservation{}, observationFailure("queue_unbounded", "Queued conversation input exceeds the observation bound.")
		}
		for _, entry := range queue {
			if !observationID(entry.ID, true) || !observationID(entry.ClientUserMessageID, false) {
				return ThreadObservation{}, observationFailure("queue_invalid", "Queued conversation input identity is invalid.")
			}
			queueIDs = append(queueIDs, [2]string{entry.ID, entry.ClientUserMessageID})
		}
		if len(queueIDs) > 0 {
			busy("queued_input", "Codex has queued messages.")
		}
		// The public owner proves resolution but exposes no attempt snapshot.
		// An error is unknown activity; no private ledger or error text is parsed.
		if err := c.RequireSubmissionAttemptsResolved(ctx, threadID); err != nil {
			return ThreadObservation{}, observationFailure("submission_unverified", "Conversation submissions cannot be proved resolved.")
		}
	} else {
		return ThreadObservation{}, observationFailure("persistence_unverified", "Conversation persistence cannot be verified.")
	}
	again, err := c.latestTurn(ctx, threadID)
	if err != nil {
		return ThreadObservation{}, err
	}
	if !reflect.DeepEqual(turn, again) {
		return ThreadObservation{}, observationFailure("activity_changed", "Conversation activity changed during observation.")
	}
	after, err := c.ReadThreadMetadata(ctx, threadID, false)
	if err != nil || validateArchiveMetadata(after, expected) != nil || !reflect.DeepEqual(before.Path, after.Path) {
		return ThreadObservation{}, observationFailure("identity_changed", "Conversation identity changed during observation.")
	}
	sort.Slice(requests, func(i, j int) bool { return fmt.Sprint(requests[i]) < fmt.Sprint(requests[j]) })
	sort.Slice(queueIDs, func(i, j int) bool { return fmt.Sprint(queueIDs[i]) < fmt.Sprint(queueIDs[j]) })
	result.ActivityToken = ObservationToken([]any{1, threadID, projectID, turn, requests, queueIDs, "submissions_resolved"})
	if turn != nil && turn.StartedAt != nil && turn.CompletedAt != nil {
		result.LastActivityAt = turn.CompletedAt
	}
	return result, nil
}

func ObservationToken(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
