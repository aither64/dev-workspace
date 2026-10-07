package web

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/repository"
	"golang.org/x/sys/unix"
)

func workspaceSourceQuery(path string) string {
	return "?" + url.Values{"path": {path}}.Encode()
}

func readWorkspaceSource(t *testing.T, s *Server, path string, status int) repository.SourceFile {
	t.Helper()
	result := reviewRequest(t, s, "GET", "/api/workspace/file"+workspaceSourceQuery(path), "", status)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var file repository.SourceFile
	if err := json.Unmarshal(encoded, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestWorkspaceSourceLinksRewriteAndRedirect(t *testing.T) {
	s := newTestServer(t)
	// Mapping does not depend on a session, Git registration, or file existence.
	for _, path := range []string{"docs/agent-instructions/lifecycle.md", "AGENTS.md", "bin/check", "notes/lesson.md"} {
		for _, suffix := range []string{"", ":93", ":93:5", ":0093", "#L93"} {
			input, err := url.Parse(s.config.Workspace + "/" + path + suffix)
			if err != nil {
				t.Fatal(err)
			}
			want := "/workspace-files" + workspaceSourceQuery(path)
			if suffix != "" {
				want += "#L93"
			}
			got, ok := s.sourceLink(input)
			if !ok || got != want {
				t.Fatalf("%s%s: %q, %v, want %q", path, suffix, got, ok, want)
			}
		}
	}
	for _, path := range []string{"space žluťoučký.md", "percent%20.txt", "hash#name?.rb", "literal:10"} {
		input := &url.URL{Path: s.config.Workspace + "/" + path}
		raw := strings.ReplaceAll(input.String(), ":", "%3A")
		parsed, _ := url.Parse(raw)
		if got, ok := s.sourceLink(parsed); !ok || got != "/workspace-files"+workspaceSourceQuery(path) {
			t.Fatalf("escaped workspace name %q: %q %v", path, got, ok)
		}
	}
	path := s.config.Workspace + "/docs/agent-instructions/lifecycle.md:93"
	want := "/workspace-files" + workspaceSourceQuery("docs/agent-instructions/lifecycle.md") + "#L93"
	original := "[workspace lifecycle rules](" + path + ")"
	transcript := codex.Transcript{Entries: []codex.TranscriptEntry{{Kind: "agentMessage", Text: original}}}
	s.presentTranscript(&transcript)
	if entry := transcript.Entries[0]; entry.Text != original || !strings.Contains(entry.HTML, want) {
		t.Fatalf("shared transcript link: %#v", entry)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", path, nil))
	if response.Code != 302 || response.Header().Get("Location") != want {
		t.Fatalf("old shared URL: %d %s", response.Code, response.Header().Get("Location"))
	}
	response = httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", want, nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `data-file-api="/api/workspace/file"`) ||
		strings.Contains(response.Body.String(), "Back to session") || response.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(response.Header().Get("Content-Security-Policy"), "style-src 'self' 'nonce-") {
		t.Fatalf("shared viewer: %d %s", response.Code, response.Body.String())
	}
	for _, raw := range []string{"../private", "%2e%2e/private", ".git/config", "repos/project.git/config", "AGENTS.md:0", "AGENTS.md:1#L2"} {
		input, _ := url.Parse(s.config.Workspace + "/" + raw)
		if got, ok := s.sourceLink(input); ok {
			t.Fatalf("rewrote invalid shared path %q as %q", raw, got)
		}
	}
}

func TestWorkspaceSourceReadsTrackedCurrentFilesAndPreservesSessionBoundary(t *testing.T) {
	s := newTestServer(t)
	root := s.config.Workspace
	runWebGit(t, "init", "--initial-branch=master", root)
	write := func(path string, content []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), content, 0644); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{"AGENTS.md", "docs/agent-instructions/lifecycle.md", "bin/check", "notes/lesson.md", "space ž.md", "literal*name", "percent%2Fname", "literal:10"}
	for _, path := range paths {
		write(path, []byte("staged\n"))
		runWebGit(t, "-C", root, "add", "--", path)
		write(path, []byte("current\n<script>window.bad=true</script>\n"))
		file := readWorkspaceSource(t, s, path, 200)
		if file.Source != "workspace" || file.Path != path || file.Repository != "" || file.Revision != "" || file.Content.Text != "current\n<script>window.bad=true</script>\n" {
			t.Fatalf("shared working contents: %#v", file)
		}
	}
	write("binary", []byte{0, 255})
	write("large", []byte(strings.Repeat("a", repository.MaxReviewBlobBytes+1)))
	runWebGit(t, "-C", root, "add", "binary", "large")
	if !readWorkspaceSource(t, s, "binary", 200).Content.Binary || !readWorkspaceSource(t, s, "large", 200).Content.Limited {
		t.Fatal("workspace preview limits changed")
	}
	write("untracked", []byte("private\n"))
	readWorkspaceSource(t, s, "untracked", 404)
	readWorkspaceSource(t, s, "missing", 404)
	for _, path := range []string{"work/example/private.md", "archive/example/private.md", "worktrees/example/project/private", "repos/project.git/private"} {
		write(path, []byte("private\n"))
		runWebGit(t, "-C", root, "add", "--", path)
		readWorkspaceSource(t, s, path, 400)
	}
	for _, path := range []string{"../private", "docs/../AGENTS.md", "/etc/passwd", ".git/config", "docs/.git/config", "a//b"} {
		readWorkspaceSource(t, s, path, 400)
	}
	for _, query := range []string{"", "?path=AGENTS.md&path=bin/check", "?path=AGENTS.md&repository=abc", "?path=%zz", "?other=AGENTS.md"} {
		reviewRequest(t, s, "GET", "/api/workspace/file"+query, "", 400)
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/workspace-files"+query, nil))
		if response.Code != 404 || !strings.Contains(response.Body.String(), "workspace file link is invalid") {
			t.Fatalf("invalid shared viewer query %q: %d %s", query, response.Code, response.Body.String())
		}
	}
	if err := os.Symlink("untracked", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", root, "add", "link")
	readWorkspaceSource(t, s, "link", 404)
	if err := os.Remove(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	readWorkspaceSource(t, s, "AGENTS.md", 404)
	if err := os.Symlink("untracked", filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	readWorkspaceSource(t, s, "AGENTS.md", 404)
	if err := os.Remove(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "AGENTS.md"), 0600); err != nil {
		t.Fatal(err)
	}
	readWorkspaceSource(t, s, "AGENTS.md", 404)
	if err := os.Rename(filepath.Join(root, "docs"), filepath.Join(root, "original-docs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("original-docs", filepath.Join(root, "docs")); err != nil {
		t.Fatal(err)
	}
	readWorkspaceSource(t, s, "docs/agent-instructions/lifecycle.md", 404)
}

func TestWorkspaceSourceRequiresExactGitRoot(t *testing.T) {
	s := newTestServer(t)
	readWorkspaceSource(t, s, "AGENTS.md", 404)
	parent := filepath.Dir(s.config.Workspace)
	runWebGit(t, "init", "--initial-branch=master", parent)
	if err := os.WriteFile(filepath.Join(s.config.Workspace, "AGENTS.md"), []byte("nested file\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", parent, "add", "--", filepath.Join(filepath.Base(s.config.Workspace), "AGENTS.md"))
	readWorkspaceSource(t, s, "AGENTS.md", 404)
}
