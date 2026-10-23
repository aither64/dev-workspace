package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestBrowserAssetVersionsForRetryAndQuestionUI(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	versionAfter := func(source, path string, previous int) int {
		t.Helper()
		matches := regexp.MustCompile(regexp.QuoteMeta(path)+`[?]v=([0-9]+)`).FindAllStringSubmatch(source, -1)
		if len(matches) != 1 {
			t.Fatalf("expected one versioned %s, got %d", path, len(matches))
		}
		version, err := strconv.Atoi(matches[0][1])
		if err != nil || version <= previous {
			t.Fatalf("%s reuses cached predecessor identity v%d: %s", path, previous, matches[0][0])
		}
		return version
	}
	// Previously selected modules can remain fresh in a browser's HTTP cache
	// for five minutes and lack the current retry notice presentation.
	// A fresh page must select a new module URL even after an older page
	// warmed that cache. Do not replace this with a missing-export fallback.
	versionAfter(read("static/app.js"), "/codex/assets/conversation.js", 24)
	session := read("templates/session.html")
	versionAfter(session, "/codex/assets/conversation.css", 11)
	versionAfter(session, "/static/style.css", 24)
	// All consumers of the changed app entry point select the same fresh
	// bytes, including navigation from index/archival to the conversation.
	var appVersion int
	for _, name := range []string{"session.html", "index.html", "automatic-archival.html"} {
		version := versionAfter(read("templates/"+name), "/static/app.js", 58)
		if appVersion != 0 && version != appVersion {
			t.Fatalf("%s selects app v%d, expected v%d", name, version, appVersion)
		}
		appVersion = version
	}
	// A new parent module still needs a new child URL: a browser may already
	// hold the old retry_activity bytes under the child's independent cache key.
	server := newTestServer(t)
	for _, path := range []string{"/codex/assets/conversation.css?v=12", "/static/style.css?v=25"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/css") {
			t.Fatalf("stylesheet %s: %d %s", path, response.Code, response.Header().Get("Content-Type"))
		}
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/codex/assets/conversation.js?v=25", nil))
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	versionAfter(response.Body.String(), "./retry_activity.js", 1)
}
