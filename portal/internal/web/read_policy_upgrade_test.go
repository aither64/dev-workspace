package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/progressobserver"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

// This fixture is the deployed predecessor's binding, independent of the new
// policy generator and its retry compatibility transformation.
func predecessorMemberInstructions(workspace, slug, address, role string, sensitive bool) string {
	prefix := ""
	if sensitive {
		prefix = fmt.Sprintf("This is a sensitive session. Keep its plans, state, notes, review evidence and handoffs locally under %s. Never stage or commit these records, copy them into tracked notes or other sessions, or force-add ignored records. Maintain substantive local tracking without the ordinary initial/checkpoint/archive tracking commits. Project code retains its normal Git workflow. Pass this sensitivity instruction to any delegated agent.\n\n", filepath.Join(workspace, "work", slug))
	}
	return prefix + fmt.Sprintf("This Codex conversation is bound to development session %q in workspace %q as team member %q. Before working on session files, run dev-session current and confirm it prints this session slug. DEV_SESSION_SLUG and DEV_SESSION_WORKSPACE must either both be absent or both match this binding. Stop and report any mismatch. %s\n\n%s", slug, workspace, address, workspacecodex.CoordinationInstructions, role)
}

func TestReadPolicyUpgradeRetainsPersistedSubmissions(t *testing.T) {
	for _, kind := range []string{"assignment", "progress", "direct"} {
		for _, phase := range []string{"prepared", "submitting", "accepted"} {
			for _, sensitive := range []bool{false, true} {
				for _, stock := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/sensitive=%t/stock=%t", kind, phase, sensitive, stock), func(t *testing.T) {
						ctx := context.Background()
						s, c, summary, now := progressFixture(t)
						workspace, slug, thread := s.config.Workspace, summary.Slug, "observer-thread"
						if sensitive {
							path := filepath.Join(workspace, "work", slug, "portal.yml")
							data, err := os.ReadFile(path)
							if err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(path, append(data, []byte("\nsensitive: true\n")...), 0600); err != nil {
								t.Fatal(err)
							}
						}
						role := "Custom observer policy"
						if stock {
							data, err := os.ReadFile("../progressobserver/testdata/extended-stock-instructions.txt")
							if err != nil {
								t.Fatal(err)
							}
							role = string(data)
						}
						service, err := s.teamService(slug)
						if err != nil {
							t.Fatal(err)
						}
						roster, err := service.Store.Update(ctx, slug, summary.Codex.ThreadID, false, func(r *teamruntime.Roster) error {
							r.Members[0].Instructions = role
							// An accepted stock assignment may have been compacted before refresh.
							if phase == "accepted" && kind != "progress" && stock {
								r.Members[0].Instructions, _ = progressobserver.RefreshedStockInstructions(role)
							}
							return nil
						})
						if err != nil {
							t.Fatal(err)
						}
						member := roster.Members[0]
						current, err := service.MemberTurnPolicy(slug, summary.Codex.ThreadID, member)
						if err != nil {
							t.Fatal(err)
						}
						original := codex.TurnOptions{Model: member.Model, ReasoningEffort: member.Effort, ThreadPolicy: current}
						original.ThreadPolicy.DeveloperInstructions = predecessorMemberInstructions(workspace, slug, member.Address, role, sensitive)
						id := "0123456789abcdef0123456789abcdef"
						text, action := "Original request", ""
						switch kind {
						case "assignment":
							text = "Team assignment from lead to observer0:\n\nOriginal request\n\nSend results or blocking questions to lead with report_to_lead. Use a new message_id for each message; reuse it only to retry that message."
							action = "team:" + slug + ":lead:observer0"
						case "progress":
							text = "Progress check v3. Original request"
							action = "progress:" + slug + ":check"
						}
						ledger := filepath.Join(t.TempDir(), "submissions.json")
						options := codex.ClientOptions{SubmissionLedgerPath: ledger}
						old := workspacecodex.NewWithOptions("/unused.sock", workspace, options)
						if err := old.PrepareSendWithOptions(thread, text, id, action, false, original); err != nil {
							t.Fatal(err)
						}
						old.Close()
						// Model the durable predecessor states using the real SDK ledger format;
						// its SDK-generated text/options digests stay untouched across restart.
						data, err := os.ReadFile(ledger)
						if err != nil {
							t.Fatal(err)
						}
						var saved map[string]any
						if err := json.Unmarshal(data, &saved); err != nil {
							t.Fatal(err)
						}
						attempt := saved["sends"].(map[string]any)[thread].(map[string]any)[id].(map[string]any)
						attempt["state"] = phase
						if phase == "accepted" {
							attempt["turnId"] = "original-turn"
						}
						data, err = json.Marshal(saved)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(ledger, data, 0600); err != nil {
							t.Fatal(err)
						}
						c.Client = workspacecodex.NewWithOptions("/unused.sock", workspace, options)
						t.Cleanup(c.Client.Close)
						if phase == "accepted" && kind != "direct" {
							if err := c.Client.CompactAcceptedSendOptions(action); err != nil {
								t.Fatal(err)
							}
						}
						before, err := os.ReadFile(ledger)
						if err != nil {
							t.Fatal(err)
						}
						service.Client = c
						var got codex.TurnOptions
						switch kind {
						case "assignment":
							receipt, err := service.Assign(ctx, slug, summary.Codex.ThreadID, "lead", "observer0", "Original request", "", "", id)
							if err != nil {
								t.Fatal(err)
							}
							if phase == "accepted" && receipt.TurnID != "original-turn" {
								t.Fatal(receipt)
							}
						case "progress":
							state, _ := s.progressStore().Load(summary.Codex.ThreadID)
							state.Pending = &progressobserver.Pending{ID: id, Thread: thread, Address: member.Address, Model: member.Model, Effort: member.Effort, Message: text, StartedAt: now}
							if err := s.sendProgressCheck(ctx, service, &member, slug, &state, false, func(e error) error { return e }); err != nil {
								t.Fatal(err)
							}
							if phase == "accepted" && state.Pending.Turn != "original-turn" {
								t.Fatal(state.Pending)
							}
						case "direct":
							client := memberConversationClient{Client: c, service: service, slug: slug, rootThreadID: summary.Codex.ThreadID, address: member.Address, threadID: thread}
							got, err = client.turnOptions(text, id, action)
							if err != nil {
								t.Fatal(err)
							}
						}
						if kind != "direct" {
							if phase == "accepted" {
								if len(c.sends) != 0 {
									t.Fatal("accepted receipt was dispatched again")
								}
							} else {
								if len(c.sends) != 1 {
									t.Fatal(c.sends)
								}
								got = c.sends[0].options
							}
						}
						if phase != "accepted" || kind == "direct" {
							if !reflect.DeepEqual(got, original) {
								t.Fatal("original submission options changed")
							}
							attempted, err := c.Client.SendAttemptedWithOptions(ctx, thread, text, id, action, got)
							if err != nil || !attempted {
								t.Fatal("real ledger rejected retry", attempted, err)
							}
						}
						after, err := os.ReadFile(ledger)
						if err != nil || !bytes.Equal(before, after) {
							t.Fatal("retry rewrote original ledger", err)
						}
						if _, _, err := teamruntime.ResolveReadPolicyRetry(ctx, c, workspace, slug, thread, text+" changed", id, action, codex.TurnOptions{ThreadPolicy: current}); err == nil {
							t.Fatal("changed text accepted")
						}
						fresh, accepted, err := teamruntime.ResolveReadPolicyRetry(ctx, c, workspace, slug, thread, "New work", "fresh-id", action, codex.TurnOptions{ThreadPolicy: current})
						if err != nil || accepted != nil || fresh.ThreadPolicy.DeveloperInstructions != current.DeveloperInstructions {
							t.Fatal("fresh ID lost current policy", err)
						}
						for _, changed := range []codex.ThreadPolicy{
							{DeveloperInstructions: current.DeveloperInstructions + " Changed role.", Sandbox: current.Sandbox, ApprovalPolicy: current.ApprovalPolicy},
							{DeveloperInstructions: current.DeveloperInstructions, Sandbox: "danger-full-access", ApprovalPolicy: "on-request"},
							{DeveloperInstructions: strings.ReplaceAll(current.DeveloperInstructions, `"example"`, `"foreign"`), Sandbox: current.Sandbox, ApprovalPolicy: current.ApprovalPolicy},
							{DeveloperInstructions: current.DeveloperInstructions, Sandbox: current.Sandbox, ApprovalPolicy: current.ApprovalPolicy, MCPServer: &codex.ThreadMCPServer{Name: "changed", Command: "changed", Tool: "changed"}},
						} {
							if teamruntime.SameMemberReadPolicy(workspace, slug, original.ThreadPolicy, changed) {
								t.Fatal("changed role/access/identity/MCP accepted")
							}
						}
					})
				}
			}
		}
	}
}
