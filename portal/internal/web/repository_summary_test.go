package web

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/repository"
)

func loadDeferredSummary(t *testing.T, s *Server, query string, history *reviewHistoryResponse) {
	t.Helper()
	if !history.SummaryDeferred || history.Summary != nil || history.SummaryError != "" {
		t.Fatalf("history waited for totals: %#v", history)
	}
	var summary reviewHistorySummary
	decodeReview(t, reviewRequest(t, s, "GET", "/api/sessions/example/repository-summary"+query+"&snapshot="+history.Snapshot, "", 200), &summary)
	history.Summary = &summary
}

func TestRepositoryHistorySummaryMatchesBatchComparisonAndFrozenPages(t *testing.T) {
	s, bare, worktree, _ := reviewWebFixture(t)
	id := repository.ReviewID("project")
	endpoint := "/api/sessions/example/repository-"
	query := "?repository=" + id
	var initial reviewHistoryResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200), &initial)
	loadDeferredSummary(t, s, query, &initial)
	if initial.Summary == nil || initial.Summary.CommitCount != 1 || initial.Summary.Stats != (repository.ReviewStats{Files: 1, Additions: 1, Deletions: 1}) {
		t.Fatalf("summary=%#v", initial.Summary)
	}
	var comparison reviewComparisonResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"comparison"+query+"&review="+initial.Review, "", 200), &comparison)
	if comparison.Stats.PendingFiles != 1 || comparison.Preview != nil {
		t.Fatal("comparison did not retain independent metadata")
	}
	var counted reviewCountsResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"counts"+query+"&snapshot="+comparison.Snapshot, "", 200), &counted)
	if counted.Stats != initial.Summary.Stats || len(counted.Files) != 1 || counted.Files[0].ID != comparison.Files[0].ID || *counted.Files[0].Additions != 1 || *counted.Files[0].Deletions != 1 {
		t.Fatal("overview and comparison counts disagree", counted)
	}
	reviewRequest(t, s, "GET", endpoint+"counts"+query+"&snapshot="+strings.Repeat("f", 32), "", 409)
	var batch struct {
		Repositories []reviewHistoryResponse `json:"repositories"`
	}
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"histories"+query, "", 200), &batch)
	if len(batch.Repositories) != 1 {
		t.Fatal(batch)
	}
	loadDeferredSummary(t, s, query, &batch.Repositories[0])
	if len(batch.Repositories) != 1 || !reflect.DeepEqual(batch.Repositories[0].Summary, initial.Summary) {
		t.Fatalf("batch=%#v", batch)
	}
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("before\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", worktree, "commit", "-am", "undo net line changes")
	var frozen, current reviewHistoryResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"history"+query+"&snapshot="+initial.Snapshot+"&page=1", "", 200), &frozen)
	loadDeferredSummary(t, s, query, &frozen)
	if !reflect.DeepEqual(frozen.Summary, initial.Summary) || len(frozen.History.Commits) != 0 {
		t.Fatal("paging changed snapshot totals")
	}
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200), &current)
	loadDeferredSummary(t, s, query, &current)
	if current.Summary == nil || current.Summary.CommitCount != 2 || current.Summary.Stats != (repository.ReviewStats{}) {
		t.Fatalf("current summary=%#v", current.Summary)
	}
	// Integration retains the same last-viewed comparison and its contribution.
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/remotes/origin/master", current.Pair.Head)
	var integrated reviewHistoryResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200), &integrated)
	loadDeferredSummary(t, s, query, &integrated)
	if !reflect.DeepEqual(integrated.Summary, current.Summary) || !strings.Contains(integrated.Pair.BaseLabel, "Saved") {
		t.Fatalf("integrated=%#v", integrated)
	}
}

func TestRepositorySummaryLimitDoesNotHideHistory(t *testing.T) {
	s, _, worktree, _ := reviewWebFixture(t)
	for i := 0; i < 5001; i++ {
		if err := os.WriteFile(filepath.Join(worktree, fmt.Sprintf("file-%d", i)), []byte("line\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runWebGit(t, "-C", worktree, "add", ".")
	runWebGit(t, "-C", worktree, "commit", "-m", "large comparison")
	query := "?repository=" + repository.ReviewID("project")
	var result reviewHistoryResponse
	decodeReview(t, reviewRequest(t, s, "GET", "/api/sessions/example/repository-history"+query, "", 200), &result)
	if result.Summary != nil || !result.SummaryDeferred || len(result.History.Commits) != 2 {
		t.Fatalf("history was lost or totals misreported: %#v", result)
	}
	reviewRequest(t, s, "GET", "/api/sessions/example/repository-summary"+query+"&snapshot="+result.Snapshot, "", 422)
	var batch struct {
		Repositories []reviewHistoryResponse `json:"repositories"`
	}
	decodeReview(t, reviewRequest(t, s, "GET", "/api/sessions/example/repository-histories"+query, "", 200), &batch)
	if len(batch.Repositories) != 1 || !batch.Repositories[0].SummaryDeferred || len(batch.Repositories[0].History.Commits) != 2 {
		t.Fatalf("batch=%#v", batch)
	}
}

func TestRepositoryOverviewAndFileCountsShareCache(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	defer s.Close()
	query := "?repository=" + repository.ReviewID("project")
	var history reviewHistoryResponse
	decodeReview(t, reviewRequest(t, s, "GET", "/api/sessions/example/repository-history"+query, "", 200), &history)
	loadDeferredSummary(t, s, query, &history)
	// A second consumer must not run Git, even with a deadline too short to start it.
	s.reviews().reader.Timeout = time.Nanosecond
	var counted reviewCountsResponse
	decodeReview(t, reviewRequest(t, s, "GET", "/api/sessions/example/repository-counts"+query+"&snapshot="+history.Snapshot, "", 200), &counted)
	if counted.Stats != history.Summary.Stats {
		t.Fatal(counted)
	}
}
