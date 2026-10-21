package web

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/cluster"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

// Run with PORTAL_BROWSER_TEST=1 and Nix-provided Node, Playwright modules and
// PLAYWRIGHT_BROWSERS_PATH. The browser controls API responses, while the actual
// server supplies the session template, CSP, scripts, styles and upload service.
func TestQuestionBrowser(t *testing.T) {
	if os.Getenv("PORTAL_BROWSER_TEST") != "1" {
		t.Skip("set PORTAL_BROWSER_TEST=1 to run the Playwright question regression")
	}
	server := newTestServer(t)
	defer server.Close()
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\ncodex:\n" +
		"  thread_id: thread-1\n  socket_path: /run/dev-workspace-codex/app-server.sock\n" +
		"  client_version: 0.152.1\ncreation:\n  state: ready\n  initial_goal_sent: true\n" +
		"artifacts:\n  - path: reports/deep/499.md\n    label: Report 499\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	runWebGit(t, "init", "--initial-branch=master", server.config.Workspace)
	for i := 0; i < 500; i++ {
		path := filepath.Join(directory, "reports", "deep", fmt.Sprintf("%03d.md", i))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# Original artifact bytes\r\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	image, imageErr := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII=")
	if imageErr != nil {
		t.Fatal(imageErr)
	}
	for name, data := range map[string][]byte{
		"preview.png": image,
		"passive.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="window.passiveFileExecuted=true"></svg>`),
		"long.md":     []byte(strings.Repeat("## Scrollable preview\n\nOriginal long file bytes.\n\n", 100)),
	} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeWebRuntimeAuthority(t, server, "example")
	events := make(chan struct{}, 1)
	server.config.Codex = &browserContractCodex{emptyPrompts: true, events: events, transcript: codex.Transcript{
		ThreadID: "thread-1", Status: "active", CollaborationMode: "plan",
	}}
	httpServer := httptest.NewUnstartedServer(nil)
	server.config.BaseURL = "https://" + httpServer.Listener.Addr().String()
	server.trustedOrigins = []string{server.config.BaseURL}
	var err error
	server.conversation, err = conversation.NewHandler(conversation.Options{
		AllowedOrigins: []string{server.config.BaseURL}, BasePath: "/codex",
		Shutdown: server.stopping, Resolver: conversation.ResolverFunc(server.resolveConversation),
	})
	if err != nil {
		t.Fatal(err)
	}
	server.uploadHandler, err = conversation.NewUploadHandler(conversation.UploadOptions{
		BasePath: "/uploads", AllowedOrigins: []string{server.config.BaseURL}, Resolve: server.resolveUploads,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	httpServer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fixture/clusters" {
			state := r.URL.Query().Get("state")
			status := cluster.Status{Kind: "alpha", Label: "Alpha", State: state, Ready: state == "running", Topology: "dual", Network: "bridge",
				Services: []cluster.Service{
					{Label: "Web UI", Accounts: []cluster.Account{{Label: "Administrator", Fields: []cluster.Field{{Label: "Login", Value: "test-user"}, {Label: "Password", Value: "fixture-secret", Secret: true}}}}},
					{Label: "Database", Accounts: []cluster.Account{{Label: "Database", Fields: []cluster.Field{{Label: "Login", Value: "database-user"}}}}},
				}, Commands: []cluster.Command{{Label: "services", Value: "alpha-devcluster ssh example services"}}}
			if r.URL.Query().Get("legacy") != "1" {
				status.Actions = []string{"start", "release"}
				if state == "running" {
					status.Actions = []string{"stop", "release"}
				}
			}
			if r.URL.Query().Get("busy") == "1" {
				status.Notice = "Cluster is changing. Status will update when the operation finishes."
			}
			data := pageData{Session: &session.Summary{Interactive: true}, Clusters: []cluster.Status{status}}
			if state == "absent" {
				data.Clusters = nil
			}
			if err := server.templates.ExecuteTemplate(w, "clusters", data); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Path == "/fixture/team" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			data := pageData{
				Session:   &session.Summary{Manifest: session.Manifest{Slug: "example", Codex: session.Codex{ThreadID: "01a12199-96b2-7773-8d2a-4f54a78c7f6e"}}, Interactive: true},
				TeamRoles: teamRoles(teamruntime.Presets()),
				DirectTeam: &teamruntime.Roster{Members: []teamruntime.Member{
					{Address: "architect0", State: "ready", Access: "workspace_write", Model: "model-1", Effort: "medium", Thread: "01a10c52-451d-78b2-a8f9-63da636e13fd"},
					{Address: "reviewer-with-long-address0", State: "ready", Access: "read_only", Model: "gpt-6.1-sol-with-a-long-model-name", Effort: "xhigh", Thread: "01a12617-e2db-7a50-8e60-9ba7687670d3"},
				}},
				RemovedMembers: []teamruntime.Member{{Address: "removed0", State: "removed", Model: "model-1", Effort: "medium", Thread: "01a1265c-f81e-7100-8915-dda1ef347a0e"}},
			}
			if err := server.templates.ExecuteTemplate(w, "member-status", data); err != nil {
				t.Error(err)
			}
			return
		}

		if r.URL.Path == "/fixture/presentation" {
			transcript := codex.Transcript{ThreadID: "thread-1", Status: "idle", CollaborationMode: "default", Entries: []codex.TranscriptEntry{
				{Kind: "userMessage", ItemID: "user", Text: "# User heading\n\n- item\n\n[Plan](" + filepath.Join(server.config.Workspace, "work", "example", "plan.md") + ":1)"},
				{Kind: "userMessage", ItemID: "team", Text: "Team assignment from lead to implementer0:\n\nInternal work."},
				{Kind: "agentMessage", ItemID: "agent", Text: "A reply."},
			}}
			token, prefix := "\ue200cite\ue202ref\ue201", "Příliš 🐈 **answer** "
			transcript.Entries[2].Text = prefix + token + "\n\n`" + token + "`"
			transcript.Entries[2].Citations = []codex.CitationSpan{{Start: len(prefix), End: len(prefix) + len(token), Sources: []codex.CitationSource{{URL: "https://example.test/source", Title: `Český "title"`, Number: 1}}}}
			display := transcript.Entries[0].Text
			transcript.Entries[0].Text += "\n\nGenerated attachment envelope"
			transcript.Entries[0].DisplayText = &display
			transcript.Entries[0].Attachments = []codex.Attachment{{ID: "fixture-attachment", Name: "fixture.txt", Size: 17, State: "ready", DownloadURL: "/fixture/download"}}
			server.presentTranscript(&transcript)
			s := codex.TranscriptPage{Transcript: transcript}
			server.writeJSON(w, http.StatusOK, s)
			return
		}
		if r.URL.Path == "/fixture/download" {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Disposition", `attachment; filename="fixture.txt"`)
			_, _ = w.Write([]byte("Artifact fixture\n"))
			return
		}
		if r.URL.Path == "/fixture/refresh" {
			select {
			case events <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		handler.ServeHTTP(w, r)
	})
	httpServer.StartTLS()
	defer httpServer.Close()
	for _, script := range []string{"portal_improvements_browser_test.cjs", "question_browser_test.cjs", "optional_questions_browser_test.cjs", "page_lifecycle_browser_test.cjs", "presentation_browser_test.cjs", "team_settings_browser_test.cjs", "team_activity_browser_test.cjs", "usability_browser_test.cjs", "repository_review_live_browser_test.cjs", "paging_browser_live_test.cjs", "mode_refresh_browser_test.cjs", "cluster_controls_browser_test.cjs", "recovery_display_browser_test.cjs"} {
		t.Run(script, func(t *testing.T) {
			command := exec.Command("node", script, httpServer.URL)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", script, err, output)
			} else {
				t.Log(string(output))
			}
		})
	}
}
