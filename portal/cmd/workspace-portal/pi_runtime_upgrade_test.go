package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	for _, outcome := range []string{"ready", "refused", "redirect", "wrong-identity", "malformed", "pending", "retired", "retirement-mismatch"} {
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
			recovering := outcome == "retired" || outcome == "retirement-mismatch"
			originalID := "33333333-3333-4333-8333-333333333333"
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
				if recovering {
					expectedFields = 6
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != expectedFields || body["workspace"] != root || body["requestId"] != requestID || body["expectedGeneration"] != strings.Repeat("a", 64) {
					t.Error("wrong request body", body, err)
					w.WriteHeader(400)
					return
				}
				if recovering && (body["retireUnknownInput"] != originalID || body["inputDigest"] != strings.Repeat("d", 64) || body["member"] != "lead") {
					t.Error("retirement tuple changed")
					w.WriteHeader(400)
					return
				}
				switch outcome {
				case "refused":
					w.WriteHeader(409)
					return
				case "redirect":
					w.Header().Set("Location", "/must-not-follow")
					w.WriteHeader(307)
					return
				case "malformed":
					w.Write([]byte("not JSON"))
					return
				}
				ref := agentbackend.SessionRef{Workspace: root, Slug: slug, Incarnation: strings.Repeat("c", 64)}
				result := piruntime.UpgradeResult{Runtime: piruntime.RuntimeRef{Session: ref, RequestID: requestID, Generation: strings.Repeat("b", 64)}, Previous: piruntime.RuntimeRef{Session: ref, Generation: strings.Repeat("a", 64)}, AppStorePath: "/nix/store/00000000000000000000000000000000-fixture", Phase: "ready"}
				if recovering {
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
				w.WriteHeader(202)
				json.NewEncoder(w).Encode(result)
			})}
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			t.Cleanup(func() { server.Close(); <-done })
			args := []string{"--workspace", root, "--slug", slug, "--request", requestID, "--expected-generation", strings.Repeat("a", 64), "--socket", socket, "--origin", "https://portal.fixture"}
			if recovering {
				args = append(args, "--retire-unknown-input", originalID, "--input-digest", strings.Repeat("d", 64), "--member", "lead")
			}
			var output bytes.Buffer
			err = runPiRuntimeUpgrade(context.Background(), args, &output)
			if (outcome == "ready" || outcome == "retired") != (err == nil) {
				t.Fatal(outcome, err)
			}
			if calls.Load() != 1 {
				t.Fatal("CLI resent or followed redirect", calls.Load())
			}
			if outcome == "ready" || outcome == "pending" {
				var result piruntime.UpgradeResult
				if json.Unmarshal(output.Bytes(), &result) != nil || result.Runtime.RequestID != requestID {
					t.Fatal(output.String())
				}
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
