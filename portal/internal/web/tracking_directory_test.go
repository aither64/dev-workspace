package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestTrackingDirectoryRouteAndValidation(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	root := filepath.Join(s.config.Workspace, "work", "example")
	if err := os.Mkdir(filepath.Join(root, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 201; i++ {
		if err := os.WriteFile(filepath.Join(root, "nested", fmt.Sprintf("%03d", i)), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	request := func(query string, status int) session.TrackingDirectoryPage {
		t.Helper()
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/sessions/example/directory"+query, nil))
		if response.Code != status || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%q: %d %s", query, response.Code, response.Body.String())
		}
		if status != 200 {
			return session.TrackingDirectoryPage{}
		}
		var page session.TrackingDirectoryPage
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	page := request("?path=", 200)
	for _, entry := range page.Entries {
		if filepath.Dir(entry.Path) != "." {
			t.Fatalf("eager descendant: %#v", entry)
		}
	}
	page = request("?path=nested", 200)
	if len(page.Entries) != 200 || page.NextCursor == nil {
		t.Fatalf("first page: %#v", page)
	}
	query := "?path=nested&cursor=" + url.QueryEscape(*page.NextCursor)
	if next := request(query, 200); len(next.Entries) != 1 || next.NextCursor != nil {
		t.Fatalf("last page: %#v", next)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "new"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	request(query, 409)
	for _, query := range []string{"", "?cursor=x", "?path=&path=", "?path=.&cursor=x", "?path=a%2F..%2Fnested", "?path=&cursor=", "?path=&cursor=x&cursor=y", "?path=&limit=200", "?path=%zz", "?path=;", "?path=.git", "?path=&cursor=bad"} {
		request(query, 400)
	}
	request("?path=missing", 404)
	request("?path=state.md", 404)
	response := httptest.NewRecorder()
	post := httptest.NewRequest("POST", "/api/sessions/example/directory?path=", nil)
	post.Header.Set("Origin", s.config.BaseURL)
	s.Handler().ServeHTTP(response, post)
	if response.Code != 405 || response.Header().Get("Allow") != "GET" {
		t.Fatal(response.Code)
	}
}
