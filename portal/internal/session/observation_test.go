package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestObservationIdentityAndExplicitThreadlessScope(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "work", "example")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	summary := &Summary{Manifest: Manifest{Schema: 1, Slug: "example"}, Workspace: workspace, Root: "work"}
	first, err := ObservationIdentity(workspace, "example", directory, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		manifest string
		valid    bool
	}{
		{"schema: 1\nslug: example\nrepositories: []\nartifacts: []\n", true},
		{"schema: 1\nslug: example\nrepositories: []\n", false},
		{"schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncreation: {state: ready, initial_goal_sent: false}\n", false},
		{"schema: 1\nslug: example\nrepositories: null\nartifacts: []\n", false},
	} {
		path := filepath.Join(directory, "portal.yml")
		if err := os.WriteFile(path+".tmp", []byte(test.manifest), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			t.Fatal(err)
		}
		if err := RequireNormalizedThreadless(summary); (err == nil) != test.valid {
			t.Fatalf("scope %q: %v", test.manifest, err)
		}
		after, err := ObservationIdentity(workspace, "example", directory, "")
		if err != nil || after != first {
			t.Fatalf("manifest write changed identity: %s %v", after, err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "artifact.md"), []byte("curated"), 0644); err != nil {
		t.Fatal(err)
	}
	artifact, _ := ObservationIdentity(workspace, "example", directory, "")
	if artifact != first {
		t.Fatal("artifact registration changed directory identity")
	}
	root, _ := ObservationIdentity(workspace, "example", directory, "new-root")
	if root == first {
		t.Fatal("root replacement kept identity")
	}
	if err := os.Rename(directory, directory+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	replacement, _ := ObservationIdentity(workspace, "example", directory, "")
	if replacement == first {
		t.Fatal("directory replacement kept identity")
	}
}
