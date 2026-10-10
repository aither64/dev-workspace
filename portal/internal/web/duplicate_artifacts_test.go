package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
)

func TestDuplicateArtifactAddedWhileSessionOpenPreservesConversation(t *testing.T) {
	server := newTestServer(t)
	prepareInteractiveConversation(t, server, "example")
	server.config.Codex = &browserContractCodex{transcript: codex.Transcript{
		ThreadID: "thread-1", Status: "idle", CollaborationMode: "default",
	}}
	directory := filepath.Join(server.config.Workspace, "work", "example")
	manifestPath := filepath.Join(directory, "portal.yml")
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(original) + "artifacts:\n  - label: First report\n    path: report.json\n"
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "report.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, response.Code, response.Body.String())
		}
		return response
	}
	get("/example/")
	manifest += "  - label: Repeated report\n    path: report.json\n  - label: Equivalent report\n    path: ./report.json\n"
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	var details struct {
		ArtifactsHTML string `json:"artifactsHTML"`
		ArtifactCount int    `json:"artifactCount"`
	}
	if err := json.Unmarshal(get("/api/sessions/example/details").Body.Bytes(), &details); err != nil {
		t.Fatal(err)
	}
	if details.ArtifactCount != 3 || strings.Count(details.ArtifactsHTML, `data-artifact-path="report.json"`) != 1 ||
		!strings.Contains(details.ArtifactsHTML, `data-artifact-label="First report"`) ||
		strings.Contains(details.ArtifactsHTML, "Repeated report") || strings.Contains(details.ArtifactsHTML, "Equivalent report") {
		t.Fatalf("unexpected artifacts: %#v", details)
	}
	get("/example/")
	thread := get("/api/sessions/example/thread")
	if !strings.Contains(thread.Body.String(), `"threadId":"thread-1"`) {
		t.Fatalf("retained conversation changed: %s", thread.Body.String())
	}
	if artifact := get("/artifacts/example/report.json"); artifact.Body.String() != "{}" {
		t.Fatalf("artifact = %q", artifact.Body.String())
	}
	if after, err := os.ReadFile(manifestPath); err != nil || string(after) != manifest {
		t.Fatalf("portal rewrote the manifest: %v", err)
	}
}
