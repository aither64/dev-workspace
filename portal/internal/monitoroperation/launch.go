package monitoroperation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
)

func PolicyFromCatalog(packageRoot string, retained, selected *agentteams.Catalog) (UtilityPolicy, error) {
	if retained == nil || selected == nil {
		return UtilityPolicy{}, errors.New("monitoring requires a managed utility policy")
	}
	if err := retained.Validate(); err != nil {
		return UtilityPolicy{}, err
	}
	if err := selected.Validate(); err != nil {
		return UtilityPolicy{}, err
	}
	instructions, err := agentteams.VerificationWatcherInstructions(packageRoot, *retained)
	if err != nil {
		return UtilityPolicy{}, err
	}
	watcher := retained.Utilities.VerificationWatcher
	sandbox := "read-only"
	if watcher.Access == "workspace_write" {
		sandbox = "workspace-write"
	}
	if selected.FullExecutionAccess() {
		sandbox = "danger-full-access"
	}
	return UtilityPolicy{PackageRoot: packageRoot, CatalogDigest: retained.CatalogDigest, NativeName: retained.NativeAgentConfigs.Utilities[0].Name, Model: watcher.Model, Effort: watcher.Effort, Instructions: instructions, Sandbox: sandbox, ApprovalPolicy: "never", MaxConcurrent: watcher.MaxConcurrent}, nil
}

func utilitySettings(op Operation) codex.ThreadSettings {
	p := op.UtilityPolicy
	return codex.ThreadSettings{Model: p.Model, ReasoningEffort: p.Effort, ModelProvider: op.Owner.Provider, ProjectID: op.Launch.ProjectID, Policy: codex.ThreadPolicy{DeveloperInstructions: p.Instructions, Sandbox: p.Sandbox, ApprovalPolicy: p.ApprovalPolicy}}
}

func initialLaunch(op Operation) Launch {
	source, _ := json.Marshal(op.Source)
	task := `Observe the exact existing CI run in Source. Never start or rerun CI.`
	if op.Source.Kind == "local" {
		task = `You own execution of this local source command. Run Source.argv exactly once in Source.cwd with your command tool, then observe that same tool session until it finishes. Preserve every argv element, including empty arguments and shell metacharacters. Registration starts your utility turn; it does not start the source command for you.

Do not poll monitor_operation status waiting for the source command to start or finish. launch.state=running describes your utility turn, not a command process. The earlier initialization marker is bookkeeping for this thread, not an instruction to skip this command. If you already attempted the source command, join/observe that attempt; never repeat it. If its launch outcome is uncertain, report incomplete with the uncertainty instead of launching again.`
	}
	envelope := fmt.Sprintf(`%s

Operation ID: %s
Source: %s
Cancellation scope: %s

Lead brief:
%s

Retain evidence and authoritative exit status or CI conclusion. Report through monitor_operation with action report, slot terminal and outcome passed, failed, or incomplete. Reports must omit id, operationId, threadId and recipient: your native caller identifies the operation. Example for a command that exited 0: {"action":"report","slot":"terminal","outcome":"passed","exitStatus":0,"elapsedMillis":123,"evidence":["exact command tool result"],"detail":"Command finished; all owned work joined"}. Use the actual result and elapsed time. For escalation use slot escalation, outcome incomplete, detail and stillRunning identity.

For terminal passed or failed, omit stillRunning or set it to an empty string. Put details of completed work in detail/evidence; do not write "none" in stillRunning. A canceled command or CI run can report failed with its authoritative exit status or conclusion once all effects are accounted for, leaving stillRunning empty or omitted. Unfinished or uncertain work requires incomplete, with exact known identities and unknown effects in stillRunning.

Account for every operation-owned code cell and tool session. Before reporting terminal passed or failed, finish and join all other outstanding operation-owned work. Await the actual reporting call and let the current reporting cell finish before ending your turn. An idle turn or the source command's exit alone does not prove all effects have ended. If a yielded cell or tool session cannot be reconciled within the original authority, report terminal incomplete. Include exact known cell, session and run IDs and evidence; put still-running identities and unknown effects in stillRunning. This grants no new rerun or interruption authority.

After escalation, retain ownership of this exact run and continue only the originally authorized observation and cancellation conditions until authoritative terminal evidence. Do not wait for a reply or infer authority for a new command, attempt, retry or cancellation. If the original scope cannot continue, report terminal incomplete with exact still-running or unknown effects.

For a cancellation check or an uncertain report acknowledgement, use {"action":"status"} with no selector. A canceledAt value revokes continuation. If status shows canceledAt and you have not attempted the source command, do not start it: report terminal incomplete with detail that it was canceled before command launch, evidence accounting for every owned cell/tool session, and no invented exit status. Then finish the reporting cell and end your turn. If a command was already attempted, continue only its originally authorized observation, account for effects and report; cancellation does not authorize killing or repeating it. Do not wait for launch.state to change: it describes this utility, which cannot terminate while you keep polling it.

Reports use your registered native caller; do not call report_to_lead, run a new attempt, diagnose, retry, edit, deploy or spawn agents. A terminal model turn without a durable report is incomplete. Cancellation revokes continuation and never grants permission to kill a command.`, task, op.ID, source, op.CancellationScope, op.Brief)
	settings := utilitySettings(op)
	return Launch{State: "reserved", MessageID: derivedID(op.ID, "launch"), Envelope: envelope, Options: codex.TurnOptions{Model: settings.Model, ReasoningEffort: settings.ReasoningEffort, ThreadPolicy: settings.Policy}}
}

func (s *Service) launch(ctx context.Context, tx *Transaction, current Owner) error {
	op := tx.Operation
	if op.CanceledAt != nil || op.Disposition != "" {
		return s.reconcileLaunch(ctx, tx)
	}
	if op.Launch.Receipt != nil {
		return nil
	}
	if !SameOwner(op.Owner, current) || current.SocketEpoch != op.Owner.SocketEpoch {
		return errors.New("monitor launch is held after owner or native epoch change")
	}
	cwd := filepath.Join(s.Store.workspace, "work", op.Owner.Slug)
	projectName := "monitor " + op.ID
	if op.Launch.ProjectID == "" {
		project, err := s.Client.CreateProject(ctx, projectName, derivedID(op.ID, "project"))
		if err != nil {
			return fmt.Errorf("register monitor project: %w", err)
		}
		if !validID(project.ID) || project.Name != projectName {
			return errors.New("monitor project returned another identity")
		}
		op.Launch.ProjectID = project.ID
		if err := tx.Save(); err != nil {
			return err
		}
	}
	project, err := s.Client.ReadProject(ctx, op.Launch.ProjectID)
	if err != nil {
		return err
	}
	if project.ID != op.Launch.ProjectID || project.Name != projectName {
		return errors.New("monitor project identity changed")
	}
	if op.Launch.ThreadID == "" {
		if !op.Launch.CreateAttempted {
			op.Launch.CreateAttempted = true
			op.Launch.State = "creating"
			if err := tx.Save(); err != nil {
				return err
			}
			environment := map[string]string{"DEV_SESSION_SLUG": op.Owner.Slug, "DEV_SESSION_WORKSPACE": s.Store.workspace, "DEV_SESSION_WORK_DIR": cwd}
			thread, startErr := s.Client.StartThreadWithSettings(ctx, cwd, environment, utilitySettings(*op))
			if startErr == nil && thread != "" {
				op.Launch.ThreadID = thread
			}
		}
		if op.Launch.ThreadID == "" {
			thread, err := s.findUtility(ctx, *op, cwd)
			if err != nil {
				return fmt.Errorf("monitor thread creation remains uncertain: %w", err)
			}
			op.Launch.ThreadID = thread
		}
		metadata, err := s.Client.ReadThreadMetadata(ctx, op.Launch.ThreadID, false)
		// Native may omit project metadata before the fresh rollout exists.
		// Bootstrap verifies the exact project after materializing its marker.
		if err != nil || metadata.ID != op.Launch.ThreadID || metadata.Cwd != cwd || metadata.ProjectID != nil && *metadata.ProjectID != op.Launch.ProjectID || metadata.ForkedFromID != "" {
			return errors.New("monitor utility identity cannot be verified")
		}
		op.Launch.BootstrapMarker = fmt.Sprintf("Internal monitor operation initialization %s for exact utility %s. Bootstrap bookkeeping only. The following verification assignment defines the work to execute.", op.ID, op.Launch.ThreadID)
		if err := tx.Save(); err != nil {
			return err
		}
	}
	if !op.Launch.BootstrapAttempted {
		op.Launch.BootstrapAttempted = true
		op.Launch.State = "bootstrapping"
		if err := tx.Save(); err != nil {
			return err
		}
		if err := s.Client.BootstrapHeadlessThread(ctx, op.Launch.ThreadID, cwd, op.Launch.ProjectID, op.Launch.BootstrapMarker); err != nil {
			return err
		}
	} else if err := s.Client.VerifyHeadlessBootstrap(ctx, op.Launch.ThreadID, cwd, op.Launch.ProjectID, op.Launch.BootstrapMarker); err != nil {
		return err
	}
	// The exact binding and bootstrap are durable before permission or input.
	if err := s.Activate(ctx, op.Owner, op.Launch.ThreadID); err != nil {
		return err
	}
	op.Launch.ActivatedEpoch = op.Owner.SocketEpoch
	op.Launch.State = "submitting"
	if err := tx.Save(); err != nil {
		return err
	}
	action := "monitor-launch:" + op.ID
	receipt, found, err := s.Client.ReconcileSendWithOptions(ctx, op.Launch.ThreadID, op.Launch.Envelope, op.Launch.MessageID, action, op.Launch.Options)
	if err != nil {
		return err
	}
	if !found {
		receipt, err = s.Client.SendWithOptions(ctx, op.Launch.ThreadID, op.Launch.Envelope, op.Launch.MessageID, action, op.Launch.Options)
		if err != nil {
			return err
		}
	}
	if receipt.ClientUserMessageID != op.Launch.MessageID || receipt.TurnID == "" || receipt.QueuedSubmissionID != "" || receipt.Steered {
		return errors.New("monitor launch has no exact new-turn acceptance receipt")
	}
	op.Launch.Receipt = &receipt
	op.Launch.State = "running"
	op.Launch.Detail = ""
	return tx.Save()
}

// reconcileLaunch recovers only existing evidence. It never creates a native
// project/thread, injects bootstrap history, loads a thread or submits input.
func (s *Service) reconcileLaunch(ctx context.Context, tx *Transaction) error {
	op := tx.Operation
	unsentState := "unsubmitted"
	if op.CanceledAt != nil || op.Disposition != "" {
		unsentState = "canceled-unsent"
	}
	if op.Launch.Receipt != nil {
		return nil
	}
	if s.Client == nil {
		return errors.New("monitor launch reconciliation client is unavailable")
	}
	if !op.Launch.CreateAttempted {
		// The durable start marker precedes every thread/start. No utility or
		// command can have begun when that marker is absent.
		if op.CanceledAt != nil && op.Launch.TerminatedAt == nil {
			now := s.now()
			op.Launch.TerminatedAt = &now
			op.Launch.Termination = "not_started"
			op.Launch.State = "canceled-unsent"
			return tx.Save()
		}
		return nil
	}
	cwd := filepath.Join(s.Store.workspace, "work", op.Owner.Slug)
	if op.Launch.ThreadID == "" {
		thread, err := s.findUtility(ctx, *op, cwd)
		if err != nil {
			return err
		}
		op.Launch.ThreadID = thread
		op.Launch.BootstrapMarker = fmt.Sprintf("Internal monitor operation initialization %s for exact utility %s. Bootstrap bookkeeping only. The following verification assignment defines the work to execute.", op.ID, thread)
		if err := tx.Save(); err != nil {
			return err
		}
	}
	if !op.Launch.BootstrapAttempted {
		op.Launch.State = unsentState
		op.Launch.Detail = ""
		return tx.Save()
	}
	if err := s.Client.VerifyHeadlessBootstrap(ctx, op.Launch.ThreadID, cwd, op.Launch.ProjectID, op.Launch.BootstrapMarker); err != nil {
		return err
	}
	if op.Launch.State != "submitting" && op.Launch.State != "canceled-unsent" && op.Launch.State != "unsubmitted" {
		op.Launch.State = unsentState
		op.Launch.Detail = ""
		return tx.Save()
	}
	receipt, found, err := s.Client.ReconcileSendWithOptions(ctx, op.Launch.ThreadID, op.Launch.Envelope, op.Launch.MessageID, "monitor-launch:"+op.ID, op.Launch.Options)
	if err != nil {
		return err
	}
	if !found {
		op.Launch.State = unsentState
		op.Launch.Detail = ""
		return tx.Save()
	}
	if receipt.ClientUserMessageID != op.Launch.MessageID || receipt.TurnID == "" || receipt.Steered || receipt.QueuedSubmissionID != "" {
		return errors.New("monitor launch receipt is not an exact started turn")
	}
	op.Launch.Receipt = &receipt
	op.Launch.State = "running"
	op.Launch.Detail = ""
	return tx.Save()
}

func (s *Service) findUtility(ctx context.Context, op Operation, cwd string) (string, error) {
	var found string
	cursor := ""
	seen := map[string]bool{}
	for {
		threads, next, err := s.Client.ListThreads(ctx, codex.ThreadListOptions{Cwd: cwd, ProjectID: op.Launch.ProjectID, ModelProviders: []string{}, Cursor: cursor, Limit: 100})
		if err != nil {
			return "", err
		}
		for _, thread := range threads {
			if thread.Cwd != cwd || thread.ProjectID == nil || *thread.ProjectID != op.Launch.ProjectID || thread.ForkedFromID != "" || thread.ID == "" {
				return "", errors.New("monitor project contains a different thread identity")
			}
			if found != "" && found != thread.ID {
				return "", errors.New("monitor project has ambiguous threads")
			}
			found = thread.ID
		}
		if next == nil || *next == "" {
			break
		}
		if seen[*next] {
			return "", errors.New("monitor project listing repeats a cursor")
		}
		seen[*next] = true
		cursor = *next
	}
	if found == "" {
		return "", errors.New("no exact monitor thread is visible; no second launch is allowed")
	}
	return found, nil
}
