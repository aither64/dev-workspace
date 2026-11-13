package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/cluster"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func TestResourceCountsDistinguishOwnedStoppedUnknownAndStale(t *testing.T) {
	for _, test := range []struct {
		name         string
		statuses     []cluster.Status
		value        int
		known, stale bool
	}{
		{"absent", nil, 0, true, false},
		{"stopped", []cluster.Status{{State: "stopped", PresenceKnown: true, Found: true}}, 1, true, false},
		{"placeholder", []cluster.Status{{State: "unavailable", Notice: "unavailable"}}, 0, false, true},
		{"cached", []cluster.Status{{State: "stopped", PresenceKnown: true, Found: true, Notice: "changing"}}, 1, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := clusterCount(test.statuses)
			if got.Value != test.value || got.Known != test.known || got.Stale != test.stale {
				t.Fatalf("count: %#v", got)
			}
		})
	}
}
func TestTeamCountUsesRetainedMembershipIncludingLead(t *testing.T) {
	roster := &teamruntime.Roster{}
	for _, state := range []string{"ready", "creating", "failed", "archived", "idle", "removed"} {
		roster.Members = append(roster.Members, teamruntime.Member{State: state})
	}
	if got := teamCount(roster, ""); !got.Known || got.Value != 6 {
		t.Fatalf("team: %#v", got)
	}
	if got := teamCount(nil, ""); !got.Known || got.Value != 1 {
		t.Fatalf("solo: %#v", got)
	}
	if got := teamCount(nil, "unreadable"); got.Known {
		t.Fatalf("unreadable: %#v", got)
	}
}

func TestResourcesTemplateUsesOneParentAndAccessibleNestedPanels(t *testing.T) {
	server := newTestServer(t)
	response := httptest.NewRecorder()
	server.render(response, "session", pageData{Session: &session.Summary{Manifest: session.Manifest{Schema: 1, Slug: "example", Codex: session.Codex{ThreadID: "root"}}}, ClusterCount: countSnapshot{Known: true}, TeamCount: countSnapshot{Known: true, Value: 6}})
	body := response.Body.String()
	for _, marker := range []string{`data-session-tab="resources"`, `data-resource-tab="clusters"`, `data-resource-tab="reservations"`, `data-resource-tab="services"`, `aria-controls="resources"`, `aria-labelledby="resource-tab-clusters"`, `aria-labelledby="resource-tab-reservations"`, `aria-labelledby="resource-tab-services"`, `Team (6)`, `data-coordination-session="example"`, `data-resource-count="clusters"`, `data-resource-count="reservations"`, `data-resource-count="services"`} {
		if !strings.Contains(body, marker) {
			t.Errorf("missing %s", marker)
		}
	}
	for _, child := range []string{"clusters", "reservations", "services"} {
		if strings.Contains(body, `data-session-tab="`+child+`"`) {
			t.Errorf("child %s remains in sidebar", child)
		}
	}
}

func TestResourceSubtabBadgesRenderKnownUnknownAndStaleCounts(t *testing.T) {
	server := newTestServer(t)
	for _, count := range []countSnapshot{{Known: true}, {Known: true, Value: 3}, {}, {Known: true, Value: 2, Stale: true, ObservedAt: "2026-11-04T04:00:00Z"}} {
		t.Run(count.Display(), func(t *testing.T) {
			response := httptest.NewRecorder()
			server.render(response, "session", pageData{Session: &session.Summary{Manifest: session.Manifest{Slug: "example"}}, ClusterCount: count, ServiceCount: count})
			for _, kind := range []string{"clusters", "reservations", "services"} {
				_, button, found := strings.Cut(response.Body.String(), `id="resource-tab-`+kind+`"`)
				button, _, closed := strings.Cut(button, "</button>")
				text := count.Display()
				if kind == "reservations" {
					text = "?"
				}
				if !found || !closed || !strings.Contains(button, `class="badge"`) ||
					!strings.Contains(button, `data-count-text>`+text+`</span>`) || strings.Contains(button, " hidden") {
					t.Errorf("%s badge must remain visible with count %s: %s", kind, text, button)
				}
			}
		})
	}
}

func TestResourceDetailsRejectReplacementIncarnation(t *testing.T) {
	server := newTestServer(t)
	prepareInteractiveConversation(t, server, "example")
	summary, findErr := session.Find(server.config.Workspace, "example")
	if findErr != nil {
		t.Fatal(findErr)
	}
	request := httptest.NewRequest("GET", "/api/sessions/example/details", nil)
	request.Header.Set("X-Session-Observation", "previous-incarnation")
	response := httptest.NewRecorder()
	server.sessionDetails(response, request, summary)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "Session identity changed") {
		t.Fatalf("replacement: %d %s", response.Code, response.Body)
	}
}

func TestResourceDetailsAcceptExactObservationAndReturnCountCertainty(t *testing.T) {
	server := newTestServer(t)
	prepareInteractiveConversation(t, server, "example")
	summary, findErr := session.Find(server.config.Workspace, "example")
	if findErr != nil {
		t.Fatal(findErr)
	}
	identity, err := session.ObservationIdentity(summary.Workspace, summary.Slug, filepath.Join(summary.Workspace, summary.Root, summary.Slug), summary.Codex.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/api/sessions/example/details", nil)
	request.Header.Set("X-Session-Observation", identity)
	response := httptest.NewRecorder()
	server.sessionDetails(response, request, summary)
	if response.Code != 200 {
		t.Fatalf("details: %d %s", response.Code, response.Body)
	}
	var payload struct {
		ObservationIdentity  string
		ClusterCountSnapshot countSnapshot
		TeamCount            countSnapshot
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ObservationIdentity != identity || !payload.ClusterCountSnapshot.Known || payload.ClusterCountSnapshot.Value != 0 || !payload.TeamCount.Known || payload.TeamCount.Value != 1 {
		t.Fatalf("snapshot: %#v", payload)
	}
}
