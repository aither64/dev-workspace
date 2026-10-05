package workspacecodex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/creationprogress"
)

const threadSourceKind = "vscode"

// Client keeps development-workspace recovery and retirement policy above the
// reusable App Server client.
type Client struct {
	*codex.Client
	CodexHome    string
	archiveProof func(context.Context, string, string, string) (ArchiveState, error)
}

func NewWithOptions(socket, workspace string, options codex.ClientOptions) *Client {
	if workspace != "" {
		options.RuntimeWorkspaceRoots = []string{workspace}
	}
	options.ThreadSourceKinds = []string{threadSourceKind}
	options.NonBlockingUserInput = &codex.NonBlockingUserInputPolicy{
		HiddenGrace: 60 * time.Second, VisibleCountdown: 60 * time.Second,
	}
	return &Client{Client: codex.NewWithOptions(socket, options), CodexHome: os.Getenv("DEV_WORKSPACE_CODEX_HOME")}
}

func ResolveNewThreadSettings(
	models []codex.Model, requested codex.ThreadSettings,
) (codex.ThreadSettings, error) {
	if requested.Model == "" {
		// The App Server resolves any omitted model and effort from its live
		// configuration. An effort-only request is also server-owned because this
		// layer does not know which configured default model will receive it.
		return requested, nil
	}

	var selected *codex.Model
	for index := range models {
		candidate := &models[index]
		if candidate.Model != requested.Model {
			continue
		}
		if selected != nil {
			return codex.ThreadSettings{}, fmt.Errorf(
				"Codex model catalog has more than one %q model", requested.Model,
			)
		}
		selected = candidate
	}
	if selected == nil {
		return codex.ThreadSettings{}, fmt.Errorf("Codex model %q is not available", requested.Model)
	}
	if requested.ReasoningEffort == "" {
		return requested, nil
	}
	for _, effort := range selected.SupportedReasoningEfforts {
		if effort.ReasoningEffort == requested.ReasoningEffort {
			return requested, nil
		}
	}
	return codex.ThreadSettings{}, fmt.Errorf(
		"reasoning effort %q is not available for %s", requested.ReasoningEffort, selected.DisplayName,
	)
}

func (c *Client) RecoverCreatingThread(
	ctx context.Context, threadID, cwd string, environment map[string]string,
) (string, error) {
	return c.RecoverCreatingThreadWithSettings(ctx, threadID, cwd, environment, codex.ThreadSettings{})
}

func (c *Client) RecoverCreatingThreadWithSettings(
	ctx context.Context,
	threadID, cwd string,
	environment map[string]string,
	settings codex.ThreadSettings,
) (string, error) {
	return c.RecoverCreatingThreadWithPolicyAndSettingsResolver(
		ctx, threadID, cwd, environment, settings.Policy,
		func() (codex.ThreadSettings, error) { return settings, nil },
	)
}

// RecoverCreatingThreadWithSettingsResolver keeps creation retries bound to
// one development-session directory. Existing threads retain their original
// settings; defaults are resolved only when a replacement must be created.
func (c *Client) RecoverCreatingThreadWithSettingsResolver(
	ctx context.Context,
	threadID, cwd string,
	environment map[string]string,
	resolveSettings func() (codex.ThreadSettings, error),
) (string, error) {
	return c.RecoverCreatingThreadWithPolicyAndSettingsResolver(
		ctx, threadID, cwd, environment, codex.ThreadPolicy{}, resolveSettings,
	)
}

// RecoverCreatingThreadWithPolicyAndSettingsResolver retains an unmaterialized
// candidate's start-time policy and refreshes a materialized candidate's policy
// without replacing its saved model or effort. Only a new root calls the resolver.
func (c *Client) RecoverCreatingThreadWithPolicyAndSettingsResolver(
	ctx context.Context,
	threadID, cwd string,
	environment map[string]string,
	policy codex.ThreadPolicy,
	resolveSettings func() (codex.ThreadSettings, error),
) (string, error) {
	return c.RecoverCreatingThreadWithOptions(ctx, threadID, cwd, environment, policy, resolveSettings, RecoveryOptions{})
}

// RecoveryOptions contains only application-validated roster identities. The
// composition layer holds the team operation lock until discovery has finished.
type RecoveryOptions struct {
	ExcludedThreads map[string]string // exact thread ID -> retained project ID (empty for legacy members)
	RetainedRoster  bool
	BeforeCreate    func() // releases the roster operation lock before any new thread
	Progress        creationprogress.Observer
}

func (c *Client) RecoverCreatingThreadWithOptions(
	ctx context.Context, threadID, cwd string, environment map[string]string,
	policy codex.ThreadPolicy, resolveSettings func() (codex.ThreadSettings, error), options RecoveryOptions,
) (string, error) {
	candidates, err := c.creationCandidates(ctx, threadID, cwd, options)
	if err != nil {
		return "", err
	}
	for candidateID, candidate := range candidates {
		if candidateID != threadID && options.RetainedRoster {
			return "", errors.New("retained team roster prevents root thread replacement")
		}
		if candidate.ForkedFromID != "" {
			return "", errors.New("refusing a fork as a creation replacement")
		}
		materialized, err := c.HistoryMaterialized(ctx, candidateID, cwd)
		if err != nil {
			return "", err
		}
		if candidateID != threadID && materialized {
			return "", errors.New("refusing a different materialized Codex thread as a creation replacement")
		}
		if candidateID != threadID {
			if err := c.requireMissingCreationThread(ctx, threadID, cwd); err != nil {
				return "", err
			}
		}
		if !materialized {
			// Selected Codex requires a rollout even to resume a loaded thread.
			// This proven fresh root still has its original policy and environment.
			return candidateID, nil
		}
		return c.ResumeThreadWithSettings(ctx, candidateID, cwd, environment, codex.ThreadSettings{Policy: policy})
	}
	if options.RetainedRoster {
		return "", errors.New("retained team roster prevents root thread replacement")
	}
	if err := c.requireMissingCreationThread(ctx, threadID, cwd); err != nil {
		return "", err
	}
	settings, err := resolveSettings()
	if err != nil {
		return "", err
	}
	if options.BeforeCreate != nil {
		options.BeforeCreate()
	}
	return c.StartThreadWithSettings(ctx, cwd, environment, settings)
}

func (c *Client) requireMissingCreationThread(ctx context.Context, threadID, cwd string) error {
	if threadID == "" {
		return nil
	}
	// Filtered discovery cannot establish that a recorded ID with changed
	// cwd/source is gone. Only an exact not-found result permits replacement.
	metadata, err := c.ReadThreadMetadata(ctx, threadID, false)
	if err == nil {
		if metadata.Cwd != cwd || !workspaceThread(metadata.Source) {
			return errors.New("recorded creation thread has the wrong identity")
		}
		return errors.New("recorded creation thread is absent from complete discovery")
	}
	if !codex.IsThreadNotFound(err, threadID) {
		return err
	}
	return nil
}

func (c *Client) RecoverForkThread(
	ctx context.Context, sourceThreadID, cwd string, environment map[string]string, settings codex.ThreadSettings,
) (string, error) {
	return c.RecoverForkThreadWithOptions(ctx, sourceThreadID, cwd, environment, settings, RecoveryOptions{})
}

func (c *Client) RecoverForkThreadWithOptions(
	ctx context.Context, sourceThreadID, cwd string, environment map[string]string, settings codex.ThreadSettings, options RecoveryOptions,
) (string, error) {
	candidates, err := c.creationCandidates(ctx, "", cwd, options)
	if err != nil {
		return "", err
	}
	for _, candidate := range candidates {
		if candidate.ForkedFromID != sourceThreadID {
			return "", errors.New("existing Codex thread does not match the requested conversation fork")
		}
		if err := c.RequireThreadTurnsIdle(ctx, candidate.ID); err != nil {
			return "", err
		}
		return c.ResumeThreadWithSettings(ctx, candidate.ID, cwd, environment, settings)
	}
	if options.RetainedRoster {
		return "", errors.New("retained team roster prevents root thread replacement")
	}
	if options.BeforeCreate != nil {
		options.BeforeCreate()
	}
	return c.ForkThread(ctx, sourceThreadID, cwd, environment, settings)
}

func recoveryIdentityEqual(left, right codex.ThreadMetadata) bool {
	project := func(value *string) string {
		if value == nil {
			return ""
		}
		return *value
	}
	return left.ID == right.ID && left.Cwd == right.Cwd &&
		workspaceThread(left.Source) == workspaceThread(right.Source) &&
		left.ForkedFromID == right.ForkedFromID && project(left.ProjectID) == project(right.ProjectID)
}

func (c *Client) creationCandidates(ctx context.Context, recordedID, cwd string, options RecoveryOptions) (map[string]codex.ThreadMetadata, error) {
	candidates := map[string]codex.ThreadMetadata{}
	var refusal error
	ambiguous := func() error {
		return fmt.Errorf("multiple Codex threads use creation directory %s; refusing ambiguous recovery", cwd)
	}
	merge := func(candidate codex.ThreadMetadata) error {
		if candidate.ID == "" {
			return errors.New("thread discovery returned an empty identity")
		}
		if project, excluded := options.ExcludedThreads[candidate.ID]; excluded {
			if candidate.Cwd != cwd || (project != "" && (candidate.ProjectID == nil || *candidate.ProjectID != project)) {
				refusal = errors.New("retained member thread has the wrong cwd or project identity")
				return refusal
			}
			return nil
		}
		if candidate.Cwd != cwd || !workspaceThread(candidate.Source) {
			return errors.New("thread discovery returned an invalid creation candidate")
		}
		if previous, exists := candidates[candidate.ID]; exists && !recoveryIdentityEqual(previous, candidate) {
			refusal = errors.New("thread discovery returned contradictory creation identities")
			return refusal
		}
		candidates[candidate.ID] = candidate
		if len(candidates) > 1 {
			return ambiguous()
		}
		return nil
	}
	finish := creationprogress.Begin(options.Progress, "recovery_loaded", "")
	loaded, err := c.LoadedThreadIDs(ctx)
	if err != nil {
		return nil, err
	}
	for _, loadedID := range loaded {
		metadata, err := c.ReadThreadMetadata(ctx, loadedID, false)
		if err != nil {
			current, listErr := c.LoadedThreadIDs(ctx)
			if listErr == nil && !slices.Contains(current, loadedID) {
				continue
			}
			return nil, err
		}
		if loadedID == recordedID && (metadata.Cwd != cwd || !workspaceThread(metadata.Source)) {
			return nil, errors.New("recorded creation thread has the wrong identity")
		}
		_, member := options.ExcludedThreads[loadedID]
		if member || (metadata.Cwd == cwd && workspaceThread(metadata.Source)) {
			if err := merge(metadata); err != nil {
				return nil, err
			}
		}
	}
	finish()
	scan := func(indexed bool) error {
		stage := "recovery_scan"
		if indexed {
			stage = "recovery_index"
		}
		finish := creationprogress.Begin(options.Progress, stage, "")
		seenCursors := map[string]bool{}
		seenRows := map[string]codex.ThreadMetadata{}
		cursor := ""
		rows := 0
		archived := false
		for page := 0; page < 64; page++ {
			listed, next, err := c.ListThreads(ctx, codex.ThreadListOptions{
				Cwd: cwd, SourceKinds: []string{threadSourceKind}, Archived: &archived,
				Limit: 100, SortDirection: "asc", Cursor: cursor, UseStateDBOnly: indexed,
			})
			if err != nil {
				return err
			}
			rows += len(listed)
			if rows > 4096 {
				return errors.New("thread discovery exceeded its row bound")
			}
			for _, row := range listed {
				if row.ID == "" || row.Cwd != cwd || !workspaceThread(row.Source) {
					return errors.New("thread/list returned an invalid creation candidate")
				}
				if previous, exists := seenRows[row.ID]; exists {
					if !recoveryIdentityEqual(previous, row) {
						return errors.New("thread/list returned contradictory creation rows")
					}
					continue
				}
				seenRows[row.ID] = row
				candidate := row
				if indexed {
					candidate, err = c.ReadThreadMetadata(ctx, row.ID, false)
					if err != nil {
						return err
					}
					if candidate.ID == recordedID && (candidate.Cwd != cwd || !workspaceThread(candidate.Source)) {
						refusal = errors.New("recorded creation thread has the wrong identity")
						return refusal
					}
					if project, member := options.ExcludedThreads[candidate.ID]; member && (candidate.Cwd != cwd || (project != "" && (candidate.ProjectID == nil || *candidate.ProjectID != project))) {
						refusal = errors.New("retained member thread has the wrong cwd or project identity")
						return refusal
					}
					if !recoveryIdentityEqual(row, candidate) {
						return errors.New("thread index returned stale creation identity")
					}
				}
				if err := merge(candidate); err != nil {
					return err
				}
			}
			if next == nil || *next == "" {
				finish()
				return nil
			}
			if seenCursors[*next] {
				return errors.New("thread/list repeated a creation cursor")
			}
			seenCursors[*next] = true
			cursor = *next
		}
		return errors.New("thread discovery exceeded its page bound")
	}
	// The index can prove ambiguity early, but even a positive page cannot prove
	// uniqueness. Every adoption or replacement still requires complete discovery.
	_ = scan(true)
	if refusal != nil {
		return nil, refusal
	}
	if len(candidates) > 1 {
		return nil, ambiguous()
	}
	if err := scan(false); err != nil {
		return nil, err
	}
	return candidates, nil
}

func (c *Client) ResolveForkSettings(
	ctx context.Context, sourceThreadID string, requested codex.ThreadSettings,
) (codex.ThreadSettings, error) {
	if sourceThreadID == "" {
		return codex.ThreadSettings{}, errors.New("fork settings require a source thread")
	}
	source, err := c.ReadThreadSettings(ctx, sourceThreadID)
	if err != nil {
		return codex.ThreadSettings{}, fmt.Errorf("read source Codex settings: %w", err)
	}
	models, err := c.ListModels(ctx)
	if err != nil {
		return codex.ThreadSettings{}, fmt.Errorf("load Codex models: %w", err)
	}
	return codex.ResolveForkThreadSettings(models, source, requested)
}

func (c *Client) RecoverArchivedThread(
	ctx context.Context, threadID, cwd string, environment map[string]string,
) (string, error) {
	if threadID == "" || cwd == "" {
		return "", errors.New("archived thread recovery requires a thread id and working directory")
	}
	active, activeFound, err := c.retirementCandidate(ctx, cwd, false, false)
	if err != nil {
		return "", err
	}
	_, archivedFound, err := c.retirementThreadByID(ctx, threadID, cwd, true)
	if err != nil {
		return "", err
	}
	if activeFound && active.ID != threadID {
		return "", errors.New("another active Codex thread uses the revived session directory")
	}
	if activeFound && archivedFound {
		return "", errors.New("the revived Codex thread exists in both active and archived history")
	}
	if activeFound {
		return c.ResumeThread(ctx, threadID, cwd, environment)
	}
	if !archivedFound {
		return "", errors.New("the revived Codex thread is neither active nor archived")
	}
	thread, err := c.UnarchiveThread(ctx, threadID)
	if err != nil {
		return "", err
	}
	if thread.Cwd != cwd || !workspaceThread(thread.Source) {
		return "", errors.New("thread/unarchive returned the wrong Codex thread identity")
	}
	return c.ResumeThread(ctx, threadID, cwd, environment)
}

func (c *Client) RetireThread(ctx context.Context, threadID, cwd string, force bool) (retireErr error) {
	stage := "find session conversation"
	defer func() {
		if retireErr != nil {
			retireErr = fmt.Errorf("%s: %w", stage, retireErr)
		}
	}()
	var candidate codex.ThreadMetadata
	var found bool
	var err error
	if threadID == "" {
		threadID, err = c.ThreadOperationAttempt(cwd)
		if err != nil {
			return err
		}
		if threadID == "" {
			candidate, found, err = c.retirementCandidate(ctx, cwd, false, false)
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
			threadID = candidate.ID
			if err := c.RecordThreadOperationAttempt(cwd, threadID); err != nil {
				return fmt.Errorf("record Codex thread retirement before archival: %w", err)
			}
		}
	}
	stage = "prove session conversation archive state"
	archiveState, err := c.ProveArchivedRootThread(ctx, threadID, cwd)
	if err != nil {
		return err
	}
	stage = "find session conversation"
	candidate, found, err = c.retirementCandidate(ctx, cwd, false, true)
	if err != nil {
		return err
	}
	if archiveState == ArchiveArchived && found {
		return errors.New("archived Codex thread also appears in active discovery")
	}
	if found && candidate.ID != threadID {
		return errors.New("another Codex thread uses the portal session directory")
	}
	if archiveState == ArchiveArchived {
		stage = "clear retired conversation attempts"
		return c.ClearConversationAttempts(threadID, cwd)
	}
	if !found {
		return errors.New("the expected Codex thread is absent from active discovery")
	}
	stage = "verify session conversation"
	metadata, err := c.ReadThreadMetadata(ctx, threadID, true)
	if err != nil {
		return err
	}
	if metadata.Cwd != cwd || !workspaceThread(metadata.Source) {
		return errors.New("Codex thread does not match the portal session being removed")
	}
	materialized, err := c.HistoryMaterialized(ctx, threadID, cwd)
	if err != nil {
		return err
	}
	stage = "interrupt session conversation"
	if force && materialized {
		turnID, err := c.ActiveTurnID(ctx, threadID)
		if err != nil {
			return err
		}
		if turnID != "" {
			if err := c.Interrupt(ctx, threadID); err != nil {
				return err
			}
		}
	}
	stage = "verify conversation is idle"
	for materialized {
		err := c.RequireThreadTurnsIdle(ctx, threadID)
		if err == nil {
			break
		}
		if !force {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for interrupted Codex thread: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	stage = "archive session conversation"
	if err := c.ArchiveThread(ctx, threadID); err != nil {
		return err
	}
	stage = "clear retired conversation attempts"
	return c.ClearConversationAttempts(threadID, cwd)
}

type ThreadActivity struct {
	ID        string
	Cwd       string
	UpdatedAt time.Time
}

func (c *Client) ListThreadActivity(
	ctx context.Context, expected []ThreadActivity,
) ([]ThreadActivity, error) {
	activities := make([]ThreadActivity, 0, len(expected))
	for _, identity := range expected {
		thread, err := c.ReadThreadMetadata(ctx, identity.ID, true)
		if err != nil {
			return nil, err
		}
		if thread.Cwd != identity.Cwd || thread.UpdatedAt < 0 || !workspaceThread(thread.Source) {
			return nil, errors.New("thread/read returned invalid activity metadata")
		}
		activities = append(activities, ThreadActivity{
			ID: thread.ID, Cwd: thread.Cwd, UpdatedAt: time.Unix(thread.UpdatedAt, 0),
		})
	}
	return activities, nil
}

func (c *Client) RequireThreadMaterialized(ctx context.Context, threadID, cwd string) error {
	materialized, err := c.HistoryMaterialized(ctx, threadID, cwd)
	if err != nil {
		return err
	}
	if !materialized {
		return errors.New("recorded Codex thread has no persisted history; archive the session and start a new one")
	}
	return nil
}

func (c *Client) retirementCandidate(
	ctx context.Context, cwd string, archived, indexed bool,
) (codex.ThreadMetadata, bool, error) {
	threads, next, err := c.ListThreads(ctx, codex.ThreadListOptions{
		Cwd: cwd, SourceKinds: []string{threadSourceKind}, Archived: &archived,
		Limit: 2, SortDirection: "asc", UseStateDBOnly: indexed,
	})
	if err != nil {
		return codex.ThreadMetadata{}, false, err
	}
	if len(threads) > 1 || next != nil {
		return codex.ThreadMetadata{}, false,
			fmt.Errorf("multiple Codex threads use %s; refusing ambiguous retirement", cwd)
	}
	if len(threads) == 0 {
		return codex.ThreadMetadata{}, false, nil
	}
	candidate := threads[0]
	if candidate.ID == "" || candidate.Cwd != cwd || !workspaceThread(candidate.Source) {
		return codex.ThreadMetadata{}, false, errors.New("thread/list returned an invalid retirement candidate")
	}
	return candidate, true, nil
}

func (c *Client) retirementThreadByID(
	ctx context.Context, threadID, cwd string, archived bool,
) (codex.ThreadMetadata, bool, error) {
	seenCursors := make(map[string]struct{})
	var cursor string
	for {
		threads, next, err := c.ListThreads(ctx, codex.ThreadListOptions{
			Cwd: cwd, SourceKinds: []string{threadSourceKind}, Archived: &archived,
			Limit: 100, SortDirection: "asc", Cursor: cursor,
		})
		if err != nil {
			return codex.ThreadMetadata{}, false, err
		}
		for _, candidate := range threads {
			if candidate.ID != threadID {
				continue
			}
			if candidate.Cwd != cwd || !workspaceThread(candidate.Source) {
				return codex.ThreadMetadata{}, false,
					errors.New("thread/list returned invalid metadata for the expected retirement thread")
			}
			return candidate, true, nil
		}
		if next == nil || *next == "" {
			return codex.ThreadMetadata{}, false, nil
		}
		cursor = *next
		if _, duplicate := seenCursors[cursor]; duplicate {
			return codex.ThreadMetadata{}, false, errors.New("thread/list repeated a retirement cursor")
		}
		seenCursors[cursor] = struct{}{}
	}
}

func workspaceThread(source any) bool {
	value, ok := source.(string)
	return ok && value == threadSourceKind
}
