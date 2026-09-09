package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/aither64/vpsfree-cz-workspace/portal/internal/cluster"
	"github.com/aither64/vpsfree-cz-workspace/portal/internal/codex"
	"github.com/aither64/vpsfree-cz-workspace/portal/internal/repository"
	"github.com/aither64/vpsfree-cz-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

func TestTransitionLockBlocksPortalMutationsDuringHostChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transition.lock")
	owner, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t)
	server.config.TransitionLock = path
	acquired := make(chan func(), 1)
	failed := make(chan error, 1)
	go func() {
		unlock, err := server.lockTransition()
		if err != nil {
			failed <- err
			return
		}
		acquired <- unlock
	}()
	select {
	case <-acquired:
		t.Fatal("shared transition lock ignored the host's exclusive lock")
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case unlock := <-acquired:
		unlock()
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("portal did not acquire the released transition lock")
	}
}

func TestPortalMutationRejectsProfileSwitchWhileWaiting(t *testing.T) {
	for _, scenario := range []string{"successful", "compensated"} {
		t.Run(scenario, func(t *testing.T) {
			server := newTestServer(t)
			path := filepath.Join(t.TempDir(), "transition.lock")
			owner, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			server.config.TransitionLock = path

			request := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(""))
			request.Header.Set("Origin", server.config.BaseURL)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				server.Handler().ServeHTTP(response, request)
				close(done)
			}()
			select {
			case <-done:
				t.Fatal("portal mutation did not wait for the transition")
			case <-time.After(100 * time.Millisecond):
			}

			oldTarget, err := os.Readlink(server.config.HostProfile)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(server.config.HostProfile); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), server.config.HostProfile); err != nil {
				t.Fatal(err)
			}
			if scenario == "compensated" {
				if err := os.Remove(server.config.HostProfile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(oldTarget, server.config.HostProfile); err != nil {
					t.Fatal(err)
				}
			}
			if err := unix.Flock(int(owner.Fd()), unix.LOCK_UN); err != nil {
				t.Fatal(err)
			}

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("portal mutation did not continue after the transition")
			}
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "superseded workspace package") {
				t.Fatalf("body = %q", response.Body.String())
			}
		})
	}
}

func TestLifecycleOperationAcquiresTransitionBeforeTheSessionMutationLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transition.lock")
	owner, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	owner.Close()
	server := newTestServer(t)
	server.config.TransitionLock = path
	helper := filepath.Join(t.TempDir(), "dev-session")
	if err := os.WriteFile(helper, []byte(
		"#!/bin/sh\n"+
			"[ \"$VPSFREE_WORKSPACE_TRANSITION_LOCK_FD\" = 3 ] || exit 23\n"+
			"[ -e /proc/$$/fd/3 ] || exit 24\n",
	), 0o755); err != nil {
		t.Fatal(err)
	}
	server.config.DevSession = helper
	slug := "2026-09-06-lock-order"
	mutationLock := server.messageLock(slug)
	mutationLock.Lock()
	result := make(chan error, 1)
	go func() {
		result <- server.runLifecycleOperation(
			context.Background(), slug, "archive", []string{"archive", slug, "--as-is"},
		)
	}()

	probe, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = unix.Flock(int(probe.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		if errors.Is(err, unix.EWOULDBLOCK) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(int(probe.Fd()), unix.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("lifecycle operation waited for the session lock before excluding portal mutations")
		}
		time.Sleep(5 * time.Millisecond)
	}

	mutationLock.Unlock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle operation did not release its locks")
	}
}

func TestLifecycleOperationRejectsProfileSwitchWhileWaiting(t *testing.T) {
	for _, scenario := range []string{"successful", "compensated"} {
		t.Run(scenario, func(t *testing.T) {
			server := newTestServer(t)
			path := filepath.Join(t.TempDir(), "transition.lock")
			owner, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			server.config.TransitionLock = path
			marker := filepath.Join(t.TempDir(), "called")
			helper := filepath.Join(t.TempDir(), "dev-session")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch \"$MARKER\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MARKER", marker)
			server.config.DevSession = helper

			result := make(chan error, 1)
			go func() {
				result <- server.runLifecycleOperation(
					context.Background(), "example", "archive", []string{"archive", "example", "--as-is"},
				)
			}()
			select {
			case err := <-result:
				t.Fatalf("lifecycle operation did not wait for the transition: %v", err)
			case <-time.After(100 * time.Millisecond):
			}

			oldTarget, err := os.Readlink(server.config.HostProfile)
			if err != nil {
				t.Fatal(err)
			}
			newTarget := t.TempDir()
			if err := os.Remove(server.config.HostProfile); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(newTarget, server.config.HostProfile); err != nil {
				t.Fatal(err)
			}
			if scenario == "compensated" {
				if err := os.Remove(server.config.HostProfile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(oldTarget, server.config.HostProfile); err != nil {
					t.Fatal(err)
				}
			}
			if err := unix.Flock(int(owner.Fd()), unix.LOCK_UN); err != nil {
				t.Fatal(err)
			}

			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), "superseded workspace package") {
					t.Fatalf("compensated switch result = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("lifecycle operation did not reject the compensated switch")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("superseded lifecycle helper ran: %v", err)
			}
		})
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

func TestArtifactPreviewRendersSanitizedMarkdownAndEscapedText(t *testing.T) {
	server := newTestServer(t)
	writeArtifactSession(t, server.config.Workspace, "report.md")
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.WriteFile(
		filepath.Join(directory, "report.md"),
		[]byte("# Report\n\n<script>alert(1)</script>\n\n[bad](javascript:alert(1))\n\n"+
			"[encoded](&#106;avascript:alert(1))\n\n<javascript:alert(document.domain)>\n\n"+
			"![alt](javascript:alert(document.domain))\n\n| A | B |\n| - | - |\n| 1 | 2 |\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/api/sessions/example/artifact-preview?path=report.md", nil,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("Markdown preview = %d %q", response.Code, response.Body.String())
	}
	var markdown map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &markdown); err != nil {
		t.Fatal(err)
	}
	if markdown["kind"] != "markdown" || strings.Contains(markdown["html"], "<script") ||
		strings.Contains(markdown["html"], `href="javascript:`) ||
		strings.Contains(markdown["html"], `src="javascript:`) ||
		strings.Contains(markdown["html"], "&#106;avascript:") {
		t.Fatalf("Markdown preview = %#v", markdown)
	}
	for _, element := range []string{"<table>", "<thead>", "<tbody>", "<th>", "<td>"} {
		if !strings.Contains(markdown["html"], element) {
			t.Fatalf("Markdown table is missing %s: %#v", element, markdown)
		}
	}

	writeArtifactSession(t, server.config.Workspace, "report.txt")
	if err := os.WriteFile(filepath.Join(directory, "report.txt"), []byte("<script>plain</script>"), 0o644); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/api/sessions/example/artifact-preview?path=report.txt", nil,
	))
	var textPreview map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &textPreview); err != nil {
		t.Fatal(err)
	}
	if textPreview["kind"] != "text" || textPreview["text"] != "<script>plain</script>" {
		t.Fatalf("text preview = %#v", textPreview)
	}
}

func TestArtifactImagePreviewIsInlineAndStillConfined(t *testing.T) {
	server := newTestServer(t)
	writeArtifactSession(t, server.config.Workspace, "image.png")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/api/sessions/example/artifact-preview?path=image.png", nil,
	))
	var preview map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview["kind"] != "image" || preview["url"] != "/artifact-previews/example/image.png" {
		t.Fatalf("image preview = %#v", preview)
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, preview["url"], nil))
	if response.Code != http.StatusOK ||
		!strings.HasPrefix(response.Header().Get("Content-Disposition"), "inline;") {
		t.Fatalf("inline image = %d %#v", response.Code, response.Header())
	}

	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/api/sessions/example/artifact-preview?path=../image.png", nil,
	))
	if response.Code != http.StatusNotFound {
		t.Fatalf("escaping preview status = %d", response.Code)
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

func TestIndexDefersCodexActivityAndReturnsEnrichedStatus(t *testing.T) {
	server := newTestServer(t)
	base := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	writeSession := func(slug, threadID string, updated time.Time) {
		directory := filepath.Join(server.config.Workspace, "work", slug)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := "schema: 1\nslug: " + slug + "\ncodex:\n  thread_id: " + threadID +
			"\ncreation:\n  state: ready\n  initial_goal_sent: true\n"
		if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		writeWebTrackingFiles(t, directory, "active")
		for _, name := range []string{"portal.yml", "plan.md", "state.md"} {
			if err := os.Chtimes(filepath.Join(directory, name), updated, updated); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeSession("2026-09-07-recent-files", "thread-1", base.Add(time.Hour))
	writeSession("2026-09-06-recent-codex", "thread-2", base)
	controller := &browserContractCodex{activities: []codex.ThreadActivity{{
		ID: "thread-2", Cwd: filepath.Join(server.config.Workspace, "work", "2026-09-06-recent-codex"),
		UpdatedAt: base.Add(2 * time.Hour),
	}}}
	server.config.Codex = controller
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	if len(controller.activityInputs) != 0 {
		t.Fatalf("initial index performed Codex I/O: %#v", controller.activityInputs)
	}
	if !strings.Contains(body, "data-index-generated-at") || !strings.Contains(body, "data-session-slug") {
		t.Fatalf("initial index lacks enrichment hooks: %s", body)
	}

	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/index-status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("index status = %d %q", response.Code, response.Body.String())
	}
	if len(controller.activityInputs) != 2 || controller.activityInputs[1].ID != "thread-2" ||
		controller.activityInputs[1].Cwd != filepath.Join(server.config.Workspace, "work", "2026-09-06-recent-codex") {
		t.Fatalf("activity identities = %#v", controller.activityInputs)
	}
	var status struct {
		Sessions      []indexSessionStatus `json:"sessions"`
		Authoritative bool                 `json:"authoritative"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Authoritative {
		t.Fatalf("complete index status was not authoritative: %q", response.Body.String())
	}
	bySlug := make(map[string]indexSessionStatus)
	for _, item := range status.Sessions {
		bySlug[item.Slug] = item
	}
	if !bySlug["2026-09-06-recent-codex"].UpdatedAt.Equal(base.Add(2*time.Hour)) ||
		!bySlug["2026-09-07-recent-files"].UpdatedAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("enriched status = %#v", status.Sessions)
	}

	controller.activityErr = errors.New("App Server unavailable")
	server.indexStatusMu.Lock()
	server.indexStatusCache = cachedIndexStatus{}
	server.indexStatusMu.Unlock()
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/index-status", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	bySlug = make(map[string]indexSessionStatus)
	for _, item := range status.Sessions {
		bySlug[item.Slug] = item
	}
	if !bySlug["2026-09-07-recent-files"].UpdatedAt.Equal(base.Add(time.Hour)) ||
		!bySlug["2026-09-06-recent-codex"].UpdatedAt.Equal(base) {
		t.Fatalf("filesystem fallback status = %#v", status.Sessions)
	}
}

func TestIndexStatusMarksPartialSessionListNonAuthoritative(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte("schema: 1\nslug: example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	if err := os.MkdirAll(filepath.Join(server.config.Workspace, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(directory, filepath.Join(server.config.Workspace, "archive", "invalid-session")); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/index-status", nil))
	var payload struct {
		Sessions      []indexSessionStatus `json:"sessions"`
		Authoritative bool                 `json:"authoritative"`
		Warning       string               `json:"warning"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &payload) != nil {
		t.Fatalf("index status = %d %q", response.Code, response.Body.String())
	}
	if payload.Authoritative || payload.Warning == "" || len(payload.Sessions) != 1 || payload.Sessions[0].Slug != "example" {
		t.Fatalf("partial index status = %#v", payload)
	}
}

func TestIndexStatusReportsArchivePlacement(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "archive", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\nfinalized_at: '2026-09-09T10:00:00Z'\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "complete")

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/index-status", nil))
	var payload struct {
		Sessions []indexSessionStatus `json:"sessions"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &payload) != nil {
		t.Fatalf("index status = %d %q", response.Code, response.Body.String())
	}
	if len(payload.Sessions) != 1 || payload.Sessions[0].Slug != "example" || !payload.Sessions[0].Archived {
		t.Fatalf("archive placement = %#v", payload.Sessions)
	}
}

func TestIndexStatusUsesOneBoundedRefreshForConcurrentRequests(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\n" +
		"creation:\n  state: ready\n  initial_goal_sent: true\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	release := make(chan struct{})
	controller := &browserContractCodex{activityWait: release}
	server.config.Codex = controller
	handler := server.Handler()
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/index-status", nil))
			responses <- response
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		controller.mu.Lock()
		calls := controller.activityCalls
		controller.mu.Unlock()
		if calls == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("index refresh calls = %d", calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
	releasedAt := time.Now()
	close(release)
	for range 2 {
		if response := <-responses; response.Code != http.StatusOK {
			t.Fatalf("index status = %d %q", response.Code, response.Body.String())
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/index-status", nil))
	controller.mu.Lock()
	calls := controller.activityCalls
	controller.mu.Unlock()
	if response.Code != http.StatusOK || calls != 1 {
		t.Fatalf("cached index status = %d, calls = %d", response.Code, calls)
	}
	server.indexStatusMu.Lock()
	created := server.indexStatusCache.created
	server.indexStatusMu.Unlock()
	if created.Before(releasedAt) {
		t.Fatalf("cache timestamp %s predates refresh completion %s", created, releasedAt)
	}
}

func TestIndexStatusIncludesOperationsBeforeTheirJournalExists(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte("schema: 1\nslug: example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	server.operationMu.Lock()
	server.operations["example"] = lifecycleOperation{Kind: "archive", State: "running", Phase: "starting"}
	server.operationMu.Unlock()

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/index-status", nil))
	var payload struct {
		Sessions []indexSessionStatus `json:"sessions"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &payload) != nil ||
		len(payload.Sessions) != 1 || payload.Sessions[0].PendingLifecycle != "archive" {
		t.Fatalf("index operation = %d %#v %q", response.Code, payload, response.Body.String())
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
	server.config.TransitionLock = filepath.Join(directory, "transition.lock")
	if err := os.WriteFile(server.config.TransitionLock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
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
				for _, marker := range []string{`id="codex-model"`, `id="codex-effort"`, `class="compact-select"`} {
					if !strings.Contains(body, marker) {
						t.Fatalf("session page is missing inline setting %s", marker)
					}
				}
				for _, marker := range []string{`id="codex-settings-open"`, `id="codex-settings-dialog"`} {
					if strings.Contains(body, marker) {
						t.Fatalf("session page retained obsolete setting %s", marker)
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
		!strings.Contains(response.Body.String(), "dev-session start &lt;short-name&gt;") ||
		!strings.Contains(response.Body.String(), `id="delete-session-open"`) {
		t.Fatalf("legacy page = %d %q", response.Code, response.Body.String())
	}
}

func writeWebRuntimeAuthority(t *testing.T, server *Server, slug string) {
	t.Helper()
	authority := session.RuntimeAuthority{
		Schema: 1, State: "ready", Slug: slug, Workspace: server.config.Workspace,
		TmuxSocket: "/run/vpsfree-workspace-tmux/tmux.sock", TmuxSessionID: "$1",
		TmuxIdentity:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
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
		authority.CodexClientVersion, "%1", authority.TmuxIdentity,
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

func TestTerminalSessionWithoutRuntimeDoesNotRenderMutationControls(t *testing.T) {
	server := newTestServer(t)
	response := httptest.NewRecorder()
	server.render(response, "session", pageData{
		BaseURL: "https://workspace.example.test",
		Session: &session.Summary{
			Manifest: session.Manifest{Slug: "example", Codex: session.Codex{ThreadID: "thread-1"}},
			Terminal: true,
		},
	})
	body := response.Body.String()
	for _, marker := range []string{`id="pending"`, `id="message-form"`, `id="interrupt"`} {
		if strings.Contains(body, marker) {
			t.Fatalf("noninteractive terminal session rendered %s", marker)
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
		Artifacts: []session.Artifact{
			{Label: "Plan", Path: "plan.md"},
			{Label: "State", Path: "state.md"},
			{Label: "Report", Path: "report.md"},
		},
	})
	body := response.Body.String()
	for _, marker := range []string{
		`class="panel session-tabs"`, `href="#codex"`, `data-session-tab="codex"`,
		`href="#handoff"`, `data-session-tab="handoff"`,
		`href="#repositories"`, `data-session-tab="repositories"`,
		`href="#clusters"`, `data-session-tab="clusters"`,
		`id="codex" class="tab-panel chat-panel active"`, `href="#artifacts"`,
		`data-session-tab="artifacts"`,
		`data-artifact-path="plan.md"`, `data-artifact-path="state.md"`,
		`data-artifact-path="report.md"`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("session page lacks %s", marker)
		}
	}
	for _, marker := range []string{`data-session-tab="plan"`, `data-session-tab="state"`, `id="plan"`, `id="state"`} {
		if strings.Contains(body, marker) {
			t.Fatalf("session page retained separate tracking tab %s", marker)
		}
	}
	stylesheet, err := assets.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"grid-template-columns: minmax(0, 1fr)", ".message { min-width: 0; max-width: 100%",
		".table-scroll { max-width: 100%; overflow-x: auto; }", ".message.markdown table",
		".document table", ".message details > summary",
	} {
		if !strings.Contains(string(stylesheet), marker) {
			t.Fatalf("transcript styling does not contain %q", marker)
		}
	}
	if strings.Contains(string(stylesheet), ".message.markdown { white-space: normal; overflow") {
		t.Fatal("Markdown messages still create overflow containers")
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
		"shouldSubmitMessage(event)", "event.shiftKey", "event.isComposing", "form.requestSubmit()",
		"await beforeRequestInputAction(snoozeAutoResolution)",
		"entry.html", "archive-session", "revive-session", "artifactPreview", "release-cluster", "fork-dialog",
		"data-cluster-service-tab", "data-reveal-secret", "index-status", "modelSelect.required",
		"const nextSignature = JSON.stringify(entries)", "client.operation().then((operation)",
		"deleteDialog.showModal()", `lifecycleKind === "revive" && needsOptions`,
		"void retryRevive(lifecycleRetry)", "indexStatusFreshForPage", "nextRefresh = 1000",
	} {
		if !strings.Contains(string(javascript), marker) {
			t.Fatalf("browser client does not contain %q", marker)
		}
	}
	if strings.Contains(string(javascript), "if (pendingLifecycle) {\n    client.operation()") {
		t.Fatal("browser reload still hides lifecycle operations that precede their journal")
	}
	if strings.Contains(string(javascript), "client.operation().then((operation) => {\n    if (operation.state === \"complete\")") {
		t.Fatal("a completed lifecycle operation still redirects on every page load")
	}
}

func TestReasoningSelectorsAllowAutomaticOnlyOutsideExistingSettings(t *testing.T) {
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
	if count := strings.Count(body, `data-effort-select`); count != 2 {
		t.Fatalf("reasoning effort selects = %d", count)
	}
	if strings.Contains(body, `name="effort" data-effort-select required`) {
		t.Fatal("fork reasoning is blocked by native required validation")
	}
	javascript, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(javascript),
		`const existingSettings = modelSelect.dataset.existingSettings === "true"`) ||
		!strings.Contains(string(javascript), `if (!existingSettings)`) {
		t.Fatal("existing-thread settings still offer unsupported automatic reasoning")
	}
}

func TestSessionPageGroupsClusterServicesAndRepositoryRevisionState(t *testing.T) {
	server := newTestServer(t)
	response := httptest.NewRecorder()
	server.render(response, "session", pageData{
		Session: &session.Summary{Manifest: session.Manifest{Slug: "example"}, Interactive: true},
		Repositories: []repository.Status{{
			Name: "workspace", Branch: "feature", DefaultBranch: "master",
			LocalHeadSHA: strings.Repeat("a", 40), RemoteHeadSHA: strings.Repeat("b", 40),
			PushStatus: repository.PushStatusDivergent,
		}},
		Clusters: []cluster.Status{{
			Kind: "vpsadmin", Label: "vpsAdmin", State: "running", Ready: true,
			Services: []cluster.Service{{
				Label: "Web UI", URL: "https://webui.example.test/",
				Accounts: []cluster.Account{{Label: "Administrator", Fields: []cluster.Field{
					{Label: "Login", Value: "admin"},
					{Label: "Password", Value: "secret", Secret: true},
				}}},
			}},
			Commands: []cluster.Command{{Label: "node1", Value: "ssh node1"}},
		}},
	})
	body := response.Body.String()
	for _, marker := range []string{
		"Local HEAD", "GitHub HEAD", "Diverged", `data-cluster-service-tab="0"`,
		`data-cluster-service-panel="0"`, "Open Web UI", "Administrator",
		`type="password"`, `data-reveal-secret`, "Connect", `class="cluster-footer"`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("session page lacks %q: %s", marker, body)
		}
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

func TestCloseCancelsAndDrainsArchiveOperations(t *testing.T) {
	server := newTestServer(t)
	slug := "2026-09-05-shutdown"
	tracking := filepath.Join(server.config.Workspace, "work", slug)
	if err := os.MkdirAll(tracking, 0o755); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, tracking, "complete")
	if err := os.WriteFile(
		filepath.Join(tracking, "portal.yml"),
		[]byte("schema: 1\nslug: "+slug+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	devSession := filepath.Join(t.TempDir(), "dev-session")
	started := filepath.Join(t.TempDir(), "started")
	script := "#!/bin/sh\nprintf started > \"$STARTED\"\nsleep 30\n"
	if err := os.WriteFile(devSession, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STARTED", started)
	server.config.DevSession = devSession
	response := httptest.NewRecorder()
	server.startArchive(response, httptest.NewRequest(
		http.MethodPost, "/", strings.NewReader(`{"mode":"complete"}`),
	), &session.Summary{
		Manifest:  session.Manifest{Slug: slug},
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
			server.operationMu.Lock()
			operation := server.operations[slug]
			server.operationMu.Unlock()
			t.Fatalf("archive helper did not start: %#v", operation)
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
	server.startArchive(retry, httptest.NewRequest(
		http.MethodPost, "/", strings.NewReader(`{"mode":"complete"}`),
	), &session.Summary{
		Manifest:  session.Manifest{Slug: slug},
		Lifecycle: "complete",
	})
	if retry.Code != http.StatusServiceUnavailable {
		t.Fatalf("archive start during shutdown status/body = %d %q", retry.Code, retry.Body.String())
	}
}

func TestLifecycleStatusReportsDurablePhaseAndRetainsFailure(t *testing.T) {
	server := newTestServer(t)
	root := filepath.Join(server.config.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(
		`{"schema":2,"slug":"example","workspace":%q,"phase":"clusters_released","mode":"complete"}`,
		server.config.Workspace,
	)
	if err := os.WriteFile(filepath.Join(root, "example.archive.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.lifecycleStatus(response, "example")
	var operation lifecycleOperation
	if err := json.Unmarshal(response.Body.Bytes(), &operation); err != nil {
		t.Fatal(err)
	}
	if operation.Kind != "archive" || operation.State != "paused" ||
		operation.Phase != "clusters_released" || operation.StartedAt == "" || operation.UpdatedAt == "" {
		t.Fatalf("durable lifecycle status = %#v", operation)
	}

	server.operationMu.Lock()
	server.operations["example"] = lifecycleOperation{
		Kind: "archive", State: "failed", StartedAt: operation.StartedAt,
		Error: "helper failed",
	}
	server.operationMu.Unlock()
	response = httptest.NewRecorder()
	server.lifecycleStatus(response, "example")
	if err := json.Unmarshal(response.Body.Bytes(), &operation); err != nil {
		t.Fatal(err)
	}
	if operation.State != "failed" || operation.Phase != "clusters_released" ||
		operation.Error != "helper failed" {
		t.Fatalf("failed lifecycle status = %#v", operation)
	}
}

func TestJournalOnlyDeletionRemainsVisibleAndRetryable(t *testing.T) {
	server := newTestServer(t)
	root := filepath.Join(server.config.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(
		`{"schema":1,"slug":"example","workspace":%q,"phase":"tracking_preserved","force":false}`,
		server.config.Workspace,
	)
	if err := os.WriteFile(filepath.Join(root, "example.removal.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), `href="/example/"`) {
		t.Fatalf("journal-only index = %d %q", index.Code, index.Body.String())
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/example/", nil))
	if page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), "Deletion recovery") ||
		!strings.Contains(page.Body.String(), `data-pending-lifecycle="delete"`) ||
		!strings.Contains(page.Body.String(), `id="delete-session-dialog"`) {
		t.Fatalf("journal-only operation page = %d %q", page.Code, page.Body.String())
	}
}

func TestWorkflowLookupErrorDoesNotClaimThereAreNoRuns(t *testing.T) {
	server := newTestServer(t)
	response := httptest.NewRecorder()
	server.render(response, "session", pageData{
		Session: &session.Summary{Manifest: session.Manifest{Slug: "example"}},
		Repositories: []repository.Status{{
			Name: "workspace", PushStatus: repository.PushStatusExactlyPushed,
			GitHubError: "request timed out",
		}},
	})
	body := response.Body.String()
	if !strings.Contains(body, "GitHub: request timed out") ||
		strings.Contains(body, "No workflow runs for this revision") {
		t.Fatalf("workflow error rendering = %q", body)
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
					Kind: "agentMessage", Text: "# Persisted answer\n\n| Item | State |\n| --- | --- |\n| Portal | Ready |\n\n<script>alert(1)</script>",
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
				!strings.Contains(transcript.Entries[0].HTML, "<table>") ||
				strings.Contains(transcript.Entries[0].HTML, "<script") {
				t.Fatalf("sanitized transcript Markdown = %#v", transcript.Entries)
			}
		})
	}
}

func TestTerminalUnarchivedSessionRemainsInteractive(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\n" +
		"  socket_path: /run/vpsfree-workspace-codex/app-server.sock\n  client_version: 0.152.1\n" +
		"creation:\n  state: ready\n  initial_goal_sent: true\nrepositories: []\nartifacts: []\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "complete")
	writeWebRuntimeAuthority(t, server, "example")

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/example/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d %q", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, control := range []string{`id="message-form"`, `id="fork-open"`, "Complete · open"} {
		if !strings.Contains(body, control) {
			t.Fatalf("terminal open session is missing %s", control)
		}
	}
}

func TestArchiveRequiresAnExplicitMode(t *testing.T) {
	server := newTestServer(t)
	summary := &session.Summary{
		Manifest:  session.Manifest{Slug: "2026-09-07-example"},
		Lifecycle: "complete",
	}
	for _, body := range []string{`{}`, `{"confirmation":"example"}`} {
		response := httptest.NewRecorder()
		server.startArchive(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), summary)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("confirmation %s returned %d %q", body, response.Code, response.Body.String())
		}
	}
	server.operationMu.Lock()
	defer server.operationMu.Unlock()
	if len(server.operations) != 0 {
		t.Fatalf("archive without a valid mode started operations: %#v", server.operations)
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
	for _, marker := range []string{"question.isOther", `input.value = "__other__"`, "None of the above"} {
		if !strings.Contains(string(javascript), marker) {
			t.Fatalf("browser client does not contain %q", marker)
		}
	}
}

func TestSessionDeletionCanClearBrowserStateBeforeTranscriptLoads(t *testing.T) {
	javascript, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	template, err := assets.ReadFile("templates/session.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(javascript), `let currentThreadId = body.dataset.threadId || "";`) {
		t.Fatal("browser client does not initialize deletion identity from the rendered session")
	}
	if !strings.Contains(string(template), `data-thread-id="{{.Session.Codex.ThreadID}}"`) {
		t.Fatal("session page does not render the persisted thread identity")
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
	mu             sync.Mutex
	message        string
	messageID      string
	actionContext  string
	sendCount      int
	sendErr        error
	queued         string
	queueDeleted   string
	queueStarted   string
	settings       codex.ThreadSettings
	settingsErr    error
	interrupt      bool
	decision       string
	answers        map[string]map[string][]string
	snoozed        string
	emptyPrompts   bool
	transcript     codex.Transcript
	activities     []codex.ThreadActivity
	activityInputs []codex.ThreadActivity
	activityErr    error
	activityWait   <-chan struct{}
	activityCalls  int
}

func (client *browserContractCodex) ListThreadActivity(
	ctx context.Context, expected []codex.ThreadActivity,
) ([]codex.ThreadActivity, error) {
	client.mu.Lock()
	client.activityInputs = append([]codex.ThreadActivity(nil), expected...)
	client.activityCalls++
	wait := client.activityWait
	activities := append([]codex.ThreadActivity(nil), client.activities...)
	err := client.activityErr
	client.mu.Unlock()
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return activities, err
}

func (client *browserContractCodex) VerifyThread(_ context.Context, threadID, _ string) error {
	if threadID != "thread-1" {
		return errors.New("unexpected verification thread")
	}
	return nil
}

func (client *browserContractCodex) ReadThread(_ context.Context, threadID string) (codex.Transcript, error) {
	if client.transcript.ThreadID != "" {
		return client.transcript, nil
	}
	return codex.Transcript{ThreadID: threadID}, nil
}

func (client *browserContractCodex) ListModels(_ context.Context) ([]codex.Model, error) {
	return []codex.Model{{
		ID: "model-1", Model: "model-1", DisplayName: "Model 1", IsDefault: true,
		DefaultReasoningEffort:    "medium",
		SupportedReasoningEfforts: []codex.ReasoningEffortOption{{ReasoningEffort: "medium"}, {ReasoningEffort: "high"}},
	}}, nil
}

func (client *browserContractCodex) ListCollaborationModes(_ context.Context) ([]codex.CollaborationMode, error) {
	return []codex.CollaborationMode{
		{Name: "Default", Mode: "default"}, {Name: "Plan", Mode: "plan"},
	}, nil
}

func (client *browserContractCodex) UpdateThreadSettings(
	_ context.Context, threadID string, update codex.ThreadSettingsUpdate,
) (codex.ThreadSettings, error) {
	if threadID != "thread-1" {
		return codex.ThreadSettings{}, errors.New("unexpected settings thread")
	}
	client.mu.Lock()
	if update.Model != nil {
		client.settings.Model = *update.Model
	}
	if update.ReasoningEffort != nil {
		client.settings.ReasoningEffort = *update.ReasoningEffort
	}
	if update.CollaborationMode != nil {
		client.settings.CollaborationMode = *update.CollaborationMode
	}
	client.mu.Unlock()
	return client.settings, client.settingsErr
}

func (client *browserContractCodex) Send(
	_ context.Context, threadID, message, clientID, actionContext string,
) (codex.SendReceipt, error) {
	if threadID != "thread-1" {
		return codex.SendReceipt{}, errors.New("unexpected message thread")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	client.message = message
	client.messageID = clientID
	client.actionContext = actionContext
	client.sendCount++
	if client.sendErr != nil {
		return codex.SendReceipt{}, client.sendErr
	}
	return codex.SendReceipt{
		TurnID: "turn-1", ClientUserMessageID: clientID, Steered: true,
	}, nil
}

func (client *browserContractCodex) PrepareSend(
	threadID, message, clientID, actionContext string, _ bool,
) error {
	if threadID != "thread-1" {
		return errors.New("unexpected message thread")
	}
	client.mu.Lock()
	client.message = message
	client.messageID = clientID
	client.actionContext = actionContext
	client.mu.Unlock()
	return nil
}

func (client *browserContractCodex) SendAttempted(
	_ context.Context, threadID, message, clientID, _ string,
) (bool, error) {
	if threadID != "thread-1" {
		return false, errors.New("unexpected message thread")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.messageID == "" {
		return false, nil
	}
	if client.messageID != clientID || client.message != message {
		return false, errors.New("message identity was reused with different text")
	}
	return true, nil
}

func (client *browserContractCodex) ListQueue(_ context.Context, threadID string) ([]codex.QueueEntry, error) {
	if threadID != "thread-1" {
		return nil, errors.New("unexpected queue thread")
	}
	return []codex.QueueEntry{{ID: "queued-1", Text: "queued item", ClientUserMessageID: "client-1"}}, nil
}

func (client *browserContractCodex) Queue(
	_ context.Context, threadID, message, clientID string,
) (codex.QueueEntry, error) {
	if threadID != "thread-1" {
		return codex.QueueEntry{}, errors.New("unexpected queue thread")
	}
	client.mu.Lock()
	client.queued = message
	client.mu.Unlock()
	return codex.QueueEntry{ID: "queued-2", Text: message, ClientUserMessageID: clientID}, nil
}

func (client *browserContractCodex) DeleteQueueEntry(_ context.Context, threadID, id string) error {
	if threadID != "thread-1" {
		return errors.New("unexpected queue thread")
	}
	client.mu.Lock()
	client.queueDeleted = id
	client.mu.Unlock()
	return nil
}

func (client *browserContractCodex) StartQueue(_ context.Context, threadID, queuedSubmissionID string) error {
	if threadID != "thread-1" {
		return errors.New("unexpected queue thread")
	}
	client.mu.Lock()
	client.queueStarted = queuedSubmissionID
	client.mu.Unlock()
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

func (client *browserContractCodex) SnoozeUserInput(id, threadID string) error {
	if id == "" || threadID != "thread-1" {
		return errors.New("unexpected snooze target")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	client.snoozed = id
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

func TestImplementPlanRejectsAStalePlan(t *testing.T) {
	server := newTestServer(t)
	controller := &browserContractCodex{transcript: codex.Transcript{
		ThreadID: "thread-1", Status: "idle", CollaborationMode: "plan",
		Entries: []codex.TranscriptEntry{{
			TurnID: "turn-new", TurnStatus: "completed", Kind: "plan", Text: "Current plan",
		}},
	}}
	server.config.Codex = controller
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"action":"same","planTurnId":"turn-old","planSha256":"bad","clientUserMessageId":"00000000-0000-4000-8000-000000000001"}`,
	))
	response := httptest.NewRecorder()

	server.implementPlan(response, request, &session.Summary{Manifest: session.Manifest{
		Slug: "example", Codex: session.Codex{ThreadID: "thread-1"},
	}})

	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "stale") {
		t.Fatalf("stale plan response = %d %q", response.Code, response.Body.String())
	}
}

func TestImplementPlanContinuesInTheSameThread(t *testing.T) {
	server := newTestServer(t)
	plan := "1. Make the change.\n2. Test it."
	controller := &browserContractCodex{
		settings: codex.ThreadSettings{
			Model: "model-1", ReasoningEffort: "high", CollaborationMode: "plan",
		},
		transcript: codex.Transcript{
			ThreadID: "thread-1", Status: "idle", CollaborationMode: "plan",
			Entries: []codex.TranscriptEntry{{
				TurnID: "turn-plan", TurnStatus: "completed", Kind: "plan", Text: plan,
			}},
		},
	}
	server.config.Codex = controller
	body := fmt.Sprintf(
		`{"action":"same","planTurnId":"turn-plan","planSha256":"%s","clientUserMessageId":"00000000-0000-4000-8000-000000000001"}`,
		planDigest(plan),
	)
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	response := httptest.NewRecorder()

	server.implementPlan(response, request, &session.Summary{Manifest: session.Manifest{
		Slug: "example", Codex: session.Codex{ThreadID: "thread-1"},
	}})

	if response.Code != http.StatusAccepted {
		t.Fatalf("same-thread plan response = %d %q", response.Code, response.Body.String())
	}
	if controller.message != "Implement the plan." ||
		controller.actionContext != "plan:"+planDigest(plan) ||
		controller.settings.CollaborationMode != "default" {
		t.Fatalf("plan implementation state = %#v", controller)
	}
}

func TestImplementPlanKeepsItsDurableAttemptWhenTheMessageFails(t *testing.T) {
	server := newTestServer(t)
	plan := "Make the change."
	controller := &browserContractCodex{
		sendErr: errors.New("send failed"),
		settings: codex.ThreadSettings{
			Model: "model-1", ReasoningEffort: "high", CollaborationMode: "plan",
		},
		transcript: codex.Transcript{
			ThreadID: "thread-1", Status: "idle", CollaborationMode: "plan",
			Entries: []codex.TranscriptEntry{{
				TurnID: "turn-plan", TurnStatus: "completed", Kind: "plan", Text: plan,
			}},
		},
	}
	server.config.Codex = controller
	body := fmt.Sprintf(
		`{"action":"same","planTurnId":"turn-plan","planSha256":"%s","clientUserMessageId":"00000000-0000-4000-8000-000000000001"}`,
		planDigest(plan),
	)
	response := httptest.NewRecorder()
	server.implementPlan(response, httptest.NewRequest(
		http.MethodPost, "/", strings.NewReader(body),
	), &session.Summary{Manifest: session.Manifest{
		Slug: "example", Codex: session.Codex{ThreadID: "thread-1"},
	}})

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed plan response = %d %q", response.Code, response.Body.String())
	}
	if controller.settings.CollaborationMode != "default" {
		t.Fatalf("collaboration mode = %q", controller.settings.CollaborationMode)
	}
}

func TestImplementPlanKeepsDefaultModeAndReconcilesAnUnknownSend(t *testing.T) {
	server := newTestServer(t)
	plan := "Make the change."
	controller := &browserContractCodex{
		sendErr: &codex.UnknownSendOutcomeError{Err: errors.New("connection changed")},
		settings: codex.ThreadSettings{
			Model: "model-1", ReasoningEffort: "high", CollaborationMode: "plan",
		},
		transcript: codex.Transcript{
			ThreadID: "thread-1", Status: "idle", CollaborationMode: "plan",
			Entries: []codex.TranscriptEntry{{
				TurnID: "turn-plan", TurnStatus: "completed", Kind: "plan", Text: plan,
			}},
		},
	}
	server.config.Codex = controller
	body := fmt.Sprintf(
		`{"action":"same","planTurnId":"turn-plan","planSha256":"%s","clientUserMessageId":"00000000-0000-4000-8000-000000000001"}`,
		planDigest(plan),
	)
	summary := &session.Summary{Manifest: session.Manifest{
		Slug: "example", Codex: session.Codex{ThreadID: "thread-1"},
	}}
	first := httptest.NewRecorder()
	server.implementPlan(first, httptest.NewRequest(
		http.MethodPost, "/", strings.NewReader(body),
	), summary)
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("unknown plan response = %d %q", first.Code, first.Body.String())
	}
	if controller.settings.CollaborationMode != "default" {
		t.Fatalf("unknown send restored collaboration mode = %q", controller.settings.CollaborationMode)
	}

	controller.sendErr = nil
	controller.transcript.CollaborationMode = "default"
	retry := httptest.NewRecorder()
	server.implementPlan(retry, httptest.NewRequest(
		http.MethodPost, "/", strings.NewReader(body),
	), summary)
	if retry.Code != http.StatusAccepted {
		t.Fatalf("reconciled plan response = %d %q", retry.Code, retry.Body.String())
	}
}

func TestImplementPlanRetriesAfterTheModeChangeResponseIsLost(t *testing.T) {
	server := newTestServer(t)
	plan := "Make the change."
	controller := &browserContractCodex{
		settingsErr: errors.New("settings response was lost"),
		settings: codex.ThreadSettings{
			Model: "model-1", ReasoningEffort: "high", CollaborationMode: "plan",
		},
		transcript: codex.Transcript{
			ThreadID: "thread-1", Status: "idle", CollaborationMode: "plan",
			Entries: []codex.TranscriptEntry{{
				TurnID: "turn-plan", TurnStatus: "completed", Kind: "plan", Text: plan,
			}},
		},
	}
	server.config.Codex = controller
	body := fmt.Sprintf(
		`{"action":"same","planTurnId":"turn-plan","planSha256":"%s","clientUserMessageId":"00000000-0000-4000-8000-000000000001"}`,
		planDigest(plan),
	)
	summary := &session.Summary{Manifest: session.Manifest{
		Slug: "example", Codex: session.Codex{ThreadID: "thread-1"},
	}}
	first := httptest.NewRecorder()
	server.implementPlan(first, httptest.NewRequest(
		http.MethodPost, "/", strings.NewReader(body),
	), summary)
	if first.Code != http.StatusConflict || controller.settings.CollaborationMode != "default" {
		t.Fatalf(
			"lost settings response = %d %q, mode %q",
			first.Code, first.Body.String(), controller.settings.CollaborationMode,
		)
	}

	controller.settingsErr = nil
	controller.transcript.CollaborationMode = "default"
	retry := httptest.NewRecorder()
	server.implementPlan(retry, httptest.NewRequest(
		http.MethodPost, "/", strings.NewReader(body),
	), summary)
	if retry.Code != http.StatusAccepted || controller.message != "Implement the plan." || controller.sendCount != 1 {
		t.Fatalf(
			"settings retry = %d %q, message %q, sends %d",
			retry.Code, retry.Body.String(), controller.message, controller.sendCount,
		)
	}
}

func TestImplementPlanStartsANewSessionWithTheExactPlan(t *testing.T) {
	server := newTestServer(t)
	directory := t.TempDir()
	arguments := filepath.Join(directory, "arguments")
	goalCopy := filepath.Join(directory, "goal")
	helper := filepath.Join(directory, "dev-session")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$ARGUMENTS"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--goal-file" ]; then
    cp "$2" "$GOAL_COPY"
    break
  fi
  shift
done
printf '{"slug":"2026-09-07-implement-feature"}\n'
`
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUMENTS", arguments)
	t.Setenv("GOAL_COPY", goalCopy)
	server.config.DevSession = helper
	plan := "1. Make the change.\n2. Test it."
	server.config.Codex = &browserContractCodex{transcript: codex.Transcript{
		ThreadID: "thread-1", Status: "idle", Model: "model-1", ReasoningEffort: "high",
		CollaborationMode: "plan", Entries: []codex.TranscriptEntry{{
			TurnID: "turn-plan", TurnStatus: "completed", Kind: "plan", Text: plan,
		}},
	}}
	body := fmt.Sprintf(
		`{"action":"new","planTurnId":"turn-plan","planSha256":"%s","name":"implement-feature","creationDate":"2026-09-07"}`,
		planDigest(plan),
	)
	response := httptest.NewRecorder()
	server.implementPlan(response, httptest.NewRequest(
		http.MethodPost, "/", strings.NewReader(body),
	), &session.Summary{Manifest: session.Manifest{
		Slug: "example", Codex: session.Codex{ThreadID: "thread-1"},
	}})

	if response.Code != http.StatusCreated {
		t.Fatalf("new-session plan response = %d %q", response.Code, response.Body.String())
	}
	goal, err := os.ReadFile(goalCopy)
	if err != nil {
		t.Fatal(err)
	}
	wantGoal := "Implement the following approved plan from session example.\n\n" + plan
	if string(goal) != wantGoal {
		t.Fatalf("new session goal = %q, want %q", goal, wantGoal)
	}
	argv, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	if len(lines) != 12 || lines[0] != "start" || lines[1] != "2026-09-07-implement-feature" ||
		lines[2] != "--as-is" || lines[3] != "--exclusive" || lines[4] != "--no-attach" ||
		lines[5] != "--goal-file" || lines[7] != "--json" ||
		lines[8] != "--model" || lines[9] != "model-1" ||
		lines[10] != "--effort" || lines[11] != "high" {
		t.Fatalf("new session arguments = %#v", lines)
	}
}

func TestDeleteSessionRequiresExactConfirmationAndUsesDestructiveCLI(t *testing.T) {
	server := newTestServer(t)
	directory := t.TempDir()
	arguments := filepath.Join(directory, "arguments")
	helper := filepath.Join(directory, "dev-session")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGUMENTS\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUMENTS", arguments)
	server.config.DevSession = helper
	bad := httptest.NewRecorder()
	server.deleteSession(bad, httptest.NewRequest(
		http.MethodPost, "/", strings.NewReader(`{"confirmation":"wrong"}`),
	), "example")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad confirmation status = %d", bad.Code)
	}

	response := httptest.NewRecorder()
	server.deleteSession(response, httptest.NewRequest(
		http.MethodPost, "/", strings.NewReader(`{"confirmation":"example","force":true}`),
	), "example")
	if response.Code != http.StatusAccepted {
		t.Fatalf("delete response = %d %q", response.Code, response.Body.String())
	}
	operation := waitLifecycleOperation(t, server, "example")
	if operation.State != "complete" || operation.Kind != "delete" ||
		operation.StartedAt == "" || operation.UpdatedAt == "" {
		t.Fatalf("delete operation = %#v", operation)
	}
	statusResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(statusResponse, httptest.NewRequest(
		http.MethodGet, "/api/sessions/example/operation", nil,
	))
	if statusResponse.Code != http.StatusOK ||
		!strings.Contains(statusResponse.Body.String(), `"state":"complete"`) {
		t.Fatalf("deleted session operation status = %d %q", statusResponse.Code, statusResponse.Body.String())
	}
	data, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "delete\nexample\n--as-is\n--portal-authorized\n--force\n" {
		t.Fatalf("delete arguments = %q", data)
	}
}

func TestPortalLifecycleOperationsDelegateToOneHighLevelCommand(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		start     func(*Server, http.ResponseWriter, *http.Request, *session.Summary)
		body      string
		summary   *session.Summary
		expected  string
		operation string
		journal   string
	}{
		{
			name: "complete archive", start: (*Server).startArchive,
			body:     `{"mode":"complete"}`,
			summary:  &session.Summary{Manifest: session.Manifest{Slug: "example"}, Lifecycle: "active"},
			expected: "archive\nexample\n--as-is\n--portal-authorized\n", operation: "archive",
		},
		{
			name: "abandoned archive", start: (*Server).startArchive,
			body:     `{"mode":"abandoned"}`,
			summary:  &session.Summary{Manifest: session.Manifest{Slug: "example"}, Lifecycle: "active"},
			expected: "archive\nexample\n--as-is\n--portal-authorized\n--abandoned\n", operation: "archive",
		},
		{
			name: "revive", start: (*Server).startRevive,
			body:     `{"allowAbandoned":false}`,
			summary:  &session.Summary{Manifest: session.Manifest{Slug: "example"}, Archived: true, Lifecycle: "complete"},
			expected: "revive\nexample\n--as-is\n--portal-authorized\n", operation: "revive",
		},
		{
			name: "retry archive after tracking moved", start: (*Server).startArchive,
			body:     `{"mode":"abandoned"}`,
			summary:  &session.Summary{Manifest: session.Manifest{Slug: "example"}, Archived: true, Lifecycle: "complete"},
			expected: "archive\nexample\n--as-is\n--portal-authorized\n", operation: "archive",
			journal: ".archive.json",
		},
		{
			name: "retry revive after tracking moved", start: (*Server).startRevive,
			body:     `{"allowAbandoned":false}`,
			summary:  &session.Summary{Manifest: session.Manifest{Slug: "example"}, Lifecycle: "active"},
			expected: "revive\nexample\n--as-is\n--portal-authorized\n", operation: "revive",
			journal: ".revive.json",
		},
		{
			name: "retry abandoned revive without another confirmation", start: (*Server).startRevive,
			body: `{"allowAbandoned":false}`,
			summary: &session.Summary{
				Manifest: session.Manifest{Slug: "example"}, Archived: true, Lifecycle: "abandoned",
			},
			expected:  "revive\nexample\n--as-is\n--portal-authorized\n--allow-abandoned\n",
			operation: "revive", journal: ".revive.json",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTestServer(t)
			arguments := filepath.Join(t.TempDir(), "arguments")
			helper := filepath.Join(t.TempDir(), "dev-session")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGUMENTS\"\n"
			if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ARGUMENTS", arguments)
			server.config.DevSession = helper
			if testCase.journal != "" {
				root := filepath.Join(server.config.Workspace, "worktrees", ".locks")
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
				journal := "{}\n"
				if testCase.journal == ".archive.json" {
					journal = fmt.Sprintf(
						`{"schema":2,"slug":"example","workspace":%q,"phase":"tracking_archived","mode":"complete"}`,
						server.config.Workspace,
					)
				}
				if err := os.WriteFile(filepath.Join(root, "example"+testCase.journal), []byte(journal), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			response := httptest.NewRecorder()
			testCase.start(server, response, httptest.NewRequest(
				http.MethodPost, "/", strings.NewReader(testCase.body),
			), testCase.summary)
			if response.Code != http.StatusAccepted {
				t.Fatalf("start = %d %q", response.Code, response.Body.String())
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				server.operationMu.Lock()
				operation := server.operations["example"]
				server.operationMu.Unlock()
				if operation.State == "complete" {
					if operation.Kind != testCase.operation {
						t.Fatalf("operation = %#v", operation)
					}
					break
				}
				if operation.State == "failed" {
					t.Fatalf("operation = %#v", operation)
				}
				if time.Now().After(deadline) {
					t.Fatal("operation did not complete")
				}
				time.Sleep(10 * time.Millisecond)
			}
			data, err := os.ReadFile(arguments)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != testCase.expected {
				t.Fatalf("arguments = %q", data)
			}
		})
	}
}

func TestDeleteSessionExcludesConcurrentWorkspaceOperations(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	if err := os.WriteFile(
		filepath.Join(directory, "portal.yml"), []byte("schema: 1\nslug: example\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "dev-session")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	server.config.DevSession = helper
	lockPath := filepath.Join(t.TempDir(), "transition.lock")
	owner, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	server.config.TransitionLock = lockPath

	request := httptest.NewRequest(
		http.MethodPost, "/api/sessions/example/delete",
		strings.NewReader(`{"confirmation":"example"}`),
	)
	request.Header.Set("Origin", "https://workspace.example.test")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("delete response = %d %q", response.Code, response.Body.String())
	}
	time.Sleep(100 * time.Millisecond)
	server.operationMu.Lock()
	operation := server.operations["example"]
	server.operationMu.Unlock()
	if operation.State != "running" {
		t.Fatalf("deletion did not wait for the exclusive transition: %#v", operation)
	}
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	operation = waitLifecycleOperation(t, server, "example")
	if operation.State != "complete" {
		t.Fatalf("deletion did not continue after the transition was released: %#v", operation)
	}
}

func TestDeleteSessionRetryUsesTheJournaledForceSetting(t *testing.T) {
	server := newTestServer(t)
	root := filepath.Join(server.config.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	journal := fmt.Sprintf(
		`{"schema":1,"slug":"example","workspace":%q,"phase":"validated","force":true}`,
		server.config.Workspace,
	)
	if err := os.WriteFile(filepath.Join(root, "example.removal.json"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	arguments := filepath.Join(t.TempDir(), "arguments")
	helper := filepath.Join(t.TempDir(), "dev-session")
	if err := os.WriteFile(
		helper, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGUMENTS\"\n"), 0o755,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUMENTS", arguments)
	server.config.DevSession = helper
	response := httptest.NewRecorder()
	server.deleteSession(response, httptest.NewRequest(
		http.MethodPost, "/", strings.NewReader(`{"confirmation":"","force":false}`),
	), "example")
	if response.Code != http.StatusAccepted {
		t.Fatalf("delete retry = %d %q", response.Code, response.Body.String())
	}
	if operation := waitLifecycleOperation(t, server, "example"); operation.State != "complete" {
		t.Fatalf("delete retry operation = %#v", operation)
	}
	data, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "delete\nexample\n--as-is\n--portal-authorized\n--force\n" {
		t.Fatalf("delete retry arguments = %q", data)
	}
}

func TestDeleteSessionRetryReachesTheRemovalJournalAfterTrackingMoved(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	if err := os.WriteFile(
		filepath.Join(directory, "portal.yml"), []byte("schema: 1\nslug: example\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "first-attempt")
	helper := filepath.Join(t.TempDir(), "dev-session")
	script := `#!/bin/sh
if [ ! -e "$DELETE_MARKER" ]; then
  : > "$DELETE_MARKER"
  rm -rf -- "$DELETE_TRACKING"
  exit 19
fi
exit 0
`
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DELETE_MARKER", marker)
	t.Setenv("DELETE_TRACKING", directory)
	server.config.DevSession = helper
	handler := server.Handler()
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(
			http.MethodPost, "/api/sessions/example/delete",
			strings.NewReader(`{"confirmation":"example"}`),
		)
		r.Header.Set("Origin", "https://workspace.example.test")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		return response
	}

	first := request()
	if first.Code != http.StatusAccepted {
		t.Fatalf("first deletion = %d %q", first.Code, first.Body.String())
	}
	operation := waitLifecycleOperation(t, server, "example")
	if operation.State != "failed" || !strings.Contains(operation.Error, "exit status 19") {
		t.Fatalf("first deletion operation = %#v", operation)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tracking still exists after simulated move: %v", err)
	}
	second := request()
	if second.Code != http.StatusAccepted {
		t.Fatalf("retry deletion = %d %q", second.Code, second.Body.String())
	}
	operation = waitLifecycleOperation(t, server, "example")
	if operation.State != "complete" {
		t.Fatalf("retry deletion operation = %#v", operation)
	}
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
			ThreadID: threadID, Status: "idle", CollaborationMode: "default",
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
	if controller.message != "browser message" ||
		controller.messageID != "00000000-0000-4000-8000-000000000004" ||
		controller.actionContext != "" ||
		controller.queued != "queue message" ||
		controller.queueDeleted != "queued-1" || controller.queueStarted != "queued-1" ||
		controller.settings.CollaborationMode != "plan" || !controller.interrupt ||
		controller.decision != "accept" || controller.snoozed != "question-1" || answer != "yes" {
		t.Fatalf("browser operations were not delivered: %#v", controller)
	}
}

func TestRepositoryCacheDoesNotCrossArchiveTransition(t *testing.T) {
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
	if len(activeStatus) != 1 || !strings.Contains(activeStatus[0].CompareURL, "master...feature") {
		t.Fatalf("active status = %#v", activeStatus)
	}
	archived := *active
	archived.Terminal = true
	archived.Archived = true
	archivedStatus := server.repositories(context.Background(), &archived)
	if len(archivedStatus) != 1 || !strings.Contains(archivedStatus[0].CompareURL, strings.Repeat("1", 40)+"..."+strings.Repeat("2", 40)) {
		t.Fatalf("archived status reused mutable cache: %#v", archivedStatus)
	}
}

func waitLifecycleOperation(t *testing.T, server *Server, slug string) lifecycleOperation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		server.operationMu.Lock()
		operation := server.operations[slug]
		server.operationMu.Unlock()
		if operation.State == "complete" || operation.State == "failed" {
			return operation
		}
		if time.Now().After(deadline) {
			t.Fatalf("lifecycle operation did not finish: %#v", operation)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	workspace := t.TempDir()
	authorityDir := filepath.Join(t.TempDir(), "authority")
	if err := os.Mkdir(authorityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(t.TempDir(), "profile")
	if err := os.Symlink(t.TempDir(), profile); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Workspace:    workspace,
		BaseURL:      "https://workspace.example.test",
		DevSession:   "/run/current-system/sw/bin/dev-session",
		HostProfile:  profile,
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
