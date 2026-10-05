package web

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

// RequireObservationReceipts is a read-only use of the receipt and journal
// owners. An expected ID is only for the archive executing under the normal
// CLI locks: that caller owns full journal/cleanup validation and tracking
// projection proof. This function never adopts a receipt or converts a target.
func RequireObservationReceipts(summary *session.Summary, stateRoot, expectedArchiveID, expectedMode string) error {
	if summary == nil || !validObservationArchiveContext(expectedArchiveID, expectedMode) {
		return errors.New("invalid observation operation identity")
	}
	store, err := newLifecycleOperationStore(summary.Workspace, stateRoot)
	if err != nil {
		return err
	}
	operations, err := store.loadReadOnly()
	if err != nil {
		return err
	}
	progress, err := session.PendingLifecycleProgress(summary.Workspace, summary.Slug)
	if err != nil {
		return err
	}
	if summary.Archived && progress == nil {
		return errors.New("moved archive observation requires its accepted journal")
	}
	if progress != nil {
		if expectedArchiveID == "" || progress.Operation != "archive" || progress.JournalID != expectedArchiveID || progress.Mode != expectedMode ||
			!progress.RootRecorded || progress.RetainedThreadID != nil {
			return errors.New("another lifecycle operation reserves the threadless session")
		}
	}
	operation, exists := operations[summary.Slug]
	if exists && operation.State != "complete" {
		if expectedArchiveID == "" || operation.Kind != "archive" || operation.Options.JournalID != expectedArchiveID || operation.Options.Mode != expectedMode {
			return errors.New("another browser operation reserves the threadless session")
		}
		if progress == nil {
			if operation.Options.JournalExpected {
				return errors.New("the expected archive journal is missing")
			}
			if _, err := validateLifecycleReceiptTarget(summary, operation); err != nil {
				return err
			}
		} else {
			if operation.Options.Mode != progress.Mode || operation.Options.JournalEvidence != "" && operation.Options.JournalEvidence != progress.Evidence {
				return errors.New("the accepted archive journal changed")
			}
			// A v2 confirmation remains positively bound after the move by
			// sampling the same directory using its accepted original location.
			target, location := operation.Options.TargetID, operation.Options.TargetLocation
			if operation.Options.JournalTargetID != "" {
				target, location = operation.Options.JournalTargetID, operation.Options.JournalTargetLocation
			} else if operation.TargetIdentityVersion != lifecycleTargetIdentityVersion {
				return errors.New("the accepted archive target is unverifiable")
			}
			current, err := lifecycleTargetIdentityAt(summary, location)
			if err != nil || current != target {
				return errors.New("the accepted archive target changed")
			}
		}
	} else if expectedArchiveID != "" && progress == nil {
		// Only Runner's fully reconstructed/revalidated prepared journal can
		// supply this context. Presence here is not a sidecar parser or proof;
		// the archive caller owns strict identity, mode, projection and refs.
		if _, err := os.Lstat(filepath.Join(summary.Workspace, "worktrees", ".locks", summary.Slug+".archive-cleanup.json")); err != nil || summary.Archived {
			return errors.New("the expected archive operation is unavailable")
		}
	}
	if expectedArchiveID == "" {
		if _, err := os.Lstat(filepath.Join(summary.Workspace, "worktrees", ".locks", summary.Slug+".archive-cleanup.json")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("archive cleanup intent reserves the threadless session")
		}
	}
	for _, kind := range []string{"creation", "fork", "start"} {
		if _, err := os.Lstat(filepath.Join(summary.Workspace, "worktrees", ".locks", summary.Slug+"."+kind+".json")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("creation journal contradicts threadless observation")
		}
	}
	// Creation receipts are independent evidence; no archive ID waives them.
	entries, err := os.ReadDir(filepath.Join(store.directory, "creations"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == summary.Slug+".json" || strings.HasPrefix(entry.Name(), summary.Slug+".") {
			return errors.New("creation evidence contradicts threadless observation")
		}
	}
	return nil
}

// ValidateObservationArchiveContext validates the paired internal coordinates.
// Neither coordinate independently authorizes excluding a pending operation.
func ValidateObservationArchiveContext(id, mode string) error {
	if !validObservationArchiveContext(id, mode) {
		return errors.New("observation requires an exact archive operation ID and mode together")
	}
	return nil
}

func validObservationArchiveContext(id, mode string) bool {
	return id == "" && mode == "" || messageDigestPattern.MatchString(id) && (mode == "complete" || mode == "abandoned")
}
