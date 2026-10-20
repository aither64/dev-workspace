package web

import (
	"fmt"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"testing"
)

func TestArtifactTreeGroupsLargeCatalogWithoutLosingLabels(t *testing.T) {
	artifacts := make([]session.Artifact, 500)
	for i := range artifacts {
		artifacts[i] = session.Artifact{Path: fmt.Sprintf("reports/deep/%03d.md", i), Label: fmt.Sprintf("Report %d", i)}
	}
	tree := artifactTree(artifacts)
	if len(tree) != 1 || tree[0].Directory != "reports" || len(tree[0].Children) != 1 {
		t.Fatalf("directories: %#v", tree)
	}
	rows := tree[0].Children[0].Children
	if len(rows) != 500 || rows[499].Name != "499.md" || rows[499].Artifact.Label != "Report 499" {
		t.Fatal("catalog rows or labels lost")
	}
}
