package web

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

const (
	lifecycleOperationSuccessRetention = 15 * time.Minute
	maxLifecycleOperationErrorBytes    = 4096
)

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
	targetID := operation.Options.TargetID
	if targetID != "" && !messageDigestPattern.MatchString(targetID) {
		return errors.New("invalid lifecycle target identity")
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
	switch progress.Operation {
	case "archive":
		operation.Options.Mode = progress.Mode
	case "delete":
		operation.Options.Force = progress.Force
	}
}

func (s *Server) lifecycleOperationForSlug(slug string) (lifecycleOperation, bool, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	return s.reconcileLifecycleOperationLocked(slug, time.Now().UTC())
}

func (s *Server) reconcileLifecycleOperationLocked(
	slug string, now time.Time,
) (lifecycleOperation, bool, error) {
	operation, exists := s.operations[slug]
	progress, progressErr := session.PendingLifecycleProgress(s.config.Workspace, slug)
	if progressErr != nil {
		if !exists {
			return lifecycleOperation{}, false, progressErr
		}
		operation.State = "failed"
		operation.Error = boundedLifecycleError("Session lifecycle state is unsafe: " + progressErr.Error())
		operation.UpdatedAt = now.Format(time.RFC3339Nano)
		if err := s.replaceLifecycleOperationLocked(slug, operation); err != nil {
			return operation, true, errors.Join(progressErr, err)
		}
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
		} else {
			operation.Phase = progress.Phase
			operation.UpdatedAt = progress.UpdatedAt.UTC().Format(time.RFC3339Nano)
			operation.applyProgressOptions(*progress)
			if operation.State == "complete" {
				operation.State = "paused"
				operation.Error = ""
			}
		}
		if !exists || operation != s.operations[slug] {
			if err := s.replaceLifecycleOperationLocked(slug, operation); err != nil {
				return operation, true, err
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
		if operation != s.operations[slug] {
			if err := s.replaceLifecycleOperationLocked(slug, operation); err != nil {
				return operation, true, err
			}
		}
	}
	if operation.State == "complete" {
		updatedAt, err := time.Parse(time.RFC3339Nano, operation.UpdatedAt)
		if err == nil && now.Sub(updatedAt) >= lifecycleOperationSuccessRetention {
			if err := s.removeLifecycleOperationLocked(slug); err != nil {
				return operation, true, err
			}
			return lifecycleOperation{}, false, nil
		}
	}
	return operation, true, nil
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

func (s *Server) lifecycleOperations() ([]lifecycleOperation, error) {
	pending, pendingErr := session.PendingLifecycles(s.config.Workspace)
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	for slug, progress := range pending {
		if _, exists := s.operations[slug]; !exists {
			operation := operationFromProgress(slug, progress)
			if err := s.replaceLifecycleOperationLocked(slug, operation); err != nil {
				return nil, errors.Join(pendingErr, err)
			}
		}
	}
	now := time.Now().UTC()
	slugs := make([]string, 0, len(s.operations))
	for slug := range s.operations {
		slugs = append(slugs, slug)
	}
	var problems []error
	operations := make([]lifecycleOperation, 0, len(slugs))
	for _, slug := range slugs {
		operation, exists, err := s.reconcileLifecycleOperationLocked(slug, now)
		if err != nil {
			problems = append(problems, err)
		}
		if exists {
			operations = append(operations, operation)
		}
	}
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].UpdatedAt != operations[j].UpdatedAt {
			return operations[i].UpdatedAt > operations[j].UpdatedAt
		}
		return operations[i].Slug < operations[j].Slug
	})
	return operations, errors.Join(append(problems, pendingErr)...)
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
