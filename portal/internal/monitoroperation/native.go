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
func ResolveDeliveryOwner(workspace, slug string) (Owner, error) {
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
	epoch, err := session.SocketIdentity(summary.Codex.SocketPath)
	if err != nil {
		return Owner{}, err
	}
	owner.Socket, owner.SocketEpoch, owner.RuntimeSchema = summary.Codex.SocketPath, epoch, Schema
	return owner, nil
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
	recovery, err := (session.RecoveryStore{Root: n.StateRoot, Workspace: n.Workspace}).Load(owner.Slug)
	if err != nil {
		return "Session execution is held", nil
	}
	epoch, err := session.SocketIdentity(n.Socket)
	if err != nil || owner.SocketEpoch != epoch || recovery.ThreadID != owner.RootThreadID || recovery.SocketPath != n.Socket || !recovery.Active(owner.RootThreadID, epoch) {
		return "Session execution is held", nil
	}
	metadata, err := n.Client.ReadThreadMetadata(ctx, owner.RootThreadID, false)
	if err != nil || metadata.ID != owner.RootThreadID || metadata.Cwd != filepath.Join(n.Workspace, "work", owner.Slug) {
		return "Lead conversation identity cannot be verified", nil
	}
	activity, err := n.Client.ReadActivity(ctx, owner.RootThreadID)
	if err != nil {
		return "", err
	}
	if activity.Stale || activity.ThreadID != owner.RootThreadID {
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
	return n.PrepareLead(ctx, owner)
}

func (n NativeDelivery) Send(ctx context.Context, owner Owner, event Event) (codex.SendReceipt, error) {
	if n.Client == nil || event.Delivery.Options == nil {
		return codex.SendReceipt{}, errors.New("monitor delivery options are unavailable")
	}
	d := event.Delivery
	receipt, err := n.Client.SendWithOptions(ctx, owner.RootThreadID, d.Envelope, d.MessageID, d.Context, *d.Options)
	var unknown *codex.UnknownSendOutcomeError
	var unsent *codex.MessageNotSentError
	if err == nil || receipt.TurnID != "" || receipt.QueuedSubmissionID != "" || errors.As(err, &unknown) || !errors.As(err, &unsent) {
		return receipt, err
	}
	prepared, proofErr := n.Client.IsPreparedSendWithOptions(owner.RootThreadID, d.Envelope, d.MessageID, d.Context, *d.Options)
	if proofErr != nil || !prepared {
		return receipt, &codex.UnknownSendOutcomeError{Err: errors.Join(err, errors.New("monitor input preparation is unconfirmed"), proofErr)}
	}
	if reason, readyErr := n.Ready(ctx, owner); readyErr != nil || reason != "" {
		return receipt, err
	}
	// Dispatch still holds session/operation admission. Retire only this exact
	// unsubmitted preparation; a later ordinary scan chooses current routing
	// with the original ID and options. Never submit a second time inline.
	discarded, discardErr := n.Client.DiscardPreparedSendWithOptions(owner.RootThreadID, d.Envelope, d.MessageID, d.Context, *d.Options)
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
	return n.Client.ReconcileSendWithOptions(ctx, owner.RootThreadID, d.Envelope, d.MessageID, d.Context, *d.Options)
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
