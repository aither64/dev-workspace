package web

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

// Consume the portal's persisted JSON with the real Ruby initializer. HTTP
// admission alone cannot detect a field missing from Ruby's strict reader.
func TestCreationReceiptRubyInitializerContract(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, kind := range []string{"new", "plan", "fork"} {
			if direct && kind == "fork" {
				continue // Forks retain their source roster rather than a new preset.
			}
			for _, provider := range []string{"", "openai", "pool"} {
				t.Run(fmt.Sprintf("direct=%t/%s/provider=%s", direct, kind, provider), func(t *testing.T) {
					server := newTestServer(t)
					defer server.Close()
					notifications := false
					now := time.Now().UTC().Format(time.RFC3339Nano)
					receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace,
						ReceiptID: strings.Repeat("a", 64), DeletionHistorySHA256: planDigest(""),
						Attempt: 1, State: "failed", Phase: "Initializing", StartedAt: now, UpdatedAt: now,
						Validated: true, Goal: "Accepted request", Model: "model-1", Effort: "high",
						Request: creationRequest{Kind: kind, Slug: "2026-10-19-ruby-contract", Provider: provider,
							BrowserNotificationsEnabled: &notifications}}
					if kind == "new" {
						receipt.Request.Goal = receipt.Goal
					} else {
						receipt.Request.Source, receipt.Request.SourceThreadID = "source", "source-thread"
						receipt.Request.SourceIdentity = strings.Repeat("b", 64)
					}
					if kind == "plan" {
						receipt.Request.PlanTurnID, receipt.Request.PlanText = "plan-turn", "Accepted plan"
						receipt.Request.PlanSHA256 = planDigest(receipt.Request.PlanText)
						receipt.Request.DestinationSettings = true
					}
					if direct {
						receipt.Schema = 3
						receipt.DirectTeam = &teamruntime.Preset{ID: "solo", Name: "Solo", Description: "A single lead",
							CatalogDigest: strings.Repeat("c", 64), TeamDigest: strings.Repeat("d", 64),
							LeadModel: receipt.Model, LeadEffort: receipt.Effort, LeadInstructions: "Own the work.",
							Roles: []string{"lead"}, Members: []teamruntime.MemberSpec{}}
						receipt.Request.Team, receipt.Request.CatalogDigest = receipt.DirectTeam.ID, receipt.DirectTeam.CatalogDigest
					}
					if err := server.saveCreation(receipt); err != nil {
						t.Fatal(err)
					}
					script := `load ARGV.fetch(0)
current = JSON.parse(File.read(ARGV.fetch(1)))
request = current.fetch('request')
runner = DevSession::Runner.new(workspace: current.fetch('workspace'),
  env: {'DEV_WORKSPACES_STATE' => ARGV.fetch(3)})
receipt = runner.send(:creation_receipt_options, current.fetch('receiptId'), ARGV.fetch(2),
  expected_source: request['source'], expected_source_thread: request['sourceThreadId'],
  expected_source_identity: request['sourceIdentity'])
runner.send(:prepare_portal_creation_receipt!, receipt, request.fetch('slug'),
  request.fetch('kind') == 'fork' ? 'fork' : 'start',
  goal: request.fetch('kind') == 'fork' ? nil : current.fetch('goal'),
  model: current.fetch('model'), effort: current.fetch('effort'),
  direct_team: current['directTeam'], provider: ARGV[4])
`
					run := func(selected string) ([]byte, error) {
						args := []string{"-e", script, "../../../libexec/dev-session", server.creationPath(receipt.Request.Slug),
							server.creationEvidencePath(receipt), server.config.UserStateRoot}
						if selected != "" {
							args = append(args, selected)
						}
						return exec.Command("ruby", args...).CombinedOutput()
					}
					for attempt := 0; attempt < 2; attempt++ {
						if output, err := run(provider); err != nil {
							t.Fatalf("Ruby receipt initialization/retry: %v\n%s", err, output)
						}
					}
					if ok, err := server.readCreationBinding(receipt); !ok || err != nil {
						t.Fatalf("Ruby binding did not match the accepted Go receipt: %t, %v", ok, err)
					}
					changed := "pool"
					if provider == changed {
						changed = ""
					}
					if output, err := run(changed); err == nil || !strings.Contains(string(output), "provider does not match the accepted request") {
						t.Fatalf("changed provider accepted or wrong rejection: %v\n%s", err, output)
					}
				})
			}
		}
	}
}
