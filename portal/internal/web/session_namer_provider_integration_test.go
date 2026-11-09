//go:build naming_provider_integration

package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
)

type nativeNamingRetryClient struct {
	client      *codex.Client
	t           *testing.T
	deadline    time.Time
	calls       int
	directories []string
}

func (c *nativeNamingRetryClient) RunPromptedEphemeralTurn(ctx context.Context, options codex.EphemeralTurnOptions) (codex.EphemeralTurnResult, error) {
	c.t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok || deadline != c.deadline || options.ModelProvider != "alternate" || options.Effort != "high" || options.Instructions != sessionNamingInstructions || string(options.OutputSchema) != sessionNamingSchema {
		c.t.Fatal("actual adapter changed provider/instructions/budget")
	}
	c.calls++
	for _, previous := range c.directories {
		if options.Directory == previous {
			c.t.Fatal("fresh naming call reused scratch")
		}
	}
	c.directories = append(c.directories, options.Directory)
	return c.client.RunPromptedEphemeralTurn(ctx, options)
}

// Exercise the real web adapter and actual client, counting calls and native
// thread identities separately from HTTP requests inside one native call.
func TestNativeSessionNamingProviderRetry(t *testing.T) {
	if os.Getenv("NAMING_RETRY_CHILD") == "" {
		root, err := os.MkdirTemp("/tmp", "npr-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(root)
		command := exec.Command(os.Args[0], "-test.run=^TestNativeSessionNamingProviderRetry$", "-test.count=1", "-test.timeout=120s", "-test.v")
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "XDG_CONFIG_HOME=" + root, "XDG_STATE_HOME=" + root, "XDG_CACHE_HOME=" + root, "TMPDIR=/tmp", "NAMING_RETRY_CHILD=1", "NAMING_RETRY_ROOT=" + root, "NAMING_PROVIDER_PACKAGE=" + os.Getenv("NAMING_PROVIDER_PACKAGE"), "NAMING_PRIMARY_TOKEN=fixture-primary", "NAMING_ALTERNATE_TOKEN=fixture-alternate"}
		out, err := command.CombinedOutput()
		t.Logf("private web naming result:\n%s", out)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	root, donor := os.Getenv("NAMING_RETRY_ROOT"), os.Getenv("NAMING_PROVIDER_PACKAGE")
	installed, err := agentteams.LoadInstalled(donor)
	if err != nil || installed.Metadata.Codex == nil || installed.Metadata.Codex.Native == nil || installed.Metadata.Codex.Provenance == nil {
		t.Fatalf("actual candidate metadata unavailable: %v", err)
	}
	metadata := installed.Metadata.Codex
	if strings.Join(metadata.AppServerArgv, " ") != "--allow-host-provider-switching" || metadata.Provenance.Revision != "902b743459a7c9ea00cb0b679129a28c8a888563" {
		t.Fatal("candidate launch/source differs")
	}
	native := filepath.Join(*metadata.Native, "libexec/codex/bin/codex")
	binary, err := os.ReadFile(native)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(binary)) != "5830f7b53034ecce5b92618a453db3b87d704c8ea8a4b956ce371b0dbdd5933a" {
		t.Fatal("native identity differs")
	}
	catalog := filepath.Join(donor, "share/workspace-portal/codex-models.json")
	var mu sync.Mutex
	posts, wrong := 0, 0
	threads := map[string]bool{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 256*1024+1))
		if err != nil || len(data) > 256*1024 {
			w.WriteHeader(400)
			return
		}
		var request struct {
			ClientMetadata map[string]any `json:"client_metadata"`
			Input          []struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		_ = json.Unmarshal(data, &request)
		thread, _ := request.ClientMetadata["thread_id"].(string)
		mu.Lock()
		if r.URL.Path != "/alternate/v1/responses" || r.Header.Get("Authorization") != "Bearer fixture-alternate" || thread == "" {
			wrong++
			mu.Unlock()
			w.WriteHeader(401)
			return
		}
		posts++
		index := posts
		threads[thread] = true
		mu.Unlock()
		instructions, input := false, false
		for _, item := range request.Input {
			for _, part := range item.Content {
				instructions = instructions || item.Role == "developer" && part.Text == sessionNamingInstructions
				input = input || item.Role == "user" && strings.Contains(part.Text, "provider retry check")
			}
		}
		if !instructions || !input {
			http.Error(w, "synthetic instructions/input absent", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if index == 1 {
			// No retry-after advice: native settles this failed turn; only the
			// application may make the one proven-safe fresh call.
			b, _ := json.Marshal(map[string]any{"type": "response.failed", "response": map[string]any{"id": "naming-overload", "status": "failed", "error": map[string]any{"code": "server_is_overloaded", "message": "synthetic naming overload"}}})
			_, _ = fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", b)
			return
		}
		for _, event := range []any{
			map[string]any{"type": "response.created", "response": map[string]any{"id": "naming-retry"}},
			map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "name", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": `{"name":"provider-retry-check"}`}}}},
			map[string]any{"type": "response.completed", "response": map[string]any{"id": "naming-retry", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
		} {
			b, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
	}))
	defer backend.Close()
	home, cwd := filepath.Join(root, "home"), filepath.Join(root, "cwd")
	for _, p := range []string{home, cwd} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := "model_provider=\"primary\"\nmodel=\"gpt-6-luna\"\n"
	for _, provider := range []string{"primary", "alternate"} {
		config += "[model_providers." + provider + "]\nname=" + strconv.Quote(provider) + "\nbase_url=" + strconv.Quote(backend.URL+"/"+provider+"/v1") + "\nwire_api=\"responses\"\nenv_key=" + strconv.Quote("NAMING_"+strings.ToUpper(provider)+"_TOKEN") + "\nsupports_websockets=false\n"
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "app.sock")
	args := []string{"-c", "model_catalog_json=" + strconv.Quote(catalog), "app-server"}
	args = append(args, metadata.AppServerArgv...)
	args = append(args, "--strict-config", "--listen", "unix://"+socket)
	command := exec.Command(native, args...)
	command.Dir = cwd
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + home, "CODEX_SQLITE_HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_STATE_HOME=" + home, "XDG_CACHE_HOME=" + home, "CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED=1", "NAMING_PRIMARY_TOKEN=fixture-primary", "NAMING_ALTERNATE_TOKEN=fixture-alternate"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	nativeLog, err := os.Create(filepath.Join(root, "native.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer nativeLog.Close()
	command.Stdout = nativeLog
	command.Stderr = nativeLog
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		err := command.Wait()
		t.Logf("owned naming native stopped/joined pid=%d wait=%v", command.Process.Pid, err)
	}()
	client := codex.New(socket)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		if client.Ensure(ctx) == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	store, err := newLifecycleOperationStore(filepath.Join(root, "state"), filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{operationStore: store}
	server.config.NamingModelCatalogFile = catalog
	deadline, _ := ctx.Deadline()
	recording := &nativeNamingRetryClient{client: client, t: t, deadline: deadline}
	output, err := server.modelSessionNameWithRetry(ctx, "provider retry check", "alternate", recording, codex.CanRetryEphemeralTurn)
	if err != nil || output != `{"name":"provider-retry-check"}` {
		t.Fatalf("actual naming retry output=%q err=%v", output, err)
	}
	if recording.calls != 2 || len(recording.directories) != 2 {
		t.Fatalf("ephemeral calls=%d scratch=%v", recording.calls, recording.directories)
	}
	for _, p := range recording.directories {
		if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
			t.Fatalf("private scratch retained: %s %v", p, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 2 || len(threads) != 2 || wrong != 0 {
		t.Fatalf("HTTP=%d freshThreads=%d wrongRouteAuth=%d", posts, len(threads), wrong)
	}
	t.Logf("actual prompted calls=%d freshScratch=2 freshThreads=%d HTTP=%d wrongRouteAuth=%d originalDeadline=true", recording.calls, len(threads), posts, wrong)
}
