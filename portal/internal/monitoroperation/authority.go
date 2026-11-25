package monitoroperation

import (
	"context"
	"errors"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"path/filepath"
	"slices"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type AuthorityClient interface {
	ActiveTurnID(context.Context, string) (string, error)
	ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
}

type NativeAuthority struct {
	Workspace, StateRoot, AuthorityDir, Socket string
	Client                                     AuthorityClient
	Admission                                  func(Owner) error
	Provider                                   func(string) (string, error)
}

func IdentityFor(workspace, slug string) (Owner, error) {
	summary, err := session.Find(workspace, slug)
	if err != nil {
		return Owner{}, err
	}
	if summary.Archived || summary.Terminal || summary.Codex.ThreadID == "" || summary.Creation.State == "creating" {
		return Owner{}, errors.New("monitor owner is not an active initialized session")
	}
	pending, err := session.PendingLifecycle(workspace, slug)
	if err != nil {
		return Owner{}, err
	}
	if pending != "" {
		return Owner{}, errors.New("monitor owner has unfinished lifecycle work")
	}
	incarnation, err := session.ObservationIdentity(workspace, slug, filepath.Join(workspace, summary.Root, slug), summary.Codex.ThreadID)
	if err != nil {
		return Owner{}, err
	}
	return Owner{Slug: slug, RootThreadID: summary.Codex.ThreadID, Incarnation: incarnation}, nil
}

func ResolveLead(workspace, thread string) (Owner, error) {
	if thread == "" {
		return Owner{}, errors.New("native monitor caller is required")
	}
	summaries, err := session.List(workspace)
	if err != nil {
		return Owner{}, err
	}
	var found *Owner
	for _, summary := range summaries {
		if summary.Codex.ThreadID != thread || summary.Codex.BindingSchema == 3 {
			continue
		}
		if found != nil {
			return Owner{}, errors.New("ambiguous monitor lead")
		}
		owner, err := IdentityFor(workspace, summary.Slug)
		if err != nil {
			return Owner{}, err
		}
		found = &owner
	}
	if found == nil {
		return Owner{}, errors.New("only the exact current session lead can start or cancel monitoring")
	}
	return *found, nil
}

func (n NativeAuthority) Resolve(slug string) (Owner, error) {
	owner, err := IdentityFor(n.Workspace, slug)
	if err != nil {
		return Owner{}, err
	}
	summary, err := session.Find(n.Workspace, slug)
	if err != nil {
		return Owner{}, err
	}
	if n.Socket == "" && summary.Codex.BindingSchema == 3 {
		n.Socket = summary.Codex.SocketPath
		if !canonicalPath(n.Socket) || filepath.Base(n.Socket) != "app-server.sock" || !codexproviders.ValidID(filepath.Base(filepath.Dir(n.Socket))) {
			return Owner{}, errors.New("monitor binding locator changed")
		}
		n.AuthorityDir = filepath.Join(filepath.Dir(n.Socket), "authority")
	}
	authority, err := session.LoadRuntimeAuthority(n.AuthorityDir, slug, n.Workspace)
	if err != nil || authority.State != "ready" || authority.CodexThreadID != owner.RootThreadID || authority.CodexSocketPath != n.Socket {
		return Owner{}, errors.New("monitor session runtime is stopped or changed")
	}
	epoch, err := session.SocketIdentity(n.Socket)
	if err != nil {
		return Owner{}, err
	}
	owner.Socket, owner.SocketEpoch, owner.RuntimeSchema = n.Socket, epoch, authority.Schema
	if summary.Codex.BindingSchema == 3 {
		if filepath.Base(n.Socket) != "app-server.sock" || !codexproviders.ValidID(filepath.Base(filepath.Dir(n.Socket))) {
			return Owner{}, errors.New("monitor binding locator changed")
		}
		catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
		if err != nil {
			return Owner{}, err
		}
		state, err := codexproviders.ReadBindingState(n.StateRoot, filepath.Base(filepath.Dir(n.Socket)), n.Workspace, n.Socket, catalog)
		if err != nil {
			return Owner{}, err
		}
		if state.BlocksSession(slug, owner.RootThreadID) {
			return Owner{}, errors.New("monitor binding transition is unfinished")
		}
		resolved, err := authority.ResolveCodexBinding(summary, state, "lead", owner.RootThreadID, true)
		if err != nil {
			return Owner{}, err
		}
		owner.LeadThreadID, owner.Provider = resolved.ThreadID, resolved.Provider
	} else if authority.Schema != 1 {
		return Owner{}, errors.New("monitor authority schema differs")
	}

	if n.Provider != nil {
		provider, providerErr := n.Provider(slug)
		err = providerErr
		if owner.RuntimeSchema == 3 && provider != owner.Provider {
			return Owner{}, errors.New("monitor current provider changed")
		}
		owner.Provider = provider
		if err != nil {
			return Owner{}, err
		}
	}
	if n.Admission != nil {
		if err := n.Admission(owner); err != nil {
			return Owner{}, err
		}
	}
	return owner, nil
}

func (n NativeAuthority) ResolveCaller(thread string) (Owner, error) {
	if thread == "" {
		return Owner{}, errors.New("native monitor caller is required")
	}
	summaries, err := session.List(n.Workspace)
	if err != nil {
		return Owner{}, err
	}
	var found *Owner
	for _, summary := range summaries {
		owner, err := n.Resolve(summary.Slug)
		if err != nil {
			continue
		}
		if owner.LeadTarget() != thread {
			continue
		}
		if found != nil {
			return Owner{}, errors.New("ambiguous current monitor lead")
		}
		found = &owner
	}
	if found == nil {
		return Owner{}, errors.New("only the exact current session lead can manage monitoring")
	}
	return *found, nil
}

func (n NativeAuthority) ResolveLead(ctx context.Context, thread string) (Owner, error) {
	owner, err := n.ResolveCaller(thread)
	if err != nil {
		return Owner{}, err
	}
	if n.Client == nil {
		return Owner{}, errors.New("monitor native authority client is unavailable")
	}
	metadata, err := n.Client.ReadThreadMetadata(ctx, thread, false)
	if err != nil || metadata.ID != thread || metadata.Cwd != filepath.Join(n.Workspace, "work", owner.Slug) {
		return Owner{}, errors.New("monitor native lead identity cannot be verified")
	}
	owner.OriginTurnID, err = n.Client.ActiveTurnID(ctx, thread)
	return owner, err
}

func (n NativeAuthority) LockSession(ctx context.Context, slug string) (func(), error) {
	lock, err := session.LockRuntimeSharedContext(ctx, n.AuthorityDir, slug)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}

// Activate is called only after launch has persisted the exact utility binding,
// while the caller holds session admission. It cannot activate a held root.
func (n NativeAuthority) Activate(ctx context.Context, owner Owner, thread string) error {
	if thread == "" || thread == owner.LeadTarget() {
		return errors.New("invalid monitor utility activation")
	}
	if err := n.Admit(ctx, owner); err != nil {
		return err
	}
	return (session.RecoveryStore{Root: n.StateRoot, Workspace: n.Workspace}).Update(ctx, owner.Slug, func(record *session.Recovery) error {
		if record.ThreadID != owner.RootThreadID || record.SocketPath != owner.Socket || record.SocketIdentity != owner.SocketEpoch || !record.Active(owner.LeadTarget(), owner.SocketEpoch) {
			return errors.New("monitor launch cannot grant execution to a stopped or held session")
		}
		if !slices.Contains(record.ActiveThreads, thread) {
			record.ActiveThreads = append(record.ActiveThreads, thread)
		}
		return nil
	})
}

func (n NativeAuthority) Admit(ctx context.Context, owner Owner) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := n.Resolve(owner.Slug)
	if err != nil || !SameLeadOwner(owner, current) || current.SocketEpoch != owner.SocketEpoch {
		return errors.New("monitor launch origin epoch or owner changed")
	}
	record, err := (session.RecoveryStore{Root: n.StateRoot, Workspace: n.Workspace}).Load(owner.Slug)
	if err != nil || record.ThreadID != owner.RootThreadID || record.SocketPath != owner.Socket || !record.Active(owner.LeadTarget(), owner.SocketEpoch) {
		return errors.New("monitor root execution is held")
	}
	return nil
}
