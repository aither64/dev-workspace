package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/progressobserver"
	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func progressTiming(a codex.ActivitySnapshot) string {
	return fmt.Sprintf("recorded working %.1f minutes; coverage complete=%t, approximate=%t, unclassified %.1f minutes, reason=%q. Idle and waits excluded; overlapping member clocks must not be summed.",
		float64(a.WorkingMS)/60000, a.CoverageComplete, a.TimingApproximate, float64(a.UnclassifiedMS)/60000, a.CoverageReason)
}

// Supply exact same-session conversation paths so the observer can inspect user
// steering and member work beyond the bounded excerpts, without another service
// or a copy of potentially very large conversations in progress state.
func (s *Server) progressConversation(ctx context.Context, cwd, address, thread string) (string, bool) {
	reader, ok := s.config.Codex.(interface {
		ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
	})
	if !ok {
		return address + ": full conversation unavailable\n", false
	}
	metadata, err := reader.ReadThreadMetadata(ctx, thread, true)
	if err != nil || metadata.ID != thread || metadata.Cwd != cwd || metadata.Path == nil || !filepath.IsAbs(*metadata.Path) {
		return address + ": full conversation unavailable\n", false
	}
	info, err := os.Stat(*metadata.Path)
	if err != nil || !info.Mode().IsRegular() {
		return address + ": full conversation unavailable\n", false
	}
	return fmt.Sprintf("%s full conversation (read-only): %q\n", address, *metadata.Path), true
}

func progressUserSteering(t codex.Transcript) string {
	var requests []string
	for _, entry := range t.Entries {
		if entry.Kind != "userMessage" || entry.Communication != nil || strings.HasPrefix(entry.Text, "Team assignment from ") {
			continue
		}
		requests = append(requests, entry.Timestamp+" "+entry.Text)
	}
	// Prefer latest steering; the original request is available in the full log.
	if len(requests) > 6 {
		requests = requests[len(requests)-6:]
	}
	return boundedProgressText(strings.Join(requests, "\n\n"), 5000)
}

func (s *Server) progressEvidence(ctx context.Context, summary *session.Summary, roster *teamruntime.Roster, state *progressobserver.State, lead codex.Transcript) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var evidence, totals strings.Builder
	complete := true
	cwd := filepath.Join(s.config.Workspace, "work", summary.Slug)
	fmt.Fprintf(&evidence, "Recent actual user messages (excerpt; inspect the full lead conversation for the original request and subsequent decisions):\n%s\n", progressUserSteering(lead))
	members := []teamruntime.Member{{Address: "lead", Thread: summary.Codex.ThreadID}}
	for _, member := range roster.Members {
		if member.Role != "observer" && member.State == "ready" && member.RetireIntent == "" {
			members = append(members, member)
		}
	}
	for _, member := range members {
		path, ok := s.progressConversation(ctx, cwd, member.Address, member.Thread)
		evidence.WriteString(path)
		complete = complete && ok
		if s.activity == nil {
			fmt.Fprintf(&evidence, "%s timing unavailable\n", member.Address)
			complete = false
			continue
		}
		a, err := s.activity.ReadActivity(ctx, member.Thread)
		if err != nil || a.Stale {
			fmt.Fprintf(&evidence, "%s timing unavailable\n", member.Address)
			complete = false
			continue
		}
		fmt.Fprintf(&evidence, "%s %s\n", member.Address, progressTiming(a))
	}

	reader := repository.ReviewReader{Workspace: s.config.Workspace}
	// Include discovered same-session worktrees, as the Repositories tab does.
	repositories, err := s.activeRepositories(ctx, summary)
	if err != nil {
		evidence.WriteString("Repository inventory incomplete; inspect this session's portal.yml and worktrees.\n")
		complete = false
	}
	if len(repositories) > 12 {
		repositories = repositories[:12]
		evidence.WriteString("Repository summary limited to 12 repositories.\n")
		complete = false
	}
	for _, item := range repositories {
		repo, err := reader.Resolve(ctx, summary.Slug, item, false)
		if err != nil {
			fmt.Fprintf(&evidence, "%s diffstat unavailable: %v\n", item.Name, err)
			complete = false
			continue
		}
		saved, err := s.reviews().loadComparison(summary.Slug, repo.ID, repo.Head)
		if err != nil {
			complete = false
		}
		pair, err := reader.Pair(ctx, repo, saved)
		if err != nil {
			fmt.Fprintf(&evidence, "%s comparison unavailable: %v\n", item.Name, err)
			complete = false
			continue
		}
		fmt.Fprintf(&evidence, "Repository %s at %q; base %s; head %s; %s %s\n", item.Name, repo.Worktree, pair.Base, pair.Head, pair.BaseLabel, pair.Warning)
		committed, committedErr := reader.Files(ctx, repo, pair)
		current, currentErr := reader.ProgressFiles(ctx, repo, pair.Base)
		if committedErr != nil || currentErr != nil {
			fmt.Fprintf(&evidence, "Diffstat unavailable: committed=%v current=%v\n", committedErr, currentErr)
			complete = false
			continue
		}
		stats := repository.FileStats(current)
		fmt.Fprintf(&evidence, "Committed: %+v\nCurrent net (base through working tree, includes untracked): %+v\n", repository.FileStats(committed), stats)
		fmt.Fprintf(&totals, "%s base=%s current=%+v\n", item.Name, pair.Base, stats)
		if stats.LineCountsIncomplete {
			complete = false
		}
		groups := map[string][]repository.ReviewFile{}
		for _, file := range current {
			category := repository.ProgressCategory(file.Path)
			groups[category] = append(groups[category], file)
		}
		for _, category := range []string{"implementation/other", "documentation", "lock/dependency", "possibly generated"} {
			if files := groups[category]; len(files) != 0 {
				fmt.Fprintf(&evidence, "Category hint %s: %+v\n", category, repository.FileStats(files))
			}
		}
		sort.SliceStable(current, func(i, j int) bool {
			return repository.FileStats(current[i:i+1]).Additions > repository.FileStats(current[j:j+1]).Additions
		})
		for i, file := range current {
			if i == 20 {
				fmt.Fprintf(&evidence, "%d more paths; inspect git diff --stat/--numstat and untracked files in this worktree.\n", len(current)-i)
				break
			}
			fmt.Fprintf(&evidence, "  %q (%s, %s): %+v\n", file.Path, file.Status, repository.ProgressCategory(file.Path), repository.FileStats([]repository.ReviewFile{file}))
		}
	}
	if state.PreviousEvidence != "" {
		fmt.Fprintf(&evidence, "Previous check's net totals (compare growth only when bases agree):\n%s\n", state.PreviousEvidence)
	}
	state.PreviousEvidence = boundedProgressText(totals.String(), 3500)
	if evidence.Len() > 16000 {
		complete = false
	}
	return boundedProgressText(evidence.String(), 16000), complete
}
