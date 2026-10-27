package web

import (
	"context"
	"errors"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
)

type coldSettings = userstate.DeferredSettings

func (s *Server) coldSettings(slug, root, thread string) (*coldSettings, error) {
	return userstate.ReadDeferredSettings(s.operationStore.directory, slug, root, thread)
}

func (c recoveryConversationClient) UpdateThreadSettings(ctx context.Context, thread string, update codex.ThreadSettingsUpdate) (codex.ThreadSettings, error) {
	// Resolution precedes the conversation mutation lock. Activation may have
	// finished while this request waited, so choose the save path again here.
	summary, err := session.Find(c.server.config.Workspace, c.slug)
	if err != nil {
		return codex.ThreadSettings{}, err
	}
	c.server.normalizeInteractivity(ctx, summary)
	if summary.Codex.ThreadID != c.root {
		return codex.ThreadSettings{}, errors.New("session thread changed")
	}
	if thread != c.thread {
		return codex.ThreadSettings{}, errors.New("saved settings thread changed")
	}
	record, err := c.server.coldSettings(c.slug, c.root, thread)
	if err != nil {
		return codex.ThreadSettings{}, err
	}
	if record == nil {
		record = &coldSettings{Slug: c.slug, Root: c.root, Thread: thread}
	}
	if record.RequestID != "" {
		return codex.ThreadSettings{}, errors.New("Recover the pending original input before changing its settings.")
	}
	if summary.Interactive && !c.server.recoveryHeld(c.slug, thread) {
		return c.Client.UpdateThreadSettings(ctx, thread, update)
	}
	settings := codex.ThreadSettings{Model: record.Model, ReasoningEffort: record.Effort, CollaborationMode: record.Mode}
	if (update.Model == nil) != (update.ReasoningEffort == nil) {
		return codex.ThreadSettings{}, errors.New("select model and reasoning effort together")
	}
	if update.Model != nil {
		settings.Model, settings.ReasoningEffort = *update.Model, *update.ReasoningEffort
		if err := c.server.validateModelSettings(ctx, settings, false, c.slug); err != nil {
			return codex.ThreadSettings{}, err
		}
	}
	if update.CollaborationMode != nil {
		if err := c.server.validateCollaborationMode(ctx, *update.CollaborationMode); err != nil {
			return codex.ThreadSettings{}, err
		}
		settings.CollaborationMode = *update.CollaborationMode
	}
	if c.address != "" {
		service, err := c.server.teamService(c.slug)
		if err != nil {
			return codex.ThreadSettings{}, err
		}
		member, err := readyConversationMember(service.Store, c.slug, c.root, c.address)
		if err != nil || member.Thread != thread {
			return codex.ThreadSettings{}, errors.New("team member changed")
		}
		if update.Model != nil {
			_, err = service.Store.Update(ctx, c.slug, c.root, false, func(roster *teamruntime.Roster) error {
				for i := range roster.Members {
					current := &roster.Members[i]
					if current.Address == c.address && current.Thread == thread && current.State == "ready" && current.RetireIntent == "" {
						current.Model, current.Effort = settings.Model, settings.ReasoningEffort
						return nil
					}
				}
				return errors.New("team member changed")
			})
			if err != nil {
				return codex.ThreadSettings{}, err
			}
		} else {
			settings.Model, settings.ReasoningEffort = member.Model, member.Effort
		}
	}
	record.Model, record.Effort, record.Mode = settings.Model, settings.ReasoningEffort, settings.CollaborationMode
	if err := c.server.saveThreadRecord("next-settings", thread, record); err != nil {
		return codex.ThreadSettings{}, err
	}

	return settings, nil
}

func (c recoveryConversationClient) ReadThread(ctx context.Context, thread string) (codex.Transcript, error) {
	transcript, err := c.Client.ReadThread(ctx, thread)
	if err != nil {
		return transcript, err
	}
	record, err := c.server.coldSettings(c.slug, c.root, thread)
	if err == nil && record != nil {
		if record.Model != "" {
			transcript.Model, transcript.ReasoningEffort = record.Model, record.Effort
		}
		if record.Mode != "" {
			transcript.CollaborationMode = record.Mode
		}
	}
	return transcript, err
}
