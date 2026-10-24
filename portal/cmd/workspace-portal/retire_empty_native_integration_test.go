//go:build live_switch_integration

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

// The native producer owns every rollout and index entry. The forwarding peer
// only counts unchanged RPCs; the HTTP trap cannot provide a model response.
func TestNativeRetirePersistedEmptyPreviewRoot(t *testing.T) {
	native := os.Getenv("LIVE_SWITCH_CODEX")
	if !filepath.IsAbs(native) {
		t.Fatal("LIVE_SWITCH_CODEX must select the exact native executable")
	}
	root := t.TempDir()
	home, workspace := filepath.Join(root, "home"), filepath.Join(root, "workspace")
	for _, path := range []string{home, workspace} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var providerRequests atomic.Int64
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerRequests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer trap.Close()
	defer func() {
		if n := providerRequests.Load(); n != 0 {
			t.Errorf("no-turn fixture made %d provider requests", n)
		}
	}()
	config := "model_provider = \"fixture\"\nmodel = \"gpt-5.5\"\napproval_policy = \"never\"\n[model_providers.fixture]\nname = \"No-turn retirement fixture\"\nbase_url = " + strconv.Quote(trap.URL+"/v1") + "\nwire_api = \"responses\"\nrequires_openai_auth = false\nsupports_websockets = false\nrequest_max_retries = 0\nstream_max_retries = 0\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	nativeSocket, socket := filepath.Join(root, "native.sock"), filepath.Join(root, "count.sock")
	command := exec.CommandContext(ctx, native, "app-server", "--strict-config", "--listen", "unix://"+nativeSocket)
	command.Dir = root
	command.Env = []string{"HOME=" + root, "CODEX_HOME=" + home, "PATH=" + os.Getenv("PATH")}
	log, err := os.Create(filepath.Join(root, "native.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = command.Wait() }()
	for {
		if _, err := os.Stat(nativeSocket); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("native socket", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	var mu sync.Mutex
	methods := map[string]int{}
	retireEmptyNativeForwarder(t, ctx, nativeSocket, socket, func(method string) { mu.Lock(); methods[method]++; mu.Unlock() })
	client := workspacecodex.NewWithOptions(socket, workspace, codex.ClientOptions{SubmissionLedgerPath: filepath.Join(root, "attempts.json")})
	client.CodexHome = home
	defer client.Close()
	id, err := client.StartThreadWithSettings(ctx, workspace, map[string]string{}, codex.ThreadSettings{Policy: codex.ThreadPolicy{Sandbox: "workspace-write"}})
	if err != nil {
		t.Fatal("native start without user turn", err)
	}
	if err := client.ArchiveThread(ctx, id); err != nil {
		t.Fatal("native producer archive", err)
	}
	if _, err := client.UnarchiveThread(ctx, id); err != nil {
		t.Fatal("native producer unarchive", err)
	}
	metadata, err := client.ReadThreadMetadata(ctx, id, false)
	if err != nil {
		t.Fatal("native producer metadata", err)
	}
	if metadata.ID != id || metadata.Cwd != workspace || metadata.Source != "vscode" || metadata.Ephemeral == nil || *metadata.Ephemeral || metadata.Preview != "" || !reflect.DeepEqual(metadata.Status, map[string]any{"type": "notLoaded"}) {
		t.Fatal("native producer did not create the required persisted empty-preview unloaded root")
	}
	state, err := workspacecodex.ProveArchivedThread(ctx, client, workspacecodex.ArchivedThreadIdentity{ThreadID: id, Cwd: workspace, CodexHome: home, SourceKind: "vscode", RequireActiveFile: true})
	if err != nil || state != workspacecodex.ArchiveActive {
		t.Fatal("native producer materialized active proof", state, err)
	}
	var indexed struct {
		Data       *[]codex.ThreadMetadata `json:"data"`
		NextCursor *string                 `json:"nextCursor"`
	}
	if err := client.Request(ctx, "thread/list", map[string]any{"cwd": workspace, "sourceKinds": []string{"vscode"}, "archived": false, "useStateDbOnly": true, "limit": 2, "sortDirection": "asc", "modelProviders": []string{}}, &indexed); err != nil {
		t.Fatal("native producer indexed discovery", err)
	}
	if indexed.Data == nil || len(*indexed.Data) != 0 || indexed.NextCursor != nil {
		t.Fatal("native producer indexed discovery is not empty")
	}
	loaded, err := client.LoadedThreadIDs(ctx)
	if err != nil || slices.Contains(loaded, id) {
		t.Fatal("native producer root is loaded", err)
	}
	if len(loaded) != 0 {
		t.Fatal("isolated native producer has unexpected loaded identities")
	}
	if err := client.RecordThreadOperationAttempt(workspace, id); err != nil {
		t.Fatal(err)
	}
	if err := client.RetireThread(ctx, id, workspace, false); err != nil {
		t.Fatal("ordinary persisted-root retirement", err)
	}
	if state, err := client.ProveArchivedRootThread(ctx, id, workspace); err != nil || state != workspacecodex.ArchiveArchived {
		t.Fatal("native retired archive proof", state, err)
	}
	if err := client.RecordThreadOperationAttempt(workspace, id); err != nil {
		t.Fatal(err)
	}
	if err := client.RetireThread(ctx, id, workspace, false); err != nil {
		t.Fatal("ordinary archived replay", err)
	}
	if attempt, err := client.ThreadOperationAttempt(workspace); err != nil || attempt != "" {
		t.Fatal("retired attempt cleanup", attempt, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if methods["thread/archive"] != 2 || methods["thread/start"] != 1 || methods["thread/unarchive"] != 1 {
		t.Fatal("native producer/retirement/replay RPC counts", methods)
	}
	for _, method := range []string{"turn/start", "thread/resume", "thread/fork", "thread/inject_items"} {
		if methods[method] != 0 {
			t.Fatal("unexpected native mutation", method, methods[method])
		}
	}
	t.Logf("native=%s root=%s: exact persisted producer, retirement and archive replay passed", native, id)
}

func retireEmptyNativeForwarder(t *testing.T, ctx context.Context, native, socket string, observe func(string)) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		down, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer down.CloseNow()
		down.SetReadLimit(16 << 20)
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", native)
		}}
		defer transport.CloseIdleConnections()
		up, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
		if err != nil {
			t.Errorf("native retirement forwarding dial: %v", err)
			return
		}
		defer up.CloseNow()
		up.SetReadLimit(16 << 20)
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer down.CloseNow()
			for {
				kind, data, err := up.Read(ctx)
				if err != nil {
					return
				}
				if down.Write(ctx, kind, data) != nil {
					return
				}
			}
		}()
		defer func() { _ = up.CloseNow(); <-done }()
		for {
			kind, data, err := down.Read(ctx)
			if err != nil {
				return
			}
			var request struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(data, &request) == nil && request.Method != "" {
				observe(request.Method)
			}
			if up.Write(ctx, kind, data) != nil {
				return
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
}
