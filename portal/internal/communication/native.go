package communication

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type NativeClient interface {
	ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
	ReadActivity(context.Context, string) (codex.ActivitySnapshot, error)
	SendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, error)
	ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error)
}

type Native struct {
	Workspace, StateRoot, AuthorityDir, Socket string
	Client                                     NativeClient
	Admission                                  func(Identity) error
}

func IdentityFor(workspace, slug string, archived bool) (Identity, error) {
	summary, err := session.Find(workspace, slug)
	if err != nil {
		return Identity{}, err
	}
	if summary.Codex.ThreadID == "" || summary.Archived && !archived {
		return Identity{}, errors.New("session has no available lead")
	}
	if !archived && summary.Creation.State == "creating" {
		return Identity{}, errors.New("session initialization has not finished")
	}
	if !archived {
		owner, err := session.PendingLifecycle(workspace, slug)
		if err != nil {
			return Identity{}, err
		}
		if owner != "" {
			return Identity{}, errors.New("session lifecycle operation is unfinished")
		}
	}
	incarnation, err := session.ObservationIdentity(workspace, slug, filepath.Join(workspace, summary.Root, slug), summary.Codex.ThreadID)
	return Identity{slug, summary.Codex.ThreadID, incarnation}, err
}

// ResolveLead accepts only a unique retained root, never a team member or a
// thread ID supplied as a tool argument.
func ResolveLead(workspace, thread string) (Identity, error) {
	if thread == "" {
		return Identity{}, errors.New("native caller thread is required")
	}
	summaries, err := session.List(workspace)
	if err != nil {
		return Identity{}, err
	}
	var found Identity
	for _, summary := range summaries {
		if summary.Codex.ThreadID == thread {
			if found.Thread != "" {
				return Identity{}, errors.New("ambiguous calling lead")
			}
			found, err = IdentityFor(workspace, summary.Slug, false)
			if err != nil {
				return Identity{}, err
			}
		}
	}
	if found.Thread == "" {
		return Identity{}, errors.New("only a current session lead can communicate with other sessions")
	}
	return found, nil
}

func (n Native) Service() (Service, error) {
	store, err := NewStore(n.StateRoot, n.Workspace)
	if err != nil {
		return Service{}, err
	}
	return Service{Store: store, Transport: n,
		Resolve: func(slug string) (Identity, error) { return IdentityFor(n.Workspace, slug, false) },
		Lock: func(ctx context.Context, slug string) (func(), error) {
			lock, err := session.LockRuntimeSharedContext(ctx, n.AuthorityDir, slug)
			if err != nil {
				return nil, err
			}
			return func() { _ = lock.Close() }, nil
		},
	}, nil
}

func (n Native) Ready(ctx context.Context, id Identity) (string, error) {
	if n.Admission != nil {
		if err := n.Admission(id); err != nil {
			return "Recipient provider is paused or changing", nil
		}
	}
	authority, err := session.LoadRuntimeAuthority(n.AuthorityDir, id.Slug, n.Workspace)
	if err != nil || authority.State != "ready" || authority.CodexThreadID != id.Thread || authority.CodexSocketPath != n.Socket {
		return "Recipient is stopped or unavailable", nil
	}
	recovery, err := (session.RecoveryStore{Root: n.StateRoot, Workspace: n.Workspace}).Load(id.Slug)
	if err != nil {
		return "Recipient execution is held", nil
	}
	epoch, err := session.SocketIdentity(n.Socket)
	if err != nil || recovery.ThreadID != id.Thread || recovery.SocketPath != n.Socket || !recovery.Active(id.Thread, epoch) {
		return "Recipient execution is held", nil
	}
	metadata, err := n.Client.ReadThreadMetadata(ctx, id.Thread, false)
	if err != nil || metadata.ID != id.Thread || metadata.Cwd != filepath.Join(n.Workspace, "work", id.Slug) {
		return "Recipient conversation identity cannot be verified", nil
	}
	activity, err := n.Client.ReadActivity(ctx, id.Thread)
	if err != nil {
		return "", err
	}
	if activity.WaitReason == "userInput" || activity.WaitReason == "approval" {
		return "Recipient is waiting for the user", nil
	}
	if activity.CurrentState != "idle" && activity.CurrentState != "working" && activity.CurrentState != "waiting" {
		return "Recipient activity is unavailable", nil
	}
	return "", nil
}

func (n Native) Prepare(ctx context.Context, id Identity) (codex.TurnOptions, error) {
	store, err := teamruntime.NewStore(n.StateRoot, n.Workspace)
	if err != nil {
		return codex.TurnOptions{}, err
	}
	instructions := ""
	roster, err := store.Load(id.Slug, id.Thread)
	if err == nil {
		instructions = roster.LeadInstructions
	} else if !errors.Is(err, os.ErrNotExist) {
		return codex.TurnOptions{}, err
	}
	return codex.TurnOptions{ThreadPolicy: workspacecodex.LeadThreadPolicy(id.Slug, n.Workspace, instructions)}, nil
}

func (n Native) Send(ctx context.Context, m Message) (codex.SendReceipt, error) {
	return n.Client.SendWithOptions(ctx, m.To.Thread, m.Envelope, m.ID, "communication:"+m.GrantID, *m.Options)
}

func (n Native) Reconcile(ctx context.Context, m Message) (codex.SendReceipt, bool, error) {
	if m.Options == nil {
		return codex.SendReceipt{}, false, errors.New("missing saved delivery options")
	}
	return n.Client.ReconcileSendWithOptions(ctx, m.To.Thread, m.Envelope, m.ID, "communication:"+m.GrantID, *m.Options)
}
