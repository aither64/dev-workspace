package session

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDuplicateArtifactsKeepCatalogAndManifestIdentity(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "work", fixtureSlug)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "portal-manifest-valid-duplicate-artifacts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(directory, ManifestName)
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	writeTrackingFiles(t, directory, "active")
	if err := os.WriteFile(filepath.Join(directory, "report.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary, err := Find(workspace, fixtureSlug)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Codex.ThreadID != "thread-1" || len(summary.Artifacts) != 5 {
		t.Fatalf("loaded manifest changed: %#v", summary.Manifest)
	}
	want := []Artifact{{Label: "Plan", Path: "plan.md"}, {Label: "State", Path: "state.md"},
		{Label: "First report", Path: "report.json"}, {Label: "Notes", Path: "notes.txt"}}
	if got := AvailableArtifacts(summary); !slices.Equal(got, want) {
		t.Fatalf("catalog = %#v, want %#v", got, want)
	}
	if summaries, err := List(workspace); err != nil || len(summaries) != 1 {
		t.Fatalf("List = %#v, %v", summaries, err)
	}
	file, _, err := OpenArtifact(summary, "./report.json", 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if content, err := io.ReadAll(file); err != nil || string(content) != "{}" {
		t.Fatalf("artifact = %q, %v", content, err)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil || !bytes.Equal(data, after) || len(summary.Artifacts) != 5 {
		t.Fatalf("read changed the manifest: %v", err)
	}
}
