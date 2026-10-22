package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
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
		!strings.Contains(entry.HTML, `target="_blank"`) || !strings.Contains(entry.HTML, "noopener") || !strings.Contains(entry.HTML, "#L10") || !strings.Contains(entry.HTML, ">The route</a>") ||
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
		root + "/repos/private", root + "/worktrees/example/project/file?download=1",
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

func TestSourceFileReadsEligibleWorkingFilesAndRejectsEscapes(t *testing.T) {
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
	write(".gitignore", ".env\nignored/\nfile\n")
	write("ignored/nested", "ignored")
	if readSource(t, s, sourceQuery("untracked"), 200).Content.Text != "private" {
		t.Fatal("non-ignored untracked file was not read")
	}
	// Index membership still authorizes a file when a later ignore rule matches.
	readSource(t, s, sourceQuery("file"), 200)
	for _, name := range []string{"unstaged space ž.rb", "unstaged*file", "percent%20file", "new:10", "new/nested.rb"} {
		write(name, "untracked bytes\n")
		if readSource(t, s, sourceQuery(name), 200).Content.Text != "untracked bytes\n" {
			t.Fatalf("untracked path was not read literally: %s", name)
		}
	}
	exclude := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "--git-path", "info/exclude"))
	if err := os.WriteFile(exclude, []byte("local-private\n"), 0644); err != nil {
		t.Fatal(err)
	}
	write("local-private", "excluded")
	for _, path := range []string{".env", "ignored/nested", "local-private", ".git", ".git/config", "../private", "a/../file", "a//file", "/etc/passwd", "*", ":(glob)*", "file/child", "new"} {
		status := 404
		if !repository.ValidSourcePath(path) {
			status = 400
		}
		readSource(t, s, sourceQuery(path), status)
	}
	if err := os.Symlink("untracked", filepath.Join(worktree, "untracked-link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(worktree, "untracked-fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	readSource(t, s, sourceQuery("untracked-link"), 404)
	readSource(t, s, sourceQuery("untracked-fifo"), 404)
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

func TestSourceTrackingFilesDoNotRequireArtifactPublication(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	root := s.config.Workspace
	runWebGit(t, "init", "--initial-branch=master", root)
	tracking := filepath.Join(root, "work", "example")
	write := func(path, text string) {
		t.Helper()
		name := filepath.Join(tracking, path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Reproduce a catalog entry with a workspace-relative path. Direct links
	// select design.md relative to the tracking directory, independently of it.
	manifest := "schema: 1\nslug: example\nrepositories: []\nartifacts:\n  - label: Design\n    path: work/example/design.md\n"
	write("portal.yml", manifest)
	write("design.md", "# Rust rewrite plan\n")
	runWebGit(t, "-C", root, "add", "--", "work/example/design.md")
	write(".gitignore", "*.secret\ndesign.md\n")
	write("reports/new space ž.md", "untracked session document\n")
	write("private.secret", "ignored session content\n")
	for _, path := range []string{"design.md", "reports/new space ž.md"} {
		file := readSource(t, s, "?"+url.Values{"artifact": {path}}.Encode(), 200)
		if file.Source != "artifact" || file.Path != path || file.Content.Text == "" {
			t.Fatalf("tracking source %q = %#v", path, file)
		}
	}
	readSource(t, s, "?artifact=private.secret", 200)
	readSource(t, s, "?artifact=missing.md", 404)
	readSource(t, s, "?artifact=reports", 404)
	readSource(t, s, "?artifact=.git/config", 404)
	// A retained own-workspace prefix aliases a missing literal nested file.
	// Both source and legacy preview routes read unregistered session files.
	reviewRequest(t, s, "GET", "/api/sessions/example/artifact-preview?path=design.md", "", 200)
	reviewRequest(t, s, "GET", "/api/sessions/example/artifact-preview?path=reports/new%20space%20%C5%BE.md", "", 200)
	if err := os.Symlink("design.md", filepath.Join(tracking, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(tracking, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	readSource(t, s, "?artifact=link.md", 404)
	readSource(t, s, "?artifact=fifo", 404)
	// Legacy publication metadata remains readable but does not govern access.
	manifest += "  - label: Published report\n    path: private.secret\n"
	write("portal.yml", manifest)
	readSource(t, s, "?artifact=private.secret", 200)
	writeWebTrackingFiles(t, tracking, "complete")
	write("portal.yml", manifest+"finalized_at: '2026-10-18T10:00:00Z'\n")
	archive := filepath.Join(root, "archive", "example")
	if err := os.MkdirAll(filepath.Dir(archive), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tracking, archive); err != nil {
		t.Fatal(err)
	}
	file := readSource(t, s, "?artifact=reports%2Fnew+space+%C5%BE.md", 200)
	if file.Source != "archived-artifact" || file.Content.Text != "untracked session document\n" {
		t.Fatalf("archived tracking source = %#v", file)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", tracking+"/design.md:1", nil))
	if response.Code != 302 || response.Header().Get("Location") != "/files/example?artifact=design.md#L1" {
		t.Fatalf("retained tracking link: %d %s", response.Code, response.Body.String())
	}
}

func TestSourceFilesRespectConfiguredGlobalExcludes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	exclude := filepath.Join(home, "global-ignore")
	if err := os.WriteFile(exclude, []byte("private.env\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "config", "--global", "core.excludesFile", exclude)
	s, _, worktree, _ := reviewWebFixture(t)
	root := s.config.Workspace
	runWebGit(t, "init", "--initial-branch=master", root)
	tracking := filepath.Join(root, "work", "example")
	for _, directory := range []string{root, worktree, tracking} {
		if err := os.WriteFile(filepath.Join(directory, "private.env"), []byte("synthetic private fixture\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if out := webGitOutput(t, "-C", directory, "--literal-pathspecs", "ls-files", "--others", "--exclude-standard", "-z", "--", "private.env"); out != "" {
			t.Fatalf("ordinary Git did not exclude the fixture: %q", out)
		}
	}
	readSource(t, s, sourceQuery("private.env"), 404)
	readWorkspaceSource(t, s, "private.env", 404)
	readSource(t, s, "?artifact=private.env", 200)
	// Repository/workspace index eligibility stays independent of session files.
	runWebGit(t, "-C", worktree, "add", "--force", "private.env")
	runWebGit(t, "-C", root, "add", "--force", "private.env")
	readSource(t, s, sourceQuery("private.env"), 200)
	readWorkspaceSource(t, s, "private.env", 200)
	manifest, err := os.ReadFile(filepath.Join(tracking, "portal.yml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest = append(manifest, []byte("artifacts:\n  - label: Published\n    path: private.env\n")...)
	if err := os.WriteFile(filepath.Join(tracking, "portal.yml"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	readSource(t, s, "?artifact=private.env", 200)
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
	readSource(t, s, "?artifact=private.md", 200)
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
	download := httptest.NewRecorder()
	s.Handler().ServeHTTP(download, httptest.NewRequest(http.MethodGet, "/api/sessions/example/file/download"+sourceQuery("file"), nil))
	if download.Code != 200 || download.Body.String() != "after\n" {
		t.Fatalf("archived raw download: %d %q", download.Code, download.Body.String())
	}
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
		if strings.Contains(path, ".git/") || strings.Contains(path, "\\") {
			readSource(t, s, query, 404)
			continue
		}
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

func TestSourceDownloadsReuseEligibilityAndPreserveBytes(t *testing.T) {
	s, _, worktree, _ := reviewWebFixture(t)
	request := func(path string, status int) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != status {
			t.Fatalf("download %s: %d %s", path, response.Code, response.Body.String())
		}
		return response
	}
	raw := []byte{0, 255, 'a', '\r', '\n'}
	name := "space ž %.bin"
	if err := os.WriteFile(filepath.Join(worktree, name), raw, 0600); err != nil {
		t.Fatal(err)
	}
	result := request("/api/sessions/example/file/download"+sourceQuery(name), 200)
	if !bytes.Equal(result.Body.Bytes(), raw) || result.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatal("download changed original bytes")
	}
	disposition, params, err := mime.ParseMediaType(result.Header().Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || params["filename"] != name {
		t.Fatalf("unsafe disposition: %v %v", params, err)
	}
	large := bytes.Repeat([]byte("x"), repository.MaxReviewBlobBytes+1)
	if err := os.WriteFile(filepath.Join(worktree, "large"), large, 0600); err != nil {
		t.Fatal(err)
	}
	if !readSource(t, s, sourceQuery("large"), 200).Content.Limited {
		t.Fatal("expected omitted preview")
	}
	if !bytes.Equal(request("/api/sessions/example/file/download"+sourceQuery("large"), 200).Body.Bytes(), large) {
		t.Fatal("limited preview prevented raw download")
	}
	if err := os.WriteFile(filepath.Join(worktree, "oversized"), bytes.Repeat([]byte("x"), repository.MaxSourceDownloadBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	request("/api/sessions/example/file/download"+sourceQuery("oversized"), 413)
	if err := os.WriteFile(filepath.Join(worktree, ".gitignore"), []byte("private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "private"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(name, filepath.Join(worktree, "linked")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"private", "linked"} {
		request("/api/sessions/example/file/download"+sourceQuery(path), 404)
	}
	request("/api/sessions/example/file/download"+sourceQuery("../escape"), 400)
	request("/api/sessions/example/file/download"+sourceQuery(".git/config"), 400)
	request("/api/sessions/example/file/download?artifact=plan.md", 200)
}

func TestUserMarkdownUsesDecoratedTextWithoutChangingCanonicalInput(t *testing.T) {
	s := newTestServer(t)
	display := "# Display heading\n\n- item\n\n```sh\necho ok\n```\n\n[unsafe](javascript:alert(1))\n<script>alert(1)</script>"
	original := "Canonical attachment envelope"
	transcript := codex.Transcript{Entries: []codex.TranscriptEntry{{Kind: "userMessage", Text: original, DisplayText: &display}}}
	s.presentTranscript(&transcript)
	entry := transcript.Entries[0]
	if entry.Text != original || entry.DisplayText == nil || *entry.DisplayText != display {
		t.Fatal("canonical/decorated input changed")
	}
	for _, markup := range []string{"<h1>Display heading</h1>", "<ul>", "<pre><code"} {
		if !strings.Contains(entry.HTML, markup) {
			t.Fatalf("missing Markdown %s: %s", markup, entry.HTML)
		}
	}
	if strings.Contains(entry.HTML, "javascript:") || strings.Contains(entry.HTML, "<script>") {
		t.Fatalf("unsafe user HTML: %s", entry.HTML)
	}
	for _, text := range []string{"Team assignment from lead to implementer0:\n\nDo work.", "quoted Team assignment from lead to implementer0:\nDo work.", "Team assignment from <unsafe> to lead:\nDo work."} {
		transcript.Entries = []codex.TranscriptEntry{{Kind: "userMessage", Text: text}}
		s.presentTranscript(&transcript)
		team := transcript.Entries[0].Communication
		if strings.HasPrefix(text, "Team assignment from lead") {
			if team == nil || team.From != "lead" || team.To != "implementer0" {
				t.Fatalf("missing envelope: %#v", team)
			}
		} else if team != nil {
			t.Fatalf("ambiguous envelope classified: %#v", team)
		}
	}
}

func TestTrackingFilesListIncludesIgnoredAndUnregistered(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	root := s.config.Workspace
	runWebGit(t, "init", "--initial-branch=master", root)
	tracking := filepath.Join(root, "work", "example")
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(tracking, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("tracked.sh", "#!/bin/sh\r\necho passive\r\n")
	runWebGit(t, "-C", root, "add", "--", "work/example/tracked.sh")
	write(".gitignore", "*.sh\n*.secret\n")
	write("ignored.sh", "unpublished")
	write("registered.sh", "registered original\r\n")
	write("notes/a space.py", "print('passive')\n")
	write("global.secret", "unpublished")
	write("design.md", "# First label\n")
	excludes := filepath.Join(t.TempDir(), "excludes")
	if err := os.WriteFile(excludes, []byte("*.excluded\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", root, "config", "core.excludesFile", excludes)
	write("private.excluded", "unpublished")
	manifest := "schema: 1\nslug: example\nrepositories: []\nartifacts:\n" +
		"  - label: First label\n    path: design.md\n" +
		"  - label: Alias label\n    path: work/example/design.md\n" +
		"  - label: Published script\n    path: registered.sh\n"
	write("portal.yml", manifest)
	if err := os.Symlink("design.md", filepath.Join(tracking, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(tracking, "pipe.txt"), 0600); err != nil {
		t.Fatal(err)
	}
	response := reviewRequest(t, s, "GET", "/api/sessions/example/files", "", 200)
	encoded, _ := json.Marshal(response["files"])
	var listed []repository.TrackingFile
	if err := json.Unmarshal(encoded, &listed); err != nil {
		t.Fatal(err)
	}
	rows := map[string]repository.TrackingFile{}
	for _, row := range listed {
		if _, duplicate := rows[row.Path]; duplicate {
			t.Fatalf("duplicate row %q", row.Path)
		}
		rows[row.Path] = row
		readSource(t, s, "?"+url.Values{"artifact": {row.Path}}.Encode(), 200)
	}
	for _, path := range []string{"tracked.sh", "registered.sh", "notes/a space.py", "design.md", "ignored.sh", "global.secret", "private.excluded"} {
		if _, ok := rows[path]; !ok {
			t.Fatalf("eligible file missing: %q (%s)", path, encoded)
		}
	}
	for _, path := range []string{"link.md", "pipe.txt", "work/example/design.md"} {
		if _, ok := rows[path]; ok {
			t.Fatalf("ineligible/duplicate file listed: %q", path)
		}
	}
	for _, row := range listed {
		if row.Label != "" || row.Registered {
			t.Fatal("listing projected obsolete registration metadata")
		}
	}
	// Scripts remain passive text in both the legacy preview and source viewer.
	reviewRequest(t, s, "GET", "/api/sessions/example/artifact-preview?path=registered.sh", "", 200)
	raw := httptest.NewRecorder()
	s.Handler().ServeHTTP(raw, httptest.NewRequest("GET", "/api/sessions/example/file/download?artifact=registered.sh", nil))
	if raw.Code != 200 || raw.Body.String() != "registered original\r\n" || raw.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("passive script download: %d", raw.Code)
	}
}

func TestTrackingFilesAliasKeepsLiteralNestedIdentity(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	tracking := filepath.Join(s.config.Workspace, "work", "example")
	for _, path := range []string{"design.md", "work/example/design.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(tracking, path)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tracking, path), []byte(path), 0644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := "schema: 1\nslug: example\nrepositories: []\nartifacts:\n  - label: Short\n    path: design.md\n  - label: Nested\n    path: work/example/design.md\n"
	if err := os.WriteFile(filepath.Join(tracking, "portal.yml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"design.md", "work/example/design.md"} {
		file := readSource(t, s, "?"+url.Values{"artifact": {path}}.Encode(), 200)
		if file.Path != path || file.Content.Text != path {
			t.Fatalf("literal alias %q resolved elsewhere: %#v", path, file)
		}
	}
	if err := os.Remove(filepath.Join(tracking, "work/example/design.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tracking, "design.md"), filepath.Join(tracking, "work/example/design.md")); err != nil {
		t.Fatal(err)
	}
	readSource(t, s, "?artifact=work/example/design.md", 404)
}

func TestTrackingFilesRegisteredFallbackAndArchiveAlias(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	tracking := filepath.Join(s.config.Workspace, "work", "example")
	manifest := "schema: 1\nslug: example\nrepositories: []\nartifacts:\n  - label: Script\n    path: work/example/view.sh\n"
	for name, content := range map[string]string{"portal.yml": manifest, "view.sh": "<script>passive</script>\r\n", "unregistered.py": "unpublished"} {
		if err := os.WriteFile(filepath.Join(tracking, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		t.Helper()
		result := reviewRequest(t, s, "GET", "/api/sessions/example/files", "", 200)
		encoded, _ := json.Marshal(result["files"])
		var rows []repository.TrackingFile
		if err := json.Unmarshal(encoded, &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 5 {
			t.Fatalf("no-Git files: %s", encoded)
		}
		for _, row := range rows {
			readSource(t, s, "?"+url.Values{"artifact": {row.Path}}.Encode(), 200)
		}
		readSource(t, s, "?artifact=work/example/view.sh", 200)
		readSource(t, s, "?artifact=archive/example/view.sh", 200)
		readSource(t, s, "?artifact=unregistered.py", 200)
	}
	check()
	writeWebTrackingFiles(t, tracking, "complete")
	if err := os.WriteFile(filepath.Join(tracking, "portal.yml"), []byte(manifest+"finalized_at: '2026-10-18T10:00:00Z'\n"), 0644); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(s.config.Workspace, "archive", "example")
	if err := os.MkdirAll(filepath.Dir(archive), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tracking, archive); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestTrackingFilesRefusesOversizedWholeResult(t *testing.T) {
	for _, outputBound := range []bool{false, true} {
		t.Run(fmt.Sprint("output-", outputBound), func(t *testing.T) {
			s, _, _, _ := reviewWebFixture(t)
			root := s.config.Workspace
			runWebGit(t, "init", "--initial-branch=master", root)
			count, prefix := 5001, ""
			if outputBound {
				count, prefix = 4500, strings.Repeat("long/", 200)
			}
			directory := filepath.Join(root, "work", "example", prefix)
			if err := os.MkdirAll(directory, 0755); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < count; i++ {
				if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("%04d.txt", i)), nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/sessions/example/files", nil))
			if response.Code != 503 || !strings.Contains(response.Body.String(), "The file list is too large to display (limit: 5,000 files or 4 MiB).") || strings.Contains(response.Body.String(), `"files"`) {
				t.Fatalf("bounded whole result: %d %s", response.Code, response.Body.String())
			}
		})
	}
}
