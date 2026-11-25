package monitoroperation

import (
	"context"
	"errors"
	"path/filepath"
	"slices"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type DeliveryClient interface {
	ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
	ReadActivity(context.Context, string) (codex.ActivitySnapshot, error)
	ReadTurnState(context.Context, string) (codex.TurnState, error)
	SendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, error)
	ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error)
	IsPreparedSendWithOptions(string, string, string, string, codex.TurnOptions) (bool, error)
	DiscardPreparedSendWithOptions(string, string, string, string, codex.TurnOptions) (bool, error)
}

// NativeDelivery uses the configured workspace client for lead sends. Policy is
// supplied by the composition layer so this package stays outside roster and
// cross-session authorization machinery.
type NativeDelivery struct {
	Workspace, StateRoot, AuthorityDir, Socket string
	Client                                     DeliveryClient
	Admission                                  func(Owner) error
	PrepareLead                                func(context.Context, Owner) (codex.TurnOptions, error)
}

// ResolveDeliveryOwner is a cold identity projection, not execution admission.
func ResolveDeliveryOwner(workspace, slug string, authorities ...NativeAuthority) (Owner, error) {
	owner, err := IdentityFor(workspace, slug)
	if err != nil {
		return Owner{}, err
	}
	summary, err := session.Find(workspace, slug)
	if err != nil {
		return Owner{}, err
	}
	if summary.Archived || summary.Codex.ThreadID == "" || summary.Creation.State == "creating" {
		return Owner{}, errors.New("monitor owner is unavailable")
	}
	if summary.Codex.BindingSchema == 3 {
		if len(authorities) != 1 || authorities[0].Workspace != workspace {
			return Owner{}, errors.New("monitor current binding context is unavailable")
		}
		return authorities[0].Resolve(slug)
	}
	epoch, err := session.SocketIdentity(summary.Codex.SocketPath)
	if err != nil {
		return Owner{}, err
	}
	owner.Socket, owner.SocketEpoch, owner.RuntimeSchema = summary.Codex.SocketPath, epoch, Schema
	return owner, nil
}

func (n NativeDelivery) requireCurrentLead(owner Owner) error {
	summary, err := session.Find(n.Workspace, owner.Slug)
	if owner.RuntimeSchema != 3 && (err != nil || summary.Codex.BindingSchema != 3) {
		return nil
	}
	current, err := (NativeAuthority{Workspace: n.Workspace, StateRoot: n.StateRoot, AuthorityDir: n.AuthorityDir, Socket: n.Socket}).Resolve(owner.Slug)
	if err != nil || !SameLeadOwner(current, owner) {
		return errors.New("saved monitor lead target changed")
	}
	return nil
}

// Ready follows the ordinary automatic-delivery holds. It never changes
// recovery permission or loads a stopped, held or cold utility thread.
func (n NativeDelivery) Ready(ctx context.Context, owner Owner) (string, error) {
	if n.Client == nil || owner.Socket != n.Socket {
		return "Session runtime is unavailable", nil
	}
	if pending, err := session.PendingLifecycle(n.Workspace, owner.Slug); err != nil || pending != "" {
		return "Session lifecycle recovery is unfinished", nil
	}
	if n.Admission != nil {
		if err := n.Admission(owner); err != nil {
			return "Session provider is paused or changing", nil
		}
	}
	authority, err := session.LoadRuntimeAuthority(n.AuthorityDir, owner.Slug, n.Workspace)
	if err != nil || authority.State != "ready" || authority.CodexThreadID != owner.RootThreadID || authority.CodexSocketPath != n.Socket {
		return "Session is stopped or unavailable", nil
	}
	if err := n.requireCurrentLead(owner); err != nil {
		return "The saved lead target is no longer current", nil
	}
	recovery, err := (session.RecoveryStore{Root: n.StateRoot, Workspace: n.Workspace}).Load(owner.Slug)
	if err != nil {
		return "Session execution is held", nil
	}
	epoch, err := session.SocketIdentity(n.Socket)
	if err != nil || owner.SocketEpoch != epoch || recovery.ThreadID != owner.RootThreadID || recovery.SocketPath != n.Socket || !recovery.Active(owner.LeadTarget(), epoch) {
		return "Session execution is held", nil
	}
	metadata, err := n.Client.ReadThreadMetadata(ctx, owner.LeadTarget(), false)
	if err != nil || metadata.ID != owner.LeadTarget() || metadata.Cwd != filepath.Join(n.Workspace, "work", owner.Slug) {
		return "Lead conversation identity cannot be verified", nil
	}
	activity, err := n.Client.ReadActivity(ctx, owner.LeadTarget())
	if err != nil {
		return "", err
	}
	if activity.Stale || activity.ThreadID != owner.LeadTarget() {
		return "Lead activity is unavailable", nil
	}
	if activity.WaitReason == "userInput" || activity.WaitReason == "approval" {
		return "Lead is waiting for the user", nil
	}
	if !slices.Contains([]string{"idle", "working", "waiting"}, activity.CurrentState) {
		return "Lead activity is unavailable", nil
	}
	return "", nil
}

func (n NativeDelivery) Prepare(ctx context.Context, owner Owner) (codex.TurnOptions, error) {
	if n.PrepareLead == nil {
		return codex.TurnOptions{}, errors.New("lead policy preparation is unavailable")
	}
	if err := n.requireCurrentLead(owner); err != nil {
		return codex.TurnOptions{}, err
	}
	return n.PrepareLead(ctx, owner)
}

func (n NativeDelivery) Send(ctx context.Context, owner Owner, event Event) (codex.SendReceipt, error) {
	if n.Client == nil || event.Delivery.Options == nil {
		return codex.SendReceipt{}, errors.New("monitor delivery options are unavailable")
	}
	if err := n.requireCurrentLead(owner); err != nil {
		return codex.SendReceipt{}, &codex.MessageNotSentError{Err: err}
	}
	d := event.Delivery
	receipt, err := n.Client.SendWithOptions(ctx, owner.LeadTarget(), d.Envelope, d.MessageID, d.Context, *d.Options)
	var unknown *codex.UnknownSendOutcomeError
	var unsent *codex.MessageNotSentError
	if err == nil || receipt.TurnID != "" || receipt.QueuedSubmissionID != "" || errors.As(err, &unknown) || !errors.As(err, &unsent) {
		return receipt, err
	}
	prepared, proofErr := n.Client.IsPreparedSendWithOptions(owner.LeadTarget(), d.Envelope, d.MessageID, d.Context, *d.Options)
	if proofErr != nil || !prepared {
		return receipt, &codex.UnknownSendOutcomeError{Err: errors.Join(err, errors.New("monitor input preparation is unconfirmed"), proofErr)}
	}
	if reason, readyErr := n.Ready(ctx, owner); readyErr != nil || reason != "" {
		return receipt, err
	}
	// Dispatch still holds session/operation admission. Retire only this exact
	// unsubmitted preparation; a later ordinary scan chooses current routing
	// with the original ID and options. Never submit a second time inline.
	discarded, discardErr := n.Client.DiscardPreparedSendWithOptions(owner.LeadTarget(), d.Envelope, d.MessageID, d.Context, *d.Options)
	if discardErr != nil || !discarded {
		return receipt, &codex.UnknownSendOutcomeError{Err: errors.Join(err, errors.New("monitor input preparation retirement is unconfirmed"), discardErr)}
	}
	return receipt, err
}

func (n NativeDelivery) Reconcile(ctx context.Context, owner Owner, event Event) (codex.SendReceipt, bool, error) {
	if n.Client == nil || event.Delivery.Options == nil {
		return codex.SendReceipt{}, false, errors.New("monitor delivery options are unavailable")
	}
	d := event.Delivery
	return n.Client.ReconcileSendWithOptions(ctx, owner.LeadTarget(), d.Envelope, d.MessageID, d.Context, *d.Options)
}

func (n NativeDelivery) ObserveUtility(ctx context.Context, op Operation) (string, bool, error) {
	if n.Client == nil || op.Launch.Receipt == nil || op.Launch.Receipt.TurnID == "" || op.Launch.ThreadID == "" || op.Launch.ProjectID == "" {
		return "", false, nil
	}
	metadata, err := n.Client.ReadThreadMetadata(ctx, op.Launch.ThreadID, true)
	if err != nil {
		return "", false, err
	}
	if metadata.ID != op.Launch.ThreadID || metadata.Cwd != filepath.Join(n.Workspace, "work", op.Owner.Slug) || metadata.ProjectID == nil || *metadata.ProjectID != op.Launch.ProjectID {
		return "", false, errors.New("utility observation identity changed")
	}
	state, err := n.Client.ReadTurnState(ctx, op.Launch.ThreadID)
	if err != nil {
		return "", false, err
	}
	if state.ThreadID != op.Launch.ThreadID || state.LatestTurnID != op.Launch.Receipt.TurnID || state.Status == "active" {
		return "", false, nil
	}
	terminal := slices.Contains([]string{"completed", "failed", "interrupted"}, state.LatestTurnStatus)
	return state.LatestTurnStatus, terminal, nil
}
