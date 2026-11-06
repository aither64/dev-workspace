package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
)

type maintenanceNoticeClient interface {
	ExportSubmissionAttempts(string) (json.RawMessage, error)
	DiscardPreparedConditionalSteer(context.Context, string, string, string, string, string, string) error
	ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error)
}

// This is the pinned client's conditional-steer:v1 action identity. Reconcile
// uses the read-only generic recovery API, which cannot dispatch even if a
// submitting record changes to prepared between observations.
func maintenanceNoticeContext(p maintenance.Participant) string {
	b, _ := json.Marshal([]string{p.Cwd, p.SourceTurn, p.NoticeAction})
	return "conditional-steer:v1:" + string(b)
}
func (m maintenanceControl) retireNotice(ctx, resultCtx context.Context, id string, index int, p maintenance.Participant, c maintenanceNoticeClient) error {
	if p.NoticeRetired || p.NoticeCleanupAccepted {
		return nil
	}
	err := m.savePauseResult(resultCtx, id, func(o *maintenance.Operation) error {
		q := &o.Participants[index]
		if q.Thread != p.Thread || q.NoticeID != p.NoticeID {
			return errors.New("notice retirement identity changed")
		}
		q.NoticeRetirementIntent = true
		return nil
	})
	if err != nil {
		return err
	}
	// The owner's intent withdraws dispatch permission. Only the client's atomic
	// exact-identity check can prove prepared/absent and retire the client row.
	if p.NoticeDisposition != "unknown" && p.NoticeDisposition != "accepted" {
		if err = c.DiscardPreparedConditionalSteer(ctx, p.Thread, p.Cwd, p.SourceTurn, p.NoticeText, p.NoticeID, p.NoticeAction); err == nil {
			err = m.savePauseResult(resultCtx, id, func(o *maintenance.Operation) error {
				q := &o.Participants[index]
				q.NoticeRetired = true
				q.Reason = ""
				return nil
			})
			return err
		}
	}
	data, err := c.ExportSubmissionAttempts(p.Thread)
	if err != nil {
		return err
	}
	if len(data) > maintenance.MaxBytes {
		return errors.New("notice snapshot exceeds bound")
	}
	var snapshot struct {
		Schema int `json:"schema"`
		Sends  map[string]map[string]struct {
			State, Context, Digest, TurnID string
			Steered                        bool
		} `json:"sends"`
	}
	if json.Unmarshal(data, &snapshot) != nil || snapshot.Schema != 3 {
		return errors.New("notice snapshot unavailable")
	}
	row, found := snapshot.Sends[p.Thread][p.NoticeID]
	digest := sha256.Sum256([]byte(p.NoticeText))
	if !found || row.Context != maintenanceNoticeContext(p) || row.Digest != hex.EncodeToString(digest[:]) || !row.Steered || (row.State != "submitting" && row.State != "accepted") {
		return errors.New("notice cleanup outcome remains unresolved")
	}
	receipt, found, err := c.ReconcileSendWithOptions(ctx, p.Thread, p.NoticeText, p.NoticeID, row.Context, codex.TurnOptions{})
	if err != nil || !found || receipt.TurnID != p.SourceTurn || !receipt.Steered {
		saveErr := m.savePauseResult(resultCtx, id, func(o *maintenance.Operation) error {
			o.Participants[index].NoticeDisposition = "unknown"
			o.Participants[index].Reason = "notice-outcome-unknown"
			return nil
		})
		if saveErr != nil {
			return saveErr
		}
		return errors.New("notice cleanup outcome remains unknown")
	}
	err = m.savePauseResult(resultCtx, id, func(o *maintenance.Operation) error {
		q := &o.Participants[index]
		q.NoticeDisposition = "accepted"
		q.NoticeCleanupAccepted = true
		q.Reason = ""
		return nil
	})
	return err
}
func (m maintenanceControl) cancelNotices(ctx context.Context, id string) (maintenance.Response, error) {
	o, err := m.store.Load(id)
	if err != nil {
		return maintenance.Response{}, err
	}
	if !o.CancelRequested || o.CutoverIntent {
		return maintenance.Response{}, errors.New("cancellation cleanup is not owned")
	}
	complete := true
	call, recovery := m.pauseLimits()
	attempt, cancel := context.WithTimeout(ctx, recovery)
	defer cancel()
	reason := "notice-cleanup-unresolved"
	for i, p := range o.Participants {
		if attempt.Err() != nil {
			complete = false
			reason = "notice-cleanup-timeout"
			break
		}
		c := m.client(o, p)
		bounded, stop := context.WithTimeout(attempt, call)
		err = m.retireNotice(bounded, ctx, id, i, p, c)
		cause := bounded.Err()
		stop()
		c.Close()
		if err != nil {
			complete = false
			if cause != nil {
				reason = "notice-cleanup-timeout"
			}
		}
	}
	err = m.savePauseResult(ctx, id, func(current *maintenance.Operation) error {
		current.NoticeCleanupComplete = complete
		if !complete {
			current.Blockers = []string{"notice-cleanup-unresolved"}
		}
		return nil
	})
	if err != nil {
		return maintenance.Response{}, err
	}
	status := "cancelling"
	if !complete {
		status = "recovery-required"
	}
	return maintenance.Response{Schema: 1, Status: status, Reason: func() string {
		if !complete {
			return reason
		}
		return ""
	}()}, nil
}
