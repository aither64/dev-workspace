package repository

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aither64/vpsfree-cz-workspace/portal/internal/session"
)

func TestInspectUsesOnlyManifestMetadata(t *testing.T) {
	gh := filepath.Join(t.TempDir(), "gh")
	script := `#!/bin/sh
case "$1" in
  repo) printf 'main\n' ;;
  run) printf '[]\n' ;;
esac
`
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	statuses := (Runner{GH: gh}).Inspect(context.Background(), []session.Repository{{
		Name: "project", GitHub: "example/project", Branch: "feature", DefaultBranch: "master",
	}}, false)
	if len(statuses) != 1 || statuses[0].DefaultBranch != "main" ||
		statuses[0].CompareURL != "https://github.com/example/project/compare/main...feature" {
		t.Fatalf("unexpected active status: %#v", statuses)
	}
}

func TestArchivedComparisonUsesImmutableManifestCommits(t *testing.T) {
	base := "1111111111111111111111111111111111111111"
	head := "2222222222222222222222222222222222222222"
	statuses := (Runner{GH: "true"}).Inspect(context.Background(), []session.Repository{{
		Name: "project", GitHub: "example/project", Branch: "feature",
		DefaultBranch: "master", InitialBaseSHA: base, FinalHeadSHA: head,
	}}, true)
	if len(statuses) != 1 || statuses[0].CompareURL != "https://github.com/example/project/compare/"+base+"..."+head ||
		statuses[0].BranchURL != "https://github.com/example/project/tree/"+head {
		t.Fatalf("unexpected archived status: %#v", statuses)
	}
}

func TestGitHubEnrichmentRunsConcurrently(t *testing.T) {
	gh := filepath.Join(t.TempDir(), "gh")
	script := `#!/bin/sh
sleep 1
case "$1" in
  repo) printf 'master\n' ;;
  run) printf '[]\n' ;;
esac
`
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	statuses := []Status{
		{Name: "one", GitHub: "example/one", Branch: "feature"},
		{Name: "two", GitHub: "example/two", Branch: "feature"},
	}
	started := time.Now()
	(Runner{GH: gh}).EnrichAll(context.Background(), statuses)
	if elapsed := time.Since(started); elapsed > 1800*time.Millisecond {
		t.Fatalf("enrichment was serialized: %s", elapsed)
	}
	for _, status := range statuses {
		if status.DefaultBranch != "master" || status.CompareURL == "" || status.GitHubError != "" {
			t.Fatalf("unexpected status: %#v", status)
		}
	}
}
