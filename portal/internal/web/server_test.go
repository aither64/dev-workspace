package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/vpsfree-cz-workspace/portal/internal/codex"
	"github.com/aither64/vpsfree-cz-workspace/portal/internal/session"
)

func TestMarkdownIsSanitized(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := "# Plan\n\n<script>alert(1)</script>\n\n[bad](javascript:alert(1))\n\n" +
		"[encoded](&#106;avascript:alert(1))\n\n<javascript:alert(document.domain)>\n\n" +
		"![alt](javascript:alert(document.domain))\n"
	if err := os.WriteFile(filepath.Join(directory, "plan.md"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	summary := &session.Summary{Manifest: session.Manifest{Slug: "example"}, Workspace: server.config.Workspace, Root: "work"}
	rendered := string(server.renderMarkdown(summary, "plan.md"))
	if strings.Contains(rendered, "<script") || strings.Contains(rendered, `href="javascript:`) ||
		strings.Contains(rendered, `src="javascript:`) || strings.Contains(rendered, "&#106;avascript:") {
		t.Fatalf("unsafe Markdown output: %s", rendered)
	}
}

func TestArtifactsUseAPassiveAllowlistAndDownloadDisposition(t *testing.T) {
	for _, extension := range []string{".shtml", ".ehtml", ".html", ".svg", ".svgz", ".js", ".xhtml", ".xml"} {
		t.Run(extension, func(t *testing.T) {
			server := newTestServer(t)
			writeArtifactSession(t, server.config.Workspace, "report"+extension)
			request := httptest.NewRequest(http.MethodGet, "/artifacts/example/report"+extension, nil)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d", response.Code)
			}
		})
	}

	server := newTestServer(t)
	writeArtifactSession(t, server.config.Workspace, "report.txt")
	request := httptest.NewRequest(http.MethodGet, "/artifacts/example/report.txt", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if !strings.HasPrefix(response.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("Content-Disposition = %q", response.Header().Get("Content-Disposition"))
	}
	if response.Header().Get("Content-Security-Policy") != "sandbox; default-src 'none'" {
		t.Fatalf("artifact CSP = %q", response.Header().Get("Content-Security-Policy"))
	}
}

func writeArtifactSession(t *testing.T, workspace, name string) {
	t.Helper()
	directory := filepath.Join(workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\nartifacts:\n  - label: Report\n    path: " + name + "\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	if err := os.WriteFile(filepath.Join(directory, name), []byte("artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeWebTrackingFiles(t *testing.T, directory, lifecycle string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, "state.md"), []byte("---\nlifecycle: "+lifecycle+"\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "plan.md"), []byte("# Plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMutationRequiresExactOrigin(t *testing.T) {
	handler := newTestServer(t).Handler()
	for name, origin := range map[string]string{
		"missing": "",
		"null":    "null",
		"wrong":   "https://unexpected.example.test",
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/not-a-route", strings.NewReader("{}"))
			request.Header.Set("Origin", origin)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d", response.Code)
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, "/not-a-route", strings.NewReader("{}"))
	request.Header.Set("Origin", "https://workspace.example.test")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("same-origin status = %d", response.Code)
	}
}

func TestSecurityResponsesAreNotCached(t *testing.T) {
	response := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if actual := response.Header().Get("Cache-Control"); actual != "no-store" {
		t.Fatalf("Cache-Control = %q", actual)
	}
	if actual := response.Header().Get("Strict-Transport-Security"); actual != "" {
		t.Fatalf("backend must leave Strict-Transport-Security to nginx, got %q", actual)
	}
	if actual := response.Header().Get("Referrer-Policy"); actual != "same-origin" {
		t.Fatalf("Referrer-Policy = %q", actual)
	}
}

func TestNewRejectsAnHTTPBaseURL(t *testing.T) {
	_, err := New(Config{Workspace: t.TempDir(), BaseURL: "http://workspace.example.test"})
	if err == nil || !strings.Contains(err.Error(), "requires an HTTPS") {
		t.Fatalf("HTTP base URL result = %v", err)
	}
}

func TestNewRequiresAnAbsoluteInstalledDevSessionCommand(t *testing.T) {
	for _, command := range []string{"", "dev-session"} {
		_, err := New(Config{
			Workspace: t.TempDir(), BaseURL: "https://workspace.example.test", DevSession: command,
		})
		if err == nil || !strings.Contains(err.Error(), "absolute dev-session") {
			t.Fatalf("dev-session %q result = %v", command, err)
		}
	}
}

func TestSessionCreationKeepsStandardOutputSeparateFromWarnings(t *testing.T) {
	server := newTestServer(t)
	helper := filepath.Join(t.TempDir(), "dev-session")
	script := "#!/bin/sh\nprintf 'diagnostic warning\\n' >&2\nprintf '{\"slug\":\"2026-09-03-example\",\"threadId\":\"thread-1\"}\\n'\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	server.config.DevSession = helper
	request := httptest.NewRequest(
		http.MethodPost,
		"/sessions",
		strings.NewReader("creation_date=2026-09-03&name=example&goal=Implement+the+feature"),
	)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", server.config.BaseURL)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/2026-09-03-example/" {
		t.Fatalf("Location = %q", location)
	}
}

func TestSessionCreationAcceptsMaximallyEncodedMessageAtPublishedLimit(t *testing.T) {
	server := newTestServer(t)
	helper := filepath.Join(t.TempDir(), "dev-session")
	script := "#!/bin/sh\nprintf '{\"slug\":\"2026-09-03-example\",\"threadId\":\"thread-1\"}\\n'\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	server.config.DevSession = helper
	goal := strings.Repeat("é", session.MaxMessageBytes/len("é"))
	form := url.Values{
		"creation_date": {"2026-09-03"},
		"name":          {"example"},
		"goal":          {goal},
	}.Encode()
	if len([]byte(form)) <= 32*1024 || len([]byte(form)) > session.MaxFormRequestBodyBytes {
		t.Fatalf("encoded form size = %d, ceiling = %d", len([]byte(form)), session.MaxFormRequestBodyBytes)
	}
	request := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", server.config.BaseURL)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
}

func TestJSONTransportAcceptsMaximallyEscapedMessageAtPublishedLimit(t *testing.T) {
	server := newTestServer(t)
	body := `{"message":"` + strings.Repeat(`\u0000`, session.MaxMessageBytes) + `"}`
	if len([]byte(body)) <= 64*1024 || len([]byte(body)) > session.MaxJSONRequestBodyBytes {
		t.Fatalf("encoded JSON size = %d, ceiling = %d", len([]byte(body)), session.MaxJSONRequestBodyBytes)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/example/message", strings.NewReader(body))
	response := httptest.NewRecorder()
	var payload struct {
		Message string `json:"message"`
	}
	if !server.decodeJSON(response, request, &payload) {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if len([]byte(payload.Message)) != session.MaxMessageBytes {
		t.Fatalf("decoded message size = %d", len([]byte(payload.Message)))
	}
}

func TestSessionCreationPassesOnlyPublicArgumentsToTheInstalledCommand(t *testing.T) {
	server := newTestServer(t)
	server.config.CodexSocket = "/run/vpsfree-workspace-codex/app-server.sock"
	server.config.CodexVersion = "0.152.1"
	directory := t.TempDir()
	arguments := filepath.Join(directory, "arguments")
	helper := filepath.Join(directory, "dev-session")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$ARGUMENTS\"\nprintf '{\"slug\":\"2026-09-03-example\",\"threadId\":\"thread-1\"}\\n'\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUMENTS", arguments)
	server.config.DevSession = helper
	request := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(
		"creation_date=2026-09-03&name=example&goal=Implement+the+feature",
	))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", server.config.BaseURL)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(
		"creation_date=2026-09-03&name=example&goal=Implement+the+feature",
	))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", server.config.BaseURL)
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("replay status = %d, body = %q", response.Code, response.Body.String())
	}
	data, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSpace(string(data)), "\n")
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "start 2026-09-03-example --as-is --exclusive") || strings.Contains(joined, "--new") {
		t.Fatalf("dev-session arguments = %q", argv)
	}
	for _, privateFlag := range []string{
		"--require-runtime", "--workspace", "--tmux-socket", "--authority-dir",
		"--codex-socket", "--codex-version", "--codex-command",
		"--portal-base-url", "--portal-command",
	} {
		if strings.Contains(joined, privateFlag) {
			t.Fatalf("portal passed private flag %q in %q", privateFlag, argv)
		}
	}
}

func TestSessionPageUsesOnlyTrustedLiveRuntimeAuthority(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		authority    bool
		codexVersion string
		wantAttach   bool
	}{
		{"trusted-live", true, "0.152.1", true},
		{"compatible-upgrade", true, "0.153.0", true},
		{"manifest-only", false, "0.152.1", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTestServer(t)
			server.config.CodexVersion = testCase.codexVersion
			directory := filepath.Join(server.config.Workspace, "work", "example")
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal(err)
			}
			manifest := "schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\n  socket_path: /run/vpsfree-workspace-codex/app-server.sock\n  client_version: 0.152.1\n" +
				"creation:\n  state: ready\n  initial_goal_sent: true\n"
			if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			writeWebTrackingFiles(t, directory, "active")
			if testCase.authority {
				writeWebRuntimeAuthority(t, server, "example")
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/example/", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
			}
			hasAttach := strings.Contains(response.Body.String(), "dev-session attach example\"")
			if hasAttach != testCase.wantAttach {
				t.Fatalf("attach visibility = %t, want %t", hasAttach, testCase.wantAttach)
			}
			if strings.Contains(response.Body.String(), "dev-session attach example --as-is") {
				t.Fatal("session page advertises an unnecessary --as-is option")
			}
			if testCase.authority {
				body := response.Body.String()
				codexEnd := strings.Index(body, `<section id="handoff"`)
				if codexEnd < 0 || strings.Contains(body[:codexEnd], `id="codex-settings"`) {
					t.Fatal("Codex settings form is still visible above the transcript")
				}
				for _, marker := range []string{`id="codex-settings-open"`, `id="codex-settings-dialog"`} {
					if !strings.Contains(body, marker) {
						t.Fatalf("session page is missing %s", marker)
					}
				}
			}
		})
	}
}

func TestSessionWithoutAThreadExplainsHowToStartSharedSessions(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "legacy")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 2\nslug: legacy\ncodex: {}\ncreation:\n  state: creating\n  initial_goal_sent: false\n  initial_goal_attempted: false\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/legacy/", nil))
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "This session has no shared Codex conversation") ||
		!strings.Contains(response.Body.String(), "dev-session start &lt;short-name&gt;") {
		t.Fatalf("legacy page = %d %q", response.Code, response.Body.String())
	}
}

func writeWebRuntimeAuthority(t *testing.T, server *Server, slug string) {
	t.Helper()
	authority := session.RuntimeAuthority{
		Schema: 1, State: "ready", Slug: slug, Workspace: server.config.Workspace,
		TmuxSocket: "/run/vpsfree-workspace-tmux/tmux.sock", TmuxSessionID: "$1",
		CodexThreadID: "thread-1", CodexSocketPath: server.config.CodexSocket,
		CodexClientVersion: "0.152.1",
	}
	data, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(server.config.AuthorityDir, slug+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	tmux := filepath.Join(t.TempDir(), "tmux")
	line := strings.Join([]string{
		"$1", slug, "1", slug, server.config.Workspace, slug,
		authority.TmuxSocket, authority.CodexThreadID, authority.CodexSocketPath,
		authority.CodexClientVersion, "%1",
	}, "\t")
	if err := os.WriteFile(tmux, []byte("#!/bin/sh\nprintf '%s\\n' '"+line+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	server.config.Tmux = tmux
	server.config.VerifyThread = func(_ context.Context, threadID, cwd string) error {
		if threadID != authority.CodexThreadID || cwd != filepath.Join(server.config.Workspace, "work", slug) {
			return errors.New("unexpected thread identity")
		}
		return nil
	}
}

func TestNonInteractiveSessionRejectsEventStreams(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\ncreation:\n  state: creating\nrepositories: []\nartifacts: []\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	request := httptest.NewRequest(http.MethodGet, "/api/sessions/example/events", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
}

func TestClosedSessionDoesNotRenderMutationControls(t *testing.T) {
	server := newTestServer(t)
	response := httptest.NewRecorder()
	server.render(response, "session", pageData{
		BaseURL: "https://workspace.example.test",
		Session: &session.Summary{
			Manifest: session.Manifest{Slug: "example", Codex: session.Codex{ThreadID: "thread-1"}},
			Closed:   true,
		},
	})
	body := response.Body.String()
	for _, marker := range []string{`id="pending"`, `id="message-form"`, `id="interrupt"`} {
		if strings.Contains(body, marker) {
			t.Fatalf("closed session rendered %s", marker)
		}
	}
}

func TestSessionPageUsesFullWidthTopLevelTabs(t *testing.T) {
	server := newTestServer(t)
	response := httptest.NewRecorder()
	server.render(response, "session", pageData{
		BaseURL: "https://workspace.example.test",
		Session: &session.Summary{Manifest: session.Manifest{
			Slug: "example", Codex: session.Codex{ThreadID: "thread-1"},
			Artifacts: []session.Artifact{{Label: "Report", Path: "report.md"}},
		}},
	})
	body := response.Body.String()
	for _, marker := range []string{
		`class="panel session-tabs"`, `data-tab="codex"`, `data-tab="handoff"`,
		`data-tab="repositories"`, `data-tab="clusters"`, `data-tab="plan"`, `data-tab="state"`,
		`id="codex" class="tab-panel chat-panel active"`, `>Report</a>`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("session page lacks %s", marker)
		}
	}
	for _, oldLayout := range []string{"session-layout", "chat-column", "workspace-column"} {
		if strings.Contains(body, oldLayout) {
			t.Fatalf("session page still contains %q", oldLayout)
		}
	}
}

func TestBrowserClientShipsMessageAndLifecycleInteractions(t *testing.T) {
	javascript, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"event.key !== \"Enter\"", "event.shiftKey", "event.isComposing", "form.requestSubmit()",
		"entry.html", "finish-session", "archive-session", "release-cluster", "fork-dialog",
		"codex-settings-dialog", "codex-settings-open", "modelSelect.required",
	} {
		if !strings.Contains(string(javascript), marker) {
			t.Fatalf("browser client does not contain %q", marker)
		}
	}
}

func TestAutomaticReasoningIsValidInSettingsAndForkForms(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\n" +
		"  socket_path: /run/vpsfree-workspace-codex/app-server.sock\n  client_version: 0.152.1\n" +
		"creation:\n  state: ready\n  initial_goal_sent: true\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	writeWebRuntimeAuthority(t, server, "example")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/example/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if count := strings.Count(body, `name="effort" data-effort-select>`); count != 2 {
		t.Fatalf("automatic effort selects = %d", count)
	}
	if strings.Contains(body, `name="effort" data-effort-select required`) {
		t.Fatal("automatic reasoning is blocked by native required validation")
	}
}

func TestForkSessionInvokesUnifiedDevSessionCommand(t *testing.T) {
	server := newTestServer(t)
	directory := t.TempDir()
	arguments := filepath.Join(directory, "arguments")
	helper := filepath.Join(directory, "dev-session")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGUMENTS\"\nprintf '{\"slug\":\"2026-09-04-forked\"}\\n'\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUMENTS", arguments)
	server.config.DevSession = helper
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/source/fork", strings.NewReader(
		`{"name":"forked","creationDate":"2026-09-04"}`,
	))
	response := httptest.NewRecorder()
	server.forkSession(response, request, &session.Summary{Manifest: session.Manifest{Slug: "source"}})
	if response.Code != http.StatusCreated {
		t.Fatalf("status/body = %d %q", response.Code, response.Body.String())
	}
	data, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(strings.Fields(string(data)), " ")
	if !strings.Contains(joined, "fork source 2026-09-04-forked --as-is --json") {
		t.Fatalf("fork arguments = %q", joined)
	}
}

func TestCommitArchivePreservesUnrelatedWorkspaceChanges(t *testing.T) {
	server := newTestServer(t)
	workspace := server.config.Workspace
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", workspace}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return string(output)
	}
	git("init", "--initial-branch=master")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	slug := "2026-09-04-complete"
	work := filepath.Join(workspace, "work", slug)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "state.md"), []byte("complete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name := range map[string]bool{"staged.txt": true, "unstaged.txt": true} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("initial\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-m", "initial")
	remote := filepath.Join(t.TempDir(), "origin.git")
	if output, err := exec.Command("git", "init", "--bare", "--initial-branch=master", remote).CombinedOutput(); err != nil {
		t.Fatalf("init remote: %s: %v", output, err)
	}
	git("remote", "add", "origin", remote)
	git("push", "-u", "origin", "master")
	if err := os.MkdirAll(filepath.Join(workspace, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(work, filepath.Join(workspace, "archive", slug)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "staged.txt"), []byte("staged change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "staged.txt")
	if err := os.WriteFile(filepath.Join(workspace, "unstaged.txt"), []byte("unstaged change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := server.commitArchive(context.Background(), slug); err != nil {
		t.Fatal(err)
	}
	changed := strings.Fields(git("show", "--format=", "--name-only", "HEAD"))
	if len(changed) != 1 || changed[0] != filepath.Join("archive", slug, "state.md") {
		t.Fatalf("archive commit paths = %#v", changed)
	}
	status := git("status", "--porcelain=v1")
	if !strings.Contains(status, "M  staged.txt") || !strings.Contains(status, " M unstaged.txt") {
		t.Fatalf("unrelated status was not preserved: %q", status)
	}
}

func TestCommitArchiveHookFailureLeavesTheSharedIndexUntouchedAndCanRetry(t *testing.T) {
	server := newTestServer(t)
	workspace := server.config.Workspace
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", workspace}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return string(output)
	}
	git("init", "--initial-branch=master")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	slug := "2026-09-05-hook-failure"
	work := filepath.Join(workspace, "work", slug)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "state.md"), []byte("complete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "staged.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "initial")
	remote := filepath.Join(t.TempDir(), "origin.git")
	if output, err := exec.Command("git", "init", "--bare", "--initial-branch=master", remote).CombinedOutput(); err != nil {
		t.Fatalf("init remote: %s: %v", output, err)
	}
	git("remote", "add", "origin", remote)
	git("push", "-u", "origin", "master")
	if err := os.MkdirAll(filepath.Join(workspace, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(work, filepath.Join(workspace, "archive", slug)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "staged.txt")
	before := git("status", "--porcelain=v1")
	hook := filepath.Join(workspace, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := server.commitArchive(context.Background(), slug); err == nil {
		t.Fatal("archive commit unexpectedly passed its failing hook")
	}
	if after := git("status", "--porcelain=v1"); after != before {
		t.Fatalf("hook failure changed shared index/status:\n before: %q\n after:  %q", before, after)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".git", "index.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hook failure left index lock: %v", err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := server.commitArchive(context.Background(), slug); err != nil {
		t.Fatalf("retry archive commit: %v", err)
	}
	if changed := strings.Fields(git("show", "--format=", "--name-only", "HEAD")); len(changed) != 1 || changed[0] != filepath.Join("archive", slug, "state.md") {
		t.Fatalf("retry archive paths = %#v", changed)
	}
}

func TestCommitArchiveRejectsAConcurrentMasterAdvanceWithoutRevertingIt(t *testing.T) {
	server := newTestServer(t)
	workspace := server.config.Workspace
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", workspace}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return string(output)
	}
	git("init", "--initial-branch=master")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	slug := "2026-09-05-concurrent"
	work := filepath.Join(workspace, "work", slug)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "state.md"), []byte("complete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "unrelated.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "initial")
	remote := filepath.Join(t.TempDir(), "origin.git")
	if output, err := exec.Command("git", "init", "--bare", "--initial-branch=master", remote).CombinedOutput(); err != nil {
		t.Fatalf("init remote: %s: %v", output, err)
	}
	git("remote", "add", "origin", remote)
	git("push", "-u", "origin", "master")
	if err := os.MkdirAll(filepath.Join(workspace, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(work, filepath.Join(workspace, "archive", slug)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKSPACE", workspace)
	hook := filepath.Join(workspace, ".git", "hooks", "pre-commit")
	script := "#!/bin/sh\n" +
		"unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE\n" +
		"parent=$(git -C \"$WORKSPACE\" rev-parse refs/heads/master)\n" +
		"tree=$(git -C \"$WORKSPACE\" rev-parse \"$parent^{tree}\")\n" +
		"concurrent=$(printf 'concurrent\\n' | git -C \"$WORKSPACE\" commit-tree \"$tree\" -p \"$parent\")\n" +
		"git -C \"$WORKSPACE\" update-ref refs/heads/master \"$concurrent\" \"$parent\"\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := server.commitArchive(context.Background(), slug)
	if err == nil {
		t.Fatalf("archive result after concurrent commit = %v", err)
	}
	if subject := strings.TrimSpace(git("log", "-1", "--format=%s")); subject != "concurrent" {
		t.Fatalf("concurrent master head = %q", subject)
	}
	if content := git("show", "HEAD:unrelated.txt"); content != "initial\n" {
		t.Fatalf("concurrent content was reverted: %q", content)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := server.commitArchive(context.Background(), slug); err != nil {
		t.Fatalf("retry archive commit: %v", err)
	}
	if content := git("show", "HEAD:unrelated.txt"); content != "initial\n" {
		t.Fatalf("retry reverted concurrent content: %q", content)
	}
}

func TestCommitArchiveCancellationCleansTheIndexAndCanRetry(t *testing.T) {
	server := newTestServer(t)
	workspace := server.config.Workspace
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", workspace}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return string(output)
	}
	git("init", "--initial-branch=master")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	slug := "2026-09-05-canceled-commit"
	work := filepath.Join(workspace, "work", slug)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "state.md"), []byte("complete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "staged.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "initial")
	remote := filepath.Join(t.TempDir(), "origin.git")
	if output, err := exec.Command("git", "init", "--bare", "--initial-branch=master", remote).CombinedOutput(); err != nil {
		t.Fatalf("init remote: %s: %v", output, err)
	}
	git("remote", "add", "origin", remote)
	git("push", "-u", "origin", "master")
	if err := os.MkdirAll(filepath.Join(workspace, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(work, filepath.Join(workspace, "archive", slug)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "staged.txt")
	before := git("status", "--porcelain=v1")
	started := filepath.Join(t.TempDir(), "hook-started")
	t.Setenv("STARTED", started)
	hook := filepath.Join(workspace, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf started > \"$STARTED\"\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.commitArchive(ctx, slug) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("pre-commit hook did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-result; err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("canceled archive commit result = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".git", "index.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled Git left its index lock: %v", err)
	}
	if after := git("status", "--porcelain=v1"); after != before {
		t.Fatalf("canceled archive changed shared status:\n before: %q\n after:  %q", before, after)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := server.commitArchive(context.Background(), slug); err != nil {
		t.Fatalf("retry canceled archive commit: %v", err)
	}
	status := git("status", "--porcelain=v1")
	if strings.Contains(status, slug) || !strings.Contains(status, "M  staged.txt") {
		t.Fatalf("retry did not preserve unrelated staged work: %q", status)
	}
}

func TestCloseCancelsAndDrainsArchiveOperations(t *testing.T) {
	server := newTestServer(t)
	started := filepath.Join(t.TempDir(), "started")
	helper := filepath.Join(t.TempDir(), "cluster-helper")
	script := "#!/bin/sh\nprintf started > \"$STARTED\"\nsleep 30\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STARTED", started)
	server.clusters.Vpsadmin = helper
	server.clusters.VpsadminOS = helper
	response := httptest.NewRecorder()
	server.startArchive(response, &session.Summary{
		Manifest:  session.Manifest{Slug: "2026-09-05-shutdown"},
		Lifecycle: "complete",
	})
	if response.Code != http.StatusAccepted {
		t.Fatalf("archive start status/body = %d %q", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("archive helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	closed := make(chan struct{})
	go func() {
		server.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("server shutdown did not drain the canceled archive operation")
	}
	server.operationMu.Lock()
	operation := server.operations["2026-09-05-shutdown"]
	server.operationMu.Unlock()
	if operation.State != "failed" || !strings.Contains(operation.Error, context.Canceled.Error()) {
		t.Fatalf("archive operation after shutdown = %#v", operation)
	}
	retry := httptest.NewRecorder()
	server.startArchive(retry, &session.Summary{
		Manifest:  session.Manifest{Slug: "2026-09-05-shutdown"},
		Lifecycle: "complete",
	})
	if retry.Code != http.StatusServiceUnavailable {
		t.Fatalf("archive start during shutdown status/body = %d %q", retry.Code, retry.Body.String())
	}
}

func TestStoppedCompleteAndArchivedSessionsKeepVerifiedReadOnlyTranscripts(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		root      string
		lifecycle string
		finalized bool
	}{{"stopped", "work", "active", false}, {"complete", "work", "complete", false}, {"archived", "archive", "complete", true}} {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTestServer(t)
			directory := filepath.Join(server.config.Workspace, testCase.root, "example")
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal(err)
			}
			manifest := "schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\n" +
				"  socket_path: /run/vpsfree-workspace-codex/app-server.sock\n  client_version: 0.152.1\n" +
				"creation:\n  state: ready\n  initial_goal_sent: true\nrepositories: []\nartifacts: []\n"
			if testCase.finalized {
				manifest += "finalized_at: '2026-09-03T12:00:00Z'\n"
			}
			if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			writeWebTrackingFiles(t, directory, testCase.lifecycle)
			expectedCwd := filepath.Join(server.config.Workspace, "work", "example")
			server.config.VerifyThread = func(_ context.Context, threadID, cwd string) error {
				if threadID != "thread-1" || cwd != expectedCwd {
					return errors.New("wrong thread identity")
				}
				return nil
			}
			server.config.ReadThread = func(_ context.Context, threadID string) (codex.Transcript, error) {
				return codex.Transcript{ThreadID: threadID, Status: "idle", Entries: []codex.TranscriptEntry{{
					Kind: "agentMessage", Text: "# Persisted answer\n\n<script>alert(1)</script>",
				}}}, nil
			}

			page := httptest.NewRecorder()
			server.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/example/", nil))
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Loading conversation") {
				t.Fatalf("page status/body = %d %q", page.Code, page.Body.String())
			}
			for _, control := range []string{`id="pending"`, `id="message-form"`, `id="interrupt"`} {
				if strings.Contains(page.Body.String(), control) {
					t.Fatalf("read-only page contains %s", control)
				}
			}

			api := httptest.NewRecorder()
			server.Handler().ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/sessions/example/thread", nil))
			if api.Code != http.StatusOK || !strings.Contains(api.Body.String(), "Persisted answer") {
				t.Fatalf("thread status/body = %d %q", api.Code, api.Body.String())
			}
			var transcript codex.Transcript
			if err := json.Unmarshal(api.Body.Bytes(), &transcript); err != nil {
				t.Fatal(err)
			}
			if len(transcript.Entries) != 1 || !strings.Contains(transcript.Entries[0].HTML, "<h1>Persisted answer</h1>") ||
				strings.Contains(transcript.Entries[0].HTML, "<script") {
				t.Fatalf("sanitized transcript Markdown = %#v", transcript.Entries)
			}
		})
	}
}

func TestCreatingAuthorityNeverGrantsControls(t *testing.T) {
	for _, authorityState := range []string{"creating", "ready"} {
		t.Run(authorityState, func(t *testing.T) {
			server := newTestServer(t)
			directory := filepath.Join(server.config.Workspace, "work", "example")
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal(err)
			}
			manifest := "schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\n  socket_path: /run/vpsfree-workspace-codex/app-server.sock\n  client_version: 0.152.1\n" +
				"creation:\n  state: creating\n  initial_goal_sent: false\n  goal_sha256: " + strings.Repeat("a", 64) + "\n"
			if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			writeWebTrackingFiles(t, directory, "active")
			writeWebRuntimeAuthority(t, server, "example")
			authorityPath := filepath.Join(server.config.AuthorityDir, "example.json")
			var authority session.RuntimeAuthority
			data, _ := os.ReadFile(authorityPath)
			if err := json.Unmarshal(data, &authority); err != nil {
				t.Fatal(err)
			}
			authority.State = authorityState
			data, _ = json.Marshal(authority)
			if err := os.WriteFile(authorityPath, data, 0o600); err != nil {
				t.Fatal(err)
			}

			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/example/", nil))
			if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `id="message-form"`) {
				t.Fatalf("creating page = %d %q", response.Code, response.Body.String())
			}
		})
	}
}

func TestPersistedThreadWithWrongCwdIsNotReadable(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\n  socket_path: /run/vpsfree-workspace-codex/app-server.sock\n  client_version: 0.152.1\ncreation:\n  state: ready\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	server.config.VerifyThread = func(context.Context, string, string) error { return errors.New("wrong cwd") }
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/example/thread", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
}

func TestBrowserClientIncludesFreeFormOtherInput(t *testing.T) {
	javascript, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"question.isOther", `other.value = "__other__"`, "Custom answer"} {
		if !strings.Contains(string(javascript), marker) {
			t.Fatalf("browser client does not contain %q", marker)
		}
	}
}

func TestPendingEndpointEncodesNoPromptsAsAnArray(t *testing.T) {
	server := newTestServer(t)
	server.config.Codex = &browserContractCodex{emptyPrompts: true}
	request := httptest.NewRequest(http.MethodGet, "/api/sessions/example/pending", nil)
	response := httptest.NewRecorder()

	server.pending(response, request, "thread-1")

	if response.Code != http.StatusOK || response.Body.String() != "[]\n" {
		t.Fatalf("empty pending response = %d %q", response.Code, response.Body.String())
	}
}

type browserContractCodex struct {
	mu           sync.Mutex
	message      string
	interrupt    bool
	decision     string
	answers      map[string]map[string][]string
	emptyPrompts bool
}

func (client *browserContractCodex) VerifyThread(_ context.Context, threadID, _ string) error {
	if threadID != "thread-1" {
		return errors.New("unexpected verification thread")
	}
	return nil
}

func (client *browserContractCodex) ReadThread(_ context.Context, threadID string) (codex.Transcript, error) {
	return codex.Transcript{ThreadID: threadID}, nil
}

func (client *browserContractCodex) ListModels(_ context.Context) ([]codex.Model, error) {
	return []codex.Model{{
		ID: "model-1", Model: "model-1", DisplayName: "Model 1", IsDefault: true,
		DefaultReasoningEffort:    "medium",
		SupportedReasoningEfforts: []codex.ReasoningEffortOption{{ReasoningEffort: "medium"}, {ReasoningEffort: "high"}},
	}}, nil
}

func (client *browserContractCodex) UpdateThreadSettings(
	_ context.Context, threadID, _ string, settings codex.ThreadSettings,
) (codex.ThreadSettings, error) {
	if threadID != "thread-1" {
		return codex.ThreadSettings{}, errors.New("unexpected settings thread")
	}
	return settings, nil
}

func (client *browserContractCodex) Send(_ context.Context, threadID, message string) error {
	if threadID != "thread-1" {
		return errors.New("unexpected message thread")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	client.message = message
	return nil
}

func (client *browserContractCodex) Interrupt(_ context.Context, threadID string) error {
	if threadID != "thread-1" {
		return errors.New("unexpected interrupt thread")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	client.interrupt = true
	return nil
}

func (client *browserContractCodex) Subscribe(_ context.Context, threadID string) (<-chan struct{}, func(), error) {
	if threadID != "thread-1" {
		return nil, nil, errors.New("unexpected event thread")
	}
	events := make(chan struct{})
	close(events)
	return events, func() {}, nil
}

func (client *browserContractCodex) PromptsWithItems(_ context.Context, threadID string) ([]codex.Prompt, error) {
	if threadID != "thread-1" {
		return nil, errors.New("unexpected pending thread")
	}
	if client.emptyPrompts {
		return nil, nil
	}
	return []codex.Prompt{{
		ID: "approval-1", Kind: "command", ThreadID: threadID,
		AvailableDecisions: []string{"accept", "decline"}, AuthorityAvailable: true,
	}}, nil
}

func (client *browserContractCodex) RespondAnswers(
	_ context.Context, id, threadID string, answers map[string]map[string][]string,
) error {
	if id != "question-1" || threadID != "thread-1" {
		return errors.New("unexpected answer target")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	client.answers = answers
	return nil
}

func (client *browserContractCodex) RespondDecision(_ context.Context, id, threadID, decision string) error {
	if id != "approval-1" || threadID != "thread-1" {
		return errors.New("unexpected decision target")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	client.decision = decision
	return nil
}

func TestShippedBrowserClientMatchesSessionAPI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the shipped browser contract test")
	}
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\ncodex:\n" +
		"  thread_id: thread-1\n  socket_path: /run/vpsfree-workspace-codex/app-server.sock\n" +
		"  client_version: 0.152.1\ncreation:\n  state: ready\n  initial_goal_sent: true\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	writeWebRuntimeAuthority(t, server, "example")
	controller := &browserContractCodex{}
	server.config.Codex = controller
	server.config.ReadThread = func(_ context.Context, threadID string) (codex.Transcript, error) {
		return codex.Transcript{
			ThreadID: threadID, Status: "idle",
			Entries: []codex.TranscriptEntry{{Kind: "agentMessage", Text: "contract response"}},
		}, nil
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	server.config.BaseURL = httpServer.URL
	command := exec.Command(node, "browser_contract_test.cjs", httpServer.URL)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("browser contract failed: %v\n%s", err, output)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	answer := ""
	if choice, ok := controller.answers["choice"]; ok {
		if answers := choice["answers"]; len(answers) > 0 {
			answer = answers[0]
		}
	}
	if controller.message != "browser message" || !controller.interrupt ||
		controller.decision != "accept" || answer != "yes" {
		t.Fatalf("browser operations were not delivered: %#v", controller)
	}
}

func TestRepositoryCacheDoesNotCrossClosedTransition(t *testing.T) {
	server := newTestServer(t)
	gh := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\ncase \"$1\" in repo) printf 'main\\n' ;; run) printf '[]\\n' ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	server.repository.GH = gh
	updated := time.Now()
	repository := session.Repository{
		Name: "example", GitHub: "vpsfreecz/example", Branch: "feature", DefaultBranch: "master",
		InitialBaseSHA: strings.Repeat("1", 40), FinalHeadSHA: strings.Repeat("2", 40),
	}
	active := &session.Summary{Manifest: session.Manifest{Slug: "example", Repositories: []session.Repository{repository}}, UpdatedAt: updated}
	activeStatus := server.repositories(context.Background(), active)
	if len(activeStatus) != 1 || !strings.Contains(activeStatus[0].CompareURL, "main...feature") {
		t.Fatalf("active status = %#v", activeStatus)
	}
	closed := *active
	closed.Closed = true
	closedStatus := server.repositories(context.Background(), &closed)
	if len(closedStatus) != 1 || !strings.Contains(closedStatus[0].CompareURL, strings.Repeat("1", 40)+"..."+strings.Repeat("2", 40)) {
		t.Fatalf("closed status reused mutable cache: %#v", closedStatus)
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	workspace := t.TempDir()
	authorityDir := filepath.Join(t.TempDir(), "authority")
	if err := os.Mkdir(authorityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Workspace:    workspace,
		BaseURL:      "https://workspace.example.test",
		DevSession:   "/run/current-system/sw/bin/dev-session",
		AuthorityDir: authorityDir,
		CodexSocket:  "/run/vpsfree-workspace-codex/app-server.sock",
		CodexVersion: "0.152.1",
		Logger:       log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}
