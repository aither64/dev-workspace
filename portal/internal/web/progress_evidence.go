package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func progressTiming(a codex.ActivitySnapshot) string {
	return fmt.Sprintf("working %.1f minutes; coverage complete=%t, approximate=%t, unclassified %.1f minutes (%s)",
		float64(a.WorkingMS)/60000, a.CoverageComplete, a.TimingApproximate, float64(a.UnclassifiedMS)/60000, boundedProgressText(a.CoverageReason, 100))
}

// Supply exact same-session conversation paths so the observer can inspect user
// steering and member work without copying conversations into the assignment.
func (s *Server) progressConversation(ctx context.Context, cwd, address, thread string) string {
	reader, ok := s.config.Codex.(interface {
		ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
	})
	if !ok {
		return address + ": full conversation unavailable\n"
	}
	metadata, err := reader.ReadThreadMetadata(ctx, thread, true)
	if err != nil || metadata.ID != thread || metadata.Cwd != cwd || metadata.Path == nil || !filepath.IsAbs(*metadata.Path) {
		return address + ": full conversation unavailable\n"
	}
	info, err := os.Stat(*metadata.Path)
	if err != nil || !info.Mode().IsRegular() {
		return address + ": full conversation unavailable\n"
	}
	return fmt.Sprintf("%s full conversation (read-only): %q\n", address, *metadata.Path)
}

// Context is a source map, not a copy of session content. The observer decides
// which records and diffs matter; missing optional metadata is not a verdict.
func (s *Server) progressContext(ctx context.Context, summary *session.Summary, roster *teamruntime.Roster) string {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var context strings.Builder
	cwd := filepath.Join(s.config.Workspace, "work", summary.Slug)
	fmt.Fprintf(&context, "Session files: %q (plan.md, state.md, portal.yml). Worktrees: %q.\n", cwd, filepath.Join(s.config.Workspace, "worktrees", summary.Slug))
	context.WriteString("Working minutes exclude idle/waits; member clocks overlap. Coverage gaps are lower bounds, not session age.\n")
	members := []teamruntime.Member{{Address: "lead", Thread: summary.Codex.ThreadID}}
	for _, member := range roster.Members {
		if member.Role != "observer" && member.State == "ready" && member.RetireIntent == "" {
			members = append(members, member)
		}
	}
	for _, member := range members {
		path := s.progressConversation(ctx, cwd, member.Address, member.Thread)
		context.WriteString(path)
		if s.activity != nil {
			if a, err := s.activity.ReadActivity(ctx, member.Thread); err == nil && !a.Stale {
				fmt.Fprintf(&context, "%s: %s\n", member.Address, progressTiming(a))
				continue
			}
		}
		fmt.Fprintf(&context, "%s: timing unavailable\n", member.Address)
	}
	return context.String()
}
