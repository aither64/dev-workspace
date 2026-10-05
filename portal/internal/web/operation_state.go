package web

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

const (
	lifecycleOperationSuccessRetention = 15 * time.Minute
	maxLifecycleOperationErrorBytes    = 4096
)

type cachedLifecycleSnapshot struct {
	operation lifecycleOperation
	checkedAt time.Time
}

func (s *Server) publishLifecycleSnapshot(slug string, operation lifecycleOperation, checkedAt time.Time) {
	s.operationSnapshotMu.Lock()
	defer s.operationSnapshotMu.Unlock()
	if s.operationSnapshots == nil {
		s.operationSnapshots = make(map[string]cachedLifecycleSnapshot)
	}
	// Receipt persistence may replace progress, but cannot manufacture a fresh
	// target observation. Keep the last observed target only for the same receipt.
	if previous, ok := s.operationSnapshots[slug]; ok && previous.operation.ReceiptID == operation.ReceiptID && operation.CurrentTarget == nil {
		operation.CurrentTarget = previous.operation.CurrentTarget
	}
	s.operationSnapshots[slug] = cachedLifecycleSnapshot{operation, checkedAt}
}

func (s *Server) requestDisplayRefresh(full bool) {
	s.displayRefreshMu.Lock()
	s.displayRefreshPending = true
	s.displayFullPending = s.displayFullPending || full
	s.displayRefreshMu.Unlock()
	select {
	case s.displayRefreshWake <- struct{}{}:
	default:
	}
}

// One cancellation-bound worker coalesces all display demand. Slow proof never
// holds operationMu or the separately protected published snapshot map.
func (s *Server) runDisplayRefreshes() {
	defer s.displayRefreshWG.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastReconcile time.Time
	backedOff := false
	for {
		select {
		case <-s.operationContext.Done():
			return
		case <-s.displayRefreshWake:
		case <-ticker.C:
		}
		now := time.Now()
		s.displayRefreshMu.Lock()
		requested, fullRequested := s.displayRefreshPending, s.displayFullPending
		s.displayRefreshMu.Unlock()
		s.indexStatusMu.Lock()
		full := fullRequested && !now.Before(s.indexStatusRetryAt)
		s.indexStatusMu.Unlock()
		operations, _ := s.lifecycleOperations()
		running := false
		for _, operation := range operations {
			running = running || operation.State == "running"
		}
		interval := 15 * time.Second
		if running && !backedOff {
			interval = time.Second
		}
		if !full && (now.Before(lastReconcile.Add(interval)) || (!requested && !running)) {
			continue
		}
		s.displayRefreshMu.Lock()
		s.displayRefreshPending = false
		if full {
			s.displayFullPending = false
		}
		s.displayRefreshMu.Unlock()
		ctx, cancel := context.WithTimeout(s.operationContext, 6*time.Second)
		err := s.refreshDisplaySnapshots(ctx, full)
		cancel()
		lastReconcile = now
		backedOff = err != nil
		s.operationSnapshotMu.Lock()
		s.operationStatusWarning = ""
		if err != nil {
			s.operationStatusWarning = "Lifecycle status is temporarily unavailable; showing the last known state."
		}
		s.operationSnapshotMu.Unlock()
		if err != nil {
			s.config.Logger.Printf("refresh portal status: %v", err)
		}
	}
}

func (s *Server) refreshDisplaySnapshots(ctx context.Context, full bool) (resultErr error) {
	attemptedAt := time.Now()
	// Include missing-record revisions so delayed journal discovery cannot revive
	// a receipt accepted and dismissed while this pass was reading its evidence.
	s.operationMu.Lock()
	originals := make(map[string]lifecycleOperation, len(s.operations))
	revisions := make(map[string]uint64, len(s.operationRevisions))
	for slug, operation := range s.operations {
		if full || operation.State == "running" {
			originals[slug] = operation
			revisions[slug] = s.operationRevisions[slug]
		}
	}
	if full {
		for slug, revision := range s.operationRevisions {
			revisions[slug] = revision
		}
	}
	s.operationMu.Unlock()
	if full {
		s.indexStatusMu.Lock()
		s.indexStatusRetryAt = attemptedAt.Add(15 * time.Second)
		s.indexStatusWait = make(chan struct{})
		s.indexStatusMu.Unlock()
		defer func() {
			s.indexStatusMu.Lock()
			if resultErr != nil {
				s.indexStatusRetryAt = attemptedAt.Add(30 * time.Second)
				s.indexStatusCache.warning = "Session status is temporarily unavailable; showing the last known state."
			}
			close(s.indexStatusWait)
			s.indexStatusWait = nil
			s.indexStatusMu.Unlock()
		}()
	}
	_, unlock, err := s.acquireTransitionContext(ctx, unix.LOCK_SH|unix.LOCK_NB)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		return err
	}
	var pending map[string]session.LifecycleProgress
	if full {
		pending, err = session.PendingLifecycles(s.config.Workspace)
		if err != nil {
			return err
		}
	}
	slugs := make(map[string]bool, len(originals)+len(pending))
	for slug := range originals {
		slugs[slug] = true
	}
	for slug := range pending {
		slugs[slug] = true
	}
	for slug := range slugs {
		if err := ctx.Err(); err != nil {
			return err
		}
		original, existed := originals[slug]
		var progress *session.LifecycleProgress
		var proofErr error
		if full {
			if found, ok := pending[slug]; ok {
				progress = &found
			}
		} else {
			progress, proofErr = session.PendingLifecycleProgress(s.config.Workspace, slug)
		}
		operation, exists, err := s.proposeLifecycleOperation(slug, original, existed, progress, proofErr, time.Now().UTC())
		if err != nil {
			return err
		}
		view := operation
		if full {
			view = s.lifecycleOperationView(slug, operation)
		}
		if err := s.acceptLifecycleReconciliation(slug, original, existed, revisions[slug], operation, exists, view); err != nil {
			return err
		}
	}
	if full {
		result := s.computeIndexStatusWithPending(ctx, pending, nil)
		if err := ctx.Err(); err != nil {
			return err
		}
		result.created = time.Now().UTC()
		s.indexStatusMu.Lock()
		s.indexStatusCache = result
		if result.warning != "" {
			s.indexStatusRetryAt = attemptedAt.Add(30 * time.Second)
		}
		s.indexStatusMu.Unlock()
	}
	return nil
}

func validateLifecycleOperation(operation lifecycleOperation) error {
	if !session.ValidSlug(operation.Slug) {
		return errors.New("invalid session slug")
	}
	if operation.Kind != "archive" && operation.Kind != "delete" && operation.Kind != "revive" {
		return errors.New("invalid operation kind")
	}
	if operation.State != "running" && operation.State != "paused" &&
		operation.State != "failed" && operation.State != "complete" {
		return errors.New("invalid operation state")
	}
	if operation.Phase == "" || len(operation.Phase) > 64 || !utf8.ValidString(operation.Phase) {
		return errors.New("invalid operation phase")
	}
	for label, value := range map[string]string{
		"startedAt": operation.StartedAt,
		"updatedAt": operation.UpdatedAt,
	} {
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return fmt.Errorf("invalid operation %s", label)
		}
	}
	if len(operation.Error) > maxLifecycleOperationErrorBytes || !utf8.ValidString(operation.Error) {
		return errors.New("invalid operation error")
	}
	if operation.ErrorCode != "" && operation.ErrorCode != "target_changed" {
		return errors.New("invalid lifecycle error code")
	}
	if operation.ReceiptID != "" && !messageDigestPattern.MatchString(operation.ReceiptID) {
		return errors.New("invalid operation receipt identity")
	}
	journalID := operation.Options.JournalID
	if journalID != "" && !messageDigestPattern.MatchString(journalID) {
		return errors.New("invalid lifecycle journal identity")
	}
	if operation.Options.JournalExpected && journalID == "" {
		return errors.New("expected lifecycle journal has no identity")
	}
	if operation.Attempt < 0 {
		return errors.New("invalid lifecycle attempt")
	}
	if operation.TargetIdentityVersion != 0 && operation.TargetIdentityVersion != lifecycleTargetIdentityVersion {
		return errors.New("unsupported lifecycle target identity version")
	}
	for _, digest := range []string{operation.Options.JournalEvidence, operation.Options.JournalTargetID} {
		if digest != "" && !messageDigestPattern.MatchString(digest) {
			return errors.New("invalid lifecycle proof identity")
		}
	}
	for _, location := range []string{operation.Options.TargetLocation, operation.Options.JournalTargetLocation} {
		if location != "" && location != "work" && location != "archive" {
			return errors.New("invalid lifecycle target location")
		}
	}
	targetID := operation.Options.TargetID
	if targetID != "" && !messageDigestPattern.MatchString(targetID) {
		return errors.New("invalid lifecycle target identity")
	}
	if operation.TargetIdentityVersion == lifecycleTargetIdentityVersion &&
		(targetID == "" || operation.Options.TargetLocation == "") {
		return errors.New("versioned lifecycle receipt has no target binding")
	}
	if operation.Redirect != "/" && operation.Redirect != "/"+operation.Slug+"/" {
		return errors.New("invalid operation redirect")
	}
	switch operation.Kind {
	case "archive":
		if operation.Options.Mode != "complete" && operation.Options.Mode != "abandoned" ||
			operation.Options.DeletedThreadID != "" {
			return errors.New("invalid archive retry mode")
		}
	case "delete":
		if operation.Options.Mode != "" || operation.Options.AllowAbandoned {
			return errors.New("invalid delete retry options")
		}
		threadID := operation.Options.DeletedThreadID
		if len(threadID) > 1024 || !utf8.ValidString(threadID) ||
			strings.ContainsAny(threadID, "\x00\r\n") {
			return errors.New("invalid deleted thread identity")
		}
	case "revive":
		if operation.Options.Mode != "" || operation.Options.Force ||
			operation.Options.DeletedThreadID != "" {
			return errors.New("invalid revive retry options")
		}
	}
	return nil
}

func operationFromProgress(slug string, progress session.LifecycleProgress) lifecycleOperation {
	updated := progress.UpdatedAt.UTC().Format(time.RFC3339Nano)
	receiptIdentity := fmt.Sprintf(
		"%s\x00%s\x00%s\x00%s", slug, progress.Operation, progress.JournalID, updated,
	)
	operation := lifecycleOperation{
		Slug: slug, Kind: progress.Operation, State: "paused", Phase: progress.Phase,
		StartedAt: updated, UpdatedAt: updated, Redirect: operationRedirect(slug, progress.Operation),
		ReceiptID: fmt.Sprintf("%x", sha256.Sum256([]byte(receiptIdentity))),
	}
	operation.applyProgressOptions(progress)
	return operation
}

func operationRedirect(slug, kind string) string {
	if kind == "revive" {
		return "/" + slug + "/"
	}
	return "/"
}

func (operation *lifecycleOperation) applyProgressOptions(progress session.LifecycleProgress) {
	operation.Options.JournalID = progress.JournalID
	operation.Options.JournalExpected = true
	operation.Options.JournalEvidence = progress.Evidence
	switch progress.Operation {
	case "archive":
		operation.Options.Mode = progress.Mode
	case "delete":
		operation.Options.Force = progress.Force
	}
}

// Mutation callers use fresh owner proof, never the display snapshot.
func (s *Server) lifecycleOperationForSlug(slug string) (lifecycleOperation, bool, error) {
	original, existed, revision := s.captureLifecycleOperation(slug)
	ctx, cancel := context.WithTimeout(s.operationContext, 15*time.Second)
	defer cancel()
	_, unlock, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return original, existed, err
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		return original, existed, err
	}
	progress, proofErr := session.PendingLifecycleProgress(s.config.Workspace, slug)
	operation, exists, err := s.proposeLifecycleOperation(slug, original, existed, progress, proofErr, time.Now().UTC())
	view := s.lifecycleOperationView(slug, operation)
	if saveErr := s.acceptLifecycleReconciliation(slug, original, existed, revision, operation, exists, view); saveErr != nil {
		return original, existed, errors.Join(err, saveErr)
	}
	current, present, _ := s.captureLifecycleOperation(slug)
	return current, present, err
}

func (s *Server) captureLifecycleOperation(slug string) (lifecycleOperation, bool, uint64) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	operation, exists := s.operations[slug]
	return operation, exists, s.operationRevisions[slug]
}

func (s *Server) proposeLifecycleOperation(slug string, operation lifecycleOperation, exists bool,
	progress *session.LifecycleProgress, progressErr error, now time.Time,
) (lifecycleOperation, bool, error) {
	if progressErr != nil {
		if !exists {
			return lifecycleOperation{}, false, progressErr
		}
		operation.State = "failed"
		operation.Error = boundedLifecycleError("Session lifecycle state is unsafe: " + progressErr.Error())
		operation.UpdatedAt = now.Format(time.RFC3339Nano)
		return operation, true, progressErr
	}
	if progress != nil {
		replace := !exists || operation.Kind != progress.Operation
		if !replace {
			replace = operation.Options.JournalID == "" ||
				progress.JournalID == "" ||
				operation.Options.JournalID != progress.JournalID
		}
		if replace {
			operation = operationFromProgress(slug, *progress)
			s.bindLifecycleJournalTarget(&operation, *progress)
		} else {
			if operation.Options.JournalEvidence != "" && operation.Options.JournalEvidence != progress.Evidence {
				return operation, true, errors.New("lifecycle journal immutable evidence changed")
			}
			s.bindLifecycleJournalTarget(&operation, *progress)
			operation.Phase = progress.Phase
			operation.UpdatedAt = progress.UpdatedAt.UTC().Format(time.RFC3339Nano)
			operation.applyProgressOptions(*progress)
			if operation.State == "complete" {
				operation.State = "paused"
				operation.Error = ""
			}
		}
		return operation, true, nil
	}
	if !exists {
		return lifecycleOperation{}, false, nil
	}

	if operation.State == "paused" || operation.State == "failed" {
		succeeded, successErr := s.lifecycleOperationSucceeded(slug, operation)
		if successErr == nil && succeeded {
			operation.State = "complete"
			operation.Phase = "complete"
			operation.Error = ""
			operation.UpdatedAt = now.Format(time.RFC3339Nano)
		} else if operation.State == "paused" {
			operation.State = "failed"
			operation.Error = "The portal restarted before this operation completed. Retry it."
			operation.UpdatedAt = now.Format(time.RFC3339Nano)
		}
	}
	if operation.State == "complete" {
		updatedAt, err := time.Parse(time.RFC3339Nano, operation.UpdatedAt)
		if err == nil && now.Sub(updatedAt) >= lifecycleOperationSuccessRetention {
			return lifecycleOperation{}, false, nil
		}
	}
	return operation, true, nil
}

// Copy target data only after native/tracking proof, outside both display and
// receipt mutexes. This response-only view never changes receipt authority.
func (s *Server) lifecycleOperationView(slug string, operation lifecycleOperation) lifecycleOperation {
	operation.CurrentTarget = s.lifecycleSnapshot(slug)
	if operation.State == "failed" && !operation.Options.JournalExpected {
		if summary, err := session.Find(s.config.Workspace, slug); err == nil {
			var changed *lifecycleTargetChangedError
			if _, err := validateLifecycleReceiptTarget(summary, operation); errors.As(err, &changed) {
				operation.ErrorCode = "target_changed"
			}
		}
	}
	return operation
}

func (s *Server) acceptLifecycleReconciliation(slug string, original lifecycleOperation, existed bool,
	revision uint64, proposed lifecycleOperation, exists bool, view lifecycleOperation,
) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	current, present := s.operations[slug]
	if present != existed || (present && current != original) || (!present && s.operationRevisions[slug] != revision) {
		return nil // Retry, dismissal or a newer executor result owns the receipt.
	}
	if exists {
		if !existed || current != proposed {
			if err := s.replaceLifecycleOperationLocked(slug, proposed); err != nil {
				return err
			}
		}
		s.publishLifecycleSnapshot(slug, view, time.Now().UTC())
	} else if existed {
		return s.removeLifecycleOperationLocked(slug)
	}
	return nil
}

func (s *Server) lifecycleOperationSucceeded(slug string, operation lifecycleOperation) (bool, error) {
	summary, err := session.Find(s.config.Workspace, slug)
	if errors.Is(err, fs.ErrNotExist) {
		if operation.Kind != "delete" {
			return false, nil
		}
		startedAt, parseErr := time.Parse(time.RFC3339Nano, operation.StartedAt)
		if parseErr != nil {
			return false, errors.New("deletion operation has an invalid start time")
		}
		return session.CompletedRemoval(
			s.config.Workspace, slug, s.config.UserStateRoot,
			operation.Options.JournalID, startedAt,
		)
	}
	if err != nil {
		return false, err
	}
	if operation.Kind != "delete" {
		target, location := operation.Options.TargetID, operation.Options.TargetLocation
		if operation.TargetIdentityVersion != lifecycleTargetIdentityVersion {
			target, location = operation.Options.JournalTargetID, operation.Options.JournalTargetLocation
			if operation.Options.JournalEvidence == "" {
				return false, nil
			}
		}
		if target == "" || location == "" {
			return false, nil
		}
		actual, identityErr := lifecycleTargetIdentityAt(summary, location)
		if identityErr != nil {
			return false, identityErr
		}
		if actual != target {
			return false, nil
		}
	}
	switch operation.Kind {
	case "archive":
		return summary.Archived && summary.Terminal && summary.FinalizedAt != "" &&
			summary.Lifecycle == operation.Options.Mode, nil
	case "revive":
		return !summary.Archived && summary.Lifecycle == "active" && summary.FinalizedAt == "", nil
	case "delete":
		return false, nil
	default:
		return false, errors.New("invalid lifecycle operation")
	}
}

// Display callers copy published immutable views; no receipt or journal I/O.
func (s *Server) lifecycleOperations() ([]lifecycleOperation, error) {
	s.operationSnapshotMu.RLock()
	defer s.operationSnapshotMu.RUnlock()
	operations := make([]lifecycleOperation, 0, len(s.operationSnapshots))
	for _, snapshot := range s.operationSnapshots {
		operations = append(operations, snapshot.operation)
	}
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].UpdatedAt != operations[j].UpdatedAt {
			return operations[i].UpdatedAt > operations[j].UpdatedAt
		}
		return operations[i].Slug < operations[j].Slug
	})
	return operations, nil
}

func (s *Server) replaceLifecycleOperationLocked(slug string, operation lifecycleOperation) error {
	previous, existed := s.operations[slug]
	s.operations[slug] = operation
	if err := s.operationStore.save(s.operations); err != nil {
		if existed {
			s.operations[slug] = previous
		} else {
			delete(s.operations, slug)
		}
		return err
	}
	s.operationRevisions[slug]++
	s.publishLifecycleSnapshot(slug, operation, time.Time{})
	return nil
}

func (s *Server) removeLifecycleOperationLocked(slug string) error {
	previous, existed := s.operations[slug]
	if !existed {
		return nil
	}
	delete(s.operations, slug)
	if err := s.operationStore.save(s.operations); err != nil {
		s.operations[slug] = previous
		return err
	}
	s.operationRevisions[slug]++
	s.operationSnapshotMu.Lock()
	delete(s.operationSnapshots, slug)
	s.operationSnapshotMu.Unlock()
	return nil
}

func boundedLifecycleError(message string) string {
	message = strings.TrimSpace(strings.ToValidUTF8(message, "�"))
	if len(message) <= maxLifecycleOperationErrorBytes {
		return message
	}
	message = message[:maxLifecycleOperationErrorBytes-len("…")]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return strings.TrimSpace(message) + "…"
}
