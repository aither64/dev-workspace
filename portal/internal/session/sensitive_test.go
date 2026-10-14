package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSensitiveManifestAndInstructions(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "work", "private")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	data := "schema: 1\nslug: private\nsensitive: true\nrepositories: []\nartifacts: []\n"
	if err := os.WriteFile(filepath.Join(root, ManifestName), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.md"), []byte("---\nlifecycle: active\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plan.md"), []byte("Private plan"), 0600); err != nil {
		t.Fatal(err)
	}
	summary, err := Find(workspace, "private")
	if err != nil || !summary.Sensitive {
		t.Fatalf("summary = %#v, %v", summary, err)
	}
	instructions := SensitiveInstructions(workspace, "private")
	if !strings.Contains(instructions, root) || !strings.Contains(instructions, "Never stage or commit") {
		t.Fatal(instructions)
	}
	if SensitiveInstructions(workspace, "ordinary") != "" {
		t.Fatal("ordinary policy changed")
	}
	for _, marker := range []string{"false", "null", "\"true\"", "1"} {
		var manifest Manifest
		if decodeManifest([]byte(strings.Replace(data, "sensitive: true", "sensitive: "+marker, 1)), &manifest) == nil {
			t.Fatalf("accepted sensitivity %s", marker)
		}
	}
}
