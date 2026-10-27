package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type recoveryInputClient interface {
	ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
	RequireThreadTurnsIdle(context.Context, string) error
	HasActiveGoal(context.Context, string) (bool, error)
	PrepareSendWithOptions(string, string, string, string, bool, codex.TurnOptions) error
	IsPreparedSendWithOptions(string, string, string, string, codex.TurnOptions) (bool, error)
	ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error)
	SendRecoveredWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, error)
}

// These bounded reads classify actual work, independently of execution permission.
// No read grants permission, watches a thread, or resumes its native runtime.
func (s *Server) coldWork(ctx context.Context, summary *session.Summary, thread string) (string, error) {
	client, ok := s.config.Codex.(recoveryInputClient)
	if !ok {
		return "", errors.New("Saved conversation status is unavailable.")
	}
	if err := s.config.VerifyThread(ctx, thread, filepath.Join(s.config.Workspace, "work", summary.Slug)); err != nil {
		return "", err
	}
	metadata, err := client.ReadThreadMetadata(ctx, thread, true)
	if err != nil {
		return "", err
	}
	if metadata.ID != thread || metadata.Cwd != filepath.Join(s.config.Workspace, "work", summary.Slug) {
		return "", errors.New("Saved conversation identity changed.")
	}
	status, valid := metadata.Status["type"].(string)
	if !valid {
		return "", errors.New("Could not check whether this conversation is idle.")
	}
	if status == "active" {
		return "interrupted", nil
	}
	if len(metadata.Status) != 1 || !slices.Contains([]string{"idle", "notLoaded"}, status) {
		return "", errors.New("Could not confirm that this conversation is idle.")
	}
	if err := client.RequireThreadTurnsIdle(ctx, thread); err != nil {
		return "", err
	}
	prompts, err := s.config.Codex.PromptsWithItems(ctx, thread)
	if err != nil {
		return "", err
	}
	if len(prompts) != 0 {
		return "interrupted", nil
	}
	queue, err := s.config.Codex.ListQueue(ctx, thread)
	if err != nil {
		return "", err
	}
	if len(queue) != 0 {
		return "queue", nil
	}
	goal, err := client.HasActiveGoal(ctx, thread)
	if err != nil {
		return "", err
	}
	if goal {
		return "goal", nil
	}
	return "", nil
}

func (s *Server) heldRecoveryStatus(parent context.Context, summary *session.Summary, thread string) recoveryStatus {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	work, err := s.coldWork(ctx, summary, thread)
	status := recoveryStatus{State: "idle", ExecutionHeld: true}
	if err == nil {
		authority, readErr := session.LoadRuntimeAuthority(s.config.AuthorityDir, summary.Slug, s.config.Workspace)
		if readErr != nil {
			err = readErr
		} else {
			err = authority.VerifyStoppedPane(ctx, s.config.Tmux)
		}
	}
	if work != "" {
		status = s.waitingRecoveryStatus(summary)
		status.ExecutionHeld, status.WorkKind = true, work
		status.InputBlockedReason = coldWorkReason(work)
	} else if err != nil {
		status.State, status.Error, status.InputBlockedReason = "unavailable", err.Error(), err.Error()
	}
	return status
}

func coldWorkReason(work string) string {
	switch work {
	case "queue":
		return "Queued messages are paused. Start or remove them before sending a new message."
	case "goal":
		return "This session has an active goal. Continue or pause it before sending a new message."
	default:
		return "Native work is still active or waiting for input. Resolve it before sending new input."
	}
}

func unsentRecovery(err error) error { return &codex.MessageNotSentError{Err: err, Retryable: false} }

// The resolver holds transition SH, Runtime EX, and (for a member) roster EX
// through this method and its receipt. Native managed-pane admission is read
// under that fence; raw operator clients are outside the managed contract.
func (c recoveryConversationClient) sendCold(ctx context.Context, text, id, action, requestedMode string) (codex.SendReceipt, error) {
	s := c.server
	client, ok := s.config.Codex.(recoveryInputClient)
	if !ok {
		return codex.SendReceipt{}, unsentRecovery(errors.New("Cold message delivery is unavailable."))
	}
	options := codex.TurnOptions{}
	if c.address != "" {
		service, err := s.teamService(c.slug)
		if err != nil {
			return codex.SendReceipt{}, unsentRecovery(err)
		}
		memberClient := memberConversationClient{Client: s.config.Codex, server: s, service: service, slug: c.slug, rootThreadID: c.root, address: c.address, threadID: c.thread}
		options, err = memberClient.turnOptions(text, id, action)
		if err != nil {
			return codex.SendReceipt{}, err
		}
	}
	// Reconciliation always precedes new preparation or native effects. An old
	// accepted receipt never releases a new socket epoch's execution hold.
	if receipt, found, err := client.ReconcileSendWithOptions(ctx, c.thread, text, id, action, options); err != nil || found {
		if found && err == nil {
			err = c.finishColdReceipt(ctx, id)
		}
		return receipt, err
	}
	settings, err := s.coldSettings(c.slug, c.root, c.thread)
	if err != nil {
		return codex.SendReceipt{}, unsentRecovery(err)
	}
	if settings == nil {
		settings = &coldSettings{Slug: c.slug, Root: c.root, Thread: c.thread}
	}
	epoch, err := session.SocketIdentity(s.config.CodexSocket)
	if err != nil {
		return codex.SendReceipt{}, unsentRecovery(err)
	}
	if settings.RequestID != "" && (settings.RequestID != id || (requestedMode != "" && requestedMode != settings.Mode)) {
		return codex.SendReceipt{}, unsentRecovery(errors.New("Recover the pending original input before changing its request or mode."))
	}
	if settings.RequestID == "" {
		if err := client.PrepareSendWithOptions(c.thread, text, id, action, false, options); err != nil {
			return codex.SendReceipt{}, err
		}
	}
	// A bound original can cross epochs only with its existing exact prepared
	// proof. Never recreate a missing preparation to justify that rebind.
	prepared, err := client.IsPreparedSendWithOptions(c.thread, text, id, action, options)
	if err != nil {
		return codex.SendReceipt{}, err
	}
	if !prepared {
		return codex.SendReceipt{}, &codex.UnknownSendOutcomeError{Err: errors.New("The original attempt is not proven unsent; recover its receipt before trying again.")}
	}
	summary, err := session.Find(s.config.Workspace, c.slug)
	if err != nil || summary.Codex.ThreadID != c.root {
		return codex.SendReceipt{}, unsentRecovery(errors.New("Saved conversation identity changed."))
	}
	checkIdentity := s.requireRetainedInputIdentity
	if c.address == "" {
		checkIdentity = func(ctx context.Context, summary *session.Summary) error {
			return s.requireOrdinaryRetainedMutationExceptPrepared(ctx, summary, &workspacecodex.PreparedSend{Text: text, ID: id, Action: action, Options: options})
		}
	}
	if err := checkIdentity(ctx, summary); err != nil {
		return codex.SendReceipt{}, unsentRecovery(err)
	}
	authority, err := session.LoadRuntimeAuthority(s.config.AuthorityDir, c.slug, s.config.Workspace)
	if err != nil {
		return codex.SendReceipt{}, unsentRecovery(err)
	}
	if err := authority.VerifyStoppedPane(ctx, s.config.Tmux); err != nil {
		return codex.SendReceipt{}, unsentRecovery(err)
	}
	work, err := s.coldWork(ctx, summary, c.thread)
	if err != nil {
		return codex.SendReceipt{}, unsentRecovery(err)
	}
	if work != "" {
		return codex.SendReceipt{}, unsentRecovery(errors.New(coldWorkReason(work)))
	}
	if requestedMode != "" {
		settings.Mode = requestedMode
	}
	if settings.Mode == "" {
		transcript, err := c.Client.ReadThread(ctx, c.thread)
		if err != nil {
			return codex.SendReceipt{}, unsentRecovery(err)
		}
		settings.Mode = transcript.CollaborationMode
	}
	if settings.Mode != "" {
		if err := s.validateCollaborationMode(ctx, settings.Mode); err != nil {
			return codex.SendReceipt{}, unsentRecovery(err)
		}
	}
	settings.RequestID, settings.Epoch = id, epoch
	if err := s.saveThreadRecord("next-settings", c.thread, settings); err != nil {
		return codex.SendReceipt{}, unsentRecovery(err)
	}
	// Loading an empty thread cannot release older queued or goal work. Keep the
	// application hold until mode confirmation and the exact delivery receipt.
	if err := s.loadRecovery(ctx, c.slug, c.root, c.thread, c.address); err != nil {
		return codex.SendReceipt{}, unsentRecovery(err)
	}
	if settings.Mode != "" {
		mode := settings.Mode
		confirmed, err := s.config.Codex.UpdateThreadSettings(ctx, c.thread, codex.ThreadSettingsUpdate{CollaborationMode: &mode})
		if err != nil {
			return codex.SendReceipt{}, unsentRecovery(err)
		}
		if confirmed.CollaborationMode != mode {
			return codex.SendReceipt{}, unsentRecovery(errors.New("The selected collaboration mode was not confirmed."))
		}
	}
	if current, err := session.SocketIdentity(s.config.CodexSocket); err != nil || current != epoch {
		return codex.SendReceipt{}, unsentRecovery(errors.New("App Server changed before input delivery."))
	}
	receipt, err := client.SendRecoveredWithOptions(ctx, c.thread, text, id, action, options)
	if err != nil {
		return receipt, err
	}
	if err := c.finishColdReceipt(ctx, id); err != nil {
		return receipt, &codex.UnknownSendOutcomeError{Err: err}
	}
	return receipt, nil
}

func (c recoveryConversationClient) finishColdReceipt(ctx context.Context, id string) error {
	saved, err := c.server.coldSettings(c.slug, c.root, c.thread)
	if err != nil || saved == nil || saved.RequestID != id {
		return err
	}
	epoch, err := session.SocketIdentity(c.server.config.CodexSocket)
	if err != nil {
		return err
	}
	if saved.Epoch == epoch {
		if err := c.server.publishRecovery(ctx, c.slug, c.root, c.thread, epoch); err != nil {
			return err
		}
	}
	// An accepted old-epoch original is reconciled without activating this epoch.
	err = os.Remove(c.server.threadRecordPath("next-settings", c.thread))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
