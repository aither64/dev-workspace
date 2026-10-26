package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/progressobserver"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

func (s *Server) progressStore() progressobserver.Store {
	return progressobserver.NewStore(s.config.UserStateRoot, s.config.Workspace)
}
func progressMember(roster *teamruntime.Roster) *teamruntime.Member {
	if roster == nil {
		return nil
	}
	for i := range roster.Members {
		m := &roster.Members[i]
		if m.Role == "observer" && m.Purpose == "general" && m.Access == "read_only" && m.State == "ready" && m.RetireIntent == "" && m.RemovedAt == nil {
			return m
		}
	}
	return nil
}
func boundedProgressText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
func (s *Server) progressTranscript(ctx context.Context, thread string) (codex.Transcript, error) {
	if reader, ok := s.config.Codex.(interface {
		ReadThreadPage(context.Context, string, string) (codex.TranscriptPage, error)
	}); ok {
		page, err := reader.ReadThreadPage(ctx, thread, "")
		return page.Transcript, err
	}
	return s.config.Codex.ReadThread(ctx, thread)
}

func progressResponse(t codex.Transcript, key string) string {
	marker := "Progress response " + key + ":"
	for i := len(t.Entries) - 1; i >= 0; i-- {
		e := t.Entries[i]
		if e.Kind != "agentMessage" {
			continue
		}
		if start := strings.Index(e.Text, marker); start >= 0 {
			return boundedProgressText(strings.TrimSpace(strings.SplitN(e.Text[start+len(marker):], "\n", 2)[0]), 700)
		}
	}
	return ""
}
func progressResult(t codex.Transcript, p *progressobserver.Pending) (progressobserver.Result, bool, error) {
	if p.Turn == "" || t.LatestTurnID != p.Turn {
		return progressobserver.Result{}, false, nil
	}
	if t.Status == "active" {
		return progressobserver.Result{}, false, nil
	}
	for i := len(t.Entries) - 1; i >= 0; i-- {
		e := t.Entries[i]
		if e.TurnID != p.Turn || e.Kind != "agentMessage" {
			continue
		}
		if e.TurnStatus != "completed" {
			return progressobserver.Result{}, true, errors.New("observer check did not complete")
		}
		r, err := progressobserver.ParseResult(e.Text)
		return r, true, err
	}
	return progressobserver.Result{}, true, errors.New("observer check has no completed result")
}

// Pending user requests are idle for oversight purposes. Waiting on a running
// tool or development member remains work, even when the model is not sampling.
func (s *Server) progressWorking(ctx context.Context, transcript codex.Transcript) (bool, error) {
	if transcript.Status != "active" {
		return false, nil
	}
	if s.activity == nil {
		return true, nil
	}
	activity, err := s.activity.ReadActivity(ctx, transcript.ThreadID)
	if err != nil {
		return false, err
	}
	if activity.Stale || activity.CurrentTurnID != transcript.LatestTurnID {
		return false, errors.New("current development activity is unavailable")
	}
	return (activity.CurrentState == "working" || activity.CurrentState == "waiting") && activity.WaitReason != "approval" && activity.WaitReason != "userInput", nil
}

func (s *Server) startProgressObservers() {
	if !s.config.RecoverSessions || s.config.UserStateRoot == "" || s.config.Codex == nil {
		return
	}
	s.operationWG.Add(1)
	go func() {
		defer s.operationWG.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-s.operationContext.Done():
				return
			case <-ticker.C:
				summaries, err := session.List(s.config.Workspace)
				if err != nil {
					continue
				}
				for _, summary := range summaries {
					if summary.Archived || summary.Codex.ThreadID == "" {
						continue
					}
					roster, err := s.loadTeamRoster(&summary)
					if err != nil || progressMember(roster) == nil {
						continue
					}
					ctx, cancel := context.WithTimeout(s.operationContext, 10*time.Second)
					err = s.stepProgressObserver(ctx, &summary, time.Now())
					cancel()
					if err != nil {
						s.config.Logger.Printf("progress observation unavailable for %s: %v", summary.Slug, err)
					}
				}
			}
		}
	}()
}

func (s *Server) stepProgressObserver(ctx context.Context, summary *session.Summary, now time.Time) error {
	return s.checkProgress(ctx, summary, now, false)
}

func (s *Server) checkProgress(ctx context.Context, summary *session.Summary, now time.Time, requested bool) error {
	generation, err := s.lockTransitionContext(ctx)
	if err != nil {
		return err
	}
	defer generation()
	if err := s.requireCurrentHostProfile(); err != nil {
		return err
	}
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	lock, err := session.LockRuntimeSharedContext(ctx, s.config.AuthorityDir, summary.Slug)
	if err != nil {
		return err
	}
	defer lock.Close()
	owner, err := session.PendingLifecycle(s.config.Workspace, summary.Slug)
	if err != nil || owner != "" {
		if err == nil && requested {
			return errors.New("A session operation is in progress.")
		}
		return err
	}
	fresh, err := session.Find(s.config.Workspace, summary.Slug)
	if err != nil {
		return err
	}
	if fresh.Archived || fresh.Codex.ThreadID != summary.Codex.ThreadID {
		if requested {
			return errors.New("The session changed.")
		}
		return nil
	}
	if err := s.requireProviderAdmission(fresh.Slug); err != nil {
		return err
	}
	if receipt, ok := s.currentCreation(fresh.Slug); ok && receipt.blocksSession() {
		if requested {
			return errors.New("Session creation is still in progress.")
		}
		return nil
	}
	service, err := s.teamService(fresh.Slug)
	if err != nil {
		return err
	}
	unlock, err := service.Store.LockOperation(ctx, fresh.Slug)
	if err != nil {
		return err
	}
	var state progressobserver.State
	active := false
	err = func() error {
		defer unlock()
		roster, err := service.Store.Load(fresh.Slug, fresh.Codex.ThreadID)
		if err != nil {
			return err
		}
		member := progressMember(roster)
		// A ready member may never have run. SendProgressCheck durably queues
		// its request before activating that exact cold recipient.
		if member == nil {
			if requested {
				return errors.New("Add an observer member before requesting a progress check.")
			}
			return nil
		}
		state, err = s.progressStore().Load(fresh.Codex.ThreadID)
		if err != nil {
			return err
		}
		fail := func(err error) error {
			state.Unavailable = boundedProgressText(err.Error(), 250)
			_ = s.progressStore().Save(state)
			return err
		}
		hadPending := state.Pending != nil
		if requested && hadPending {
			return errors.New("A progress check is already pending.")
		}
		if p := state.Pending; p != nil {
			if p.Thread != member.Thread || p.Address != member.Address {
				retired := false
				for _, old := range roster.Members {
					if old.Thread == p.Thread && old.Address == p.Address && (old.State == "removed" || old.State == "archived") {
						retired = true
					}
				}
				if !retired {
					return fail(errors.New("scheduled observer changed"))
				}
				// Normal team retirement proves the old native work settled.
				// Its ownership never transfers to a replacement member.
				state.Pending, state.Notice = nil, nil
				state.LastCheck = now
				state.Unavailable = "Previous observer retired; future checks use the replacement."
				return s.progressStore().Save(state)
			}
			if err := s.settleProgressCheck(ctx, &state, now); err != nil {
				return fail(err)
			}
			if state.Pending != nil && state.Pending.Turn != "" {
				return nil
			}
		}
		if state.Notice != nil && state.Notice.Address != member.Address {
			state.Notice = nil
			if err := s.progressStore().Save(state); err != nil {
				return err
			}
		}
		if !requested && state.Paused && state.Pending == nil {
			return nil
		}
		if !requested && !hadPending && state.Notice == nil && !state.LastCheck.IsZero() && now.Before(state.LastCheck.Add(state.CheckInterval())) {
			return nil
		}
		s.normalizeInteractivity(ctx, fresh)
		if !fresh.Interactive || fresh.ExecutionHeld || s.requireOrdinaryRetainedMutation(ctx, fresh) != nil {
			if requested {
				return errors.New("The session is not ready for a progress check.")
			}
			return nil
		}
		cwd := filepath.Join(s.config.Workspace, "work", fresh.Slug)
		if err = s.config.Codex.VerifyThread(ctx, fresh.Codex.ThreadID, cwd); err != nil {
			return fail(err)
		}
		lead, err := s.progressTranscript(ctx, fresh.Codex.ThreadID)
		if err != nil {
			return fail(err)
		}
		active, err = s.progressWorking(ctx, lead)
		if err != nil {
			return fail(err)
		}
		if state.Concern != nil {
			if response := progressResponse(lead, state.Concern.Key); response != "" {
				state.Concern.Response = response
			}
		}
		for _, m := range roster.Members {
			if m.Role == "observer" || m.State != "ready" || m.RetireIntent != "" {
				continue
			}
			t, err := s.progressTranscript(ctx, m.Thread)
			if err != nil {
				return fail(err)
			}
			working, err := s.progressWorking(ctx, t)
			if err != nil {
				return fail(err)
			}
			active = active || working
		}
		if hadPending {
			if state.Pending != nil && active && !state.Paused {
				if member.Model != state.Pending.Model || member.Effort != state.Pending.Effort {
					return fail(errors.New("scheduled observer settings changed"))
				}
				return s.sendProgressCheck(ctx, service, member, fresh.Slug, &state, false, fail)
			}
			return s.progressStore().Save(state)
		}
		if !requested && state.LastCheck.IsZero() {
			if !active {
				return nil
			}
			state.LastCheck = now
			return s.progressStore().Save(state)
		}
		if !requested && !state.Due(now, active) {
			if active && state.Concern != nil {
				return s.progressStore().Save(state)
			}
			return nil
		}
		// A manually assigned observer turn is never steered or interrupted by a check.
		observer, err := s.progressTranscript(ctx, member.Thread)
		if err != nil {
			return fail(err)
		}
		if observer.Status == "active" {
			if requested {
				return errors.New("The observer is already working on another request.")
			}
			return nil
		}
		if err := s.refreshProgressInstructions(ctx, service, fresh, roster, member); err != nil {
			return fail(err)
		}
		prompt := progressPrompt(s.progressContext(ctx, fresh, roster), state.Concern)
		state.LastCheck = now
		state.Pending = &progressobserver.Pending{ID: progressobserver.ID(fresh.Codex.ThreadID, member.Thread, now.UTC().Format(time.RFC3339Nano)), Thread: member.Thread, Address: member.Address, Model: member.Model, Effort: member.Effort, Message: prompt, StartedAt: now}
		if err := s.progressStore().Save(state); err != nil {
			return err
		}
		return s.sendProgressCheck(ctx, service, member, fresh.Slug, &state, true, fail)
	}()
	if err != nil {
		return err
	}
	if active && !state.Paused && state.Notice != nil {
		notice := state.Notice
		_, err := service.Assign(ctx, fresh.Slug, fresh.Codex.ThreadID, notice.Address, "lead", notice.Message, "", "", notice.ID)
		if err != nil {
			state.Unavailable = "Progress concern delivery is unavailable."
			_ = s.progressStore().Save(state)
			return err
		}
		state.Notice = nil
		return s.progressStore().Save(state)
	}
	return nil
}

// Settle a scheduled turn before consulting unrelated development evidence.
// Failed activity reads can withhold new inference/notices, never its cleanup.
func (s *Server) settleProgressCheck(ctx context.Context, state *progressobserver.State, now time.Time) error {
	p := state.Pending
	t, err := s.progressTranscript(ctx, p.Thread)
	if err != nil {
		return err
	}
	if p.Turn == "" {
		for _, e := range t.Entries {
			if e.Kind == "userMessage" && e.ClientUserMessageID == p.ID {
				p.Turn = e.TurnID
				break
			}
		}
	}
	if p.Turn == "" && t.Status == "active" {
		return errors.New("observer has unrelated work; scheduled submission remains held")
	}
	if result, done, resultErr := progressResult(t, p); done {
		if resultErr != nil {
			state.Pending = nil
			return resultErr
		}
		if result.Status == "on_track" && !strings.HasPrefix(p.Message, "Progress check v3.") && !p.EvidenceComplete {
			result = progressobserver.Result{Status: "unavailable", Summary: "Independent progress evidence was incomplete; an on-track conclusion cannot be accepted."}
		}
		challenge := state.Apply(result)
		if challenge && state.Concern != nil {
			concern := state.Concern
			state.Notice = &progressobserver.Notice{ID: progressobserver.ID(p.ID, "notice"), Address: p.Address, Message: fmt.Sprintf("Progress concern %s: %s\n\nSuggested next step: %s\n\nAt your next safe boundary, stop optional expansion and choose the shortest valid delivery path. Explain the actual correction or necessary remaining check in your normal update: Progress response %s: <decision and reason>. Acknowledgment alone does not resolve this concern. This is advisory, not an approval gate.", concern.Key, concern.Summary, concern.Recommendation, concern.Key)}
		}
		if state.Concern == nil {
			state.Notice = nil
		}
		return s.progressStore().Save(*state)
	}
	if !now.Before(p.StartedAt.Add(progressobserver.Deadline)) {
		if p.Turn != "" && t.LatestTurnID == p.Turn && t.Status == "active" {
			requester, ok := s.config.Codex.(interface {
				Request(context.Context, string, any, any) error
			})
			if !ok {
				return errors.New("observer cancellation unavailable")
			}
			if err := requester.Request(ctx, "turn/interrupt", map[string]any{"threadId": p.Thread, "turnId": p.Turn}, nil); err != nil {
				return err
			}
			state.Unavailable = "Observer check exceeded three minutes; waiting for it to stop."
			return s.progressStore().Save(*state)
		}
		if p.Turn != "" {
			state.Pending = nil
			return errors.New("observer result unavailable after deadline")
		}
		return errors.New("observer submission outcome is unknown; no replacement check will start")
	}
	return s.progressStore().Save(*state)
}

func (s *Server) sendProgressCheck(ctx context.Context, service teamruntime.Service, member *teamruntime.Member, slug string, state *progressobserver.State, firstAttempt bool, fail func(error) error) error {
	p := state.Pending
	policy, err := service.MemberTurnPolicy(slug, state.Root, *member)
	if err != nil {
		return fail(err)
	}
	if strings.HasPrefix(p.Message, "Progress check v2.") {
		// Reproduce the released scheduled-turn override before retrying an
		// existing ID; v3 refresh happens only after this request settles.
		instructions := progressobserver.PendingV2Instructions(member.Instructions)
		if instructions != member.Instructions {
			policy.DeveloperInstructions = strings.TrimSuffix(policy.DeveloperInstructions, member.Instructions) + instructions
		}
	}
	if err := service.Client.CompactAcceptedSendOptions("progress:" + slug + ":"); err != nil {
		return fail(err)
	}
	sender, ok := service.Client.(interface {
		SendProgressCheck(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, error)
	})
	if !ok {
		return fail(errors.New("scheduled progress delivery is unavailable"))
	}
	receipt, err := sender.SendProgressCheck(ctx, p.Thread, p.Message, p.ID, "progress:"+slug+":check", codex.TurnOptions{Model: p.Model, ReasoningEffort: p.Effort, ThreadPolicy: policy})
	if err != nil {
		if firstAttempt && errors.Is(err, workspacecodex.ErrProgressCheckDeferred) {
			// Retry admission refused before dispatch. There is no uncertain
			// submission to retain, and manual work keeps its own ownership.
			state.Pending = nil
		}
		return fail(err)
	}
	p.Turn = receipt.TurnID
	return s.progressStore().Save(*state)
}

// The caller holds the session and roster operation locks and has no pending
// scheduled check. Persist only after native injection applies the new policy.
func (s *Server) refreshProgressInstructions(ctx context.Context, service teamruntime.Service, summary *session.Summary, roster *teamruntime.Roster, member *teamruntime.Member) error {
	if !progressobserver.LegacyStockInstructions(member.Instructions) {
		return nil
	}
	updated := *member
	updated.Instructions = progressobserver.Instructions
	policy, err := service.MemberTurnPolicy(summary.Slug, summary.Codex.ThreadID, updated)
	if err != nil {
		return err
	}
	refresher, ok := service.Client.(interface {
		RefreshProgressPolicy(context.Context, string, string, codex.ThreadSettings) error
	})
	if !ok {
		return errors.New("observer instruction refresh is unavailable")
	}
	cwd := filepath.Join(s.config.Workspace, "work", summary.Slug)
	if err := refresher.RefreshProgressPolicy(ctx, member.Thread, cwd, codex.ThreadSettings{Model: member.Model, ReasoningEffort: member.Effort, Policy: policy}); err != nil {
		return err
	}
	_, err = service.Store.Update(ctx, summary.Slug, summary.Codex.ThreadID, false, func(current *teamruntime.Roster) error {
		if current.Revision != roster.Revision {
			return errors.New("observer roster changed during instruction refresh")
		}
		for i := range current.Members {
			if current.Members[i].Thread == member.Thread && current.Members[i].Address == member.Address {
				current.Members[i].Instructions = updated.Instructions
				current.Members[i].PolicyCatalogDigest = ""
				return nil
			}
		}
		return errors.New("observer disappeared during instruction refresh")
	})
	if err == nil {
		member.Instructions = updated.Instructions
	}
	return err
}

func progressPrompt(context string, concern *progressobserver.Concern) string {
	previous, _ := json.Marshal(concern)
	return `Progress check v3.
Use tools for targeted inspection of the session below. Read the actual user goal and scope decisions, then check progress, working time and Git diffstats including uncommitted/untracked work. Read deeper only where it changes your assessment. You do not need to review every change or test. Missing optional context is not a failure; use unavailable only when essential information cannot be obtained.
Return ONLY JSON (summary/recommendation together at most 150 words):
{"status":"on_track","evidence":"what you inspected and concluded"}
{"status":"concern","key":"stable-key","summary":"concrete scope/time concern","recommendation":"shortest valid delivery step"}
{"status":"unavailable","summary":"essential information you could not obtain"}
To clear an open concern, include "resolution":{"key":"the-open-key","evidence":"observed correction or justified necessity"}. Acknowledgment alone is insufficient.
Previous concern and lead response: ` + string(previous) + "\n" + context
}

func (s *Server) progressAPI(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	var body struct {
		Enabled         *bool `json:"enabled"`
		IntervalMinutes *int  `json:"intervalMinutes"`
		CheckNow        bool  `json:"checkNow"`
	}
	if r.Method == http.MethodPost {
		if !s.decodeJSON(w, r, &body) {
			return
		}
		if body.CheckNow {
			if body.Enabled != nil || body.IntervalMinutes != nil {
				s.writeError(w, r, 400, "Save settings separately from requesting a check.")
				return
			}
			if err := s.checkProgress(r.Context(), summary, time.Now(), true); err != nil {
				s.writeError(w, r, 409, err.Error())
				return
			}
		} else if body.Enabled == nil && body.IntervalMinutes == nil {
			s.writeError(w, r, 400, "Choose progress settings or request a check.")
			return
		}
		if body.IntervalMinutes != nil && (*body.IntervalMinutes < 1 || *body.IntervalMinutes > 1440) {
			s.writeError(w, r, 400, "Choose an interval from 1 to 1440 minutes.")
			return
		}
	}
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	roster, err := s.loadTeamRoster(summary)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.writeError(w, r, 409, "Progress status is unavailable.")
		return
	}
	member := progressMember(roster)
	state, err := s.progressStore().Load(summary.Codex.ThreadID)
	if err != nil {
		s.writeError(w, r, 503, "Progress status is unavailable.")
		return
	}
	if r.Method == http.MethodPost && !body.CheckNow {
		lock, err := session.LockRuntimeShared(s.config.AuthorityDir, summary.Slug)
		if err != nil {
			s.writeError(w, r, 409, err.Error())
			return
		}
		defer lock.Close()
		fresh, err := session.Find(s.config.Workspace, summary.Slug)
		if err != nil || fresh.Archived || fresh.Codex.ThreadID != summary.Codex.ThreadID {
			s.writeError(w, r, 409, "The session changed.")
			return
		}
		owner, err := session.PendingLifecycle(s.config.Workspace, summary.Slug)
		if err != nil || owner != "" {
			s.writeError(w, r, 409, "A session operation is in progress.")
			return
		}
		s.normalizeInteractivity(r.Context(), fresh)
		if member == nil || !fresh.Interactive || fresh.ExecutionHeld || s.requireOrdinaryRetainedMutation(r.Context(), fresh) != nil {
			s.writeError(w, r, 409, "Progress checks require an active observer member.")
			return
		}
		if body.Enabled != nil {
			state.Paused = !*body.Enabled
		}
		if body.IntervalMinutes != nil {
			state.IntervalMinutes = *body.IntervalMinutes
		}
		if err := s.progressStore().Save(state); err != nil {
			s.writeError(w, r, 503, "Progress settings could not be saved.")
			return
		}
	} else if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		s.writeError(w, r, 405, "Unsupported progress operation.")
		return
	}
	s.writeJSON(w, 200, map[string]any{"available": member != nil, "enabled": member != nil && !state.Paused, "intervalMinutes": int(state.CheckInterval() / time.Minute), "lastCheck": state.LastCheck, "running": state.Pending != nil, "concern": state.Concern, "warning": state.Warning(), "unavailable": state.Unavailable})
}
