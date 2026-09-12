package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

// Creation requests are intentionally separate from strict lifecycle journals.
// Old packages ignore these private files; the existing CLI journals retain
// their original format and continue to own runtime initialization.
type creationRequest struct {
	Kind           string `json:"kind"`
	Slug           string `json:"slug"`
	Goal           string `json:"goal,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	Source         string `json:"source,omitempty"`
	SourceThreadID string `json:"sourceThreadId,omitempty"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
	PlanTurnID     string `json:"planTurnId,omitempty"`
	PlanSHA256     string `json:"planSha256,omitempty"`
	PlanText       string `json:"planText,omitempty"`
}

type creationReceipt struct {
	Schema                int             `json:"schema"`
	Workspace             string          `json:"workspace"`
	Request               creationRequest `json:"request"`
	ReceiptID             string          `json:"receiptId"`
	DeletionHistorySHA256 string          `json:"deletionHistorySha256"`
	Attempt               int             `json:"attempt"`
	State                 string          `json:"state"`
	Phase                 string          `json:"phase"`
	Error                 string          `json:"error,omitempty"`
	StartedAt             string          `json:"startedAt"`
	UpdatedAt             string          `json:"updatedAt"`
	// Resolved settings and goal are immutable once validation has succeeded.
	Validated bool   `json:"validated"`
	Goal      string `json:"goal,omitempty"`
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"`
}

type creationStatus struct {
	StartedAt    string `json:"startedAt"`
	UpdatedAt    string `json:"updatedAt"`
	SourceURL    string `json:"sourceUrl,omitempty"`
	CanonicalURL string `json:"canonicalUrl,omitempty"`

	Slug      string `json:"slug"`
	URL       string `json:"url"`
	ReceiptID string `json:"receiptId"`
	Attempt   int    `json:"attempt"`
	State     string `json:"state"`
	Phase     string `json:"phase"`
	Error     string `json:"error,omitempty"`
}

func (receipt creationReceipt) status() creationStatus {
	sourceURL := ""
	if receipt.Request.Source != "" {
		sourceURL = "/" + receipt.Request.Source + "/"
	}
	canonicalURL := ""
	if receipt.State == "conflict" {
		canonicalURL = "/" + receipt.Request.Slug + "/"
	}
	return creationStatus{CanonicalURL: canonicalURL, Slug: receipt.Request.Slug, URL: "/" + receipt.Request.Slug + "/", ReceiptID: receipt.ReceiptID,
		Attempt: receipt.Attempt, State: receipt.State, Phase: receipt.Phase, Error: receipt.Error,
		StartedAt: receipt.StartedAt, UpdatedAt: receipt.UpdatedAt, SourceURL: sourceURL}
}

func (s *Server) creationDirectory() string {
	return filepath.Join(s.operationStore.directory, "creations")
}
func (s *Server) creationPath(slug string) string {
	return filepath.Join(s.creationDirectory(), slug+".json")
}
func (s *Server) creationEvidencePath(receipt creationReceipt) string {
	return filepath.Join(s.creationDirectory(), receipt.Request.Slug+"."+receipt.ReceiptID+".complete.json")
}

// The accepted and resolved message snapshots may each use six JSON bytes
// per input byte (control characters or HTML escaping).
const maxCreationReceiptBytes = 12*session.MaxMessageBytes + 16384
const maxCreationReceipts = 512

// Terminal receipts are a bounded retry cache. Conflict records describe a
// separate canonical session; they never prove the request was completed.
func (s *Server) retireReadyCreations(reserve int) error {
	if len(s.creations)+reserve <= maxCreationReceipts {
		return nil
	}
	// A CLI deletion may finish without another request for that slug.
	// Reclaim those receipts before treating the remaining ones as unfinished.
	for slug, receipt := range s.creations {
		if receipt.State == "paused" || receipt.State == "failed" {
			if err := s.retireAbsentCreation(slug); err != nil {
				return err
			}
		}
	}
	var terminal []creationReceipt
	for _, receipt := range s.creations {
		if receipt.State != "ready" && receipt.State != "conflict" {
			continue
		}
		terminal = append(terminal, receipt)
	}
	sort.Slice(terminal, func(i, j int) bool {
		if terminal[i].UpdatedAt == terminal[j].UpdatedAt {
			return terminal[i].Request.Slug < terminal[j].Request.Slug
		}
		return terminal[i].UpdatedAt < terminal[j].UpdatedAt
	})
	for _, receipt := range terminal {
		if len(s.creations)+reserve <= maxCreationReceipts {
			break
		}
		if err := s.retireIdleCreation(receipt); err != nil {
			return err
		}
	}
	if len(s.creations)+reserve > maxCreationReceipts {
		return errors.New("too many unfinished session creations")
	}
	return nil
}

func (s *Server) retireIdleCreation(receipt creationReceipt) error {
	lock, err := session.LockRuntimeShared(s.config.AuthorityDir, receipt.Request.Slug)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return nil
	} else if err != nil {
		return err
	}
	defer lock.Close()
	pending, err := s.creationJournalPresent(receipt.Request.Slug, false)
	if err != nil || pending {
		return err
	}
	return s.retireCreation(receipt)
}

// Call with operationMu held. Removing the receipt last keeps an interrupted
// retirement recoverable; a ready receipt no longer needs CLI binding evidence.
func (s *Server) retireCreation(receipt creationReceipt) error {
	for _, path := range []string{s.creationEvidencePath(receipt) + ".request", s.creationEvidencePath(receipt), s.creationPath(receipt.Request.Slug)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("retire creation receipt: %w", err)
		}
	}
	directory, err := os.Open(s.creationDirectory())
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return err
	}
	delete(s.creations, receipt.Request.Slug)
	return nil
}

// Completed deletion releases a slug even when the retry cache is not full.
// Existing tracking, authority and journals preserve the old identity until
// their owning lifecycle operation has finished.
func (s *Server) retireAbsentCreation(slug string) error {
	receipt, ok := s.creations[slug]
	if !ok || receipt.State == "running" {
		return nil
	}
	lock, err := session.LockRuntimeShared(s.config.AuthorityDir, slug)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return nil
	} else if err != nil {
		return err
	}
	defer lock.Close()
	paths := []string{filepath.Join(s.config.AuthorityDir, slug+".json")}
	for _, root := range []string{"work", "archive", "worktrees"} {
		paths = append(paths, filepath.Join(s.config.Workspace, root, slug))
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	pending, err := s.creationJournalPresent(slug, true)
	if err != nil || pending {
		return err
	}
	if receipt.State != "ready" && receipt.State != "conflict" {
		history, err := session.CompletedRemovalHistory(s.config.Workspace, slug, s.config.UserStateRoot)
		if err != nil || history == receipt.DeletionHistorySHA256 {
			return err
		}
	}

	return s.retireCreation(receipt)
}

func (s *Server) creationJournalPresent(slug string, includeCompleted bool) (bool, error) {
	owner, err := session.PendingLifecycle(s.config.Workspace, slug)
	if err != nil || owner != "" {
		return owner != "", err
	}
	for _, suffix := range []string{".start.json", ".fork.json"} {
		_, err := os.Lstat(filepath.Join(s.config.Workspace, "worktrees", ".locks", slug+suffix))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	path := filepath.Join(s.config.Workspace, "worktrees", ".locks", slug+".creation.json")
	var journal map[string]any
	if err := readCreationJSON(path, &journal); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return true, err
	}
	return includeCompleted || journal["schema"] != float64(1) || journal["slug"] != slug || journal["state"] != "ready", nil
}

func readCreationJSON(path string, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxCreationReceiptBytes {
		return errors.New("creation state must be a private bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxCreationReceiptBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("creation state has trailing data")
	}
	return nil
}

func (s *Server) loadCreations() error {
	s.creations = make(map[string]creationReceipt)
	entries, err := os.ReadDir(s.creationDirectory())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		slug := strings.TrimSuffix(entry.Name(), ".json")
		if !strings.HasSuffix(entry.Name(), ".json") || !session.ValidSlug(slug) {
			continue
		}
		var receipt creationReceipt
		if err := readCreationJSON(s.creationPath(slug), &receipt); err != nil {
			return fmt.Errorf("load creation %s: %w", slug, err)
		}
		if receipt.Schema != 1 || receipt.Workspace != s.config.Workspace || receipt.Request.Slug != slug ||
			!messageDigestPattern.MatchString(receipt.ReceiptID) || !messageDigestPattern.MatchString(receipt.DeletionHistorySHA256) || receipt.Attempt < 1 ||
			(receipt.Request.Kind != "new" && receipt.Request.Kind != "fork" && receipt.Request.Kind != "plan") ||
			(receipt.State != "running" && receipt.State != "paused" && receipt.State != "failed" && receipt.State != "ready" && receipt.State != "conflict") {
			return fmt.Errorf("invalid session creation receipt for %s", slug)
		}
		if receipt.State == "running" {
			receipt.State = "paused"
			receipt.Phase = "Initialization was interrupted. Retry to continue."
		}
		s.creations[slug] = receipt
		if receipt.State == "paused" || receipt.State == "failed" {
			if err := s.retireAbsentCreation(slug); err != nil {
				return err
			}
		}
		if err := s.retireReadyCreations(0); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) saveCreation(receipt creationReceipt) error {
	if err := os.MkdirAll(s.creationDirectory(), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if len(data) > maxCreationReceiptBytes {
		return errors.New("session creation receipt is too large")
	}
	file, err := os.CreateTemp(s.creationDirectory(), ".creation-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(file.Name(), s.creationPath(receipt.Request.Slug)); err != nil {
		return err
	}
	directory, err := os.Open(s.creationDirectory())
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
