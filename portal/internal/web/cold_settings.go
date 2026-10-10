package web

import (
	"context"
	"errors"
	"os"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type coldSettings struct {
	Slug   string `json:"slug"`
	Root   string `json:"root"`
	Thread string `json:"thread"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

func (s *Server) coldSettings(slug, root, thread string) (*coldSettings, error) {
	var record coldSettings
	if err := s.loadThreadRecord("next-settings", thread, &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if record.Slug != slug || record.Root != root || record.Thread != thread || record.Model == "" || record.Effort == "" {
		return nil, errors.New("saved model settings identity changed")
	}
	return &record, nil
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
	if summary.Interactive && !c.server.recoveryHeld(c.slug, thread) {
		return c.Client.UpdateThreadSettings(ctx, thread, update)
	}
	if thread != c.thread || update.CollaborationMode != nil {
		return codex.ThreadSettings{}, errors.New("only model and reasoning settings can be saved before activation")
	}
	if update.Model == nil || update.ReasoningEffort == nil {
		return codex.ThreadSettings{}, errors.New("select model and reasoning effort together")
	}
	settings := codex.ThreadSettings{Model: *update.Model, ReasoningEffort: *update.ReasoningEffort}
	if err := c.server.validateModelSettings(ctx, settings, false); err != nil {
		return codex.ThreadSettings{}, err
	}
	if c.address != "" {
		service, err := c.server.teamService()
		if err != nil {
			return codex.ThreadSettings{}, err
		}
		_, err = service.Store.Update(ctx, c.slug, c.root, false, func(roster *teamruntime.Roster) error {
			for i := range roster.Members {
				member := &roster.Members[i]
				if member.Address == c.address && member.Thread == thread && member.State == "ready" && member.RetireIntent == "" {
					member.Model, member.Effort = settings.Model, settings.ReasoningEffort
					return nil
				}
			}
			return errors.New("team member changed")
		})
		if err != nil {
			return codex.ThreadSettings{}, err
		}
	} else if err := c.server.saveThreadRecord("next-settings", thread, coldSettings{Slug: c.slug, Root: c.root, Thread: thread, Model: settings.Model, Effort: settings.ReasoningEffort}); err != nil {
		return codex.ThreadSettings{}, err
	}
	return settings, nil
}

func (c recoveryConversationClient) ReadThread(ctx context.Context, thread string) (codex.Transcript, error) {
	transcript, err := c.Client.ReadThread(ctx, thread)
	if err != nil || c.address != "" {
		return transcript, err
	}
	record, err := c.server.coldSettings(c.slug, c.root, thread)
	if err == nil && record != nil {
		transcript.Model, transcript.ReasoningEffort = record.Model, record.Effort
	}
	return transcript, err
}
