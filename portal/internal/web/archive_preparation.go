package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"golang.org/x/sys/unix"
)

type archiveBlocker struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Path    *string `json:"path"`
}

type archiveCheckResult struct {
	Schema    int              `json:"schema"`
	Workspace string           `json:"workspace"`
	Slug      string           `json:"slug"`
	Ready     bool             `json:"ready"`
	Passed    int              `json:"passedChecks"`
	Total     int              `json:"totalChecks"`
	Stage     int              `json:"readyStage"`
	Count     int              `json:"blockerCount"`
	Blockers  []archiveBlocker `json:"blockers"`
}

type archivePreparationStatus struct {
	State      string                    `json:"state"`
	Round      int                       `json:"round"`
	NoProgress int                       `json:"noProgress"`
	Check      *archiveCheckResult       `json:"check,omitempty"`
	History    []archivePreparationRound `json:"history,omitempty"`
}

type archivePreparationRound struct {
	Round    int `json:"round"`
	Passed   int `json:"passedChecks"`
	Blockers int `json:"blockerCount"`
}

// Kept outside lifecycle journals and receipts. Preflight cannot reserve a
// package generation or make an older executor interpret a new journal schema.
type archivePreparation struct {
	Schema           int                       `json:"schema"`
	Workspace        string                    `json:"workspace"`
	Slug             string                    `json:"slug"`
	ReceiptID        string                    `json:"receiptId"`
	Attempt          int                       `json:"attempt"`
	JournalID        string                    `json:"journalId"`
	TargetID         string                    `json:"targetId"`
	Thread           string                    `json:"thread"`
	State            string                    `json:"state"`
	Round            int                       `json:"round"`
	NoProgress       int                       `json:"noProgress"`
	DeliveryFailures int                       `json:"deliveryFailures,omitempty"`
	BestStage        int                       `json:"bestStage"`
	BestCount        int                       `json:"bestCount"`
	Check            *archiveCheckResult       `json:"check,omitempty"`
	MessageID        string                    `json:"messageId,omitempty"`
	Message          string                    `json:"message,omitempty"`
	Options          *codex.TurnOptions        `json:"options,omitempty"`
	Delivery         *codex.SendReceipt        `json:"delivery,omitempty"`
	Error            string                    `json:"error,omitempty"`
	History          []archivePreparationRound `json:"history,omitempty"`
}

func (p archivePreparation) active() bool {
	return p.State == "checking" || p.State == "lead_fixing_blockers" || p.State == "rechecking" || p.State == "paused"
}

func (p archivePreparation) matches(op lifecycleOperation) bool {
	return op.Kind == "archive" && op.Options.Mode == "archived" && !op.Options.JournalExpected &&
		p.ReceiptID == op.ReceiptID && p.Attempt == op.Attempt && p.JournalID == op.Options.JournalID && p.TargetID == op.Options.TargetID
}

type archiveRepairClient interface {
	ReadThreadSettings(context.Context, string) (codex.ThreadSettings, error)
	ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
	SendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, error)
	ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error)
	Request(context.Context, string, any, any) error
}

func (s *Server) archivePreparationPath(slug string) string {
	return filepath.Join(s.operationStore.directory, "archive-preparations", slug+".json")
}

func (s *Server) archivePreparationFor(slug string) (archivePreparation, bool) {
	s.archivePrepMu.Lock()
	defer s.archivePrepMu.Unlock()
	p, ok := s.archivePreparations[slug]
	return p, ok
}

// The caller serializes writes with archivePrepMu. Rename plus directory sync
// gives each delivery intent a durable boundary before any native submission.
func (s *Server) saveArchivePreparationLocked(p archivePreparation) error {
	path := s.archivePreparationPath(p.Slug)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(data) > 512*1024 {
		return errors.New("archive preparation exceeds its storage limit")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".preparation-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = directory.Sync()
	directory.Close()
	if err != nil {
		return err
	}
	if s.archivePreparations == nil {
		s.archivePreparations = make(map[string]archivePreparation)
	}
	s.archivePreparations[p.Slug] = p
	return nil
}

func (s *Server) updateArchivePreparation(expected archivePreparation, change func(*archivePreparation)) (archivePreparation, error) {
	s.archivePrepMu.Lock()
	defer s.archivePrepMu.Unlock()
	current, ok := s.archivePreparations[expected.Slug]
	if !ok || current.ReceiptID != expected.ReceiptID || current.Attempt != expected.Attempt || !current.active() {
		return current, errors.New("archive preparation was canceled or replaced")
	}
	change(&current)
	return current, s.saveArchivePreparationLocked(current)
}

// Called before publishing the ordinary lifecycle receipt while operationMu
// is held. A crash between files can leave only an inert, unmatched sidecar.
func (s *Server) beginArchivePreparation(op lifecycleOperation) error {
	if op.Kind != "archive" || op.Options.Mode != "archived" || op.Options.JournalExpected {
		return nil
	}
	summary, err := session.Find(s.config.Workspace, op.Slug)
	if err != nil {
		return err
	}
	p := archivePreparation{Schema: 1, Workspace: s.config.Workspace, Slug: op.Slug,
		ReceiptID: op.ReceiptID, Attempt: op.Attempt, JournalID: op.Options.JournalID,
		TargetID: op.Options.TargetID, Thread: summary.Codex.ThreadID,
		State: "checking", BestStage: -1, BestCount: int(^uint(0) >> 1)}
	s.archivePrepMu.Lock()
	defer s.archivePrepMu.Unlock()
	if previous, ok := s.archivePreparations[op.Slug]; ok && previous.ReceiptID == p.ReceiptID && previous.TargetID == p.TargetID && previous.Thread == p.Thread {
		// An explicit Retry must reconcile an uncertain delivery from the prior
		// attempt, rather than create a different repair message.
		p.MessageID, p.Message, p.Options, p.Delivery = previous.MessageID, previous.Message, previous.Options, previous.Delivery
		p.Round = previous.Round
	}
	return s.saveArchivePreparationLocked(p)
}

func (s *Server) resumeArchivePreparations() error {
	s.operationMu.Lock()
	candidates := make(map[string]lifecycleOperation)
	for slug, op := range s.operations {
		if op.Kind == "archive" && op.Options.Mode == "archived" && !op.Options.JournalExpected && op.State != "complete" {
			candidates[slug] = op
		}
	}
	s.operationMu.Unlock()
	for slug, original := range candidates {
		path := s.archivePreparationPath(slug)
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		var data []byte
		if err == nil {
			data, err = io.ReadAll(io.LimitReader(f, 512*1024+1))
			f.Close()
		}
		var p archivePreparation
		if err == nil {
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&p)
			if err == nil && (len(data) > 512*1024 || rejectDuplicateCreationJSONKeys(data) != nil || p.Schema != 1 || p.Workspace != s.config.Workspace || p.Slug != slug || p.Round < 0 || p.NoProgress < 0 || (p.MessageID != "" && (p.Options == nil || p.Message == "" || !queueClientMessageIDPattern.MatchString(p.MessageID)))) {
				err = errors.New("archive preparation identity or delivery is invalid")
			}
		}
		if err != nil {
			s.config.Logger.Printf("read archive preparation for %s: %v", slug, err)
			s.operationMu.Lock()
			if current, ok := s.operations[slug]; ok && current.ReceiptID == original.ReceiptID && current.Attempt == original.Attempt {
				current.State = "failed"
				current.Error = "Archive preparation could not be read. Other sessions remain available; review this preparation before retrying."
				if saveErr := s.replaceLifecycleOperationLocked(slug, current); saveErr != nil {
					s.config.Logger.Printf("park invalid archive preparation: %v", saveErr)
				}
			}
			s.operationMu.Unlock()
			continue
		}
		if !p.matches(original) {
			continue
		} // Orphaned or superseded preparation.
		s.archivePrepMu.Lock()
		if s.archivePreparations == nil {
			s.archivePreparations = make(map[string]archivePreparation)
		}
		s.archivePreparations[p.Slug] = p
		s.archivePrepMu.Unlock()
		if !p.active() {
			continue
		}
		progress, err := session.PendingLifecycleProgress(s.config.Workspace, p.Slug)
		if err != nil || progress != nil {
			continue
		}
		s.operationMu.Lock()
		op, ok := s.operations[slug]
		if !ok || !p.matches(op) {
			s.operationMu.Unlock()
			continue
		}
		op.State = "running"
		err = s.replaceLifecycleOperationLocked(p.Slug, op)
		if err == nil {
			s.operationWG.Add(1)
		}
		s.operationMu.Unlock()
		if err != nil {
			return err
		}
		args := []string{"archive", p.Slug, "--as-is", "--portal-authorized", "--portal-operation-id", p.JournalID}
		go s.runLifecycleJob(op, args)
	}
	return nil
}

func (s *Server) parkArchivePreparations() {
	s.archivePrepMu.Lock()
	defer s.archivePrepMu.Unlock()
	for slug, p := range s.archivePreparations {
		if !p.active() {
			continue
		}
		p.State = "paused"
		if err := s.saveArchivePreparationLocked(p); err != nil {
			s.config.Logger.Printf("park archive preparation: %v", err)
		}
		if op, ok := s.operations[slug]; ok && p.matches(op) {
			op.State = "failed"
			op.Phase = "checking"
			op.Error = "Archive preparation was paused for a portal restart. Retry to continue."
			if err := s.replaceLifecycleOperationLocked(slug, op); err != nil {
				s.config.Logger.Printf("park archive receipt: %v", err)
			}
		}
	}
}

func (s *Server) requireArchivePreparationCurrent(op lifecycleOperation, p archivePreparation) error {
	s.operationMu.Lock()
	current, ok := s.operations[p.Slug]
	closing := s.closing
	s.operationMu.Unlock()
	if closing || !ok || !p.matches(current) || current.State != "running" {
		return errors.New("archive request changed")
	}
	present, ok := s.archivePreparationFor(p.Slug)
	if !ok || !present.matches(op) || !present.active() {
		return errors.New("archive preparation was canceled")
	}
	return nil
}

// No admission lock survives a model turn or a polling interval.
func (s *Server) withArchivePreparationAdmission(ctx context.Context, op lifecycleOperation, p archivePreparation, action func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, unlock, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer unlock()
	if err = s.requireCurrentHostProfile(); err != nil {
		return err
	}
	if err = s.requireProviderAdmission(p.Slug); err != nil {
		return err
	}
	runtime, err := session.LockRuntimeShared(s.config.AuthorityDir, p.Slug)
	if err != nil {
		return err
	}
	defer runtime.Close()
	if err := s.requireArchivePreparationCurrent(op, p); err != nil {
		return err
	}
	progress, err := session.PendingLifecycleProgress(s.config.Workspace, p.Slug)
	if err != nil {
		return err
	}
	if progress != nil {
		return errors.New("session lifecycle changed during preparation")
	}
	summary, err := session.Find(s.config.Workspace, p.Slug)
	if err != nil {
		return err
	}
	if summary.Archived || summary.Codex.ThreadID != p.Thread {
		return errors.New("archive lead identity changed")
	}
	if p.Thread != "" {
		if err := session.RequireRetainedReady(summary, s.config.CodexSocket); err != nil {
			return err
		}
		if err := RequireObservationReceipts(summary, s.config.UserStateRoot, p.JournalID, "archived"); err != nil {
			return err
		}
	}

	if _, err = validateLifecycleTarget(summary, p.TargetID, "archive"); err != nil {
		return err
	}
	return action(ctx)
}

func (s *Server) runArchivePreparation(ctx context.Context, op lifecycleOperation) error {
	p, ok := s.archivePreparationFor(op.Slug)
	if !ok || !p.matches(op) {
		return nil
	} // Existing journal/legacy operation.
	fail := func(err error) error {
		if ctx.Err() == nil {
			_, _ = s.updateArchivePreparation(p, func(value *archivePreparation) {
				value.State = "failed"
				value.Error = boundedLifecycleError(err.Error())
			})
		}
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.MessageID != "" {
			var complete bool
			err := s.withArchivePreparationAdmission(ctx, op, p, func(call context.Context) error {
				message := s.messageLock(p.Slug)
				if err := message.Lock(call); err != nil {
					return err
				}
				defer message.Unlock()
				if err := s.requireArchivePreparationCurrent(op, p); err != nil {
					return err
				}
				client, ok := s.config.Codex.(archiveRepairClient)
				if !ok || p.Thread == "" {
					return errors.New("The retained lead is unavailable for archive cleanup.")
				}
				if s.recoveryHeld(p.Slug, p.Thread) {
					return errors.New("Activate the retained lead before retrying archive preparation.")
				}
				metadata, err := client.ReadThreadMetadata(call, p.Thread, true)
				if err != nil {
					return err
				}
				if metadata.ID != p.Thread || metadata.Cwd != filepath.Join(s.config.Workspace, "work", p.Slug) {
					return errors.New("native archive lead identity changed")
				}
				action := "archive-preparation:" + p.JournalID
				receipt, found, err := client.ReconcileSendWithOptions(call, p.Thread, p.Message, p.MessageID, action, *p.Options)
				if err != nil {
					return err
				}
				if !found {
					receipt, err = client.SendWithOptions(call, p.Thread, p.Message, p.MessageID, action, *p.Options)
					if err != nil {
						return err
					}
				}
				p, err = s.updateArchivePreparation(p, func(value *archivePreparation) { value.Delivery = &receipt; value.State = "lead_fixing_blockers" })
				if err != nil {
					return err
				}
				if receipt.TurnID == "" {
					return errors.New("Repair delivery is queued; activate the retained lead and retry using this same delivery.")
				}
				var turns struct {
					Data *[]struct {
						ID     string `json:"id"`
						Status string `json:"status"`
					} `json:"data"`
				}
				if err = client.Request(call, "thread/turns/list", map[string]any{"threadId": p.Thread, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}, &turns); err != nil {
					return err
				}
				if turns.Data == nil || len(*turns.Data) == 0 || (*turns.Data)[0].ID != receipt.TurnID {
					return errors.New("The accepted repair turn cannot be verified.")
				}
				switch (*turns.Data)[0].Status {
				case "completed":
					complete = true
				case "inProgress":
				default:
					return errors.New("The lead's repair turn did not complete successfully; review its conversation.")
				}
				prompts, err := s.config.Codex.PromptsWithItems(call, p.Thread)
				if err != nil {
					return err
				}
				for _, prompt := range prompts {
					if prompt.IsBlocking {
						return errors.New("The lead needs your input before archive cleanup can continue.")
					}
				}
				return nil
			})
			if err != nil {
				var unknown *codex.UnknownSendOutcomeError
				var transport *codex.TransportError
				if ctx.Err() == nil && (errors.As(err, &unknown) || errors.As(err, &transport)) {
					var saveErr error
					p, saveErr = s.updateArchivePreparation(p, func(value *archivePreparation) {
						value.DeliveryFailures++
						value.Error = boundedLifecycleError(err.Error())
					})
					if saveErr != nil {
						return saveErr
					}
					if p.DeliveryFailures < 3 {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(time.Duration(1<<p.DeliveryFailures) * time.Second):
						}
						continue
					}
				}
				return fail(err)
			}
			if p.DeliveryFailures != 0 {
				p, err = s.updateArchivePreparation(p, func(value *archivePreparation) { value.DeliveryFailures = 0; value.Error = "" })
				if err != nil {
					return err
				}
			}
			if !complete {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Second):
				}
				continue
			}
			p, err = s.updateArchivePreparation(p, func(value *archivePreparation) {
				value.MessageID = ""
				value.Message = ""
				value.Options = nil
				value.Delivery = nil
				value.State = "rechecking"
			})
			if err != nil {
				return err
			}
		}
		var result archiveCheckResult
		err := s.withArchivePreparationAdmission(ctx, op, p, func(call context.Context) error {
			stdout, stderr, err := s.runDevSession(call, 30*time.Second, "archive-check", p.Slug, "--as-is", "--json")
			if err != nil {
				return commandFailure("check archive readiness", stdout, stderr, err)
			}
			if err = json.Unmarshal([]byte(stdout), &result); err != nil {
				return errors.New("Archive readiness returned invalid output.")
			}
			if result.Schema != 1 || result.Workspace != s.config.Workspace || result.Slug != p.Slug || result.Passed < 0 || result.Total <= 0 || result.Total > 32 || result.Passed > result.Total || result.Stage < 0 || result.Stage > result.Passed || result.Count < len(result.Blockers) || len(result.Blockers) > 128 || result.Ready != (result.Count == 0) || (result.Ready && (result.Passed != result.Total || result.Stage != result.Total)) || (!result.Ready && len(result.Blockers) == 0) {
				return errors.New("Archive readiness returned inconsistent output.")
			}
			return nil
		})
		if err != nil {
			return fail(err)
		}
		p, err = s.updateArchivePreparation(p, func(value *archivePreparation) {
			value.Check = &result
			value.History = append(value.History, archivePreparationRound{Round: value.Round, Passed: result.Passed, Blockers: result.Count})
			if len(value.History) > 16 {
				value.History = value.History[len(value.History)-16:]
			}
			if result.Stage > value.BestStage || (result.Stage == value.BestStage && result.Count < value.BestCount) {
				value.BestStage = result.Stage
				value.BestCount = result.Count
				value.NoProgress = 0
			} else {
				value.NoProgress++
			}
		})
		if err != nil {
			return err
		}
		if result.Ready {
			_, err = s.updateArchivePreparation(p, func(value *archivePreparation) { value.State = "archiving" })
			return err
		}
		if p.NoProgress >= 3 {
			return fail(errors.New("Archive cleanup made no measurable progress in three repair cycles. Review the lead conversation and blockers before retrying."))
		}
		if p.Thread == "" {
			return fail(errors.New("Archive checks found blockers, but this session has no retained lead. Fix the listed paths and retry."))
		}
		err = s.withArchivePreparationAdmission(ctx, op, p, func(call context.Context) error {
			client, ok := s.config.Codex.(archiveRepairClient)
			if !ok {
				return errors.New("The retained lead is unavailable for archive cleanup.")
			}
			settings, err := client.ReadThreadSettings(call, p.Thread)
			if err != nil {
				return err
			}
			instructions := ""
			summary, err := session.Find(s.config.Workspace, p.Slug)
			if err != nil {
				return err
			}
			roster, err := s.loadTeamRoster(summary)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if roster != nil {
				instructions = roster.LeadInstructions
			}
			message := "The user requested archival of this session. Prepare it for archive: pause normal development, repair only the session-owned local blockers below, and finish this turn. Keep the lead available. Do not run archive/delete/stop, change shared infrastructure, rewrite Git history, invent ownership, or edit lifecycle journals/receipts. Do not resume queued user work. If operator input is required, explain why and request it. The portal will independently recheck readiness and retry archival.\n\n"
			for index, blocker := range result.Blockers {
				if index >= 12 {
					break
				}
				text := blocker.Message
				if len(text) > 512 {
					text = text[:512]
				}
				message += fmt.Sprintf("- [%s] %s\n", blocker.Code, text)
			}
			digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", p.JournalID, p.Round+1)))
			id := hex.EncodeToString(digest[:16])
			id = fmt.Sprintf("%s-%s-%s-%s-%s", id[:8], id[8:12], id[12:16], id[16:20], id[20:])
			options := codex.TurnOptions{Model: settings.Model, ReasoningEffort: settings.ReasoningEffort, ThreadPolicy: workspacecodex.LeadThreadPolicy(p.Slug, s.config.Workspace, instructions)}
			p, err = s.updateArchivePreparation(p, func(value *archivePreparation) {
				value.Round++
				value.MessageID = id
				value.Message = message
				value.Options = &options
				value.State = "lead_fixing_blockers"
			})
			return err
		})
		if err != nil {
			return fail(err)
		}
	}
}

func (s *Server) cancelArchivePreparation(w http.ResponseWriter, r *http.Request, slug string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		ReceiptID string `json:"receiptId"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	// Wait for an already admitted send to finish before revoking later sends.
	message := s.messageLock(slug)
	if err := message.Lock(r.Context()); err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	defer message.Unlock()
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	op, ok := s.operations[slug]
	s.archivePrepMu.Lock()
	defer s.archivePrepMu.Unlock()
	p, exists := s.archivePreparations[slug]
	if !ok || !exists || body.ReceiptID != op.ReceiptID || !p.matches(op) || !p.active() {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Archive preparation is no longer cancelable."})
		return
	}
	p.State = "canceled"
	p.Error = "Archive preparation canceled. An accepted cleanup turn may finish, but archival will not start."
	if err := s.saveArchivePreparationLocked(p); err != nil {
		s.writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	op.State = "failed"
	op.Phase = "checking"
	op.Error = p.Error
	if err := s.replaceLifecycleOperationLocked(slug, op); err != nil {
		s.writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, op)
}
