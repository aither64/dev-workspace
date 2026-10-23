package workspacecodex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

func (c *Client) RetryStore() turnretry.Store {
	return turnretry.Store{Root: c.RecoveryRoot, Workspace: c.workspace}
}

func (c *Client) RetryAvailable() bool {
	return c != nil && c.RetryPolicyResolver != nil && c.RecoveryRoot != "" && c.workspace != ""
}

// Callers already proved a retained target under their runtime/roster lock.
// Cancellation takes no outer lock and never submits or loads native work.
func (c *Client) CancelAutomaticRetry(ctx context.Context, thread, reason string) error {
	if !c.RetryAvailable() {
		return nil
	}
	scope, err := c.RetryScope(ctx, thread)
	if err != nil {
		return err
	}
	if _, err := c.RetryStore().Load(scope); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	tx, err := c.RetryStore().Lock(ctx, scope)
	if err != nil {
		return err
	}
	defer tx.Close()
	if reason == "settings_changed" {
		if err := c.invalidateRetrySettings(ctx, tx); err != nil {
			return err
		}
	} else {
		tx.State.Cancel(reason)
	}
	if err := tx.Save(); err != nil {
		return err
	}
	if tx.State.Uncertain() {
		return errors.New("automatic submission requires reconciliation")
	}
	return nil
}

func (c *Client) RequireThreadIdle(ctx context.Context, thread, cwd string) error {
	if c.RecoveryRoot != "" && c.RetryPolicyResolver != nil {
		if scope, err := c.RetryScope(ctx, thread); err == nil {
			state, err := c.RetryStore().Load(scope)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil && state.Owned() {
				return errors.New("conversation has pending automatic retry work")
			}
		}
	}
	return c.Client.RequireThreadIdle(ctx, thread, cwd)
}

func (c *Client) RetryScope(ctx context.Context, thread string) (turnretry.Scope, error) {
	metadata, err := c.ReadThreadMetadata(ctx, thread, false)
	if err != nil {
		return turnretry.Scope{}, err
	}
	slug := filepath.Base(metadata.Cwd)
	if metadata.ID != thread || !session.ValidSlug(slug) || metadata.Cwd != filepath.Join(c.workspace, "work", slug) || !workspaceThread(metadata.Source) {
		return turnretry.Scope{}, errors.New("retry thread ownership is unavailable")
	}
	summary, err := session.Find(c.workspace, slug)
	if err != nil || summary.Archived {
		return turnretry.Scope{}, errors.New("retry session is unavailable")
	}
	root := summary.Codex.ThreadID
	if root == "" {
		root = thread
	} // new managed root, before manifest binding
	return turnretry.Scope{Workspace: c.workspace, Slug: slug, Root: root, Thread: thread, Socket: c.socket}, nil
}

// The resolver is also an ownership check. The admission lock is deliberately
// below the roster/runtime locks already held by browser, team and CLI callers.
func (c *Client) retryAdmission(ctx context.Context, thread string) (*turnretry.Transaction, codex.ThreadPolicy, error) {
	if c.RetryPolicyResolver == nil || c.RecoveryRoot == "" {
		return nil, codex.ThreadPolicy{}, nil
	}
	policy, err := c.RetryPolicyResolver(ctx, thread)
	if err != nil {
		return nil, policy, err
	}
	scope, err := c.RetryScope(ctx, thread)
	if err != nil {
		return nil, policy, err
	}
	tx, err := c.RetryStore().Lock(ctx, scope)
	return tx, policy, err
}

func textHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (c *Client) retryProvider(ctx context.Context, scope turnretry.Scope) (string, error) {
	var response struct {
		Thread struct {
			ID       string `json:"id"`
			Provider string `json:"modelProvider"`
		} `json:"thread"`
	}
	if err := c.Request(ctx, "thread/read", map[string]any{"threadId": scope.Thread, "excludeTurns": true}, &response); err != nil {
		return "", err
	}
	if response.Thread.ID != scope.Thread || response.Thread.Provider == "" {
		return "", errors.New("loaded provider binding is unavailable")
	}
	return response.Thread.Provider, nil
}

// ConfirmRetryBinding records a binding only after the trusted provider/recovery
// owner has verified its explicit native load. Automatic dispatch never calls it.
func (c *Client) ConfirmRetryBinding(ctx context.Context, thread string, policy codex.ThreadPolicy) error {
	if c.RetryPolicyResolver == nil || c.RecoveryRoot == "" {
		return nil
	}
	scope, err := c.RetryScope(ctx, thread)
	if err != nil {
		return err
	}
	tx, err := c.RetryStore().Lock(ctx, scope)
	if err != nil {
		return err
	}
	defer tx.Close()
	return c.recordRetryBinding(ctx, tx, policy)
}

func (c *Client) captureRetry(ctx context.Context, tx *turnretry.Transaction, policy codex.ThreadPolicy, text, id, origin string, options codex.TurnOptions) error {
	if options.ThreadPolicy != (codex.ThreadPolicy{}) && !reflect.DeepEqual(options.ThreadPolicy, policy) {
		return errors.New("managed submission policy changed")
	}
	for _, p := range tx.State.Snapshots {
		if p.ID == id {
			if p.TextDigest != textHash(text) {
				return errors.New("managed admission identity changed")
			}
			return nil
		}
	}
	tx.State.Cancel("manual_input")
	if err := tx.Save(); err != nil {
		return err
	}
	if tx.State.Uncertain() {
		return errors.New("automatic submission requires reconciliation before new work")
	}
	currentTurn := ""
	if origin != "initial" {
		latest, err := c.latestTurn(ctx, tx.State.Scope.Thread)
		if err != nil {
			return err
		}
		if latest != nil {
			currentTurn = latest.ID
		}
	}
	// Enrollment precedes the managed send. Latest-state observation by the
	// scheduler is used for existing threads; managed admission cannot consume
	// its own new terminal failure as a first-deployment baseline.
	if origin == "initial" && !tx.State.Enrolled {
		tx.State.Enroll(turnretry.Observation{})
	}
	if !tx.State.Enrolled {
		latest, err := c.latestTurn(ctx, tx.State.Scope.Thread)
		if err != nil {
			return err
		}
		o := turnretry.Observation{}
		if latest != nil {
			o.Turn, o.Status = latest.ID, latest.Status
		}
		tx.State.Enroll(o)
		if err := tx.Save(); err != nil {
			return err
		}
		again, err := c.latestTurn(ctx, tx.State.Scope.Thread)
		if err != nil {
			return err
		}
		if again != nil && (again.ID != o.Turn || again.Status != o.Status) {
			tx.State.Baseline, tx.State.BaselineActive = again.ID, false
		}
	}
	boundary, err := session.ObservationIdentity(c.workspace, tx.State.Scope.Slug, filepath.Join(c.workspace, "work", tx.State.Scope.Slug), tx.State.Scope.Root)
	if err != nil {
		return err
	}
	if tx.State.Boundary != "" && tx.State.Boundary != boundary {
		return errors.New("retry session incarnation changed")
	}
	tx.State.Boundary = boundary
	snapshot := turnretry.Snapshot{Epoch: tx.State.Epoch, Version: 1, ID: id, Origin: origin, CommonInstructions: c.retryCommonInstructions, Options: options, TextDigest: textHash(text)}
	snapshot.Options.ThreadPolicy = policy
	provider, err := c.retryProvider(ctx, tx.State.Scope)
	if err != nil {
		return err
	}
	snapshot.Provider = provider
	epoch, err := session.SocketIdentity(c.socket)
	if err != nil {
		return err
	}
	snapshot.SocketIdentity = epoch
	if binding := tx.State.Binding; binding != nil && binding.Epoch == tx.State.Epoch && binding.SocketIdentity == epoch && binding.Provider == provider && binding.CommonInstructions == c.retryCommonInstructions && reflect.DeepEqual(binding.Policy, policy) {
		snapshot.BindingDigest = binding.Digest
	}
	settings, err := c.Client.ReadThreadSettings(ctx, tx.State.Scope.Thread)
	if err == nil {
		if snapshot.Options.Model == "" {
			snapshot.Options.Model = settings.Model
		}
		if snapshot.Options.ReasoningEffort == "" {
			snapshot.Options.ReasoningEffort = settings.ReasoningEffort
		}
	}
	// Mode and actual queue execution settings are bound from the exact native
	// turn context after acceptance. A pre-send snapshot is not yet proof.
	snapshot.Digest, err = snapshot.Hash()
	if err != nil {
		return err
	}
	if err := tx.State.AdmitSnapshot(snapshot, currentTurn); err != nil {
		return err
	}
	return tx.Save()
}

// Generic delivery identity is authoritative even when bounded retry evidence
// has been retired. Reconciliation precedes cancellation, capture and rebind;
// accepted input must never acquire today's settings or epoch on replay.
func (c *Client) reconcileRetrySend(ctx context.Context, tx *turnretry.Transaction, thread, text, id, action string, options codex.TurnOptions) (codex.SendReceipt, bool, error) {
	receipt, found, err := c.Client.ReconcileSendWithOptions(ctx, thread, text, id, action, options)
	if err != nil || found {
		return receipt, found, err
	}
	// Reconcile alone deliberately does not distinguish absent from prepared,
	// and acknowledged accepted IDs may now exist only in native history.
	attempted, err := c.Client.SendAttemptedWithOptions(ctx, thread, text, id, action, options)
	if err != nil || !attempted {
		return codex.SendReceipt{}, false, err
	}
	receipt, found, err = c.Client.ReconcileSendWithOptions(ctx, thread, text, id, action, options)
	if err != nil || found {
		return receipt, found, err
	}
	for _, p := range tx.State.Snapshots {
		if p.ID == id && p.TextDigest == textHash(text) {
			return codex.SendReceipt{}, false, nil
		}
	}
	if prepared, err := c.Client.IsPreparedSendWithOptions(thread, text, id, action, options); err != nil || prepared {
		return codex.SendReceipt{}, false, err
	}
	return codex.SendReceipt{}, false, errors.New("earlier submission has no managed admission proof")
}

func recoverRetryReceipt(tx *turnretry.Transaction, id, text string, receipt codex.SendReceipt) error {
	for _, p := range tx.State.Snapshots {
		if p.ID == id {
			if p.TextDigest != textHash(text) {
				return errors.New("managed admission identity changed")
			}
			if receipt.TurnID != "" && p.Turn == "" {
				return bindRetryReceipt(tx, id, receipt)
			}
			if p.Turn != "" && receipt.TurnID != "" && p.Turn != receipt.TurnID {
				return errors.New("managed receipt identity changed")
			}
			return nil
		}
	}
	return nil // Accepted generic receipt, deliberately retired retry evidence.
}

func (c *Client) Send(ctx context.Context, thread, text, id, action string) (codex.SendReceipt, error) {
	return c.SendWithOptions(ctx, thread, text, id, action, codex.TurnOptions{})
}

func (c *Client) SendWithOptions(ctx context.Context, thread, text, id, action string, options codex.TurnOptions) (codex.SendReceipt, error) {
	tx, policy, err := c.retryAdmission(ctx, thread)
	if err != nil {
		return codex.SendReceipt{}, err
	}
	if tx == nil {
		return c.sendWithRecovery(ctx, thread, text, id, action, options)
	}
	defer tx.Close()
	if receipt, found, err := c.reconcileRetrySend(ctx, tx, thread, text, id, action, options); err != nil || found {
		if err == nil {
			if err = recoverRetryReceipt(tx, id, text, receipt); err != nil {
				err = &codex.UnknownSendOutcomeError{Err: err}
			}
		}
		return receipt, err
	}
	origin := "send"
	if strings.HasPrefix(action, "team:") {
		origin = "team"
	}
	if err := c.captureRetry(ctx, tx, policy, text, id, origin, options); err != nil {
		return codex.SendReceipt{}, err
	}
	// Refresh the managed binding only as part of this explicitly requested
	// idle send, never from the automatic continuation path. Steering retains
	// the active binding and is proved from that turn's native context.
	if active, readErr := c.Client.ActiveTurnID(ctx, thread); readErr == nil && active == "" && c.allowImplicitResume(thread) {
		if err := c.Client.ReconcileThreadInstructionsWithPolicy(ctx, thread, policy); err != nil {
			return codex.SendReceipt{}, err
		}
		if err := c.recordRetryBinding(ctx, tx, policy); err != nil {
			return codex.SendReceipt{}, err
		}
	}
	// Keep the generic submission identity identical to the caller's immutable
	// options. Root policy is explicitly rebound before a fresh managed send by
	// the normal client path; team sends already provide their retained policy.
	receipt, err := c.sendWithRecovery(context.WithValue(ctx, retryTransactionKey{}, tx), thread, text, id, action, options)
	if err == nil && receipt.TurnID != "" {
		if saveErr := bindRetryReceipt(tx, id, receipt); saveErr != nil {
			return codex.SendReceipt{}, &codex.UnknownSendOutcomeError{Err: saveErr}
		}
	}
	return receipt, err
}

func bindRetryReceipt(tx *turnretry.Transaction, id string, receipt codex.SendReceipt) error {
	for i := range tx.State.Snapshots {
		p := &tx.State.Snapshots[i]
		if p.ID == id {
			p.Turn = receipt.TurnID
			if receipt.Steered {
				p.Origin = "steer"
				var err error
				p.Digest, err = p.Hash()
				if err != nil {
					return err
				}
			}
			return tx.Save()
		}
	}
	return errors.New("managed receipt has no admission snapshot")
}

func (c *Client) Queue(ctx context.Context, thread, text, id string) (codex.QueueEntry, error) {
	return c.QueueWithPolicy(ctx, thread, text, id, codex.ThreadPolicy{})
}
func (c *Client) QueueWithPolicy(ctx context.Context, thread, text, id string, policy codex.ThreadPolicy) (codex.QueueEntry, error) {
	tx, current, err := c.retryAdmission(ctx, thread)
	if err != nil {
		return codex.QueueEntry{}, err
	}
	if tx == nil {
		return c.Client.QueueWithPolicy(ctx, thread, text, id, policy)
	}
	defer tx.Close()
	if !reflect.DeepEqual(policy, codex.ThreadPolicy{}) && !reflect.DeepEqual(policy, current) {
		return codex.QueueEntry{}, errors.New("managed queue policy changed")
	}
	if receipt, found, err := c.Client.ReconcileSendWithOptions(ctx, thread, text, id, "", codex.TurnOptions{ThreadPolicy: current}); err != nil || found {
		if err == nil {
			err = recoverRetryReceipt(tx, id, text, receipt)
		}
		return codex.QueueEntry{ID: receipt.QueuedSubmissionID, ClientUserMessageID: id, Text: text}, err
	}
	if err := c.captureRetry(ctx, tx, current, text, id, "queue", codex.TurnOptions{ThreadPolicy: current}); err != nil {
		return codex.QueueEntry{}, err
	}
	if active, err := c.Client.ActiveTurnID(ctx, thread); err == nil && active == "" && c.allowImplicitResume(thread) {
		if err := c.Client.ReconcileThreadInstructionsWithPolicy(ctx, thread, current); err != nil {
			return codex.QueueEntry{}, err
		}
		if err := c.recordRetryBinding(ctx, tx, current); err != nil {
			return codex.QueueEntry{}, err
		}
	}
	return c.Client.QueueWithPolicy(ctx, thread, text, id, current)
}

type retryTransactionKey struct{}

func (c *Client) recordRetryBinding(ctx context.Context, tx *turnretry.Transaction, policy codex.ThreadPolicy) error {
	provider, err := c.retryProvider(ctx, tx.State.Scope)
	if err != nil {
		return err
	}
	epoch, err := session.SocketIdentity(c.socket)
	if err != nil {
		return err
	}
	b := turnretry.Binding{Epoch: tx.State.Epoch, Provider: provider, SocketIdentity: epoch, CommonInstructions: c.retryCommonInstructions, Policy: policy}
	b.Digest, err = b.Hash()
	if err != nil {
		return err
	}
	if old := tx.State.Binding; old != nil && old.Digest != b.Digest {
		tx.State.Cancel("binding_changed")
	}
	tx.State.Binding = &b
	for i := range tx.State.Snapshots {
		p := &tx.State.Snapshots[i]
		if p.Epoch == tx.State.Epoch && p.BindingDigest == "" && p.SocketIdentity == epoch && p.Provider == provider && p.CommonInstructions == b.CommonInstructions && reflect.DeepEqual(p.Options.ThreadPolicy, policy) {
			p.BindingDigest = b.Digest
			p.Digest, err = p.Hash()
			if err != nil {
				return err
			}
		}
	}
	return tx.Save()
}

func (c *Client) ActivateThread(ctx context.Context, thread string, policy codex.ThreadPolicy) error {
	return c.ActivateThreadWithSettings(ctx, thread, codex.ThreadSettings{Policy: policy})
}
func (c *Client) ActivateThreadWithSettings(ctx context.Context, thread string, settings codex.ThreadSettings) error {
	if held, ok := ctx.Value(retryTransactionKey{}).(*turnretry.Transaction); ok && held.State.Scope.Thread == thread {
		if err := c.Client.ActivateThreadWithSettings(ctx, thread, settings); err != nil {
			return err
		}
		return c.recordRetryBinding(ctx, held, settings.Policy)
	}
	tx, policy, err := c.retryAdmission(ctx, thread)
	if err != nil {
		return err
	}
	if tx == nil {
		return c.Client.ActivateThreadWithSettings(ctx, thread, settings)
	}
	defer tx.Close()
	tx.State.Cancel("activated")
	if err := tx.Save(); err != nil {
		return err
	}
	if tx.State.Uncertain() {
		return errors.New("automatic submission requires reconciliation")
	}
	if settings.Policy != (codex.ThreadPolicy{}) {
		policy = settings.Policy
	}
	if err := c.Client.ActivateThreadWithSettings(ctx, thread, settings); err != nil {
		return err
	}
	return c.recordRetryBinding(ctx, tx, policy)
}

func (c *Client) StartThreadWithSettings(ctx context.Context, cwd string, environment map[string]string, settings codex.ThreadSettings) (string, error) {
	id, err := c.Client.StartThreadWithSettings(ctx, cwd, environment, settings)
	if err != nil || c.RetryPolicyResolver == nil || c.RecoveryRoot == "" {
		return id, err
	}
	scope, err := c.RetryScope(ctx, id)
	if err != nil {
		return id, err
	}
	tx, err := c.RetryStore().Lock(ctx, scope)
	if err != nil {
		return id, err
	}
	defer tx.Close()
	tx.State.Enroll(turnretry.Observation{})
	return id, c.recordRetryBinding(ctx, tx, settings.Policy)
}

func (c *Client) ReconcileThreadInstructionsWithPolicy(ctx context.Context, thread string, policy codex.ThreadPolicy) error {
	tx, _, err := c.retryAdmission(ctx, thread)
	if err != nil {
		return err
	}
	if tx == nil {
		return c.Client.ReconcileThreadInstructionsWithPolicy(ctx, thread, policy)
	}
	defer tx.Close()
	if tx.State.Binding != nil && (tx.State.Binding.CommonInstructions != c.retryCommonInstructions || !reflect.DeepEqual(tx.State.Binding.Policy, policy)) {
		tx.State.Cancel("binding_changed")
		if err := tx.Save(); err != nil {
			return err
		}
		if tx.State.Uncertain() {
			return errors.New("automatic submission requires reconciliation")
		}
	}
	if err := c.Client.ReconcileThreadInstructionsWithPolicy(ctx, thread, policy); err != nil {
		return err
	}
	return c.recordRetryBinding(ctx, tx, policy)
}

func (c *Client) EnsureInitialMessage(ctx context.Context, thread, cwd, text string, fresh bool) error {
	return c.EnsureInitialMessageWithOptions(ctx, thread, cwd, text, fresh, codex.TurnOptions{})
}
func (c *Client) EnsureInitialMessageWithOptions(ctx context.Context, thread, cwd, text string, fresh bool, options codex.TurnOptions) error {
	tx, policy, err := c.retryAdmission(ctx, thread)
	if err != nil {
		return err
	}
	if tx == nil {
		return c.Client.EnsureInitialMessageWithOptions(ctx, thread, cwd, text, fresh, options)
	}
	defer tx.Close()
	id := "initial:" + textHash(text)
	if err := c.captureRetry(ctx, tx, policy, text, id, "initial", options); err != nil {
		return err
	}
	return c.Client.EnsureInitialMessageWithOptions(ctx, thread, cwd, text, fresh, options)
}

func (c *Client) cancelRetryAdmission(ctx context.Context, thread, reason string, run func() error) error {
	tx, _, err := c.retryAdmission(ctx, thread)
	if err != nil {
		return err
	}
	if tx == nil {
		return run()
	}
	defer tx.Close()
	if reason == "settings_changed" {
		if err := c.invalidateRetrySettings(ctx, tx); err != nil {
			return err
		}
	} else {
		tx.State.Cancel(reason)
	}
	if err := tx.Save(); err != nil {
		return err
	}
	if tx.State.Uncertain() {
		return errors.New("automatic submission requires reconciliation")
	}
	return run()
}

func (c *Client) invalidateRetrySettings(ctx context.Context, tx *turnretry.Transaction) error {
	current, err := c.Client.ReadTurnState(ctx, tx.State.Scope.Thread)
	if err != nil {
		return err
	}
	return tx.State.InvalidateSettings(current.LatestTurnID)
}
func (c *Client) UpdateThreadSettings(ctx context.Context, thread string, update codex.ThreadSettingsUpdate) (codex.ThreadSettings, error) {
	tx, policy, err := c.retryAdmission(ctx, thread)
	if err != nil {
		return codex.ThreadSettings{}, err
	}
	if tx == nil {
		return c.Client.UpdateThreadSettings(ctx, thread, update)
	}
	defer tx.Close()
	before, err := c.Client.ReadTurnState(ctx, thread)
	if err != nil {
		return codex.ThreadSettings{}, err
	}
	prior := tx.State.Binding
	warm := prior != nil && prior.Epoch == tx.State.Epoch && activeRetrySettingsTurn(before) && c.retrySettingsBindingUnchanged(ctx, tx.State.Scope, prior, policy)
	if err := tx.State.InvalidateSettings(before.LatestTurnID); err != nil {
		return codex.ThreadSettings{}, err
	}
	if err := tx.Save(); err != nil {
		return codex.ThreadSettings{}, err
	}
	if tx.State.Uncertain() {
		return codex.ThreadSettings{}, errors.New("automatic submission requires reconciliation")
	}
	result, err := c.Client.UpdateThreadSettings(ctx, thread, update)
	if err != nil || !warm {
		return result, err
	}
	// The same ongoing turn proves the settings owner's implicit resume kept
	// the warm object, including MCP/instructions/access, under managed admission.
	// A persisted sessionId cannot prove this: it survives native reloads.
	after, err := c.Client.ReadTurnState(ctx, thread)
	if err != nil || !activeRetrySettingsTurn(after) || after.LatestTurnID != before.LatestTurnID {
		return result, nil
	}
	current, err := c.RetryPolicyResolver(ctx, thread)
	if err != nil || !reflect.DeepEqual(current, policy) || !c.retrySettingsBindingUnchanged(ctx, tx.State.Scope, prior, current) {
		return result, nil
	}
	// Promote only the inherited witness. Old admissions and uncertain tuples
	// keep their original epoch/digest; queue admission never repairs authority.
	promoted := *prior
	promoted.Epoch = tx.State.Epoch
	promoted.Digest, err = promoted.Hash()
	if err != nil {
		return result, err
	}
	tx.State.Binding = &promoted
	if err := tx.Save(); err != nil {
		tx.State.Binding = nil
		return result, errors.Join(err, tx.Save())
	}
	return result, nil
}

func activeRetrySettingsTurn(state codex.TurnState) bool {
	return state.Status == "active" && state.LatestTurnID != "" && state.LatestTurnStatus == "inProgress"
}

func (c *Client) retrySettingsBindingUnchanged(ctx context.Context, scope turnretry.Scope, binding *turnretry.Binding, policy codex.ThreadPolicy) bool {
	if binding.CommonInstructions != c.retryCommonInstructions || !reflect.DeepEqual(binding.Policy, policy) || !c.allowImplicitResume(scope.Thread) {
		return false
	}
	identity, err := session.SocketIdentity(scope.Socket)
	if err != nil || identity != binding.SocketIdentity {
		return false
	}
	provider, err := c.retryProvider(ctx, scope)
	return err == nil && provider == binding.Provider
}
func (c *Client) Interrupt(ctx context.Context, thread string) error {
	return c.cancelRetryAdmission(ctx, thread, "interrupted", func() error { return c.Client.Interrupt(ctx, thread) })
}
func (c *Client) StartQueue(ctx context.Context, thread, id string) error {
	return c.StartQueueWithPolicy(ctx, thread, id, codex.ThreadPolicy{})
}
func (c *Client) StartQueueWithPolicy(ctx context.Context, thread, id string, policy codex.ThreadPolicy) error {
	return c.cancelRetryAdmission(ctx, thread, "queue_started", func() error { return c.Client.StartQueueWithPolicy(ctx, thread, id, policy) })
}
