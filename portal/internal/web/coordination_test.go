package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestOperationsNavigationDoesNotReadOrEmbedStatus(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.config.DevSession = filepath.Join(t.TempDir(), "unavailable-dev-session")
	for _, path := range []string{"/", "/operations"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d: %s", path, response.Code, response.Body.String())
		}
		body := response.Body.String()
		if path == "/" {
			if !strings.Contains(body, `href="/operations"`) || !strings.Contains(body, "data-session-reservation") || strings.Contains(body, "data-coordination aria-live") {
				t.Fatalf("index does not link to the separate status page: %s", body)
			}
		} else if !strings.Contains(body, "data-coordination class=") || !strings.Contains(body, `href="/"`) {
			t.Fatalf("Operations page lacks status or workspace navigation: %s", body)
		}
	}
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"portal.yml": "schema: 1\nslug: example\n", "plan.md": "# Plan\n", "state.md": "---\nlifecycle: active\n---\n"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/example/", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `href="#reservations"`) || !strings.Contains(response.Body.String(), `data-coordination-session="example"`) {
		t.Fatalf("session lacks reservation navigation: %d: %s", response.Code, response.Body.String())
	}
	identity, err := session.ObservationIdentity(server.config.Workspace, "example", directory, "")
	if err != nil || !strings.Contains(response.Body.String(), `data-coordination-observation="`+identity+`"`) {
		t.Fatal("session reservation pane lacks its observed incarnation")
	}
	if strings.Index(response.Body.String(), `href="/operations"`) > strings.Index(response.Body.String(), `href="/automatic-archival"`) {
		t.Fatal("Operations must precede Automatic archival")
	}
}

func TestReservationCardsCarryObservationThroughInitialAndCachedIndex(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"portal.yml": "schema: 1\nslug: example\n", "plan.md": "# Plan\n", "state.md": "---\nlifecycle: active\n---\n"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := session.ObservationIdentity(server.config.Workspace, "example", directory, "")
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if !strings.Contains(response.Body.String(), `data-session-observation="`+identity+`"`) {
			t.Fatalf("card lacks incarnation: %s", response.Body.String())
		}
	}
	check()
	status := server.computeIndexStatus(context.Background())
	if len(status.statuses) != 1 || status.statuses[0].ObservationIdentity != identity {
		t.Fatalf("wrong index identity: %+v", status.statuses)
	}
	server.indexStatusMu.Lock()
	status.created = time.Now()
	server.indexStatusCache = status
	server.indexStatusMu.Unlock()
	check()
	retained := filepath.Join(t.TempDir(), "original")
	if err := os.Rename(directory, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"portal.yml", "plan.md", "state.md"} {
		content, err := os.ReadFile(filepath.Join(retained, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	replacement, err := session.ObservationIdentity(server.config.Workspace, "example", directory, "")
	if err != nil {
		t.Fatal(err)
	}
	if replacement == identity {
		t.Fatal("replacement retained the original identity")
	}
	summaries, err := server.listSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, summary := range summaries {
		if summary.Slug == "example" && summary.ObservationIdentity != replacement {
			t.Fatal("replacement card retained the old identity")
		}
	}
}
