package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func TestPiRuntimeUpgradeCLIUsesOneOriginCheckedPrivateRequest(t *testing.T) {
	for _, outcome := range []string{"ready", "refused", "redirect", "wrong-identity", "malformed", "pending", "retired", "retirement-mismatch", "ordinary-team-echo", "retired-team-echo", "team-ready", "team-compact", "team-pending", "team-starting", "team-wrong-id", "team-normalized-id", "team-missing-id", "team-wrong-disposition", "team-input-id", "team-input-digest", "team-pending-echo", "team-pending-input", "team-pending-digest", "team-pending-disposition", "team-starting-echo", "team-unknown-field", "team-trailing", "team-wrong-scope", "team-wrong-workspace", "team-wrong-generation", "team-wrong-request", "team-wrong-phase", "team-malformed", "team-refused", "team-redirect"} {
		t.Run(outcome, func(t *testing.T) {
			// Long test names plus Nix's TMPDIR can exceed sockaddr_un.
			root, err := os.MkdirTemp("", "pi-cli-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			socket := filepath.Join(root, "portal.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(socket, 0660); err != nil {
				t.Fatal(err)
			}
			requestID := "11111111-1111-4111-8111-111111111111"
			retiring := outcome == "retired" || outcome == "retirement-mismatch" || outcome == "retired-team-echo"
			recoveringTeam := strings.HasPrefix(outcome, "team-")
			originalID := "33333333-3333-4333-8333-333333333333"
			member := "lead"
			if outcome == "team-compact" {
				originalID = strings.Repeat("3", 32)
				member = "implementer1"
			}
			slug := "2026-11-17-ready"
			var calls atomic.Int32
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.URL.Path != "/api/sessions/"+slug+"/pi-runtime-upgrade" || r.Header.Get("Origin") != "https://portal.fixture" || r.Host != "portal.fixture" {
					t.Error("wrong ordinary portal request", r.Method, r.URL.Path, r.Host)
					w.WriteHeader(400)
					return
				}
				var body map[string]string
				expectedFields := 3
				if retiring {
					expectedFields = 6
				} else if recoveringTeam {
					expectedFields = 5
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != expectedFields || body["workspace"] != root || body["requestId"] != requestID || body["expectedGeneration"] != strings.Repeat("a", 64) {
					t.Error("wrong request body", body, err)
					w.WriteHeader(400)
					return
				}
				if retiring && (body["retireUnknownInput"] != originalID || body["inputDigest"] != strings.Repeat("d", 64) || body["member"] != "lead") {
					t.Error("retirement tuple changed")
					w.WriteHeader(400)
					return
				}
				if recoveringTeam && (body["recoverTeamConflict"] != originalID || body["member"] != member || body["retireUnknownInput"] != "" || body["inputDigest"] != "") {
					t.Error("team recovery selector changed", body)
					w.WriteHeader(400)
					return
				}
				switch outcome {
				case "refused", "team-refused":
					w.WriteHeader(409)
					return
				case "redirect", "team-redirect":
					w.Header().Set("Location", "/must-not-follow")
					w.WriteHeader(307)
					return
				case "malformed", "team-malformed":
					w.Write([]byte("not JSON"))
					return
				}
				ref := agentbackend.SessionRef{Workspace: root, Slug: slug, Incarnation: strings.Repeat("c", 64)}
				result := piruntime.UpgradeResult{Runtime: piruntime.RuntimeRef{Session: ref, RequestID: requestID, Generation: strings.Repeat("b", 64)}, Previous: piruntime.RuntimeRef{Session: ref, Generation: strings.Repeat("a", 64)}, AppStorePath: "/nix/store/00000000000000000000000000000000-fixture", Phase: "ready"}
				if retiring {
					result.Disposition = "retired-unknown"
					result.InputID = originalID
					result.InputDigest = strings.Repeat("d", 64)
				}
				if outcome == "retirement-mismatch" {
					result.InputID = requestID
				}
				if outcome == "wrong-identity" {
					result.Runtime.RequestID = "22222222-2222-4222-8222-222222222222"
				}
				if outcome == "pending" {
					result.Phase = "preparing"
				}
				if recoveringTeam {
					result.Disposition = "recovered-team-conflict"
					result.TeamMessageID = originalID
				}
				switch outcome {
				case "ordinary-team-echo", "retired-team-echo":
					result.TeamMessageID = originalID
				case "team-wrong-id":
					result.TeamMessageID = requestID
				case "team-normalized-id":
					result.TeamMessageID = strings.ReplaceAll(originalID, "-", "")
				case "team-missing-id":
					result.TeamMessageID = ""
				case "team-wrong-disposition":
					result.Disposition = "retired-unknown"
				case "team-input-id":
					result.InputID = originalID
				case "team-input-digest":
					result.InputDigest = strings.Repeat("d", 64)
				case "team-wrong-scope":
					result.Runtime.Session.Slug = "2026-11-17-other"
				case "team-wrong-workspace":
					result.Runtime.Session.Workspace = "/other"
				case "team-wrong-generation":
					result.Previous.Generation = strings.Repeat("e", 64)
				case "team-wrong-request":
					result.Runtime.RequestID = originalID
				case "team-wrong-phase":
					result.Phase = "settled"
				}
				if strings.HasPrefix(outcome, "team-pending") || strings.HasPrefix(outcome, "team-starting") {
					result.Phase = "preparing"
					if strings.HasPrefix(outcome, "team-starting") {
						result.Phase = "starting"
					}
					result.Disposition = ""
					result.TeamMessageID = ""
					if strings.HasSuffix(outcome, "echo") {
						result.TeamMessageID = originalID
					}
					if outcome == "team-pending-disposition" {
						result.Disposition = "recovered-team-conflict"
					}
					if outcome == "team-pending-input" {
						result.InputID = originalID
					}
					if outcome == "team-pending-digest" {
						result.InputDigest = strings.Repeat("d", 64)
					}
				}
				if outcome == "team-ready" {
					w.WriteHeader(200)
				} else {
					w.WriteHeader(202)
				}
				if outcome == "team-unknown-field" {
					data, _ := json.Marshal(result)
					w.Write(append(data[:len(data)-1], []byte(`,"unexpected":true}`)...))
					return
				}
				json.NewEncoder(w).Encode(result)
				if outcome == "team-trailing" {
					w.Write([]byte(`{}`))
				}
			})}
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			t.Cleanup(func() { server.Close(); <-done })
			args := []string{"--workspace", root, "--slug", slug, "--request", requestID, "--expected-generation", strings.Repeat("a", 64), "--socket", socket, "--origin", "https://portal.fixture"}
			if retiring {
				args = append(args, "--retire-unknown-input", originalID, "--input-digest", strings.Repeat("d", 64), "--member", "lead")
			}
			if recoveringTeam {
				args = append(args, "--recover-team-conflict", originalID, "--member", member)
			}
			var output bytes.Buffer
			err = runPiRuntimeUpgrade(context.Background(), args, &output)
			success := outcome == "ready" || outcome == "retired" || outcome == "team-ready" || outcome == "team-compact"
			if success != (err == nil) {
				t.Fatal(outcome, err)
			}
			if calls.Load() != 1 {
				t.Fatal("CLI resent or followed redirect", calls.Load())
			}
			if success || outcome == "pending" || outcome == "team-pending" || outcome == "team-starting" {
				var result piruntime.UpgradeResult
				if json.Unmarshal(output.Bytes(), &result) != nil || result.Runtime.RequestID != requestID {
					t.Fatal(output.String())
				}
				if recoveringTeam && success && (result.TeamMessageID != originalID || !bytes.Contains(output.Bytes(), []byte(`"TeamMessageID"`))) {
					t.Fatal("original recovery ID lost", output.String())
				}
				if outcome == "pending" || outcome == "team-pending" || outcome == "team-starting" {
					if err == nil || !strings.Contains(err.Error(), requestID) || !strings.Contains(err.Error(), "reconcile this same request") {
						t.Fatal("pending operation was not preserved", err)
					}
				}
			} else if output.Len() != 0 {
				t.Fatal("invalid reply exposed as a result", output.String())
			}
		})
	}
}

func TestPiRuntimeUpgradeCLIRefusesAmbiguousArgumentsBeforeRequest(t *testing.T) {
	for _, args := range [][]string{nil, {"--workspace", "/workspace"},
		{"--workspace", "/workspace", "--slug", "2026-11-17-ready", "--request", "bad", "--expected-generation", strings.Repeat("a", 64), "--socket", "/socket", "--origin", "https://portal.fixture"},
		{"--workspace", "/workspace", "--workspace", "/workspace", "--request", "11111111-1111-4111-8111-111111111111", "--expected-generation", strings.Repeat("a", 64), "--socket", "/socket", "--origin", "https://portal.fixture"},
	} {
		if err := runPiRuntimeUpgrade(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid command accepted", args)
		}
	}
}

func TestPiRuntimeUpgradeCLIRejectsRecoverySelectorsWithoutHTTP(t *testing.T) {
	root, err := os.MkdirTemp("", "pi-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	socket := filepath.Join(root, "portal.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0660); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) })}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() { server.Close(); <-done })
	base := []string{"--workspace", root, "--slug", "2026-11-17-ready", "--request", "11111111-1111-4111-8111-111111111111", "--expected-generation", strings.Repeat("a", 64), "--socket", socket, "--origin", "https://portal.fixture"}
	original := "33333333-3333-4333-8333-333333333333"
	recovery := []string{"--recover-team-conflict", original, "--member", "lead"}
	cases := map[string][]string{
		"missing-member":            {"--recover-team-conflict", original},
		"member-alone":              {"--member", "lead"},
		"digest-alone":              {"--input-digest", strings.Repeat("d", 64)},
		"invalid-original":          {"--recover-team-conflict", "unknown", "--member", "lead"},
		"uppercase-original":        {"--recover-team-conflict", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "--member", "lead"},
		"empty-original":            {"--recover-team-conflict", "", "--member", "lead"},
		"invalid-member":            {"--recover-team-conflict", original, "--member", "../lead"},
		"unknown-flag":              {"--recover-team-conflict", original, "--sender", "lead"},
		"duplicate-recovery":        {"--recover-team-conflict", original, "--recover-team-conflict", original},
		"duplicate-member":          {"--recover-team-conflict", original, "--member", "lead", "--member", "lead"},
		"mixed-retirement":          {"--recover-team-conflict", original, "--member", "lead", "--retire-unknown-input", original, "--input-digest", strings.Repeat("d", 64)},
		"mixed-input-digest":        {"--recover-team-conflict", original, "--member", "lead", "--input-digest", strings.Repeat("d", 64)},
		"retirement-without-digest": {"--retire-unknown-input", original, "--member", "lead"},
		"retirement-without-member": {"--retire-unknown-input", original, "--input-digest", strings.Repeat("d", 64)},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			args := append(append([]string{}, base...), extra...)
			if err := runPiRuntimeUpgrade(context.Background(), args, &bytes.Buffer{}); !errors.Is(err, piruntime.ErrScope) {
				t.Fatal("invalid selectors not refused", err)
			}
		})
	}
	for name, change := range map[string]struct {
		index int
		value string
	}{
		"compact-request-UUID":   {5, strings.Repeat("1", 32)},
		"uppercase-request-UUID": {5, "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"},
		"bad-generation":         {7, "old"},
		"relative-workspace":     {1, "relative"},
		"unclean-workspace":      {1, root + "/../workspace"},
		"relative-socket":        {9, "portal.sock"},
		"origin-credentials":     {11, "https://user:secret@portal.fixture"},
		"origin-path":            {11, "https://portal.fixture/path"},
		"origin-query":           {11, "https://portal.fixture?query"},
	} {
		t.Run(name, func(t *testing.T) {
			args := append(append([]string{}, base...), recovery...)
			args[change.index] = change.value
			if err := runPiRuntimeUpgrade(context.Background(), args, &bytes.Buffer{}); !errors.Is(err, piruntime.ErrScope) {
				t.Fatal("invalid binding not refused", err)
			}
		})
	}
	if err := os.Chmod(socket, 0666); err != nil {
		t.Fatal(err)
	}
	if err := runPiRuntimeUpgrade(context.Background(), append(append([]string{}, base...), recovery...), &bytes.Buffer{}); !errors.Is(err, piruntime.ErrScope) {
		t.Fatal("public socket accepted", err)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid arguments reached HTTP", calls.Load())
	}
}
