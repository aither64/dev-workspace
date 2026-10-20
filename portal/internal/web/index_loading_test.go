package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
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

func TestIndexMembershipRetainsOnlySameIdentityMeasuredClusterCounts(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	prepareInteractiveConversation(t, server, "example")
	summaries, err := server.listSessions()
	if err != nil || len(summaries) != 1 {
		t.Fatalf("sessions: %v %v", summaries, err)
	}
	for _, count := range []int{0, 2} {
		for _, same := range []bool{false, true} {
			identity := summaries[0].ObservationIdentity
			if !same {
				identity = "previous-incarnation"
			}
			server.indexStatusCache = cachedIndexStatus{statuses: []indexSessionStatus{{Slug: "example", ObservationIdentity: identity, RunningClusters: count, RunningClustersKnown: true}}}
			var minimal cachedIndexStatus
			server.computeIndexStatusPublishing(context.Background(), nil, nil, func(snapshot cachedIndexStatus) { minimal = snapshot })
			if len(minimal.statuses) != 1 {
				t.Fatalf("membership missing: %#v", minimal)
			}
			got := minimal.statuses[0]
			if got.RunningClustersKnown != same || got.RunningClusters != map[bool]int{true: count, false: 0}[same] {
				t.Fatalf("same=%v measured=%d got=%#v", same, count, got)
			}
		}
	}
}

func TestIndexClusterPresentationMixedAssets(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required")
	}
	if output, err := exec.Command(node, "index_cluster_contract_test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("cluster presentation: %v\n%s", err, output)
	}
}
