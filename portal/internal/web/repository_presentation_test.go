package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestWorkflowSummaryClassifiesReturnedRuns(t *testing.T) {
	tests := []struct {
		name, status, conclusion string
		category                 string
	}{
		{"queued", "queued", "", "queued"},
		{"requested", "requested", "", "queued"},
		{"waiting", "waiting", "", "queued"},
		{"pending", "pending", "", "queued"},
		{"running", "in_progress", "", "running"},
		{"success takes precedence", "in_progress", "success", "successful"},
		{"failure", "completed", "failure", "failed"},
		{"cancelled", "completed", "cancelled", "failed"},
		{"timed out", "completed", "timed_out", "failed"},
		{"action required", "completed", "action_required", "failed"},
		{"startup failure", "completed", "startup_failure", "failed"},
		{"stale", "completed", "stale", "failed"},
		{"unknown conclusion", "queued", "new_terminal_result", "failed"},
		{"completed without conclusion", "completed", "", "failed"},
		{"skipped", "completed", "skipped", ""},
		{"neutral", "completed", "neutral", ""},
		{"unknown nonterminal", "new_status", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			view := workflowSummary(repository.Status{Runs: []repository.Run{{Status: test.status, Conclusion: test.conclusion}}})
			if !view.Show || !view.Available || !view.HasRuns || len(view.Counters) != 5 {
				t.Fatalf("summary = %#v", view)
			}
			for index, category := range []string{"total", "queued", "running", "successful", "failed"} {
				want := "0"
				if index == 0 || category == test.category {
					want = "1"
				}
				if view.Counters[index].Class != category || view.Counters[index].Value != want {
					t.Fatalf("%s counter = %#v, want %s", category, view.Counters[index], want)
				}
			}
		})
	}
}

func TestWorkflowSummaryAvailability(t *testing.T) {
	origin := &repository.Origin{Provider: "github", Repository: "example/project", Label: "GitHub"}
	for _, test := range []struct {
		name      string
		status    repository.Status
		show      bool
		available bool
		hasRuns   bool
		compact   string
	}{
		{"local only", repository.Status{}, false, false, false, ""},
		{"origin skeleton", repository.Status{Origin: origin}, true, false, false, "Workflows · unavailable"},
		{"legacy origin skeleton", repository.Status{GitHub: "example/project"}, true, false, false, "Workflows · unavailable"},
		{"lookup failed", repository.Status{Origin: origin, OriginError: "unavailable", Runs: []repository.Run{{Conclusion: "success"}}}, true, false, false, "Workflows · unavailable"},
		{"successful zero", repository.Status{Origin: origin, Runs: []repository.Run{}}, true, true, false, "Workflows · 0 total"},
		{"local supplied runs", repository.Status{Runs: []repository.Run{{Conclusion: "success"}}}, true, true, true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := workflowSummary(test.status)
			if view.Show != test.show || view.Available != test.available || view.HasRuns != test.hasRuns {
				t.Fatalf("summary = %#v", view)
			}
			if !test.hasRuns {
				if len(view.Counters) != 0 || view.CompactText != test.compact {
					t.Fatalf("compact summary = %#v", view)
				}
				if test.show && (view.CompactTitle == "" || !strings.Contains(view.CompactLabel, "revision")) {
					t.Fatalf("compact summary lacks scope: %#v", view)
				}
				return
			}
			if len(view.Counters) != 5 || view.Counters[0].Value != "1" {
				t.Fatalf("run counters = %#v", view.Counters)
			}
			for _, counter := range view.Counters {
				if counter.Title == "" || !strings.Contains(counter.AccessibleLabel, counter.Label) ||
					!strings.Contains(counter.AccessibleLabel, counter.Value) {
					t.Fatalf("inaccessible counter = %#v", counter)
				}
			}
		})
	}
}

func TestWorkflowCardsShowClosedCountersAndRunDetails(t *testing.T) {
	server := newTestServer(t)
	origin := &repository.Origin{Provider: "github", Repository: "example/project", Label: "GitHub"}
	for _, test := range []struct {
		name   string
		status repository.Status
		want   []string
		absent []string
	}{
		{"local only", repository.Status{Name: "local"}, nil,
			[]string{`data-repository-workflows`, `repository-workflows-compact`}},
		{"archived skeleton", repository.Status{Name: "project", Origin: origin},
			[]string{`Workflows · unavailable`, `role="note"`, `Workflow runs are unavailable for the exact selected revision`},
			[]string{`data-repository-workflows`, `repository-workflow-counters`, `Workflows · 0 total`}},
		{"successful zero", repository.Status{Name: "project", Origin: origin, Runs: []repository.Run{}},
			[]string{`Workflows · 0 total`, `role="note"`, `Workflows: 0 runs returned for this revision`,
				`No workflow runs were returned for this revision. The lookup returns up to 100 runs.`},
			[]string{`data-repository-workflows`, `repository-workflow-counters`, `Workflows · unavailable`}},
		{"lookup error", repository.Status{Name: "project", Origin: origin, OriginError: "<timeout>",
			Runs: []repository.Run{{Conclusion: "success"}}},
			[]string{`GitHub: &lt;timeout&gt;`, `Workflows · unavailable`},
			[]string{`data-repository-workflows`, `repository-workflow-counters`, `Workflows · 0 total`}},
		{"returned runs", repository.Status{Name: "project", Origin: origin, Runs: []repository.Run{
			{WorkflowName: "<build>", Status: "completed", Conclusion: "success", HeadSHA: strings.Repeat("a", 40),
				URL: "https://github.com/example/project/actions/runs/123"},
			{WorkflowName: "cancelled", Status: "completed", Conclusion: "cancelled", HeadSHA: strings.Repeat("a", 40)},
			{WorkflowName: "queued", Status: "queued", HeadSHA: strings.Repeat("a", 40)},
		}}, []string{`Total workflow runs: 3`, `Queued workflow runs: 1`, `Running workflow runs: 0`, `Successful workflow runs: 1`,
			`Failed workflow runs: 1`, `title="Runs in progress with no conclusion."`, `&lt;build&gt;`,
			`class="run-state cancelled" title="Status: completed; conclusion: cancelled"`,
			`href="https://github.com/example/project/actions/runs/123" target="_blank" rel="noreferrer"`},
			[]string{`<build>`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.render(response, "session", pageData{
				Session:      &session.Summary{Manifest: session.Manifest{Slug: "example"}, Archived: true},
				Repositories: []repository.Status{test.status},
			})
			body := response.Body.String()
			for _, marker := range test.want {
				if !strings.Contains(body, marker) {
					t.Fatalf("missing %q in card: %s", marker, body)
				}
			}
			for _, marker := range test.absent {
				if strings.Contains(body, marker) {
					t.Fatalf("unexpected %q in card: %s", marker, body)
				}
			}
			if len(test.status.Runs) > 0 && test.status.OriginError == "" {
				if !strings.Contains(body, `<details class="repository-workflows" data-repository-workflows>`) ||
					!strings.Contains(body, `<summary>Workflows `) ||
					!strings.Contains(body, `role="group"`) || !strings.Contains(body, `title="Returned workflow runs for this revision, up to 100.`) {
					t.Fatalf("workflow summary is not a closed, described disclosure: %s", body)
				}
			}
		})
	}
}
