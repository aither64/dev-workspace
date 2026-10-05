package web

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// ObservationRetainedContext carries only coordinates from the validated Ruby
// start/revive owners. It never selects a root or authorizes another receipt.
type ObservationRetainedContext struct {
	StartTmuxIdentity string
	ReviveOperationID string
}

func ValidateObservationContext(archiveID, archiveMode string, retained ObservationRetainedContext) error {
	if !validObservationArchiveContext(archiveID, archiveMode) ||
		retained.StartTmuxIdentity != "" && !messageDigestPattern.MatchString(retained.StartTmuxIdentity) ||
		retained.ReviveOperationID != "" && !messageDigestPattern.MatchString(retained.ReviveOperationID) ||
		(archiveID != "" || archiveMode != "") && retained != (ObservationRetainedContext{}) {
		return errors.New("observation operation coordinates are invalid or conflicting")
	}
	return nil
}

// RequireObservationReceipts is a read-only use of the receipt and journal
// owners. Expected coordinates come only from the executing archive or retained
// start/revive caller under normal CLI locks. Those callers own full journal,
// cleanup, tmux and tracking projection proof. No receipt or target is adopted.
func RequireObservationReceipts(summary *session.Summary, stateRoot, expectedArchiveID, expectedMode string, contexts ...ObservationRetainedContext) error {
	retained := ObservationRetainedContext{}
	if len(contexts) > 1 {
		return errors.New("multiple observation operation contexts")
	}
	if len(contexts) == 1 {
		retained = contexts[0]
	}
	if summary == nil || ValidateObservationContext(expectedArchiveID, expectedMode, retained) != nil ||
		retained != (ObservationRetainedContext{}) && (summary.Archived || summary.Codex.ThreadID == "" || summary.HasCreation()) {
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
	if retained.ReviveOperationID != "" {
		if progress == nil || progress.Operation != "revive" || progress.JournalID != retained.ReviveOperationID ||
			progress.Phase != "runtime_starting" || !progress.RootRecorded || !observationRootMatches(summary.Codex.ThreadID, progress.RetainedThreadID) {
			return errors.New("the expected same-root revive journal is unavailable or changed")
		}
	} else if progress != nil {
		if expectedArchiveID == "" || progress.Operation != "archive" || progress.JournalID != expectedArchiveID || progress.Mode != expectedMode ||
			!progress.RootRecorded || !observationRootMatches(summary.Codex.ThreadID, progress.RetainedThreadID) {
			return errors.New("another lifecycle operation reserves the session")
		}
	}
	operation, exists := operations[summary.Slug]
	if exists && operation.State != "complete" {
		expectedKind, expectedOperation := "archive", expectedArchiveID
		if retained.ReviveOperationID != "" {
			expectedKind, expectedOperation = "revive", retained.ReviveOperationID
		}
		if expectedOperation == "" || operation.Kind != expectedKind || operation.Options.JournalID != expectedOperation ||
			expectedKind == "archive" && operation.Options.Mode != expectedMode {
			return errors.New("another browser operation reserves the session")
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
			return errors.New("archive cleanup intent reserves the session")
		}
	}
	return requireOrdinaryCreationReceipts(summary, store, retained.StartTmuxIdentity)
}

func observationRootMatches(root string, recorded *string) bool {
	return root == "" && recorded == nil || recorded != nil && *recorded == root
}

// Receipt absence is an actual filesystem proof. A creation-less manifest
// cannot ignore completed or rotated receipts left by another creation owner.
func requireOrdinaryCreationReceipts(summary *session.Summary, store *lifecycleOperationStore, expectedStart string) error {
	workspace, slug := summary.Workspace, summary.Slug
	for _, kind := range []string{"fork", "start"} {
		path := filepath.Join(workspace, "worktrees", ".locks", slug+"."+kind+".json")
		if kind == "start" && expectedStart != "" {
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Size() > 64*1024 {
				return errors.New("the expected start journal is unavailable or unsafe")
			}
			var journal struct {
				Schema       int    `json:"schema"`
				Slug         string `json:"slug"`
				State        string `json:"state"`
				TmuxIdentity string `json:"tmux_identity"`
			}
			if err := readCreationJSON(path, &journal); err != nil || journal.Schema != 1 || journal.Slug != slug ||
				journal.State != "creating" || journal.TmuxIdentity != expectedStart {
				return errors.New("the expected start journal changed")
			}
			continue
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.New("unfinished initialization conflicts with ordinary observation")
		}
	}
	readyCreation := summary.HasCreation() && summary.Creation.State == "ready" &&
		(summary.Creation.GoalSHA256 == "" || summary.Creation.InitialGoalSent)
	journal, present, err := agentteams.ReadCreationJournal(workspace, slug)
	if err != nil || present && (!readyCreation || journal.State != "ready") {
		return errors.New("unresolved creation journal conflicts with ordinary observation")
	}
	path := filepath.Join(store.directory, "creations", slug+".json")
	if !readyCreation {
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, entry := range entries {
			if entry.Name() == slug+".json" || strings.HasPrefix(entry.Name(), slug+".") {
				return errors.New("creation evidence conflicts with creation-less tracking")
			}
		}
	}
	var raw map[string]json.RawMessage
	err = readCreationJSON(path, &raw)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !readyCreation {
		return errors.New("creation receipt contradicts ordinary tracking")
	}
	if err := validateCreationReceiptShape(raw); err != nil {
		return err
	}
	var receipt creationReceipt
	if err := readCreationJSON(path, &receipt); err != nil || receipt.Workspace != workspace || receipt.Request.Slug != slug || receipt.State != "ready" {
		return errors.New("retained creation receipt is unresolved")
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
