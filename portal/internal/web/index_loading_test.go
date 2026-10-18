package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIndexStatusDistinguishesColdLoadingFromRefreshFailure(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	for _, test := range []struct {
		name    string
		cache   cachedIndexStatus
		loading bool
	}{
		{"cold", cachedIndexStatus{}, true},
		{"cold failure", cachedIndexStatus{warning: "Refresh failed"}, false},
		{"cached", cachedIndexStatus{created: time.Now(), authoritative: true}, false},
		{"cached failure", cachedIndexStatus{created: time.Now(), warning: "Refresh failed"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server.indexStatusMu.Lock()
			server.indexStatusCache = test.cache
			server.indexStatusMu.Unlock()
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/index-status?progress=1", nil))
			var result struct {
				Loading *bool `json:"loading"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Loading == nil || *result.Loading != test.loading {
				t.Fatalf("loading response = %s", response.Body.String())
			}
		})
	}
}
