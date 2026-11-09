//go:build naming_provider_integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
)

const namingProviderNativeSHA = "5830f7b53034ecce5b92618a453db3b87d704c8ea8a4b956ce371b0dbdd5933a"

// A fresh test process prevents the real owner from inheriting serving credentials.
// The package is an actual emitted metadata/native donor, not a built new portal.
func TestNativeNamingProviderLaunch(t *testing.T) {
	if os.Getenv("NAMING_PROVIDER_CHILD") == "" {
		root, err := os.MkdirTemp("/tmp", "npl-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(root)
		cmd := exec.Command(os.Args[0], "-test.run=^TestNativeNamingProviderLaunch$", "-test.count=1", "-test.timeout=120s", "-test.v")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "XDG_CONFIG_HOME=" + root, "XDG_STATE_HOME=" + root, "XDG_CACHE_HOME=" + root, "TMPDIR=/tmp", "NAMING_PROVIDER_CHILD=1", "NAMING_PROVIDER_ROOT=" + root, "NAMING_PROVIDER_PACKAGE=" + os.Getenv("NAMING_PROVIDER_PACKAGE"), "NAMING_PRIMARY_TOKEN=fixture-primary", "NAMING_ALTERNATE_TOKEN=fixture-alternate"}
		out, err := cmd.CombinedOutput()
		t.Logf("private naming owner result:\n%s", out)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	root, donor := os.Getenv("NAMING_PROVIDER_ROOT"), os.Getenv("NAMING_PROVIDER_PACKAGE")
	installed, err := agentteams.LoadInstalled(donor)
	if err != nil || installed.Metadata.Codex == nil || installed.Metadata.Codex.Native == nil || installed.Metadata.Codex.Provenance == nil {
		t.Fatalf("actual candidate metadata unavailable: %v", err)
	}
	metadata := installed.Metadata.Codex
	if strings.Join(metadata.AppServerArgv, " ") != "--allow-host-provider-switching" || metadata.Provenance.Revision != "902b743459a7c9ea00cb0b679129a28c8a888563" {
		t.Fatal("candidate metadata does not declare the reviewed native launch")
	}
	native := filepath.Join(*metadata.Native, "libexec/codex/bin/codex")
	binary, err := os.ReadFile(native)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(binary)) != namingProviderNativeSHA {
		t.Fatal("candidate native identity differs")
	}
	catalog := filepath.Join(donor, "share/workspace-portal/codex-models.json")
	var mu sync.Mutex
	hits := map[string]int{}
	wrong := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		provider := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
		mu.Lock()
		if (provider != "primary" && provider != "alternate") || r.Header.Get("Authorization") != "Bearer fixture-"+provider {
			wrong++
			mu.Unlock()
			w.WriteHeader(401)
			return
		}
		hits[provider]++
		mu.Unlock()
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 256*1024))
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []any{
			map[string]any{"type": "response.created", "response": map[string]any{"id": "naming-owner"}},
			map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "name", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": `{"name":"provider-launch-check"}`}}}},
			map[string]any{"type": "response.completed", "response": map[string]any{"id": "naming-owner", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
		} {
			b, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
	}))
	defer backend.Close()
	projection := []string{"-c", `model_provider="primary"`}
	for _, provider := range []string{"primary", "alternate"} {
		projection = append(projection, "-c", "model_providers."+provider+"="+fmt.Sprintf(`{ name=%q, base_url=%q, wire_api="responses", env_key=%q, supports_websockets=false }`, provider, backend.URL+"/"+provider+"/v1", "NAMING_"+strings.ToUpper(provider)+"_TOKEN"))
	}
	fixture := filepath.Join(root, "package")
	for _, dir := range []string{"share/dev-workspace", "share/workspace-portal", "libexec/codex/libexec", "bin"} {
		if err := os.MkdirAll(filepath.Join(fixture, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// The actual donor is unmanaged; copy its exact emitted metadata through the
	// typed reader, without manufacturing a launch-capability declaration.
	if installed.Managed {
		t.Fatal("this narrow donor fixture requires an unmanaged package")
	}
	for _, p := range []string{"share/dev-workspace/package.json", "share/workspace-portal/codex-models.json"} {
		b, err := os.ReadFile(filepath.Join(donor, p))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture, p), b, 0444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(*metadata.Native, "libexec/codex"), filepath.Join(fixture, "libexec/codex/libexec/codex")); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"argv": projection})
	if err := os.WriteFile(filepath.Join(fixture, "bin/workspace-host"), []byte("#!/bin/sh\n[ \"$*\" = 'provider native-args fixture' ] || exit 2\nprintf '%s' '"+string(data)+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := func(label, workspace string) *namingRuntime {
		t.Helper()
		home := filepath.Join(root, label)
		if err := os.Mkdir(home, 0700); err != nil {
			t.Fatal(err)
		}
		config := "model_provider = \"primary\"\nmodel = \"gpt-6-luna\"\n"
		for _, provider := range []string{"primary", "alternate"} {
			config += "[model_providers." + provider + "]\nname=" + strconv.Quote(provider) + "\nbase_url=" + strconv.Quote(backend.URL+"/"+provider+"/v1") + "\nwire_api=\"responses\"\nenv_key=" + strconv.Quote("NAMING_"+strings.ToUpper(provider)+"_TOKEN") + "\nsupports_websockets=false\n"
		}
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
		owner := startNamingRuntime(fixture, filepath.Join(root, "portal.sock"), workspace, "0.167.0", log.New(io.Discard, "", 0))
		if owner == nil {
			t.Fatal("naming owner unavailable")
		}
		t.Cleanup(owner.Close)
		client := codex.New(owner.socket)
		defer client.Close()
		for {
			if client.Ensure(ctx) == nil {
				break
			}
			select {
			case <-owner.done:
				t.Fatal("naming child exited before ready")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
		}
		return owner
	}
	strict := start("strict-home", "")
	strictClient := codex.New(strict.socket)
	defer strictClient.Close()
	cwd := filepath.Join(root, "raw-cwd")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := strictClient.Request(ctx, "thread/start", map[string]any{"model": "gpt-6-luna", "modelProvider": "alternate", "cwd": cwd, "ephemeral": true}, &started); err != nil {
		t.Fatal("strict alternate thread/start:", err)
	}
	var turn any
	err = strictClient.Request(ctx, "turn/start", map[string]any{"threadId": started.Thread.ID, "input": []any{map[string]any{"type": "text", "text": "strict mismatch control"}}}, &turn)
	if err == nil || !strings.Contains(err.Error(), "This chat uses alternate, but your current provider is primary.") {
		t.Fatalf("strict control rejected for another reason: %v", err)
	}
	t.Logf("strict alternate input RPC refusal: %v", err)
	call := func(owner *namingRuntime, provider string) error {
		dir, err := os.MkdirTemp(root, "utility-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		if err := os.Mkdir(filepath.Join(dir, "cwd"), 0700); err != nil {
			return err
		}
		instruction := filepath.Join(dir, "instructions.txt")
		if err := os.WriteFile(instruction, []byte(codex.EphemeralInstructionFileContent), 0600); err != nil {
			return err
		}
		client := codex.New(owner.socket)
		defer client.Close()
		result, err := client.RunPromptedEphemeralTurn(ctx, codex.EphemeralTurnOptions{Directory: filepath.Join(dir, "cwd"), InstructionFile: instruction, ModelCatalogFile: owner.catalog, ModelProvider: provider, Model: "gpt-6-luna", Effort: "high", Instructions: "Return only the requested name JSON. Do not use tools or ask questions.", Input: "provider launch check", OutputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`)})
		if err == nil && result.Text != `{"name":"provider-launch-check"}` {
			return fmt.Errorf("unexpected name output")
		}
		return err
	}
	if err := call(strict, "alternate"); err == nil {
		t.Fatal("strict prompted client accepted alternate input")
	}
	mu.Lock()
	before := hits["primary"] + hits["alternate"]
	mu.Unlock()
	if before != 0 {
		t.Fatal("strict mismatch reached inference")
	}
	if err := call(strict, "primary"); err != nil {
		t.Fatal("primary control:", err)
	}
	flagged := start("flagged-home", "fixture")
	if err := call(flagged, "alternate"); err != nil {
		t.Fatal("registered alternate naming:", err)
	}
	flagged.Close()
	var config any
	if err := strictClient.Request(ctx, "config/read", map[string]any{"includeLayers": false}, &config); err != nil {
		t.Fatal("unrelated strict daemon lost continuity:", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["primary"] != 1 || hits["alternate"] != 1 || wrong != 0 {
		t.Fatalf("routes=%v wrongAuth=%d", hits, wrong)
	}
	t.Logf("strict mismatch/noPOST and primary control; registered alternate endpoint/auth success; strict daemon continuity; native=%s catalog=%s", namingProviderNativeSHA, catalog)
}
