package web

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

const lifecycleTargetIdentityVersion = 2

// Lifecycle confirmation follows the tracking directory and retained root,
// not administrative writes to that directory or its manifest.
func lifecycleTargetIdentity(summary *session.Summary) (string, error) {
	if summary == nil {
		return "", errors.New("session tracking is missing")
	}
	return lifecycleTargetIdentityAt(summary, summary.Root)
}

func lifecycleTargetIdentityAt(summary *session.Summary, location string) (string, error) {
	if summary == nil || (summary.Root != "work" && summary.Root != "archive") ||
		(location != "work" && location != "archive") || !session.ValidSlug(summary.Slug) {
		return "", errors.New("session tracking is missing")
	}
	workspace, err := filepath.EvalSymlinks(summary.Workspace)
	if err != nil {
		return "", fmt.Errorf("resolve session workspace: %w", err)
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	var stat unix.Stat_t
	if err := unix.Lstat(filepath.Join(workspace, summary.Root, summary.Slug), &stat); err != nil {
		return "", fmt.Errorf("inspect session tracking: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", errors.New("session tracking is not a directory")
	}
	root := "absent"
	if summary.Codex.ThreadID != "" {
		root = "present\x00" + summary.Codex.ThreadID
	}
	identity := fmt.Sprintf("2\x00%s\x00%s\x00%s\x00%d\x00%d\x00%s",
		workspace, summary.Slug, location, stat.Dev, stat.Ino, root)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity))), nil
}

// Creation retains this source-proof contract. Predecessor lifecycle receipt
// conversion reuses it only for an exact positive match; a mismatch cannot be
// attributed to harmless ctime changes. The temporary conversion branch below
// is owned by portal lifecycle maintainers; its removal inventory is in
// docs/workspace-portal.md#browser-lifecycle-confirmation-and-recovery.
func legacyLifecycleTargetIdentity(summary *session.Summary) (string, error) {
	tracking := filepath.Join(summary.Workspace, summary.Root, summary.Slug)
	var stat unix.Stat_t
	if err := unix.Lstat(tracking, &stat); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", errors.New("session tracking is not a directory")
	}
	identity := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d\x00%s",
		filepath.Clean(tracking), stat.Dev, stat.Ino, stat.Ctim.Sec, stat.Ctim.Nsec, summary.Codex.ThreadID)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity))), nil
}

type lifecycleTargetSnapshot struct {
	TargetID              string `json:"targetId"`
	TargetIdentityVersion int    `json:"targetIdentityVersion"`
	Slug                  string `json:"slug"`
	Location              string `json:"location"`
	Lifecycle             string `json:"lifecycle"`
	ThreadID              string `json:"threadId"`
	Archived              bool   `json:"archived"`
}

func currentLifecycleTarget(summary *session.Summary) (*lifecycleTargetSnapshot, error) {
	identity, err := lifecycleTargetIdentity(summary)
	if err != nil {
		return nil, err
	}
	return &lifecycleTargetSnapshot{TargetID: identity, TargetIdentityVersion: lifecycleTargetIdentityVersion,
		Slug: summary.Slug, Location: summary.Root, Lifecycle: summary.Lifecycle,
		ThreadID: summary.Codex.ThreadID, Archived: summary.Archived}, nil
}

type lifecycleTargetChangedError struct {
	Current   *lifecycleTargetSnapshot
	ReceiptID string
}

func (err *lifecycleTargetChangedError) Error() string {
	return "This request does not match the current session. Review the current session and confirm it again."
}

func validateLifecycleTarget(summary *session.Summary, expectedTargetID, action string) (string, error) {
	current, err := currentLifecycleTarget(summary)
	if err != nil {
		return "", fmt.Errorf("verify %s target: %w", action, err)
	}
	if expectedTargetID == "" || expectedTargetID != current.TargetID {
		return "", &lifecycleTargetChangedError{Current: current}
	}
	return current.TargetID, nil
}

func (s *Server) writeLifecycleError(w http.ResponseWriter, err error) {
	var changed *lifecycleTargetChangedError
	if errors.As(err, &changed) {
		if changed.Current != nil && changed.ReceiptID != "" {
			s.operationSnapshotMu.Lock()
			snapshot, exists := s.operationSnapshots[changed.Current.Slug]
			if exists && snapshot.operation.ReceiptID == changed.ReceiptID && !snapshot.operation.Options.JournalExpected {
				snapshot.operation.CurrentTarget = changed.Current
				snapshot.operation.ErrorCode = "target_changed"
				snapshot.checkedAt = time.Now().UTC()
				s.operationSnapshots[changed.Current.Slug] = snapshot
			}
			s.operationSnapshotMu.Unlock()
		}
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "target_changed", "currentTarget": changed.Current, "receiptId": changed.ReceiptID})
		return
	}
	s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
}

func (s *Server) lifecycleSnapshot(slug string) *lifecycleTargetSnapshot {
	summary, err := session.Find(s.config.Workspace, slug)
	if err != nil {
		return nil
	}
	current, _ := currentLifecycleTarget(summary)
	return current
}

// Called under the transition gate on a captured receipt. The caller compares
// that complete receipt under operationMu before saving. Journal ownership
// always precedes page-target checks; a moved directory never retargets a retry.
func (s *Server) prepareLifecycleRetry(operation *lifecycleOperation, progress *session.LifecycleProgress) error {
	if progress != nil {
		if progress.Operation != operation.Kind || progress.JournalID != operation.Options.JournalID ||
			(operation.Options.JournalEvidence != "" && progress.Evidence != operation.Options.JournalEvidence) {
			return errors.New("The lifecycle operation changed. Review its current status before retrying.")
		}
		operation.applyProgressOptions(*progress)
		return nil
	}
	if operation.Options.JournalExpected {
		return errors.New("The lifecycle recovery journal is no longer available.")
	}
	summary, err := session.Find(s.config.Workspace, operation.Slug)
	if err != nil {
		return err
	}
	current, err := validateLifecycleReceiptTarget(summary, *operation)
	if err != nil {
		return err
	}
	if operation.TargetIdentityVersion == 0 {
		operation.Options.TargetID = current.TargetID
		operation.Options.TargetLocation = current.Location
		operation.TargetIdentityVersion = lifecycleTargetIdentityVersion
	}
	if operation.Kind == "delete" && operation.Options.DeletedThreadID != summary.Codex.ThreadID {
		return errors.New("The accepted deletion root changed.")
	}
	operation.Options.TargetLocation = summary.Root
	return nil
}

func (s *Server) bindLifecycleJournalTarget(operation *lifecycleOperation, progress session.LifecycleProgress) {
	if operation.Options.JournalTargetID != "" {
		return
	}
	if operation.Kind != "delete" && !progress.RootRecorded {
		return
	}
	summary, err := session.Find(s.config.Workspace, operation.Slug)
	if err != nil {
		return
	}
	// Root absence is a real identity. Legacy revive can create a new root only
	// inside its owning command; ambiguous post-restart completion stays refused.
	if progress.RootRecorded {
		root := ""
		if progress.RetainedThreadID != nil {
			root = *progress.RetainedThreadID
		}
		if summary.Codex.ThreadID != root {
			return
		}
	}
	identity, err := lifecycleTargetIdentity(summary)
	if err == nil {
		operation.Options.JournalTargetID = identity
		operation.Options.JournalTargetLocation = summary.Root
	}
}

// Read-only inspection also serves status snapshots. Legacy conversion brackets
// the v2 sample with positive predecessor proof so it cannot adopt a directory
// replaced between the two identity reads.
func validateLifecycleReceiptTarget(summary *session.Summary, operation lifecycleOperation) (*lifecycleTargetSnapshot, error) {
	if operation.TargetIdentityVersion == 0 {
		legacy, err := legacyLifecycleTargetIdentity(summary)
		if err != nil || legacy != operation.Options.TargetID {
			current, _ := currentLifecycleTarget(summary)
			return nil, &lifecycleTargetChangedError{Current: current}
		}
	}
	current, err := currentLifecycleTarget(summary)
	if err != nil {
		return nil, err
	}
	if operation.TargetIdentityVersion == 0 {
		legacy, err := legacyLifecycleTargetIdentity(summary)
		if err != nil || legacy != operation.Options.TargetID {
			return nil, &lifecycleTargetChangedError{Current: current}
		}
	} else if current.TargetID != operation.Options.TargetID {
		return nil, &lifecycleTargetChangedError{Current: current}
	}
	return current, nil
}
