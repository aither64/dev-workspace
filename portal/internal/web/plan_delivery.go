package web

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type preparedPlanClient interface {
	IsPreparedSendWithOptions(string, string, string, string, codex.TurnOptions) (bool, error)
}

var errPlanDeliveryIdentity = errors.New("retained runtime authority changed")

func planDeliveryIdentity(body implementPlanRequest) *workspacecodex.PreparedSend {
	action := strings.TrimSpace(body.Action)
	if (action != "same" && action != "recover") || !queueClientMessageIDPattern.MatchString(strings.TrimSpace(body.ClientUserMessageID)) || !messageDigestPattern.MatchString(strings.TrimSpace(body.PlanSHA256)) {
		return nil
	}
	context := "plan:" + strings.TrimSpace(body.PlanSHA256)
	if body.PlanContextVersion == 2 && strings.TrimSpace(body.PlanTurnID) != "" {
		context = "plan:" + strings.TrimSpace(body.PlanTurnID) + ":" + strings.TrimSpace(body.PlanSHA256)
	} else if action != "recover" || body.PlanContextVersion != 0 {
		return nil
	}
	return &workspacecodex.PreparedSend{Text: "Implement the plan.", ID: strings.TrimSpace(body.ClientUserMessageID), Action: context}
}

// Called under package/runtime admission, before the activity-known gate. It
// can only reconcile the exact request or select its prepared observation;
// neither operation grants execution or changes settings.
func (s *Server) reconcilePlanDelivery(w http.ResponseWriter, r *http.Request, summary *session.Summary, body implementPlanRequest) (*workspacecodex.PreparedSend, bool) {
	p := planDeliveryIdentity(body)
	client, ok := s.config.Codex.(preparedPlanClient)
	if p == nil || !ok {
		return nil, false
	}
	fail := func(err error) (*workspacecodex.PreparedSend, bool) {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return nil, true
	}
	ctx := r.Context()
	if err := s.requirePlanDeliveryIdentity(ctx, summary); err != nil {
		return fail(err)
	}
	service, err := s.teamService(summary.Slug)
	if err != nil {
		return fail(err)
	}
	unlock, err := service.Store.LockOperation(ctx, summary.Slug)
	if err != nil {
		return fail(err)
	}
	defer unlock()
	conversation := s.messageLock(summary.Slug)
	if err := conversation.Lock(ctx); err != nil {
		return fail(err)
	}
	defer conversation.Unlock()
	if err := s.requirePlanDeliveryIdentity(ctx, summary); err != nil {
		return fail(err)
	}
	thread := summary.Codex.ThreadID
	receipt, found, err := s.config.Codex.ReconcileSend(ctx, thread, p.Text, p.ID, p.Action)
	if err != nil {
		return fail(err)
	}
	if !found {
		// Acknowledgment can compact an accepted ledger entry. The generic
		// exact history recovery preserves its delivery identity and receipt.
		if _, err = s.config.Codex.SendAttempted(ctx, thread, p.Text, p.ID, p.Action); err != nil {
			return fail(err)
		}
		receipt, found, err = s.config.Codex.ReconcileSend(ctx, thread, p.Text, p.ID, p.Action)
		if err != nil {
			return fail(err)
		}
	}
	if found {
		s.writeJSON(w, http.StatusAccepted, receipt)
		return nil, true
	}
	prepared, err := client.IsPreparedSendWithOptions(thread, p.Text, p.ID, p.Action, p.Options)
	if err != nil {
		return fail(err)
	}
	if prepared {
		return p, false
	}
	return nil, false
}

func (s *Server) requirePlanDeliveryIdentity(ctx context.Context, expected *session.Summary) error {
	if err := s.requireProviderAdmission(expected.Slug); err != nil {
		return err
	}
	current, err := session.Find(s.config.Workspace, expected.Slug)
	if err != nil {
		return err
	}
	if current.Archived || current.Lifecycle != "active" || current.Codex != expected.Codex || current.Root != expected.Root || session.RequireRetainedReady(current, s.config.CodexSocket) != nil {
		return errPlanDeliveryIdentity
	}
	if err := RequireObservationReceipts(current, s.config.UserStateRoot, "", ""); err != nil {
		return err
	}
	authority, err := session.LoadRuntimeAuthority(s.config.AuthorityDir, current.Slug, s.config.Workspace)
	if err != nil {
		return err
	}
	if authority.State != "ready" || authority.CodexThreadID != current.Codex.ThreadID || authority.CodexSocketPath != current.Codex.SocketPath || authority.CodexClientVersion != current.Codex.ClientVersion {
		return errPlanDeliveryIdentity
	}
	if err := authority.VerifyTmux(ctx, s.config.Tmux); err != nil {
		return err
	}
	if s.config.VerifyThread == nil {
		return errPlanDeliveryIdentity
	}
	return s.config.VerifyThread(ctx, current.Codex.ThreadID, filepath.Join(s.config.Workspace, current.Root, current.Slug))
}
