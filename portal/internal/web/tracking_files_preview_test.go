package web

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/repository"
)

func TestTrackingFilesPreviewsSniffRasterAndKeepActiveContentPassive(t *testing.T) {
	s := newTestServer(t)
	writeArtifactSession(t, s.config.Workspace, "seed.txt")
	root := filepath.Join(s.config.Workspace, "work", "example")
	fixtures := map[string][]byte{
		"unregistered.md": []byte("# Exact\r\n\r\n<script>active()</script>\r\n\r\n[bad](javascript:active())\r\n"),
		"active.svg":      []byte("<svg onload='active()'></svg>\r\n"),
		"active.html":     []byte("<script>active()</script>\r\n"),
		"passive.sh":      []byte("#!/bin/sh\r\nprintf original\r\n"),
		"fake.png":        []byte("<html>not a raster</html>"),
		"image.png":       []byte("\x89PNG\r\n\x1a\nfixture"),
		"image.jpg":       []byte("\xff\xd8\xfffixture"),
		"image.gif":       []byte("GIF89afixture"),
		"image.webp":      []byte("RIFF\x0c\x00\x00\x00WEBPVP8 fixture"),
		"wrong.jpg":       []byte("\x89PNG\r\n\x1a\nfixture"),
		"invalid.md":      {0, 0xff},
		"long.md":         []byte(strings.Repeat("x\n", repository.MaxReviewLines+1)),
		"large.md":        bytes.Repeat([]byte{'x'}, repository.MaxReviewBlobBytes+1),
		"empty.txt":       {},
	}
	for name, data := range fixtures {
		if err := os.WriteFile(filepath.Join(root, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := os.ReadFile(filepath.Join(root, "portal.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range fixtures {
		t.Run(name, func(t *testing.T) {
			file := readSource(t, s, "?"+url.Values{"artifact": {name}}.Encode(), 200)
			imageType, image := artifactImageType(name)
			if name == "fake.png" || name == "wrong.jpg" {
				image = false
			}
			if image {
				if file.Preview == nil || file.Preview.Kind != "image" {
					t.Fatalf("raster preview=%#v", file)
				}
				response := httptest.NewRecorder()
				s.Handler().ServeHTTP(response, httptest.NewRequest("GET", file.Preview.URL, nil))
				if response.Code != 200 || response.Header().Get("Content-Type") != imageType || !bytes.Equal(response.Body.Bytes(), data) || response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Content-Security-Policy") != "sandbox; default-src 'none'" {
					t.Fatalf("raster response=%d %v", response.Code, response.Header())
				}
			} else if name == "unregistered.md" {
				if file.Preview == nil || file.Preview.Kind != "markdown" || strings.Contains(file.Preview.HTML, "<script") || strings.Contains(file.Preview.HTML, "javascript:") {
					t.Fatalf("unsafe Markdown=%#v", file.Preview)
				}
			} else if file.Preview != nil {
				t.Fatalf("unexpected rich preview: %#v", file)
			}
			if !file.Content.Binary && !file.Content.Limited && file.Content.Text != string(data) {
				t.Fatal("raw source bytes changed")
			}
			if !image {
				response := httptest.NewRecorder()
				s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/artifact-previews/example/"+name, nil))
				if response.Code != 415 {
					t.Fatalf("non-raster inline response=%d", response.Code)
				}
			}
			for _, path := range []string{"/artifacts/example/" + name, "/api/sessions/example/file/download?" + url.Values{"artifact": {name}}.Encode()} {
				response := httptest.NewRecorder()
				s.Handler().ServeHTTP(response, httptest.NewRequest("GET", path, nil))
				if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), data) || response.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(response.Header().Get("Content-Disposition"), "attachment;") {
					t.Fatalf("download=%d %v", response.Code, response.Header())
				}
			}
		})
	}
	if after, err := os.ReadFile(filepath.Join(root, "portal.yml")); err != nil || !bytes.Equal(after, manifest) {
		t.Fatal("read rewrote registration manifest")
	}
	for _, name := range []string{"huge.png", "huge.txt"} {
		file, err := os.Create(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(repository.MaxSourceDownloadBytes + 1); err != nil {
			t.Fatal(err)
		}
		file.Close()
		got := readSource(t, s, "?artifact="+name, 200)
		if !got.Content.Limited || got.Preview != nil {
			t.Fatal("large file preview admitted")
		}
		for _, path := range []string{"/artifacts/example/" + name, "/api/sessions/example/file/download?artifact=" + name} {
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest("GET", path, nil))
			if response.Code != 413 {
				t.Fatalf("large download=%d", response.Code)
			}
		}
	}
	// Optional preview is absent for passive ordinary text, preserving old clients.
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/sessions/example/file?artifact=empty.txt", nil))
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, found := payload["preview"]; found {
		t.Fatal("ordinary text exposed preview")
	}
}
