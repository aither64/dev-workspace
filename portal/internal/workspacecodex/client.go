package workspacecodex

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aither64/codex-web/codex"
)

const (
	DefaultNewThreadModel           = "gpt-6-astra"
	DefaultNewThreadReasoningEffort = "xhigh"
	threadSourceKind                = "vscode"
)

var defaultThreadSettings = codex.ThreadSettings{
	Model: DefaultNewThreadModel, ReasoningEffort: DefaultNewThreadReasoningEffort,
}

// Client keeps development-workspace recovery and retirement policy above the
// reusable App Server client.
type Client struct {
	*codex.Client
}

func NewWithOptions(socket, workspace string, options codex.ClientOptions) *Client {
	if workspace != "" {
		options.RuntimeWorkspaceRoots = []string{workspace}
	}
	options.ThreadSourceKinds = []string{threadSourceKind}
	options.NonBlockingUserInput = &codex.NonBlockingUserInputPolicy{
		HiddenGrace: 60 * time.Second, VisibleCountdown: 60 * time.Second,
	}
	return &Client{Client: codex.NewWithOptions(socket, options)}
}

func ResolveNewThreadSettings(
	models []codex.Model, requested codex.ThreadSettings,
) (codex.ThreadSettings, error) {
	return codex.ResolveNewThreadSettings(models, requested, defaultThreadSettings)
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
	return c.RecoverCreatingThreadWithSettingsResolver(
		ctx, threadID, cwd, environment,
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
	candidates := make(map[string]struct{})
	loaded, err := c.LoadedThreadIDs(ctx)
	if err != nil {
		return "", err
	}
	for _, loadedID := range loaded {
		metadata, err := c.ReadThreadMetadata(ctx, loadedID, false)
		if err != nil {
			current, listErr := c.LoadedThreadIDs(ctx)
			if listErr == nil && !slices.Contains(current, loadedID) {
				continue
			}
			return "", err
		}
		if loadedID == threadID && (metadata.Cwd != cwd || !workspaceThread(metadata.Source)) {
			return "", errors.New("recorded creation thread has the wrong identity")
		}
		if metadata.Cwd == cwd && workspaceThread(metadata.Source) {
			candidates[loadedID] = struct{}{}
		}
	}
	listed, next, err := c.ListThreads(ctx, codex.ThreadListOptions{
		Cwd: cwd, SourceKinds: []string{threadSourceKind}, Limit: 2, SortDirection: "asc",
	})
	if err != nil {
		return "", err
	}
	if next != nil {
		return "", fmt.Errorf("multiple Codex threads use creation directory %s; refusing ambiguous recovery", cwd)
	}
	for _, candidate := range listed {
		if candidate.ID == "" || candidate.Cwd != cwd {
			return "", errors.New("thread/list returned an invalid creation candidate")
		}
		candidates[candidate.ID] = struct{}{}
	}
	if len(candidates) > 1 {
		return "", fmt.Errorf("multiple Codex threads use creation directory %s; refusing ambiguous recovery", cwd)
	}
	for candidateID := range candidates {
		materialized, err := c.HistoryMaterialized(ctx, candidateID, cwd)
		if err != nil {
			return "", err
		}
		if candidateID != threadID && materialized {
			return "", errors.New("refusing a different materialized Codex thread as a creation replacement")
		}
		if !materialized {
			return candidateID, nil
		}
		return c.ResumeThread(ctx, candidateID, cwd, environment)
	}
	settings, err := resolveSettings()
	if err != nil {
		return "", err
	}
	return c.StartThreadWithSettings(ctx, cwd, environment, settings)
}

func (c *Client) RecoverForkThread(
	ctx context.Context,
	sourceThreadID, cwd string,
	environment map[string]string,
	settings codex.ThreadSettings,
) (string, error) {
	threads, next, err := c.ListThreads(ctx, codex.ThreadListOptions{
		Cwd: cwd, SourceKinds: []string{threadSourceKind}, Limit: 2, SortDirection: "asc",
	})
	if err != nil {
		return "", err
	}
	if len(threads) > 1 || next != nil {
		return "", fmt.Errorf("multiple Codex threads use fork directory %s; refusing ambiguous recovery", cwd)
	}
	if len(threads) == 0 {
		return c.ForkThread(ctx, sourceThreadID, cwd, environment, settings)
	}
	candidate := threads[0]
	if candidate.ID == "" || candidate.Cwd != cwd || candidate.ForkedFromID != sourceThreadID {
		return "", errors.New("existing Codex thread does not match the requested conversation fork")
	}
	if err := c.RequireThreadTurnsIdle(ctx, candidate.ID); err != nil {
		return "", err
	}
	return c.ResumeThreadWithSettings(ctx, candidate.ID, cwd, environment, settings)
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
	active, activeFound, err := c.retirementCandidate(ctx, cwd, false)
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

func (c *Client) RetireThread(ctx context.Context, threadID, cwd string, force bool) error {
	var candidate codex.ThreadMetadata
	var found bool
	var err error
	if threadID == "" {
		threadID, err = c.ThreadOperationAttempt(cwd)
		if err != nil {
			return err
		}
		if threadID == "" {
			candidate, found, err = c.retirementCandidate(ctx, cwd, false)
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
	candidate, found, err = c.retirementCandidate(ctx, cwd, false)
	if err != nil {
		return err
	}
	if found && candidate.ID != threadID {
		return errors.New("another Codex thread uses the portal session directory")
	}
	if !found {
		_, archivedFound, err := c.retirementThreadByID(ctx, threadID, cwd, true)
		if err != nil {
			return err
		}
		if archivedFound {
			return c.ClearConversationAttempts(threadID, cwd)
		}
		return errors.New("the expected Codex thread is neither active nor archived")
	}
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
	if err := c.ArchiveThread(ctx, threadID); err != nil {
		return err
	}
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
	ctx context.Context, cwd string, archived bool,
) (codex.ThreadMetadata, bool, error) {
	threads, next, err := c.ListThreads(ctx, codex.ThreadListOptions{
		Cwd: cwd, SourceKinds: []string{threadSourceKind}, Archived: &archived,
		Limit: 2, SortDirection: "asc",
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
