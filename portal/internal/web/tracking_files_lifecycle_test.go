package web

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/repository"
)

// Exercise ordinary archive/revive, not an in-test directory rename or private
// finalizer. The threadless Ruby fixture stubs only external runtime observation.
func TestTrackingFilesArchiveRevive(t *testing.T) {
	s := newTestServer(t)
	const slug = "2026-10-22-tracking-files-fixture"
	transition := func(phase string) {
		t.Helper()
		command := exec.Command("ruby", "../../../test/support/tracking_files_lifecycle_fixture.rb", s.config.Workspace, phase)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("normal disposable %s: %v\n%s", phase, err, output)
		}
	}
	transition("initialize")
	tracking := filepath.Join(s.config.Workspace, "work", slug)
	fixtures := map[string][]byte{
		".gitignore":             []byte("ignored*\n"),
		"ignored report.md":      []byte("# Kept\r\n\r\n<script>unsafe()</script>\r\n"),
		"nested/unregistered.sh": []byte("#!/bin/sh\r\nprintf passive\r\n"),
		"nested/image.png":       []byte("\x89PNG\r\n\x1a\nfixture"),
	}
	for name, data := range fixtures {
		path := filepath.Join(tracking, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		return response
	}
	for _, phase := range []string{"active", "archived", "revived"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "archived" {
				transition("archive")
			}
			if phase == "revived" {
				transition("revive")
			}
			var list struct {
				Files []repository.TrackingFile `json:"files"`
			}
			if err := json.Unmarshal(get("/api/sessions/"+slug+"/files").Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			listed := map[string]bool{}
			for _, row := range list.Files {
				listed[row.Path] = true
				if row.Registered || row.Label != "" {
					t.Fatal("registration metadata projected")
				}
			}
			for name, data := range fixtures {
				if !listed[name] {
					t.Fatalf("%s missing %s", phase, name)
				}
				query := url.Values{"artifact": {name}}.Encode()
				var file repository.SourceFile
				if err := json.Unmarshal(get("/api/sessions/"+slug+"/file?"+query).Body.Bytes(), &file); err != nil {
					t.Fatal(err)
				}
				if file.Path != name || (phase == "archived") != (file.Source == repository.SourceArchivedArtifact) {
					t.Fatalf("source identity: %#v", file)
				}
				if name != "nested/image.png" && file.Content.Text != string(data) {
					t.Fatal("original source bytes changed")
				}
				for _, path := range []string{"/api/sessions/" + slug + "/file/download?" + query, "/artifacts/" + slug + "/" + strings.ReplaceAll(url.PathEscape(name), "%2F", "/")} {
					if got := get(path); !bytes.Equal(got.Body.Bytes(), data) || got.Header().Get("Content-Type") != "application/octet-stream" {
						t.Fatalf("download changed: %s", path)
					}
				}
				preview := get("/api/sessions/" + slug + "/artifact-preview?" + url.Values{"path": {name}}.Encode())
				if strings.HasSuffix(name, ".md") {
					if file.Preview == nil || file.Preview.Kind != "markdown" || strings.Contains(file.Preview.HTML, "<script") || !strings.Contains(preview.Body.String(), `"kind":"markdown"`) {
						t.Fatal("Markdown preview not sanitized")
					}
				}
				if strings.HasSuffix(name, ".png") {
					if file.Preview == nil || file.Preview.Kind != "image" {
						t.Fatal("missing raster preview")
					}
					if image := get(file.Preview.URL); !bytes.Equal(image.Body.Bytes(), data) || image.Header().Get("Content-Type") != "image/png" {
						t.Fatal("image response changed")
					}
				}
			}
		})
	}
}
