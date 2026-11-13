package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/web"
	"golang.org/x/sys/unix"
)

// The child only closes its duplicate; LOCK_UN would release the parent's OFD.
func inheritedMonitorArchiveLock(workspace, authorityDir, slug string) (*os.File, error) {
	if os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") != "archive" {
		return nil, errors.New("monitor quiescence requires the admitted archive parent")
	}
	fd, err := strconv.Atoi(os.Getenv("DEV_SESSION_LIFECYCLE_LOCK_FD"))
	if err != nil || fd < 3 {
		return nil, errors.New("archive parent lock descriptor is missing")
	}
	expected := filepath.Join(workspace, "worktrees", ".locks", slug+".lock")
	if authorityDir != "" {
		if !canonicalAbsolutePath(authorityDir) {
			return nil, errors.New("archive authority directory is not canonical")
		}
		expected = filepath.Join(authorityDir, slug+".lock")
	}
	path := os.Getenv("DEV_SESSION_LIFECYCLE_LOCK_PATH")
	if path != expected || !canonicalAbsolutePath(path) {
		return nil, errors.New("archive parent lock path differs")
	}
	duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, err
	}
	inherited := os.NewFile(uintptr(duplicate), "archive-parent-lock")
	fail := func(e error) (*os.File, error) { _ = inherited.Close(); return nil, e }
	var held unix.Stat_t
	if err := unix.Fstat(duplicate, &held); err != nil {
		return fail(err)
	}
	probe, err := unix.Open(path, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	defer unix.Close(probe)
	var current unix.Stat_t
	if err := unix.Fstat(probe, &current); err != nil {
		return fail(err)
	}
	if held.Mode&unix.S_IFMT != unix.S_IFREG || current.Mode&unix.S_IFMT != unix.S_IFREG || held.Mode&0777 != 0600 || current.Mode&0777 != 0600 || held.Uid != uint32(os.Geteuid()) || current.Uid != uint32(os.Geteuid()) || held.Dev != current.Dev || held.Ino != current.Ino {
		return fail(errors.New("archive parent lock ownership/inode differs"))
	}
	err = unix.Flock(probe, unix.LOCK_SH|unix.LOCK_NB)
	if err == nil {
		return fail(errors.New("archive parent lock is not held exclusively"))
	}
	if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
		return fail(err)
	}
	if err := unix.Flock(duplicate, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(errors.New("inherited archive descriptor does not own exclusion"))
	}
	return inherited, nil
}

func monitorArchiveGeneration(stateRoot string) error {
	profile := os.Getenv("DEV_WORKSPACES_PROFILE")
	if profile == "" {
		profile = filepath.Join(stateRoot, "profile")
	}
	selected, err := filepath.EvalSymlinks(profile)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	if filepath.Dir(filepath.Dir(executable)) != selected {
		return errors.New("monitor archive executor is not the current selected package")
	}
	return nil
}

func selectMonitorArchiveAuthority(ctx context.Context, store *monitoroperation.Store, workspace string, a monitoroperation.ArchiveAuthority) (monitoroperation.ArchiveAuthority, error) {
	summary, err := session.Find(workspace, a.Slug)
	if err != nil {
		return a, err
	}
	if summary.Codex.ThreadID != a.RootThreadID || summary.Creation.State == "creating" {
		return a, errors.New("monitor archive retained root changed")
	}
	incarnation, err := session.ObservationIdentity(workspace, a.Slug, filepath.Join(workspace, summary.Root, a.Slug), a.RootThreadID)
	if err != nil {
		return a, err
	}
	progress, err := session.PendingLifecycleProgress(workspace, a.Slug)
	if err != nil {
		return a, err
	}
	if summary.Root != "work" && (progress == nil || progress.Operation != "archive") {
		return a, errors.New("monitor archive retained root changed")
	}
	if progress != nil {
		if progress.Operation != "archive" {
			return a, errors.New("monitor archive conflicts with another lifecycle operation")
		}
		if progress.Mode != "" && progress.Mode != a.Mode {
			return a, errors.New("monitor archive policy differs from its original journal")
		}
		if progress.JournalID != "" {
			if a.OperationID != "" && a.OperationID != progress.JournalID {
				return a, errors.New("monitor archive journal identity differs")
			}
			a.OperationID = progress.JournalID
		}
		if progress.RootRecorded {
			matches := progress.RetainedThreadID == nil && a.RootThreadID == ""
			if progress.RetainedThreadID != nil {
				matches = *progress.RetainedThreadID == a.RootThreadID
			}
			if !matches {
				return a, errors.New("monitor archive journal root differs")
			}
		}
	}
	records, err := store.ListObservers(ctx, a.Slug)
	if err != nil {
		return a, err
	}
	for _, record := range records {
		if record.Workspace != workspace || record.Owner.RootThreadID != a.RootThreadID || record.Owner.Incarnation != incarnation {
			return a, errors.New("monitor archive observer owner changed")
		}
		if record.Archive == nil {
			continue
		}
		prior := *record.Archive
		if prior.Slug != a.Slug || prior.RootThreadID != a.RootThreadID || a.TargetID != "" && prior.TargetID != a.TargetID || prior.Mode != a.Mode || prior.ReceiptID != a.ReceiptID || a.OperationID != "" && prior.OperationID != a.OperationID {
			return a, errors.New("monitor records retain a conflicting accepted archive")
		}
		a.OperationID, a.TargetID = prior.OperationID, prior.TargetID
	}
	if a.OperationID == "" {
		var id [32]byte
		if _, err := rand.Read(id[:]); err != nil {
			return a, err
		}
		a.OperationID = hex.EncodeToString(id[:])
	}
	return a, nil
}

func monitorAcceptedArchive(ctx context.Context, action, workspace, state, authorityDir, slug, root, socket string, a monitoroperation.ArchiveAuthority, client monitoroperation.LifecycleClient) error {
	if err := monitorArchiveGeneration(state); err != nil {
		return err
	}
	var inherited *os.File
	if a.ReceiptID == "" {
		lock, err := inheritedMonitorArchiveLock(workspace, authorityDir, slug)
		if err != nil {
			return err
		}
		inherited = lock
		defer inherited.Close()
	}
	store, err := monitoroperation.NewStore(state, workspace)
	if err != nil {
		return err
	}
	a.Slug, a.RootThreadID = slug, root
	a, err = selectMonitorArchiveAuthority(ctx, store, workspace, a)
	if err != nil {
		return err
	}
	validate := func(_ context.Context, got monitoroperation.ArchiveAuthority) error {
		if got != a {
			return errors.New("accepted monitor archive binding changed")
		}
		if err := monitorArchiveGeneration(state); err != nil {
			return err
		}
		if err := web.ValidateMonitorArchiveAuthority(workspace, state, got); err != nil {
			return err
		}
		if got.ReceiptID == "" && inherited == nil {
			return errors.New("archive parent exclusion is missing")
		}
		return nil
	}
	if err := validate(ctx, a); err != nil {
		return err
	}
	lifecycle := monitoroperation.Lifecycle{Store: store, Client: client, AuthorizeArchive: validate}
	safety := monitoroperation.LocalSafety{State: "quiescent", OperationIDs: []string{}}
	switch action {
	case "begin-archive":
		if err := lifecycle.BeginArchiveQuiescence(ctx, a); err != nil {
			return err
		}
		safety, err = lifecycle.AssessLocalSafety(ctx, slug, root)
	case "complete-archive":
		safety, err = lifecycle.CompleteArchiveQuiescence(ctx, a)
	default:
		return fmt.Errorf("unsupported monitor archive action %s", action)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Authority monitoroperation.ArchiveAuthority `json:"authority"`
		Local     monitoroperation.LocalSafety      `json:"local"`
		Pending   bool                              `json:"pending"`
	}{a, safety, monitorArchivePollPending(ctx, lifecycle, a, safety)})
}

// Pending is only a wait hint. The local result stays blocked until the original
// execution owner publishes a joined receipt; it is never proof of a live owner.
func monitorArchivePollPending(ctx context.Context, lifecycle monitoroperation.Lifecycle, a monitoroperation.ArchiveAuthority, local monitoroperation.LocalSafety) bool {
	if local.State != "blocked" || len(local.OperationIDs) == 0 {
		return false
	}
	assessment, err := lifecycle.AssessLocalSafety(ctx, a.Slug, a.RootThreadID)
	if err != nil || assessment.State != "blocked" || len(assessment.OperationIDs) == 0 {
		return false
	}
	ids := append(append([]string{}, local.OperationIDs...), assessment.OperationIDs...)
	for _, id := range ids {
		record, err := lifecycle.Store.GetObserver(ctx, id)
		if err != nil || record.Driver != "direct-ci" || record.Archive == nil || *record.Archive != a || record.Phase != "quiescing" || record.Poll == nil || record.Poll.State == "joined" {
			return false
		}
		for _, event := range record.Events {
			if event.Delivery.Status == "submitting" || event.Delivery.Receipt != nil && event.Delivery.Receipt.TurnID == "" {
				return false
			}
		}
	}
	return true
}
