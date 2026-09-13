package web

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/repository"
)

func TestRepositoryHistorySummaryMatchesBatchComparisonAndFrozenPages(t *testing.T) {
	s, bare, worktree, _ := reviewWebFixture(t)
	id := repository.ReviewID("project")
	endpoint := "/api/sessions/example/repository-"
	query := "?repository=" + id
	var initial reviewHistoryResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200), &initial)
	if initial.Summary == nil || initial.Summary.CommitCount != 1 || initial.Summary.Stats != (repository.ReviewStats{Files: 1, Additions: 1, Deletions: 1}) {
		t.Fatalf("summary=%#v", initial.Summary)
	}
	var comparison reviewComparisonResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"comparison"+query+"&review="+initial.Review, "", 200), &comparison)
	if comparison.Stats != initial.Summary.Stats {
		t.Fatal("overview and comparison disagree")
	}
	var batch struct {
		Repositories []reviewHistoryResponse `json:"repositories"`
	}
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"histories"+query, "", 200), &batch)
	if len(batch.Repositories) != 1 || !reflect.DeepEqual(batch.Repositories[0].Summary, initial.Summary) {
		t.Fatalf("batch=%#v", batch)
	}
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("before\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", worktree, "commit", "-am", "undo net line changes")
	var frozen, current reviewHistoryResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"history"+query+"&snapshot="+initial.Snapshot+"&page=1", "", 200), &frozen)
	if !reflect.DeepEqual(frozen.Summary, initial.Summary) || len(frozen.History.Commits) != 0 {
		t.Fatal("paging changed snapshot totals")
	}
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200), &current)
	if current.Summary == nil || current.Summary.CommitCount != 2 || current.Summary.Stats != (repository.ReviewStats{}) {
		t.Fatalf("current summary=%#v", current.Summary)
	}
	// Integration retains the same last-viewed comparison and its contribution.
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/remotes/origin/master", current.Pair.Head)
	var integrated reviewHistoryResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200), &integrated)
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
	if result.Summary != nil || result.SummaryError == "" || len(result.History.Commits) != 2 {
		t.Fatalf("history was lost or totals misreported: %#v", result)
	}
	var batch struct {
		Repositories []reviewHistoryResponse `json:"repositories"`
	}
	decodeReview(t, reviewRequest(t, s, "GET", "/api/sessions/example/repository-histories"+query, "", 200), &batch)
	if len(batch.Repositories) != 1 || batch.Repositories[0].SummaryError == "" || len(batch.Repositories[0].History.Commits) != 2 {
		t.Fatalf("batch=%#v", batch)
	}
}
