package web

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/repository"
)

func TestRepositoryLegacyEditedRenameLinkAtMetadataLimit(t *testing.T) {
	s, bare, worktree, _ := reviewWebFixture(t)
	defer s.Close()
	before := strings.Repeat("retained line\n", 20)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(worktree, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("file", before)
	for i := 0; i < 4999; i++ {
		write(fmt.Sprintf("z%04d", i), "before\n")
	}
	runWebGit(t, "-C", worktree, "add", ".")
	runWebGit(t, "-C", worktree, "commit", "-m", "rename base at file limit")
	base := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/remotes/origin/master", base)
	runWebGit(t, "-C", worktree, "mv", "file", "moved")
	write("moved", before+"added\n")
	for i := 0; i < 4999; i++ {
		write(fmt.Sprintf("z%04d", i), "after\n")
	}
	runWebGit(t, "-C", worktree, "commit", "-am", "rename and modify at file limit")
	endpoint := "/api/sessions/example/repository-"
	query := "?repository=" + repository.ReviewID("project")
	var history reviewHistoryResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200), &history)
	url := endpoint + "comparison" + query + "&review=" + history.Review
	// Normal metadata still enforces the existing 5,000-record limit.
	reviewRequest(t, s, "GET", url, "", 422)
	reviewRequest(t, s, "GET", url+"&file=invalid", "", 400)
	legacyID := repository.ReviewID("0\x00moved")
	var legacy reviewComparisonResponse
	decodeReview(t, reviewRequest(t, s, "GET", url+"&file="+legacyID, "", 200), &legacy)
	if len(legacy.Files) != 5000 || legacy.Files[0].ID != legacyID || legacy.Files[0].OldPath != "file" || legacy.Preview == nil || legacy.Preview.Content.Before.Text != before {
		t.Fatalf("legacy files=%d preview=%#v", len(legacy.Files), legacy.Preview)
	}
	reviewRequest(t, s, "GET", url+"&file="+repository.ReviewID("unknown"), "", 404)
	reviewRequest(t, s, "GET", url, "", 422)
}

func TestRepositoryMetadataSurvivesMissingBlob(t *testing.T) {
	s, bare, worktree, _ := reviewWebFixture(t)
	defer s.Close()
	if err := os.WriteFile(filepath.Join(worktree, "available"), []byte("still readable\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", worktree, "add", "available")
	runWebGit(t, "-C", worktree, "commit", "-m", "available file")
	oid := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD:file"))
	if err := os.Remove(filepath.Join(bare, "objects", oid[:2], oid[2:])); err != nil {
		t.Fatal(err)
	}
	// Missing promised content must not trigger fetching while browsing.
	runWebGit(t, "--git-dir="+bare, "config", "remote.origin.promisor", "true")
	marker := filepath.Join(t.TempDir(), "fetch-attempt")
	ssh := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\necho attempted > '"+marker+"'\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "--git-dir="+bare, "config", "remote.origin.url", "ssh://invalid.invalid/repo")
	runWebGit(t, "--git-dir="+bare, "config", "core.sshCommand", ssh)
	t.Cleanup(func() {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("review attempted a fetch: %v", err)
		}
	})
	endpoint := "/api/sessions/example/repository-"
	query := "?repository=" + repository.ReviewID("project")
	var history reviewHistoryResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200), &history)
	if !history.SummaryDeferred || history.Summary != nil {
		t.Fatalf("eager summary: %#v", history)
	}
	reviewRequest(t, s, "GET", endpoint+"summary"+query+"&snapshot="+history.Snapshot, "", 422)
	reviewRequest(t, s, "GET", endpoint+"counts"+query+"&snapshot="+history.Snapshot, "", 422)
	var comparison reviewComparisonResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"comparison"+query+"&review="+history.Review, "", 200), &comparison)
	if comparison.Preview != nil || len(comparison.Files) != 2 {
		t.Fatal(comparison)
	}
	batchQuery := query + "&snapshot=" + comparison.Snapshot
	for _, file := range comparison.Files {
		batchQuery += "&file=" + file.ID
	}
	var batch struct {
		Files []struct {
			File    string
			Content *repository.ReviewContent
			Error   string
		}
	}
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"files"+batchQuery, "", 200), &batch)
	if len(batch.Files) != 2 {
		t.Fatal(batch)
	}
	for i, file := range comparison.Files {
		result := batch.Files[i]
		if file.Path == "file" {
			if result.Error == "" || result.Content != nil {
				t.Fatalf("missing content=%#v", result)
			}
		} else if result.Error != "" || result.Content == nil || result.Content.After.Text != "still readable\n" || result.Content.Additions == nil || *result.Content.Additions != 1 {
			t.Fatalf("available content=%#v", result)
		}
	}
}

func TestRepositoryLegacyEditedRenameLink(t *testing.T) {
	s, bare, worktree, _ := reviewWebFixture(t)
	defer s.Close()
	before := strings.Repeat("retained line\n", 20)
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte(before), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", worktree, "commit", "-am", "rename base")
	base := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/remotes/origin/master", base)
	runWebGit(t, "-C", worktree, "mv", "file", "moved")
	if err := os.WriteFile(filepath.Join(worktree, "moved"), []byte(before+"added\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", worktree, "commit", "-am", "rename and edit")
	endpoint := "/api/sessions/example/repository-"
	query := "?repository=" + repository.ReviewID("project")
	var history reviewHistoryResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200), &history)
	url := endpoint + "comparison" + query + "&review=" + history.Review
	var current, legacy reviewComparisonResponse
	decodeReview(t, reviewRequest(t, s, "GET", url, "", 200), &current)
	if len(current.Files) != 2 || current.RenameNotice == "" {
		t.Fatalf("metadata=%#v", current)
	}
	legacyID := repository.ReviewID("0\x00moved")
	decodeReview(t, reviewRequest(t, s, "GET", url+"&file="+legacyID, "", 200), &legacy)
	if len(legacy.Files) != 1 || legacy.Files[0].ID != legacyID || legacy.Files[0].OldPath != "file" || legacy.Preview == nil || legacy.Preview.Content.Before.Text != before || legacy.Snapshot == current.Snapshot {
		t.Fatalf("legacy=%#v", legacy)
	}
	var content repository.ReviewContent
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"file"+query+"&snapshot="+legacy.Snapshot+"&file="+legacyID, "", 200), &content)
	if content.After.Text != before+"added\n" {
		t.Fatal(content)
	}
	// Resolving an old link cannot change the representation of an existing view.
	decodeReview(t, reviewRequest(t, s, "GET", url, "", 200), &current)
	if len(current.Files) != 2 {
		t.Fatal(current)
	}
}
