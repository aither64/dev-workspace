package web

import (
	"context"
	"errors"

	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

// Stop intent is saved before the normal resolver waits for package exclusion.
// This records intent only: native interruption retains ordinary admission.
func (s *Server) recordMaintenanceStop(ctx context.Context, slug, address string) error {
	summary, e := session.Find(s.config.Workspace, slug)
	if e != nil {
		return e
	}
	thread := summary.Codex.ThreadID
	if address != "" {
		team, e := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
		if e != nil {
			return e
		}
		roster, e := team.Load(slug, thread)
		if e != nil {
			return e
		}
		thread = ""
		for _, member := range roster.Members {
			if member.Address == address && member.State == "ready" && member.RetireIntent == "" {
				thread = member.Thread
			}
		}
		if thread == "" {
			return errors.New("maintenance stop member changed")
		}
	}
	return maintenance.SuppressActive(ctx, s.config.UserStateRoot, s.config.HostProfile, s.config.Workspace, thread)
}

func (s *Server) requireMaintenanceAdmission() error {
	return maintenance.RequireAdmission(s.config.UserStateRoot, s.config.HostProfile, s.config.Workspace)
}
