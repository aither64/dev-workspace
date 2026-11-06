package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"os"
	"sort"
	"time"
)

type maintenanceTurnClient interface {
	ActiveTurnID(context.Context, string) (string, error)
	Request(context.Context, string, any, any) error
}

// thread/list's ancestor filter identifies native children. Retained participants
// and independent monitor threads always win over ancestry; they are not disposable.
func maintenanceChildren(ctx context.Context, c maintenanceTurnClient, actors map[string]maintenance.Participant, utilities map[string]bool) ([]maintenance.Child, error) {
	children := map[string]string{}
	for parent := range actors {
		cursor := ""
		seen := map[string]bool{}
		for {
			params := map[string]any{"ancestorThreadId": parent, "archived": false, "modelProviders": []string{}, "limit": 100}
			if cursor != "" {
				params["cursor"] = cursor
			}
			var page struct {
				Data *[]struct {
					ID string `json:"id"`
				} `json:"data"`
				NextCursor *string `json:"nextCursor"`
			}
			if err := c.Request(ctx, "thread/list", params, &page); err != nil {
				return nil, err
			}
			if page.Data == nil {
				return nil, errors.New("native child inventory unavailable")
			}
			for _, child := range *page.Data {
				if child.ID == "" || child.ID == parent {
					return nil, errors.New("invalid native child identity")
				}
				if _, known := actors[child.ID]; known {
					continue
				}
				if utilities[child.ID] {
					continue
				}
				if owner := children[child.ID]; owner != "" && owner != parent {
					return nil, errors.New("ambiguous native child ownership")
				}
				children[child.ID] = parent
				if len(children) > 4096 {
					return nil, errors.New("native child inventory exceeds bound")
				}
			}
			if page.NextCursor == nil {
				break
			}
			if *page.NextCursor == "" || seen[*page.NextCursor] {
				return nil, errors.New("native child cursor repeated")
			}
			cursor = *page.NextCursor
			seen[cursor] = true
		}
	}
	ids := []string{}
	for thread := range children {
		ids = append(ids, thread)
	}
	sort.Strings(ids)
	out := []maintenance.Child{}
	for _, thread := range ids {
		out = append(out, maintenance.Child{Parent: children[thread], Thread: thread})
	}
	return out, nil
}

// An interrupt racing natural completion is harmless only when a new ordinary
// read proves there is no active turn. Other RPC failures retain recovery.
func maintenanceInterrupt(ctx context.Context, c maintenanceTurnClient, thread string) error {
	turn, err := c.ActiveTurnID(ctx, thread)
	if err != nil || turn == "" {
		return err
	}
	if err = c.Request(ctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": turn}, nil); err != nil {
		if ctx.Err() != nil {
			return err
		}
		active, readErr := c.ActiveTurnID(ctx, thread)
		if ctx.Err() == nil && readErr == nil && active == "" {
			return nil
		}
		return err
	}
	return nil
}

var errMaintenancePauseCancelled = errors.New("maintenance pause cancelled")
var errMaintenanceGraceExpired = errors.New("maintenance grace expired")

func (m maintenanceControl) pauseLimits() (time.Duration, time.Duration) {
	call, recovery := m.pauseCallTimeout, m.pauseRecoveryTimeout
	if call <= 0 {
		call = 5 * time.Second
	}
	if recovery <= 0 {
		recovery = 30 * time.Second
	}
	return call, recovery
}
func (m maintenanceControl) checkPause(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o, err := m.store.Load(id)
	if err != nil {
		return err
	}
	if o.CancelRequested {
		return errMaintenancePauseCancelled
	}
	return nil
}

// A single context covers a complete logical client operation, including its
// nested reconciliation/pages/race recheck. The shared allowance never renews.
func (m maintenanceControl) pauseNative(ctx context.Context, id string, grace, allowance time.Time, run func(context.Context) error) error {
	if err := m.checkPause(ctx, id); err != nil {
		return err
	}
	now := time.Now()
	call, _ := m.pauseLimits()
	deadline := now.Add(call)
	graceBound := !grace.IsZero() && now.Before(grace) && !deadline.Before(grace)
	if graceBound {
		deadline = grace
	}
	if allowance.Before(deadline) {
		deadline = allowance
		graceBound = false
	}
	if !now.Before(deadline) {
		return &maintenance.ProofError{Reason: "pause-interrupt-timeout"}
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	err := run(bounded)
	cause := bounded.Err()
	cancel()
	if parentErr := ctx.Err(); parentErr != nil {
		return errors.Join(err, parentErr)
	}
	if cancelErr := m.checkPause(ctx, id); cancelErr != nil {
		return errors.Join(err, cancelErr)
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		if graceBound && (err == nil || errors.Is(err, context.DeadlineExceeded)) {
			return errors.Join(err, errMaintenanceGraceExpired)
		}
		reason := "pause-native-timeout"
		if grace.IsZero() || !now.Before(grace) {
			reason = "pause-interrupt-timeout"
		}
		return errors.Join(err, cause, &maintenance.ProofError{Reason: reason})
	}
	return err
}

// Only local result bookkeeping may detach from caller cancellation. In
// particular, an uncertain SDK submission keeps its original durable identity.
func (m maintenanceControl) savePauseResult(ctx context.Context, id string, change func(*maintenance.Operation) error) error {
	if ctx.Err() != nil {
		ctx = context.WithoutCancel(ctx)
	}
	call, _ := m.pauseLimits()
	bounded, cancel := context.WithTimeout(ctx, call)
	defer cancel()
	_, err := m.update(bounded, id, 0, change)
	return err
}
func (m maintenanceControl) pause(ctx context.Context, id string) (result maintenance.Response, err error) {
	defer func() {
		if ctx.Err() == nil && errors.Is(err, errMaintenancePauseCancelled) {
			result = maintenance.Response{Schema: 1, Status: "cancelled"}
			err = nil
		}
	}()
	o, err := m.store.Load(id)
	if err != nil {
		return result, err
	}
	if err = m.checkPause(ctx, id); err != nil {
		return result, err
	}
	if o.CutoverIntent || o.Phase == "draining-writers" {
		return maintenance.Response{Schema: 1, Status: "paused"}, nil
	}
	if o.PauseDeadline.IsZero() {
		var prepared struct {
			PauseTimeout int `json:"pauseTimeout"`
		}
		if err = json.Unmarshal(o.Preparation, &prepared); err != nil || prepared.PauseTimeout <= 0 {
			return result, errors.New("invalid prepared pause timeout")
		}
		o, err = m.update(ctx, id, 0, func(current *maintenance.Operation) error {
			if current.CancelRequested {
				return errMaintenancePauseCancelled
			}
			if current.PauseDeadline.IsZero() {
				current.PauseDeadline = time.Now().UTC().Add(time.Duration(prepared.PauseTimeout) * time.Second)
			}
			current.Phase = "pausing"
			return nil
		})
		if err != nil {
			return result, err
		}
	}
	_, recovery := m.pauseLimits()
	allowance := o.PauseDeadline.Add(recovery)
	if !time.Now().Before(allowance) {
		allowance = time.Now().Add(recovery)
	}
	for {
		if err = m.checkPause(ctx, id); err != nil {
			return result, err
		}
		o, err = m.store.Load(id)
		if err != nil {
			return result, err
		}
		interruptPass := !time.Now().Before(o.PauseDeadline)
		settled, passErr := m.pausePass(ctx, id, o, allowance)
		var refusal *maintenance.ProofError
		if errors.Is(passErr, errMaintenanceGraceExpired) && !errors.As(passErr, &refusal) && ctx.Err() == nil {
			continue
		}
		if passErr != nil {
			return result, passErr
		}
		if err = m.checkPause(ctx, id); err != nil {
			return result, err
		}
		if !interruptPass && !time.Now().Before(o.PauseDeadline) {
			continue
		}
		if settled || interruptPass {
			if !time.Now().Before(allowance) {
				return result, &maintenance.ProofError{Reason: "pause-interrupt-timeout"}
			}
			bounded, cancel := context.WithDeadline(ctx, allowance)
			_, err = m.update(bounded, id, 0, func(current *maintenance.Operation) error {
				if err := bounded.Err(); err != nil {
					return err
				}
				if current.CancelRequested {
					return errMaintenancePauseCancelled
				}
				current.Phase = "draining-writers"
				return nil
			})
			cancel()
			if err != nil {
				return result, err
			}
			return maintenance.Response{Schema: 1, Status: "paused"}, nil
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
func (m maintenanceControl) pausePass(ctx context.Context, id string, o maintenance.Operation, allowance time.Time) (bool, error) {
	settled := true
	for i, p := range o.Participants {
		if err := m.checkPause(ctx, id); err != nil {
			return false, err
		}
		if err := m.identity(ctx, o, p); err != nil {
			return false, err
		}
		c := m.client(o, p)
		err := func() error {
			var active string
			if err := m.pauseNative(ctx, id, o.PauseDeadline, allowance, func(call context.Context) error { var e error; active, e = c.ActiveTurnID(call, p.Thread); return e }); err != nil {
				return err
			}
			if active != "" {
				settled = false
			}
			if p.NoticeDisposition == "prepared" || p.NoticeDisposition == "unknown" {
				if active == p.SourceTurn && time.Now().Before(o.PauseDeadline) {
					attempted := false
					sendErr := m.pauseNative(ctx, id, o.PauseDeadline, allowance, func(call context.Context) error {
						if !time.Now().Before(o.PauseDeadline) {
							return errMaintenanceGraceExpired
						}
						attempted = true
						_, e := c.ConditionalSteer(call, p.Thread, p.Cwd, p.SourceTurn, p.NoticeText, p.NoticeID, p.NoticeAction)
						return e
					})
					if !attempted {
						return sendErr
					}
					disposition := "accepted"
					var unsent *codex.MessageNotSentError
					knownUnsent := errors.As(sendErr, &unsent)
					if sendErr != nil {
						disposition = "unknown"
						if knownUnsent {
							disposition = "refused"
						}
					}
					if err := m.savePauseResult(ctx, id, func(current *maintenance.Operation) error {
						current.Participants[i].NoticeDisposition = disposition
						return nil
					}); err != nil {
						return err
					}
					if sendErr != nil {
						if errors.Is(sendErr, errMaintenancePauseCancelled) || ctx.Err() != nil {
							return sendErr
						}
						if !knownUnsent {
							return errors.Join(&maintenance.ProofError{Reason: "notice-outcome-unknown"}, sendErr)
						}
						if errors.Is(sendErr, errMaintenanceGraceExpired) || errors.Is(sendErr, context.DeadlineExceeded) {
							return sendErr
						}
					}
				} else {
					if p.NoticeDisposition == "prepared" {
						if err := m.savePauseResult(ctx, id, func(current *maintenance.Operation) error {
							current.Participants[i].NoticeDisposition = "skipped"
							return nil
						}); err != nil {
							return err
						}
					}
					if err := m.pauseNative(ctx, id, o.PauseDeadline, allowance, func(call context.Context) error { return m.retireNotice(call, ctx, id, i, p, c) }); err != nil {
						return err
					}
				}
			}
			if active != "" && !time.Now().Before(o.PauseDeadline) {
				return m.pauseNative(ctx, id, time.Time{}, allowance, func(call context.Context) error { return maintenanceInterrupt(call, c, p.Thread) })
			}
			return nil
		}()
		c.Close()
		if err != nil {
			return false, err
		}
	}
	for _, s := range o.Services {
		if err := m.checkPause(ctx, id); err != nil {
			return false, err
		}
		c := m.client(o, maintenance.Participant{Workspace: s.Workspace})
		actors := map[string]maintenance.Participant{}
		for _, p := range o.Participants {
			if p.Workspace == s.Workspace {
				actors[p.Thread] = p
			}
		}
		// Retained independent members remain protected even when they were idle
		// at capture. Ancestry never turns one into an interruptible native child.
		protected := map[string]bool{}
		summaries, listErr := session.List(s.Workspace)
		if listErr != nil {
			c.Close()
			return false, listErr
		}
		rosters, storeErr := teamruntime.NewStore(m.store.Root, s.Workspace)
		if storeErr != nil {
			c.Close()
			return false, storeErr
		}
		for _, summary := range summaries {
			if summary.Archived || summary.Codex.SocketPath != s.Socket {
				continue
			}
			protected[summary.Codex.ThreadID] = true
			roster, rosterErr := rosters.Load(summary.Slug, summary.Codex.ThreadID)
			if rosterErr != nil && !errors.Is(rosterErr, os.ErrNotExist) {
				c.Close()
				return false, rosterErr
			}
			if roster != nil {
				for _, member := range roster.Members {
					protected[member.Thread] = true
				}
			}
		}

		var children []maintenance.Child
		err := m.pauseNative(ctx, id, o.PauseDeadline, allowance, func(call context.Context) error {
			var e error
			children, e = maintenanceChildren(call, c, actors, protected)
			return e
		})
		if err != nil {
			c.Close()
			return false, err
		}
		known := map[string]bool{}
		for _, child := range children {
			known[child.Thread] = true
		}
		for _, child := range o.Children {
			if child.Workspace == s.Workspace {
				known[child.Thread] = true
			}
		}
		for thread := range known {
			var active string
			err = m.pauseNative(ctx, id, o.PauseDeadline, allowance, func(call context.Context) error { var e error; active, e = c.ActiveTurnID(call, thread); return e })
			if err != nil {
				c.Close()
				return false, err
			}
			if active != "" {
				settled = false
				if !time.Now().Before(o.PauseDeadline) {
					if err = m.pauseNative(ctx, id, time.Time{}, allowance, func(call context.Context) error { return maintenanceInterrupt(call, c, thread) }); err != nil {
						c.Close()
						return false, err
					}
				}
			}
		}
		c.Close()
	}
	return settled, nil
}
