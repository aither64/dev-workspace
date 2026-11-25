package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"golang.org/x/sys/unix"
)

const lifecycleTargetIdentityVersion = 2

func sameLifecycleRequest(a, b lifecycleOperation) bool {
	x, y := a.Options, b.Options
	return a.Slug == b.Slug && a.Kind == b.Kind && a.ReceiptID == b.ReceiptID && a.Attempt == b.Attempt && a.StartedAt == b.StartedAt &&
		a.TargetIdentityVersion == b.TargetIdentityVersion && x.Mode == y.Mode && x.AllowAbandoned == y.AllowAbandoned && x.Force == y.Force &&
		x.TargetID == y.TargetID && x.DeletedThreadID == y.DeletedThreadID && x.JournalID == y.JournalID && x.TargetLocation == y.TargetLocation &&
		session.SameCodexTargets(x.CodexTargets, y.CodexTargets)
}

// This private admission owns result bookkeeping only. It consumes an exact
// durable accepted receipt under the caller's transition lock, never a current
// native selection or execution grant.
func (s *Server) admitSavedLifecycleOutcome(expected lifecycleOperation) (lifecycleOperation, error) {
	if expected.Options.CodexTargets == nil {
		return expected, s.requireCurrentHostProfile()
	}
	identity, err := readHostProfileIdentity(s.config.HostProfile)
	selected, selectErr := filepath.EvalSymlinks(s.config.HostProfile)
	if err != nil || selectErr != nil || identity != s.hostProfile || s.config.PackageRoot == "" || selected != s.config.PackageRoot {
		return lifecycleOperation{}, errors.New("saved lifecycle result package generation changed")
	}
	data, err := os.ReadFile(filepath.Join(selected, "share/workspace-portal/runtime-contract.json"))
	var contract map[string]json.RawMessage
	if err != nil || session.DecodePiCreationJSON(data, &contract, 64<<10) != nil || string(contract["codexProviderBindingStateSchema"]) != "3" ||
		string(contract["codexProviderForkTransitionPolicy"]) != "1" || string(contract["codexRuntimeAuthoritySchema"]) != "3" {
		return lifecycleOperation{}, errors.New("saved lifecycle result consumers are unavailable")
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		return lifecycleOperation{}, err
	}
	state, err := codexproviders.ReadBindingState(s.config.UserStateRoot, s.config.WorkspaceName, s.config.Workspace, s.config.CodexSocket, catalog)
	if err != nil {
		return lifecycleOperation{}, err
	}
	receipts, err := s.operationStore.loadReadOnly()
	if err != nil {
		return lifecycleOperation{}, err
	}
	durable, ok := receipts[expected.Slug]
	if !ok || expected.ReceiptID == "" || !sameLifecycleRequest(durable, expected) {
		return lifecycleOperation{}, errors.New("saved lifecycle result receipt changed")
	}
	progress, err := session.PendingLifecycleProgress(s.config.Workspace, expected.Slug)
	if err != nil {
		return lifecycleOperation{}, err
	}
	root := ""
	if expected.Kind == "delete" {
		root = expected.Options.DeletedThreadID
	}
	if summary, findErr := session.Find(s.config.Workspace, expected.Slug); findErr == nil {
		if summary.Codex.ThreadID != "" && summary.Codex.SocketPath != s.config.CodexSocket {
			return lifecycleOperation{}, errors.New("saved lifecycle result original socket changed")
		}
		actual, err := lifecycleTargetIdentityAt(summary, expected.Options.TargetLocation)
		if err == nil && actual == expected.Options.TargetID {
			root = summary.Codex.ThreadID
		}
	} else if !errors.Is(findErr, os.ErrNotExist) {
		return lifecycleOperation{}, findErr
	}
	if progress != nil {
		if progress.Operation != expected.Kind || progress.JournalID == "" || progress.JournalID != expected.Options.JournalID ||
			!lifecycleJournalEvidenceMatches(s.config.Workspace, durable, *progress) ||
			expected.Kind == "archive" && progress.Mode != expected.Options.Mode || expected.Kind == "delete" && progress.Force && !expected.Options.Force {
			return lifecycleOperation{}, errors.New("saved lifecycle result owner changed")
		}
		if progress.RootRecorded {
			if root != "" && !observationRootMatches(root, progress.RetainedThreadID) {
				return lifecycleOperation{}, errors.New("saved lifecycle result retained root changed")
			}
			if progress.RetainedThreadID != nil {
				root = *progress.RetainedThreadID
			}
		}
	}
	if state.BlocksSession(expected.Slug, root) || root == "" && state.Operation.Reserved() &&
		(state.Operation.Strategy != "scoped-v1" || state.Operation.Session == "" || state.Operation.Session == expected.Slug) {
		return lifecycleOperation{}, errors.New("saved lifecycle result provider scope is fenced")
	}
	return durable, nil
}

func (s *Server) withSavedLifecycleOutcome(expected lifecycleOperation, mutate func() error) error {
	if expected.Options.CodexTargets == nil {
		return s.withCurrentGeneration(mutate)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, unlock, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.admitSavedLifecycleOutcome(expected); err != nil {
		return err
	}
	return mutate()
}

// New browser acceptance freezes targets while runtime -> team ownership is
// retained through receipt publication. Accepted retries never call this path.
func (s *Server) captureLifecycleTargets(summary *session.Summary, kind string) (*session.CodexTargets, func(), error) {
	// Ruby alone reconstructs an auxiliary-only archive intent. Its presence
	// cannot become a new browser capture, including when its bytes are invalid.
	requireNoAuxiliaryOwner := func() error {
		path := filepath.Join(s.config.Workspace, "worktrees", ".locks", summary.Slug+".archive-cleanup.json")
		if _, err := os.Lstat(path); err == nil {
			return errors.New("An accepted archive cleanup must be restored by its original command before another lifecycle request.")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return session.RequireNoPendingRemoval(s.config.Workspace, summary.Slug, s.config.UserStateRoot)
	}
	if summary.Codex.BindingSchema != 3 {
		return nil, func() {}, requireNoAuxiliaryOwner()
	}
	if err := s.requireCurrentHostProfileFor(summary.Slug); err != nil {
		return nil, nil, err
	}
	runtime, err := session.LockRuntimeShared(s.config.AuthorityDir, summary.Slug)
	if err != nil {
		return nil, nil, err
	}
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		runtime.Close()
		return nil, nil, err
	}
	release, err := store.TryLockOperation(summary.Slug)
	if err != nil {
		runtime.Close()
		return nil, nil, err
	}
	unlock := func() { release(); runtime.Close() }
	current, err := session.Find(s.config.Workspace, summary.Slug)
	if err != nil || current.Codex != summary.Codex || current.Root != summary.Root {
		unlock()
		return nil, nil, errors.New("lifecycle origin changed before target capture")
	}
	if err := requireNoAuxiliaryOwner(); err != nil {
		unlock()
		return nil, nil, err
	}
	targets, err := store.CaptureLifecycleTargets(summary.Slug, summary.Codex.ThreadID, kind)
	if err == nil {
		err = s.requireCurrentHostProfileFor(summary.Slug)
	}
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return targets, unlock, nil
}

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
			!lifecycleJournalEvidenceMatches(s.config.Workspace, *operation, *progress) {
			return errors.New("The lifecycle operation changed. Review its current status before retrying.")
		}
		operation.applyProgressOptions(*progress)
		operation.Phase = progress.Phase
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
