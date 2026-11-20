package monitoroperation

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

// DiagnosticError retains a fixed proof boundary without changing the existing
// error text. Operator projections must validate its metadata and omit causes.
type DiagnosticError struct {
	Stage, Category, MonitorID, OwnerSlug string
	Err, Cause                            error
}

func (e *DiagnosticError) Error() string { return e.Err.Error() }
func (e *DiagnosticError) Unwrap() error { return errors.Join(e.Err, e.Cause) }

func lifecycleDiagnostic(op Operation, stage, category string, err, cause error) error {
	if cause != nil {
		category = "unavailable-proof"
	}
	return &DiagnosticError{Stage: stage, Category: category, MonitorID: op.ID, OwnerSlug: op.Owner.Slug, Err: err, Cause: cause}
}

type LifecycleClient interface {
	ReadProject(context.Context, string) (codex.ProjectMetadata, error)
	ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
	VerifyHeadlessBootstrap(context.Context, string, string, string, string) error
	ProveArchivedThread(context.Context, string, string, string) (workspacecodex.ArchiveState, error)
	RequireArchiveThreadIdle(context.Context, string, string, workspacecodex.ArchiveState) error
	ArchiveThread(context.Context, string) error
}

// Lifecycle participates only in its caller's existing session admission. It
// never acquires a session lock, resumes a utility, interrupts a command, or
// performs session lifecycle actions. Completed utilities retain their history.
type Lifecycle struct {
	Store            *Store
	Client           LifecycleClient
	Observer         CIObserver
	AuthorizeArchive func(context.Context, ArchiveAuthority) error
	Now              func() time.Time
	PiProof          PiLifecycleVerifier
}

// Pi lifecycle proof is domain tagged and current, never the legacy absence of
// a Codex thread. No implementation is registered before utility/drain support.
type PiLifecycleVerifier interface {
	ProvePiQuiescent(context.Context, PiOperation) error
}

func (l Lifecycle) State(ctx context.Context, op Operation) (workspacecodex.ArchiveState, error) {
	if l.Store == nil || l.Client == nil || op.Workspace != l.Store.workspace || op.Launch.ThreadID == "" || op.Launch.ProjectID == "" {
		return workspacecodex.ArchiveUnknown, lifecycleDiagnostic(op, "native-thread", "uncertain-state", errors.New("monitor utility has no exact retained identity"), nil)
	}
	finish := BeginProof(ctx, op.ID, "native-project")
	project, err := l.Client.ReadProject(ctx, op.Launch.ProjectID)
	finish(err, project.ID != op.Launch.ProjectID || project.Name != "monitor "+op.ID)
	if err != nil || project.ID != op.Launch.ProjectID || project.Name != "monitor "+op.ID {
		return workspacecodex.ArchiveUnknown, lifecycleDiagnostic(op, "native-project", "identity-mismatch", errors.New("monitor project identity is unproved"), err)
	}
	cwd := filepath.Join(l.Store.workspace, "work", op.Owner.Slug)
	finish = BeginProof(ctx, op.ID, "native-thread")
	metadata, err := l.Client.ReadThreadMetadata(ctx, op.Launch.ThreadID, true)
	finish(err, metadata.ID != op.Launch.ThreadID || metadata.Cwd != cwd || metadata.ProjectID != nil && *metadata.ProjectID != op.Launch.ProjectID || metadata.Source != "vscode" || metadata.ForkedFromID != "")
	if err != nil || metadata.ID != op.Launch.ThreadID || metadata.Cwd != cwd ||
		metadata.ProjectID != nil && *metadata.ProjectID != op.Launch.ProjectID || metadata.Source != "vscode" || metadata.ForkedFromID != "" {
		return workspacecodex.ArchiveUnknown, lifecycleDiagnostic(op, "native-thread", "identity-mismatch", errors.New("monitor thread identity is unproved"), err)
	}
	if metadata.ProjectID == nil {
		var proofErr error
		if !op.Launch.BootstrapAttempted && op.Launch.Receipt == nil {
			if fresh, ok := l.Client.(interface {
				HeadlessThreadMaterialized(context.Context, string, string, string) (bool, error)
			}); ok {
				finish = BeginProof(ctx, op.ID, "native-materialized")
				materialized, err := fresh.HeadlessThreadMaterialized(ctx, op.Launch.ThreadID, cwd, op.Launch.ProjectID)
				finish(err, false)
				proofErr = err
				if err == nil && !materialized {
					return workspacecodex.ArchiveFresh, nil
				}
			}
		}
		return workspacecodex.ArchiveUnknown, lifecycleDiagnostic(op, "native-project", "uncertain-state", errors.New("monitor native project is not materialized or exactly proved fresh"), proofErr)
	}
	if op.Launch.BootstrapAttempted {
		finish = BeginProof(ctx, op.ID, "native-bootstrap")
		err := l.Client.VerifyHeadlessBootstrap(ctx, op.Launch.ThreadID, cwd, op.Launch.ProjectID, op.Launch.BootstrapMarker)
		finish(err, false)
		if err != nil {
			return workspacecodex.ArchiveUnknown, lifecycleDiagnostic(op, "native-bootstrap", "unavailable-proof", fmt.Errorf("monitor bootstrap identity is unproved: %w", err), nil)
		}
	}
	finish = BeginProof(ctx, op.ID, "native-archive-state")
	state, err := l.Client.ProveArchivedThread(ctx, op.Launch.ThreadID, cwd, op.Launch.ProjectID)
	finish(err, false)
	if err != nil {
		err = lifecycleDiagnostic(op, "native-archive-state", "unavailable-proof", err, nil)
	}
	return state, err
}

// ProveQuiescent ignores only the external-effects fence, so an exact lead can
// attach evidence without pretending an unknown launch or native send ended.
// It is safe inside Service.CheckResolution's existing operation transaction.
func (l Lifecycle) ProveQuiescent(ctx context.Context, op Operation) error {
	if op.Pi != nil {
		if op.Pi.Validate() != nil || op.Pi.Settlement == nil || op.Pi.Settlement.State != "settled" || op.Pi.Launch.TerminatedAt == nil {
			return ErrPiUtilityUnavailable
		}
		if l.PiProof == nil {
			return ErrPiUtilityUnavailable
		}
		return l.PiProof.ProvePiQuiescent(ctx, *op.Pi)
	}
	if err := knownMonitorReceipts(op); err != nil {
		return lifecycleDiagnostic(op, "receipt-proof", "uncertain-state", err, nil)
	}
	return l.proveUtilityQuiescent(ctx, op)
}

func knownMonitorReceipts(op Operation) error {
	if op.Pi != nil {
		return ErrPiUtilityUnavailable
	}
	if (op.Launch.State == "submitting" || op.Launch.State == "running") && op.Launch.Receipt == nil {
		return errors.New("monitor launch submission is still uncertain")
	}
	if op.Launch.Receipt != nil && (op.Launch.Receipt.TurnID == "" || op.Launch.Receipt.QueuedSubmissionID != "") {
		return errors.New("monitor launch has no exact turn receipt")
	}
	for _, event := range op.Events {
		if event.Delivery.Status == "submitting" || event.Delivery.Receipt != nil && event.Delivery.Receipt.TurnID == "" {
			return errors.New("monitor lead submission is still uncertain")
		}
	}
	return nil
}

func (l Lifecycle) proveUtilityQuiescent(ctx context.Context, op Operation) error {
	if op.Launch.ThreadID == "" {
		if op.Launch.CreateAttempted {
			return lifecycleDiagnostic(op, "native-thread", "uncertain-state", errors.New("monitor thread creation is still uncertain"), nil)
		}
		return nil // Durable intent proves thread/start was never attempted.
	}
	state, err := l.State(ctx, op)
	if err != nil {
		return err
	}
	cwd := filepath.Join(l.Store.workspace, "work", op.Owner.Slug)
	if op.CanceledAt != nil && (op.Launch.State == "canceled-unsent" || op.Launch.State == "unsubmitted") {
		// Preserve a definitely-unsubmitted preparation instead of deleting its
		// receipt identity. This exception is exact; other attempts remain fenced.
		if observer, ok := l.Client.(interface {
			IsPreparedSendWithOptions(string, string, string, string, codex.TurnOptions) (bool, error)
			ObserveThreadIdentityExceptPreparedSend(context.Context, string, string, string, workspacecodex.ArchiveState, workspacecodex.PreparedSend) (workspacecodex.ThreadObservation, error)
		}); ok {
			finish := BeginProof(ctx, op.ID, "prepared-receipt")
			prepared, err := observer.IsPreparedSendWithOptions(op.Launch.ThreadID, op.Launch.Envelope, op.Launch.MessageID, "monitor-launch:"+op.ID, op.Launch.Options)
			finish(err, false)
			if err != nil {
				return lifecycleDiagnostic(op, "receipt-proof", "unavailable-proof", err, nil)
			}
			if prepared {
				finish = BeginProof(ctx, op.ID, "native-prepared-idle")
				observation, err := observer.ObserveThreadIdentityExceptPreparedSend(ctx, op.Launch.ThreadID, cwd, op.Launch.ProjectID, state,
					workspacecodex.PreparedSend{Text: op.Launch.Envelope, ID: op.Launch.MessageID, Action: "monitor-launch:" + op.ID, Options: op.Launch.Options})
				finish(err, false)
				if err != nil {
					return lifecycleDiagnostic(op, "native-idle", "unavailable-proof", err, nil)
				}
				if !observation.ActivityKnown || !observation.Idle {
					return lifecycleDiagnostic(op, "native-idle", "uncertain-state", errors.New("canceled monitor preparation has unproved native quiescence"), nil)
				}
				return nil
			}
		}
	}
	finish := BeginProof(ctx, op.ID, "native-idle")
	err = l.Client.RequireArchiveThreadIdle(ctx, op.Launch.ThreadID, cwd, state)
	finish(err, false)
	if err != nil {
		return lifecycleDiagnostic(op, "native-idle", "unavailable-proof", err, nil)
	}
	return nil
}

func (l Lifecycle) RequireIdle(ctx context.Context, slug, root string, executionOnly bool) error {
	operations, err := l.Store.List(ctx, slug)
	if err != nil {
		return &DiagnosticError{Stage: "inventory-store", Category: "unavailable-proof", Err: err}
	}
	for _, op := range operations {
		if op.Owner.RootThreadID != root {
			if op.Disposition != "archived" {
				return lifecycleDiagnostic(op, "owner-match", "identity-mismatch", errors.New("monitor lifecycle inventory has a different retained owner"), nil)
			}
			continue
		}
		if HasUnresolvedEffects(op) {
			return lifecycleDiagnostic(op, "receipt-proof", "uncertain-state", fmt.Errorf("monitor %s retains external work; record exact evidence with monitor resolve", op.ID), nil)
		}
		if err := knownMonitorReceipts(op); err != nil {
			return lifecycleDiagnostic(op, "receipt-proof", "uncertain-state", err, nil)
		}
		if executionOnly && op.Launch.ThreadID != "" && op.Disposition == "" {
			proof, ok := l.Client.(interface {
				RequireThreadTurnsIdle(context.Context, string) error
				ListQueue(context.Context, string) ([]codex.QueueEntry, error)
				RequireSubmissionAttemptsKnown(context.Context, string) error
			})
			if !ok {
				return errors.New("monitor transition execution proof is unavailable")
			}
			if _, err := l.State(ctx, op); err != nil {
				return err
			}
			metadata, err := l.Client.ReadThreadMetadata(ctx, op.Launch.ThreadID, true)
			if err != nil || (metadata.Status["type"] != "idle" && metadata.Status["type"] != "notLoaded") {
				return errors.New("monitor native execution status is not quiescent")
			}
			if err := proof.RequireThreadTurnsIdle(ctx, op.Launch.ThreadID); err != nil {
				return err
			}
			queued, err := proof.ListQueue(ctx, op.Launch.ThreadID)
			if err != nil || len(queued) != 0 {
				return errors.Join(err, errors.New("monitor utility has unconfirmed queued work"))
			}
			if err := proof.RequireSubmissionAttemptsKnown(ctx, op.Launch.ThreadID); err != nil {
				return err
			}
			// Utilities never enroll in automatic turn retry. Read their original
			// storage directly instead of resolving them as retained team roles.
			continue
		}
		if err := l.ProveQuiescent(ctx, op); err != nil {
			return fmt.Errorf("monitor %s is not quiescent: %w", op.ID, err)
		}
	}
	return l.requireObserverIdle(ctx, slug, root, executionOnly)
}

func (l Lifecycle) RequireArchived(ctx context.Context, slug, root string) error {
	if err := l.requireObserverIdle(ctx, slug, root, false); err != nil {
		return err
	}
	if err := l.requireObserverRetired(ctx, slug, root); err != nil {
		return err
	}
	operations, err := l.Store.List(ctx, slug)
	if err != nil {
		return err
	}
	for _, op := range operations {
		if op.Owner.RootThreadID != root {
			continue
		}
		if op.Disposition != "archived" {
			return errors.New("monitor operation retirement is unfinished")
		}
		if op.Launch.ThreadID != "" {
			state, err := l.State(ctx, op)
			if err != nil || state != workspacecodex.ArchiveArchived {
				return errors.Join(err, errors.New("monitor utility is not positively archived"))
			}
		}
	}
	return nil
}

func (l Lifecycle) Revoke(ctx context.Context, slug, root string) error {
	operations, err := l.Store.List(ctx, slug)
	if err != nil {
		return err
	}
	for _, op := range operations {
		if op.Owner.RootThreadID != root {
			if op.Disposition != "archived" {
				return errors.New("monitor revocation has a different retained owner")
			}
			continue
		}
		tx, err := l.Store.Lock(ctx, op.ID)
		if err != nil {
			return err
		}
		if tx.Operation.CanceledAt == nil {
			now := time.Now().UTC()
			tx.Operation.CanceledAt = &now
		}
		for i := range tx.Operation.Events {
			delivery := &tx.Operation.Events[i].Delivery
			if delivery.Status != "accepted" && delivery.Status != "submitting" {
				delivery.Status = "canceled"
			}
		}
		err = tx.Save()
		_ = tx.Close()
		if err != nil {
			return err
		}
	}
	return l.revokeDirectObservers(ctx, slug, root)
}

// Archive is invoked only by the ordinary explicitly authorized lifecycle
// owner. It never interrupts work or clears original submission receipts.
func (l Lifecycle) Archive(ctx context.Context, slug, root string) error {
	if err := l.Revoke(ctx, slug, root); err != nil {
		return err
	}
	if err := l.RequireIdle(ctx, slug, root, false); err != nil {
		return err
	}
	operations, err := l.Store.List(ctx, slug)
	if err != nil {
		return err
	}
	for _, op := range operations {
		if op.Owner.RootThreadID != root {
			continue
		}
		tx, err := l.Store.Lock(ctx, op.ID)
		if err != nil {
			return err
		}
		err = l.archiveLocked(ctx, tx)
		_ = tx.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (l Lifecycle) archiveLocked(ctx context.Context, tx *Transaction) error {
	op := tx.Operation
	if err := l.ProveQuiescent(ctx, *op); err != nil {
		return err
	}
	if op.Launch.ThreadID == "" && !op.Launch.CreateAttempted {
		op.Disposition = "archived"
		return tx.Save()
	}
	state, err := l.State(ctx, *op)
	if err != nil {
		return err
	}
	if state != workspacecodex.ArchiveArchived {
		op.Disposition = "archiving"
		if err := tx.Save(); err != nil {
			return err
		}
		archiveErr := l.Client.ArchiveThread(ctx, op.Launch.ThreadID)
		state, err = l.State(ctx, *op)
		if err != nil || state != workspacecodex.ArchiveArchived {
			return errors.Join(archiveErr, err, errors.New("exact monitor archive outcome remains uncertain"))
		}
	}
	op.Disposition = "archived"
	return tx.Save()
}
