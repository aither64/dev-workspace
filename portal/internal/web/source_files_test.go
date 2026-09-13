package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/repository"
	"golang.org/x/sys/unix"
)

func sourceQuery(path string) string {
	return "?" + url.Values{"repository": {repository.ReviewID("project")}, "path": {path}}.Encode()
}

func readSource(t *testing.T, s *Server, query string, status int) repository.SourceFile {
	t.Helper()
	result := reviewRequest(t, s, "GET", "/api/sessions/example/file"+query, "", status)
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

func TestSourceLinksRewriteHistoryAndRedirect(t *testing.T) {
	s, _, worktree, _ := reviewWebFixture(t)
	path := filepath.Join(worktree, "file")
	for _, suffix := range []string{":10", ":10:5", "#L10", ":0010"} {
		input, err := url.Parse(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := s.sourceLink(input)
		want := "/files/example" + sourceQuery("file") + "#L10"
		if !ok || got != want {
			t.Fatalf("%s: %q, %v, want %q", suffix, got, ok, want)
		}
	}
	original := "[The route](" + path + ":10) and [external](https://example.test/a:10)."
	transcript := codex.Transcript{Entries: []codex.TranscriptEntry{{Kind: "agentMessage", Text: original}}}
	s.presentTranscript(&transcript)
	entry := transcript.Entries[0]
	if entry.Text != original || !strings.Contains(entry.HTML, "/files/example?") ||
		!strings.Contains(entry.HTML, "#L10") || !strings.Contains(entry.HTML, ">The route</a>") ||
		!strings.Contains(entry.HTML, "https://example.test/a:10") {
		t.Fatalf("transcript changed or links missing: %#v", entry)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", path+":10", nil))
	if response.Code != 302 || response.Header().Get("Location") != "/files/example"+sourceQuery("file")+"#L10" {
		t.Fatalf("old copied URL: %d %s", response.Code, response.Header().Get("Location"))
	}
	response = httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", responseURL("example", "file"), nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `data-slug="example"`) ||
		!strings.Contains(response.Header().Get("Content-Security-Policy"), "style-src 'self' 'nonce-") ||
		response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("viewer page: %d %s", response.Code, response.Body.String())
	}
}

func responseURL(slug, path string) string { return "/files/" + slug + sourceQuery(path) }

func TestSourceLinksEscapingAndRejectedDestinations(t *testing.T) {
	s := newTestServer(t)
	root := s.config.Workspace
	for _, path := range []string{"space žluťoučký.nix", "percent%20.txt", "hash#name?.rb", "literal:10"} {
		u := &url.URL{Path: root + "/worktrees/example/project/" + path}
		// Escape literal colons so a filename ending in :10 is distinguishable
		// from a line suffix in an absolute Markdown destination.
		raw := strings.ReplaceAll(u.String(), ":", "%3A")
		input, _ := url.Parse(raw)
		got, ok := s.sourceLink(input)
		if !ok || got != "/files/example"+sourceQuery(path) {
			t.Fatalf("%q: %q %v", path, got, ok)
		}
	}
	for _, raw := range []string{
		"https://example.test" + root + "/worktrees/example/project/file:10",
		"//example.test/elsewhere", "/etc/passwd:1", root + "-other/worktrees/example/project/file",
		root + "/worktrees/example/project/../private", root + "/worktrees/example/project/%2e%2e/private",
		root + "/worktrees/example/project/.git/config", root + "/worktrees/example/project/file:0",
		root + "/worktrees/example/project/file:9007199254740992", root + "/worktrees/example/project/file:10:0",
		root + "/worktrees/example/project/file#L0", root + "/worktrees/example/project/file:10#L11",
		root + "/notes/private", root + "/worktrees/example/project/file?download=1",
	} {
		input, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := s.sourceLink(input); ok {
			t.Errorf("accepted %q as %q", raw, got)
		}
	}
	html := string(s.renderTextMarkdown("[bad](javascript:alert%281%29) <script>alert(1)</script>"))
	if strings.Contains(html, "javascript:") || strings.Contains(html, "<script>") {
		t.Fatalf("Markdown sanitization regressed: %s", html)
	}
}

func TestSourceFileReadsCurrentIndexMembersAndRejectsEscapes(t *testing.T) {
	s, _, worktree, _ := reviewWebFixture(t)
	write := func(name, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(worktree, name)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("file", "working\r\n<script>window.bad=true</script>\nfinal")
	file := readSource(t, s, sourceQuery("file"), 200)
	if file.Source != "worktree" || file.Revision != "" || file.Content.Text != "working\r\n<script>window.bad=true</script>\nfinal" {
		t.Fatalf("did not read current worktree: %#v", file)
	}
	for _, name := range []string{"new space ž.rb", "literal*file", "percent%2Ffile", "literal:10", "nested/route.nix"} {
		write(name, "staged version\n")
		runWebGit(t, "-C", worktree, "add", "--", name)
		write(name, "unstaged version\n")
		if readSource(t, s, sourceQuery(name), 200).Content.Text != "unstaged version\n" {
			t.Fatalf("did not read staged addition's current bytes: %s", name)
		}
	}
	write("untracked", "private")
	write(".env", "private")
	for _, path := range []string{"untracked", ".env", ".git", ".git/config", "../private", "a/../file", "a//file", "/etc/passwd", "*", ":(glob)*", "file/child"} {
		status := 404
		if !repository.ValidSourcePath(path) {
			status = 400
		}
		readSource(t, s, sourceQuery(path), status)
	}
	for _, query := range []string{sourceQuery("file") + "&path=untracked", sourceQuery("file") + "&artifact=plan.md", "?artifact=plan.md&artifact=state.md", "?artifact=%zz"} {
		readSource(t, s, query, 400)
	}
	if err := os.Remove(filepath.Join(worktree, "file")); err != nil {
		t.Fatal(err)
	}
	readSource(t, s, sourceQuery("file"), 404)
	if err := os.Symlink(filepath.Join(worktree, "untracked"), filepath.Join(worktree, "file")); err != nil {
		t.Fatal(err)
	}
	readSource(t, s, sourceQuery("file"), 404)
	if err := os.Remove(filepath.Join(worktree, "file")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(worktree, "file"), 0600); err != nil {
		t.Fatal(err)
	}
	readSource(t, s, sourceQuery("file"), 404)
	reviewRequest(t, s, "GET", "/api/sessions/other/file"+sourceQuery("new space ž.rb"), "", 404)
}

func TestSourceFilesUseArchivedFinalCommitAndCuratedArtifacts(t *testing.T) {
	s, bare, worktree, _ := reviewWebFixture(t)
	if err := os.Mkdir(filepath.Join(worktree, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "nested", "route.nix"), []byte("# nested route\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(worktree, "link")); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", worktree, "add", "nested/route.nix", "link")
	runWebGit(t, "-C", worktree, "commit", "-m", "nested source and symlink")
	head := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	tracking := filepath.Join(s.config.Workspace, "work", "example")
	plan := readSource(t, s, "?artifact=plan.md", 200)
	if plan.Source != "artifact" || plan.Content.Text == "" {
		t.Fatalf("built-in artifact missing: %#v", plan)
	}
	if err := os.WriteFile(filepath.Join(tracking, "private.md"), []byte("not published"), 0644); err != nil {
		t.Fatal(err)
	}
	readSource(t, s, "?artifact=private.md", 404)
	manifest, err := os.ReadFile(filepath.Join(tracking, "portal.yml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest = append(manifest, []byte(fmt.Sprintf("    final_head_sha: %s\nartifacts:\n  - label: Report\n    path: report.md\n", head))...)
	if err := os.WriteFile(filepath.Join(tracking, "report.md"), []byte("# Source\n[File]("+worktree+"/file:1)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tracking, "portal.yml"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	if readSource(t, s, "?artifact=report.md", 200).Content.Text == "" {
		t.Fatal("curated artifact missing")
	}
	preview := reviewRequest(t, s, "GET", "/api/sessions/example/artifact-preview?path=report.md", "", 200)
	if !strings.Contains(reviewString(t, preview["html"]), "/files/example?") {
		t.Fatal("artifact Markdown links were not rewritten")
	}
	runWebGit(t, "--git-dir="+bare, "worktree", "remove", worktree)
	readSource(t, s, sourceQuery("file"), 404)
	writeWebTrackingFiles(t, tracking, "complete")
	if err := os.WriteFile(filepath.Join(tracking, "portal.yml"), append(manifest, []byte("finalized_at: '2026-09-13T12:00:00Z'\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(s.config.Workspace, "archive", "example")
	if err := os.MkdirAll(filepath.Dir(archive), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tracking, archive); err != nil {
		t.Fatal(err)
	}
	// Move the retained branch away from its recorded head. Archived reads must
	// still load the final snapshot and must not require a live worktree.
	base := strings.TrimSpace(webGitOutput(t, "--git-dir="+bare, "rev-parse", head+"^"))
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/heads/feature", base)
	file := readSource(t, s, sourceQuery("file"), 200)
	if file.Source != "archive" || file.Revision != head || file.Content.Text != "after\n" {
		t.Fatalf("archived file = %#v", file)
	}
	if readSource(t, s, sourceQuery("nested/route.nix"), 200).Content.Text != "# nested route\n" {
		t.Fatal("nested archived path was not read literally")
	}
	readSource(t, s, sourceQuery("link"), 404)
	if readSource(t, s, "?artifact=plan.md", 200).Source != "archived-artifact" {
		t.Fatal("archived artifact source is not identified")
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", tracking+"/report.md:2", nil))
	if response.Code != 302 || response.Header().Get("Location") != "/files/example?artifact=report.md#L2" {
		t.Fatalf("old tracking link: %d %s", response.Code, response.Body.String())
	}
}

func TestSourceFilePreviewBounds(t *testing.T) {
	s, _, worktree, _ := reviewWebFixture(t)
	for _, test := range []struct {
		name            string
		text            string
		binary, limited bool
	}{
		{"empty", "", false, false}, {"binary", "a\x00b", true, false}, {"utf8", "\xff", true, false},
		{"large", strings.Repeat("x", repository.MaxReviewBlobBytes+1), false, true},
		{"lines", strings.Repeat("x\n", repository.MaxReviewLines) + "last", false, true},
		{"at-limit", strings.Repeat("x\n", repository.MaxReviewLines-1) + "last", false, false},
	} {
		if err := os.WriteFile(filepath.Join(worktree, test.name), []byte(test.text), 0644); err != nil {
			t.Fatal(err)
		}
		runWebGit(t, "-C", worktree, "add", "--", test.name)
		got := readSource(t, s, sourceQuery(test.name), 200).Content
		if got.Binary != test.binary || got.Limited != test.limited || (test.binary || test.limited) && got.Text != "" {
			t.Fatalf("%s: %#v", test.name, got)
		}
	}
}

func TestSourceArtifactsFollowCatalogPathContract(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	tracking := filepath.Join(s.config.Workspace, "work", "example")
	for _, path := range []string{"reports/.git/notes.md", "reports\\notes.md", "reports/sub/../notes.md"} {
		name := filepath.Join(tracking, filepath.Clean(path))
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("# Published artifact\n"), 0644); err != nil {
			t.Fatal(err)
		}
		manifest := fmt.Sprintf("schema: 1\nslug: example\nartifacts:\n  - label: Report\n    path: %q\n", path)
		if err := os.WriteFile(filepath.Join(tracking, "portal.yml"), []byte(manifest), 0644); err != nil {
			t.Fatal(err)
		}
		query := "?" + url.Values{"artifact": {path}}.Encode()
		if got := readSource(t, s, query, 200); got.Content.Text != "# Published artifact\n" || got.Path != filepath.Clean(path) {
			t.Fatalf("artifact %q: %#v", path, got)
		}
		input := &url.URL{Path: tracking + "/" + path, Fragment: "L1"}
		want := "/files/example?" + url.Values{"artifact": {filepath.Clean(path)}}.Encode() + "#L1"
		if target, ok := s.sourceLink(input); !ok || target != want {
			t.Fatalf("artifact link %q = %q %v", path, target, ok)
		}
		if err := os.Truncate(name, 11*1024*1024); err != nil {
			t.Fatal(err)
		}
		if !readSource(t, s, query, 200).Content.Limited {
			t.Fatal("large artifact did not receive a preview-limit status")
		}
	}
	for _, path := range []string{"../private", "a/../../private", "/etc/passwd", ".", ""} {
		readSource(t, s, "?"+url.Values{"artifact": {path}}.Encode(), 400)
	}
}

func TestSourceFileBrowserContracts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for browser contracts")
	}
	if output, err := exec.Command(node, "source_files_browser_test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("source browser contracts: %v\n%s", err, output)
	}
}

func TestSourceLegacyPathsRejectTraversalBeforeMuxCleaning(t *testing.T) {
	s := newTestServer(t)
	for _, suffix := range []string{"/work/example/../other/plan.md", "/work/example/%2e%2e/other/plan.md", "/work//example/plan.md"} {
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, s.config.Workspace+suffix, nil))
		if response.Code != 404 {
			t.Fatalf("%s = %d", suffix, response.Code)
		}
	}
}
