package web

import (
	"strconv"

	"github.com/aither64/dev-workspace/portal/internal/repository"
)

type workflowCounterView struct {
	Label           string
	Value           string
	Class           string
	AccessibleLabel string
	Title           string
}

type workflowSummaryView struct {
	Show         bool
	Available    bool
	HasRuns      bool
	CompactText  string
	CompactLabel string
	CompactTitle string
	Counters     []workflowCounterView
}

// workflowSummary describes only the runs returned for the selected revision.
// A nil run slice means that no successful lookup has supplied run data yet.
func workflowSummary(status repository.Status) workflowSummaryView {
	if status.Origin == nil && status.GitHub == "" && status.OriginError == "" && status.Runs == nil {
		return workflowSummaryView{}
	}
	view := workflowSummaryView{Show: true, Available: status.Runs != nil && status.OriginError == ""}
	view.HasRuns = view.Available && len(status.Runs) > 0
	if !view.HasRuns {
		if view.Available {
			view.CompactText = "Workflows · 0 total"
			view.CompactLabel = "Workflows: 0 runs returned for this revision"
			view.CompactTitle = "No workflow runs were returned for this revision. The lookup returns up to 100 runs."
		} else {
			view.CompactText = "Workflows · unavailable"
			view.CompactLabel = "Workflow runs are unavailable for the exact selected revision"
			view.CompactTitle = "Workflow run data for the exact selected revision is unavailable."
		}
		return view
	}
	var total, queued, running, successful, failed int
	for _, run := range status.Runs {
		total++
		switch {
		case run.Conclusion == "success":
			successful++
		case run.Conclusion != "":
			if run.Conclusion != "skipped" && run.Conclusion != "neutral" {
				failed++
			}
		case run.Status == "completed":
			failed++
		case run.Status == "in_progress":
			running++
		case run.Status == "queued" || run.Status == "requested" || run.Status == "waiting" || run.Status == "pending":
			queued++
		}
	}
	for _, counter := range []struct {
		label, class, title string
		count               int
	}{
		{"Total", "total", "Returned workflow runs for this revision, up to 100. Includes skipped and neutral runs.", total},
		{"Queued", "queued", "Queued, requested, waiting, or pending runs with no conclusion.", queued},
		{"Running", "running", "Runs in progress with no conclusion.", running},
		{"Successful", "successful", "Runs with a successful conclusion.", successful},
		{"Failed", "failed", "Cancelled and other unsuccessful terminal runs, including completed runs without a conclusion.", failed},
	} {
		value := strconv.Itoa(counter.count)
		view.Counters = append(view.Counters, workflowCounterView{
			Label: counter.label, Value: value, Class: counter.class,
			AccessibleLabel: counter.label + " workflow runs: " + value,
			Title:           counter.title,
		})
	}
	return view
}
