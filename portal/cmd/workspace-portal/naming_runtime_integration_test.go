//go:build naming_runtime_integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
	"github.com/coder/websocket"
	"golang.org/x/net/html"
)

// Explicit invocation requires the reviewed VM and frozen assembled package.
// This fixture owns only disposable synthetic processes and state in that VM.
// It never discovers an installed profile or registers a workspace.
const managedSmokeCatalogHash = "fd219bd9f061278275f528939f82f54d2eb97df4b25c23b022adbe48813d920b"
const managedSmokeOrigin = "https://managed-smoke.example.test"
const managedSmokeCanaryPort = "39991"

type managedSmokeManifest struct {
	Schema                     int               `json:"schema"`
	RuntimeSource              string            `json:"runtimeSource"`
	PackageRoot                string            `json:"packageRoot"`
	PackageDerivation          string            `json:"packageDerivation"`
	FixtureBinary              string            `json:"fixtureBinary"`
	FixtureSHA256              string            `json:"fixtureSHA256"`
	NativeSHA256               string            `json:"nativeSHA256"`
	NativeSource               string            `json:"nativeSource"`
	Heads                      map[string]string `json:"heads"`
	ProtectedRuntimeAncestor   string            `json:"protectedRuntimeAncestor"`
	ProtectedExtensionAncestor string            `json:"protectedExtensionAncestor"`
	PackageFiles               map[string]string `json:"packageFiles"`
	SourceFiles                map[string]string `json:"sourceFiles"`
}

type managedSmokeProcess struct {
	pid, processGroup int
	start, group      string
}
type managedSmokeUnit struct {
	name, root, socket, group string
	main                      managedSmokeProcess
}
type managedSmokePreparation struct {
	RequestID     string `json:"requestId"`
	ReceiptID     string `json:"receiptId"`
	Attempt       int    `json:"attempt"`
	State         string `json:"state"`
	Base          string `json:"base"`
	NamingOutcome string `json:"namingOutcome"`
	Slug          string `json:"slug"`
}

// The mock retains counts/categories and fixed synthetic names, never requests.
type managedSmokeProvider struct {
	server                         *http.Server
	listener                       net.Listener
	url                            string
	ordinary                       bool
	mu                             sync.Mutex
	posts, active, peak, cancelled int
	failure                        string
	names                          map[string]bool
	hold                           chan struct{}
	entered                        chan struct{}
	stopped                        chan struct{}
	once                           sync.Once
}

type managedSmokeHarness struct {
	t                                     *testing.T
	manifest                              managedSmokeManifest
	root, native, catalog, ordinarySocket string
	ordinary                              *codex.Client
	ordinaryCancel                        context.CancelFunc
	ordinaryDone                          chan struct{}
	ordinaryProcess                       managedSmokeProcess
	proxy                                 *http.Server
	proxyListener                         net.Listener
	proxyWG                               sync.WaitGroup
	proxyStop                             context.CancelFunc
	ordinaryProvider, namingProvider      *managedSmokeProvider
	namerRoutes                           atomic.Int32
	proxyFailure                          atomic.Bool
	canary                                *http.Server
	canaryListener                        net.Listener
	canaryHits                            atomic.Int32
	baseline                              uint64
	units                                 []*managedSmokeUnit
}

func managedSmokeFail(t *testing.T, category string) {
	t.Helper()
	t.Fatal("managed naming smoke: " + category)
}

func managedSmokeRead(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("bounded read failed")
	}
	return data, nil
}

func managedSmokeHash(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		managedSmokeFail(t, "hash prerequisite unavailable")
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		managedSmokeFail(t, "hash read failed")
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func managedSmokeHex(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size && value == strings.ToLower(value)
}

func managedSmokeManifestFor(t *testing.T) managedSmokeManifest {
	t.Helper()
	if os.Getenv("MANAGED_NAMING_SMOKE_VM") != "disposable-nixos-user-manager-v1" || os.Geteuid() != 1000 {
		managedSmokeFail(t, "explicit disposable VM prerequisite missing")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		managedSmokeFail(t, "unified cgroup prerequisite missing")
	}
	var manifest managedSmokeManifest
	data, err := managedSmokeRead(os.Getenv("MANAGED_NAMING_SMOKE_MANIFEST"), 1024*1024)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err != nil || decoder.Decode(&manifest) != nil || decoder.Decode(new(any)) != io.EOF || manifest.Schema != 1 {
		managedSmokeFail(t, "frozen manifest prerequisite invalid")
	}
	for _, pair := range [][2]string{{"MANAGED_NAMING_SMOKE_PACKAGE", manifest.PackageRoot}, {"MANAGED_NAMING_SMOKE_SOURCE", manifest.RuntimeSource}, {"MANAGED_NAMING_SMOKE_BINARY", manifest.FixtureBinary}} {
		resolved, err := filepath.EvalSymlinks(pair[1])
		if err != nil || !strings.HasPrefix(pair[1], "/nix/store/") || resolved != pair[1] || os.Getenv(pair[0]) != pair[1] {
			managedSmokeFail(t, "explicit immutable candidate mismatch")
		}
	}
	executable, err := os.Executable()
	if err != nil || executable != manifest.FixtureBinary || !managedSmokeHex(manifest.FixtureSHA256, 32) || managedSmokeHash(t, executable) != manifest.FixtureSHA256 {
		managedSmokeFail(t, "reviewed fixture binary mismatch")
	}
	if !strings.HasPrefix(manifest.PackageDerivation, "/nix/store/") || !strings.HasSuffix(manifest.PackageDerivation, ".drv") || manifest.NativeSource != "/nix/store/kdq60x8qayhgkx6d3zbfdgw1i7dqp9q5-source" || !managedSmokeHex(manifest.NativeSHA256, 32) {
		managedSmokeFail(t, "candidate provenance missing")
	}
	for _, project := range []string{"runtime", "provider", "extension", "workspace"} {
		if !managedSmokeHex(manifest.Heads[project], 20) {
			managedSmokeFail(t, "exact source head missing")
		}
	}
	if manifest.ProtectedRuntimeAncestor != "4ef298b30f9cdbdcfe02bf6526e0f69ecc9bf7b4" || manifest.ProtectedExtensionAncestor != "399c33023a568a8d7a21e4e4df52829628720a28" {
		managedSmokeFail(t, "consumed ancestry provenance missing")
	}
	if len(manifest.PackageFiles) > 512 || len(manifest.SourceFiles) > 32 {
		managedSmokeFail(t, "manifest bounds exceeded")
	}
	for _, relative := range []string{"bin/workspace-portal", "share/systemd/user/workspace-portal@.service", "share/dev-workspace/package.json", "share/dev-workspace/agent-teams.json", "share/workspace-portal/codex/ephemeral_policy.json", "share/workspace-portal/runtime-contract.json", "share/workspace-portal/codex-models.json"} {
		if !managedSmokeHex(manifest.PackageFiles[relative], 32) {
			managedSmokeFail(t, "package resource guard missing")
		}
	}
	for _, relative := range []string{"portal/cmd/workspace-portal/main.go", "portal/cmd/workspace-portal/naming_runtime.go", "portal/cmd/workspace-portal/naming_runtime_integration_test.go", "portal/internal/web/server.go", "portal/internal/web/session_namer.go"} {
		if !managedSmokeHex(manifest.SourceFiles[relative], 32) {
			managedSmokeFail(t, "owning function source guard missing")
		}
	}
	for _, group := range []struct {
		root  string
		files map[string]string
	}{{manifest.PackageRoot, manifest.PackageFiles}, {manifest.RuntimeSource, manifest.SourceFiles}} {
		for relative, expected := range group.files {
			if relative == "." || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || strings.HasPrefix(relative, "../") || !managedSmokeHex(expected, 32) || managedSmokeHash(t, filepath.Join(group.root, relative)) != expected {
				managedSmokeFail(t, "frozen source or resource digest mismatch")
			}
		}
	}
	if err := filepath.WalkDir(filepath.Join(manifest.PackageRoot, "share/dev-workspace/agent-teams"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(manifest.PackageRoot, path)
		if err != nil || !managedSmokeHex(manifest.PackageFiles[relative], 32) {
			return errors.New("unguarded team resource")
		}
		return nil
	}); err != nil {
		managedSmokeFail(t, "complete installed team resources not guarded")
	}
	return manifest
}

func managedSmokeWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		managedSmokeFail(t, "private fixture write failed")
	}
}

func managedSmokeEnvironment(root, home string) []string {
	return []string{"PATH=/run/current-system/sw/bin", "HOME=" + filepath.Join(root, "home"), "USER=developer", "LOGNAME=developer", "LANG=C.UTF-8", "XDG_RUNTIME_DIR=" + filepath.Join(root, "xdg-runtime"), "XDG_CONFIG_HOME=" + filepath.Join(root, "xdg-config"), "XDG_STATE_HOME=" + filepath.Join(root, "xdg-state"), "XDG_CACHE_HOME=" + filepath.Join(root, "xdg-cache"), "CODEX_HOME=" + home, "DEV_WORKSPACE_CODEX_HOME=" + filepath.Join(root, "naming-home"), "CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED=1"}
}

type managedSmokeOutput struct {
	data     []byte
	overflow bool
}

func (output *managedSmokeOutput) Write(data []byte) (int, error) {
	count := len(data)
	remaining := 64*1024 - len(output.data)
	if len(data) > remaining {
		output.overflow = true
		data = data[:remaining]
	}
	output.data = append(output.data, data...)
	return count, nil
}

func managedSmokeCommand(t *testing.T, budget time.Duration, name string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.WaitDelay = 2 * time.Second
	var output managedSmokeOutput
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil || output.overflow {
		managedSmokeFail(t, "bounded fixture command failed")
	}
	return output.data
}

func (h *managedSmokeHarness) counter() uint64 {
	data := managedSmokeCommand(h.t, time.Second, "/run/wrappers/bin/sudo", "-n", "/run/current-system/sw/bin/managed-naming-smoke-counter")
	var result struct {
		NFTables []struct {
			Counter *struct {
				Name    string
				Packets uint64
			} `json:"counter"`
		} `json:"nftables"`
	}
	if json.Unmarshal(data, &result) != nil {
		managedSmokeFail(h.t, "counter read invalid")
	}
	for _, entry := range result.NFTables {
		if entry.Counter != nil && entry.Counter.Name == "denied" {
			return entry.Counter.Packets
		}
	}
	managedSmokeFail(h.t, "owned deny counter missing")
	return 0
}

func managedSmokeAwait(t *testing.T, budget time.Duration, category string, predicate func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if predicate() {
			return
		}
		select {
		case <-ctx.Done():
			managedSmokeFail(t, category)
		case <-ticker.C:
		}
	}
}

func managedSmokeUpgrade(r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/v1/responses" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.Header.Get("Sec-WebSocket-Version") != "13" {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(r.Header.Get("Sec-WebSocket-Key"))
	if err != nil || len(key) != 16 {
		return false
	}
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

func managedSmokeNewProvider(t *testing.T, ordinary bool) *managedSmokeProvider {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		managedSmokeFail(t, "synthetic provider listener failed")
	}
	p := &managedSmokeProvider{listener: listener, url: "http://" + listener.Addr().String() + "/v1", ordinary: ordinary, names: map[string]bool{}, entered: make(chan struct{}, 16), stopped: make(chan struct{}, 16)}
	p.server = &http.Server{Handler: http.HandlerFunc(p.respond), ReadHeaderTimeout: time.Second}
	go func() { _ = p.server.Serve(listener) }()
	return p
}

func (p *managedSmokeProvider) respond(w http.ResponseWriter, r *http.Request) {
	if managedSmokeUpgrade(r) {
		w.WriteHeader(http.StatusUpgradeRequired)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 256*1024+1))
	var request map[string]any
	if err != nil || len(data) > 256*1024 || json.Unmarshal(data, &request) != nil {
		p.mu.Lock()
		p.failure = "provider-body-shape"
		p.mu.Unlock()
		w.WriteHeader(400)
		return
	}
	name := "verify-ordinary-control"
	if !p.ordinary {
		name = ""
		for goal, candidate := range map[string]string{"Managed first request": "verify-managed-first", "Managed second request": "verify-managed-second", "Managed third request": "verify-managed-third"} {
			if bytes.Contains(data, []byte(goal)) {
				name = candidate
			}
		}
	}
	p.mu.Lock()
	p.posts++
	index := p.posts
	p.active++
	p.peak = max(p.peak, p.active)
	if p.posts > 32 {
		p.failure = "provider-count-bound"
	}
	if !p.ordinary {
		reasoning, _ := request["reasoning"].(map[string]any)
		tools, ok := request["tools"].([]any)
		text, _ := request["text"].(map[string]any)
		format, _ := text["format"].(map[string]any)
		schema, _ := format["schema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		nameProperty, _ := properties["name"].(map[string]any)
		required, _ := schema["required"].([]any)
		input, inputOK := request["input"].([]any)
		if request["model"] != "gpt-5.5" || reasoning["effort"] != "low" || !ok || len(tools) != 0 || request["store"] != false || request["stream"] != true || format["type"] != "json_schema" || schema["type"] != "object" || name == "" || !inputOK {
			p.failure = "native-naming-request-shape"
		}
		if len(properties) != 1 || nameProperty["type"] != "string" || nameProperty["maxLength"] != float64(48) || nameProperty["pattern"] != "^[a-z0-9]+(?:-[a-z0-9]+){2,5}$" || len(required) != 1 || required[0] != "name" || schema["additionalProperties"] != false {
			p.failure = "native-naming-output-schema"
		}
		for _, value := range input {
			if item, ok := value.(map[string]any); !ok || item["type"] == "additional_tools" || item["tools"] != nil {
				p.failure = "hidden-tool-role"
			}
		}
		if request["additional_tools"] != nil {
			p.failure = "hidden-tool-role"
		}
		if p.names[name] {
			p.failure = "duplicate-naming-inference"
		}
		p.names[name] = true
	}
	hold := p.hold
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.active--; p.mu.Unlock() }()
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(value any) { encoded, _ := json.Marshal(value); _, _ = fmt.Fprintf(w, "data: %s\n\n", encoded) }
	event(map[string]any{"type": "response.created", "response": map[string]any{"id": fmt.Sprintf("smoke-response-%d", index)}})
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
	select {
	case p.entered <- struct{}{}:
	default:
		p.mu.Lock()
		p.failure = "provider-event-bound"
		p.mu.Unlock()
	}
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			p.mu.Lock()
			p.cancelled++
			p.mu.Unlock()
			select {
			case p.stopped <- struct{}{}:
			default:
			}
			return
		}
	}
	answer := "ordinary-smoke-completed"
	if !p.ordinary {
		encoded, _ := json.Marshal(map[string]string{"name": name})
		answer = string(encoded)
	}
	event(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": fmt.Sprintf("smoke-answer-%d", index), "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": answer}}}})
	event(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("smoke-response-%d", index), "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
}

func (p *managedSmokeProvider) barrier() {
	p.mu.Lock()
	p.hold = make(chan struct{})
	p.once = sync.Once{}
	p.mu.Unlock()
}
func (p *managedSmokeProvider) release() {
	p.mu.Lock()
	hold := p.hold
	p.mu.Unlock()
	if hold != nil {
		p.once.Do(func() { close(hold) })
	}
}
func (p *managedSmokeProvider) check(t *testing.T) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure != "" {
		managedSmokeFail(t, p.failure)
	}
}

func newManagedSmokeHarness(t *testing.T) *managedSmokeHarness {
	t.Helper()
	h := &managedSmokeHarness{t: t, manifest: managedSmokeManifestFor(t)}
	root, err := os.MkdirTemp("/tmp", "nms-")
	if err != nil {
		managedSmokeFail(t, "private root unavailable")
	}
	h.root = root
	t.Cleanup(func() {
		if h.close() {
			_ = os.RemoveAll(root)
		}
	})
	for _, relative := range []string{"runtime", "home", "xdg-runtime", "xdg-config", "xdg-state", "xdg-cache", "ordinary-home", "naming-home", "ordinary-cwd"} {
		if os.Mkdir(filepath.Join(root, relative), 0700) != nil {
			managedSmokeFail(t, "private directory failed")
		}
	}
	h.native, err = filepath.EvalSymlinks(filepath.Join(h.manifest.PackageRoot, "libexec/codex/libexec/codex/bin/codex"))
	if err != nil || managedSmokeHash(t, h.native) != h.manifest.NativeSHA256 {
		managedSmokeFail(t, "frozen native digest mismatch")
	}
	h.catalog, err = filepath.EvalSymlinks(filepath.Join(h.manifest.PackageRoot, "share/workspace-portal/codex-models.json"))
	info, statErr := os.Stat(h.catalog)
	if err != nil || statErr != nil || !info.Mode().IsRegular() || info.Size() != 472512 || info.Mode().Perm()&0222 != 0 || managedSmokeHash(t, h.catalog) != managedSmokeCatalogHash {
		managedSmokeFail(t, "full original catalog mismatch")
	}
	if string(managedSmokeCommand(t, 2*time.Second, h.native, "--version")) != "codex-cli 0.160.0\n" {
		managedSmokeFail(t, "native version mismatch")
	}
	h.ordinaryProvider, h.namingProvider = managedSmokeNewProvider(t, true), managedSmokeNewProvider(t, false)
	for _, entry := range []struct{ home, base string }{{"ordinary-home", h.ordinaryProvider.url}, {"naming-home", h.namingProvider.url}} {
		managedSmokeWrite(t, filepath.Join(root, entry.home, "auth.json"), "{\"auth_mode\":\"apikey\",\"OPENAI_API_KEY\":\"synthetic-fixture-only\"}\n", 0600)
		managedSmokeWrite(t, filepath.Join(root, entry.home, "config.toml"), "model_provider = \"openai\"\nmodel = \"gpt-5.5\"\nmodel_reasoning_effort = \"low\"\nopenai_base_url = "+strconv.Quote(entry.base)+"\n", 0600)
	}
	h.canaryListener, err = net.Listen("tcp4", "127.0.0.1:"+managedSmokeCanaryPort)
	if err != nil {
		managedSmokeFail(t, "reserved canary listener unavailable")
	}
	h.canary = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.canaryHits.Add(1); w.WriteHeader(500) }), ReadHeaderTimeout: time.Second}
	go func() { _ = h.canary.Serve(h.canaryListener) }()
	before := h.counter()
	probeCtx, stopProbe := context.WithTimeout(context.Background(), 100*time.Millisecond)
	connection, probeErr := (&net.Dialer{}).DialContext(probeCtx, "tcp4", "127.0.0.1:"+managedSmokeCanaryPort)
	if connection != nil {
		_ = connection.Close()
	}
	stopProbe()
	after := h.counter()
	if probeErr == nil || after <= before || h.canaryHits.Load() != 0 {
		managedSmokeFail(t, "completed blocked-dial prerequisite failed")
	}
	// Positive connectivity proof has an owned, synchronously closed transport.
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	probeClient := &http.Client{Transport: transport, Timeout: 100 * time.Millisecond}
	response, err := probeClient.Get(h.namingProvider.url + "/responses")
	if err != nil {
		managedSmokeFail(t, "allowed provider loopback probe failed")
	}
	_ = response.Body.Close()
	transport.CloseIdleConnections()
	if response.StatusCode != 404 {
		managedSmokeFail(t, "allowed provider probe was not plain GET404")
	}
	ordinaryNativeSocket := filepath.Join(root, "runtime", "ordinary-native.sock")
	ctx, cancel := context.WithCancel(context.Background())
	h.ordinaryCancel = cancel
	h.ordinaryDone = make(chan struct{})
	command := exec.Command(h.native, "app-server", "--strict-config", "--listen", "unix://"+ordinaryNativeSocket)
	command.Dir = filepath.Join(root, "ordinary-cwd")
	command.Env = managedSmokeEnvironment(root, filepath.Join(root, "ordinary-home"))
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	go func() { _ = runNamingChild(ctx, command); close(h.ordinaryDone) }()
	managedSmokeAwait(t, 3*time.Second, "ordinary native listener prerequisite missing", func() bool {
		info, err := os.Stat(ordinaryNativeSocket)
		return err == nil && info.Mode()&os.ModeSocket != 0
	})
	h.ordinarySocket = filepath.Join(root, "runtime", "ordinary.sock")
	h.startProxy(ordinaryNativeSocket)
	h.ordinary = codex.NewWithOptions(h.ordinarySocket, codex.ClientOptions{ClientInfo: codex.ClientInfo{Name: "managed-smoke-ordinary-control", Title: "Synthetic ordinary control", Version: "1"}})
	h.control()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		managedSmokeFail(t, "ordinary PID evidence unavailable")
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		args, err := managedSmokeRead(filepath.Join("/proc", entry.Name(), "cmdline"), 8192)
		if err != nil || !bytes.HasSuffix(args, []byte("unix://"+ordinaryNativeSocket+"\x00")) {
			continue
		}
		process, ok := managedSmokeProcessAt(pid)
		status, err := managedSmokeRead(filepath.Join("/proc", entry.Name(), "status"), 8192)
		if !ok || err != nil || h.ordinaryProcess.pid != 0 || !strings.Contains(string(status), "Uid:\t1000\t1000\t1000\t1000") || managedSmokeHash(t, filepath.Join("/proc", entry.Name(), "exe")) != h.manifest.NativeSHA256 {
			managedSmokeFail(t, "ordinary native UID or identity mismatch")
		}
		h.ordinaryProcess = process
	}
	if h.ordinaryProcess.pid == 0 {
		managedSmokeFail(t, "actual ordinary native process missing")
	}
	h.baseline = h.counter()
	if h.baseline != after {
		managedSmokeFail(t, "ordinary or positive prerequisite attempted denied egress")
	}
	return h
}

func (h *managedSmokeHarness) startProxy(upstreamSocket string) {
	listener, err := net.Listen("unix", h.ordinarySocket)
	if err != nil {
		managedSmokeFail(h.t, "ordinary proxy listener unavailable")
	}
	h.proxyListener = listener
	ctx, stop := context.WithCancel(context.Background())
	h.proxyStop = stop
	h.proxy = &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.proxyWG.Add(1)
		defer h.proxyWG.Done()
		downstream, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer downstream.CloseNow()
		transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", upstreamSocket)
		}}
		defer transport.CloseIdleConnections()
		upstream, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
		if err != nil {
			h.proxyFailure.Store(true)
			return
		}
		defer upstream.CloseNow()
		downstream.SetReadLimit(1024 * 1024)
		upstream.SetReadLimit(1024 * 1024)
		finished := make(chan struct{}, 2)
		go func() {
			defer func() { finished <- struct{}{} }()
			controlConnection := false
			for {
				kind, raw, err := downstream.Read(ctx)
				if err != nil {
					return
				}
				var message struct {
					Method string
					Params struct {
						ClientInfo struct {
							Name string `json:"name"`
						} `json:"clientInfo"`
					} `json:"params"`
				}
				if json.Unmarshal(raw, &message) != nil {
					h.proxyFailure.Store(true)
					return
				}
				if message.Method == "initialize" {
					controlConnection = message.Params.ClientInfo.Name == "managed-smoke-ordinary-control"
					if message.Params.ClientInfo.Name == "dev-workspace-session-namer" {
						h.namerRoutes.Add(1)
					}
				}
				if (message.Method == "thread/start" || message.Method == "turn/start") && !controlConnection {
					h.namerRoutes.Add(1)
				}
				if err := upstream.Write(ctx, kind, raw); err != nil {
					return
				}
			}
		}()
		go func() {
			defer func() { finished <- struct{}{} }()
			for {
				kind, raw, err := upstream.Read(ctx)
				if err != nil {
					return
				}
				if upstreamMessageErr := downstream.Write(ctx, kind, raw); upstreamMessageErr != nil {
					return
				}
			}
		}()
		<-finished
		_ = upstream.CloseNow()
		_ = downstream.CloseNow()
		<-finished
	})}
	go func() { _ = h.proxy.Serve(listener) }()
}

func (h *managedSmokeHarness) control() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Retry only connection readiness, before any control thread is issued.
	managedSmokeAwait(h.t, 3*time.Second, "ordinary native readiness failed", func() bool {
		var result any
		return h.ordinary.Request(ctx, "config/read", map[string]any{"includeLayers": false}, &result) == nil
	})
	id, err := h.ordinary.StartThreadWithSettings(ctx, filepath.Join(h.root, "ordinary-cwd"), map[string]string{}, codex.ThreadSettings{Model: "gpt-5.5", ReasoningEffort: "low"})
	if err != nil {
		managedSmokeFail(h.t, "ordinary native control start failed")
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if h.ordinary.Request(ctx, "turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": "Synthetic ordinary continuity request"}}, "model": "gpt-5.5", "effort": "low"}, &started) != nil || started.Turn.ID == "" {
		managedSmokeFail(h.t, "ordinary native control inference failed")
	}
	managedSmokeAwait(h.t, 4*time.Second, "ordinary control completion missing", func() bool {
		var result struct {
			Thread struct {
				Turns []struct {
					ID, Status string
					Items      []struct{ Type, Text string }
				}
			}
		}
		if h.ordinary.Request(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": true}, &result) != nil {
			return false
		}
		for _, turn := range result.Thread.Turns {
			if turn.ID == started.Turn.ID && turn.Status == "completed" {
				for _, item := range turn.Items {
					if item.Type == "agentMessage" && item.Text == "ordinary-smoke-completed" {
						return true
					}
				}
			}
		}
		return false
	})
	h.ordinaryProvider.check(h.t)
}

func managedSmokeProcessAt(pid int) (managedSmokeProcess, bool) {
	data, err := managedSmokeRead(fmt.Sprintf("/proc/%d/stat", pid), 4096)
	if err != nil {
		return managedSmokeProcess{}, false
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return managedSmokeProcess{}, false
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 || fields[0] == "Z" {
		return managedSmokeProcess{}, false
	}
	group, err := managedSmokeRead(fmt.Sprintf("/proc/%d/cgroup", pid), 4096)
	if err != nil {
		return managedSmokeProcess{}, false
	}
	processGroup, err := strconv.Atoi(fields[2])
	if err != nil {
		return managedSmokeProcess{}, false
	}
	return managedSmokeProcess{pid: pid, processGroup: processGroup, start: fields[19], group: strings.TrimSpace(strings.TrimPrefix(string(group), "0::"))}, true
}

func managedSmokeAlive(process managedSmokeProcess) bool {
	current, ok := managedSmokeProcessAt(process.pid)
	return ok && current.start == process.start
}

func managedSmokeGroupEmpty(process managedSmokeProcess) bool {
	if process.processGroup != process.pid || process.pid <= 0 {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		current, ok := managedSmokeProcessAt(pid)
		if ok && current.processGroup == process.processGroup {
			return false
		}
	}
	return true
}

func (h *managedSmokeHarness) nativeFor(root, group string) managedSmokeProcess {
	var found managedSmokeProcess
	managedSmokeAwait(h.t, 4*time.Second, "sole owned native child not ready", func() bool {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return false
		}
		count := 0
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil {
				continue
			}
			command, err := managedSmokeRead(filepath.Join("/proc", entry.Name(), "cmdline"), 8192)
			if err != nil {
				continue
			}
			args := strings.Split(strings.TrimSuffix(string(command), "\x00"), "\x00")
			if len(args) != 7 || args[1] != "-c" || args[2] != "model_catalog_json="+strconv.Quote(h.catalog) || args[3] != "app-server" || args[4] != "--strict-config" || args[5] != "--listen" || !strings.HasPrefix(args[6], "unix://"+filepath.Join(root, "runtime", "naming-")) {
				continue
			}
			process, ok := managedSmokeProcessAt(pid)
			if !ok {
				continue
			}
			if process.processGroup != process.pid {
				managedSmokeFail(h.t, "native process group ownership missing")
			}
			if group != "" && process.group != group && !strings.HasPrefix(process.group, group+"/") {
				managedSmokeFail(h.t, "native child outside owned unit cgroup")
			}
			status, err := managedSmokeRead(filepath.Join("/proc", entry.Name(), "status"), 8192)
			if err != nil || !strings.Contains(string(status), "Uid:\t1000\t1000\t1000\t1000") || managedSmokeHash(h.t, filepath.Join("/proc", entry.Name(), "exe")) != h.manifest.NativeSHA256 {
				managedSmokeFail(h.t, "native executable or UID ownership mismatch")
			}
			socket := strings.TrimPrefix(args[6], "unix://")
			cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
			contents, readErr := os.ReadDir(cwd)
			if err != nil || cwd != filepath.Join(filepath.Dir(socket), "cwd") || readErr != nil || len(contents) != 0 {
				managedSmokeFail(h.t, "native startup cwd not empty and private")
			}
			found = process
			count++
		}
		if count > 1 {
			managedSmokeFail(h.t, "multiple naming children in one lifetime")
		}
		return count == 1
	})
	return found
}

func (h *managedSmokeHarness) probe(root string) managedSmokeProcess {
	process := h.nativeFor(root, "")
	entries, err := os.ReadDir(filepath.Join(root, "runtime"))
	if err != nil {
		managedSmokeFail(h.t, "owned runtime directory unavailable")
	}
	var socket string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "naming-") {
			if socket != "" {
				managedSmokeFail(h.t, "ambiguous owned alias")
			}
			socket = filepath.Join(root, "runtime", entry.Name(), "app.sock")
		}
	}
	if socket == "" {
		managedSmokeFail(h.t, "owned naming alias missing")
	}
	client := codex.New(socket)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var result struct {
		Config struct {
			Catalog string `json:"model_catalog_json"`
		} `json:"config"`
		Origins map[string]struct {
			Name struct {
				Type string `json:"type"`
			} `json:"name"`
		} `json:"origins"`
	}
	if client.Request(ctx, "config/read", map[string]any{"includeLayers": false}, &result) != nil || result.Config.Catalog != h.catalog || result.Origins["model_catalog_json"].Name.Type != "sessionFlags" {
		managedSmokeFail(h.t, "actual native static catalog or sessionFlags origin missing")
	}
	h.t.Logf("owned nativePID=%d start=%s cgroup=%s nativeSHA256=%s catalogSHA256=%s", process.pid, process.start, process.group, h.manifest.NativeSHA256, managedSmokeCatalogHash)
	return process
}

func (h *managedSmokeHarness) privateCase() string {
	root, err := os.MkdirTemp(h.root, "case-")
	if err != nil {
		managedSmokeFail(h.t, "private case unavailable")
	}
	for _, relative := range []string{"workspace/work", "workspace/archive", "workspace/worktrees", "authority", "state", "runtime", "home", "xdg-runtime", "xdg-config", "xdg-state", "xdg-cache", "ordinary-home", "naming-home"} {
		if os.MkdirAll(filepath.Join(root, relative), 0700) != nil {
			managedSmokeFail(h.t, "private case directory failed")
		}
	}
	if os.Symlink(h.ordinarySocket, filepath.Join(root, "runtime", "ordinary.sock")) != nil {
		managedSmokeFail(h.t, "private ordinary socket alias failed")
	}
	for _, entry := range []struct{ home, base string }{{"ordinary-home", h.ordinaryProvider.url}, {"naming-home", h.namingProvider.url}} {
		managedSmokeWrite(h.t, filepath.Join(root, entry.home, "auth.json"), "{\"auth_mode\":\"apikey\",\"OPENAI_API_KEY\":\"synthetic-fixture-only\"}\n", 0600)
		managedSmokeWrite(h.t, filepath.Join(root, entry.home, "config.toml"), "model_provider = \"openai\"\nmodel = \"gpt-5.5\"\nmodel_reasoning_effort = \"low\"\nopenai_base_url = "+strconv.Quote(entry.base)+"\n", 0600)
	}
	if os.Symlink(h.manifest.PackageRoot, filepath.Join(root, "profile")) != nil {
		managedSmokeFail(h.t, "private profile failed")
	}
	managedSmokeWrite(h.t, filepath.Join(root, "transition.lock"), "", 0600)
	managedSmokeWrite(h.t, filepath.Join(root, "fail-dev-session"), "#!/bin/sh\nexit 73\n", 0700)
	managedSmokeWrite(h.t, filepath.Join(root, "fail-helper"), "#!/bin/sh\nexit 73\n", 0700)
	return root
}

func (h *managedSmokeHarness) startUnit(missingHome bool) *managedSmokeUnit {
	root := h.privateCase()
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		managedSmokeFail(h.t, "unit identity allocation failed")
	}
	unit := &managedSmokeUnit{name: "managed-naming-smoke-" + hex.EncodeToString(random[:]) + ".service", root: root, socket: filepath.Join(root, "runtime", "portal.sock")}
	h.units = append(h.units, unit)
	environment := managedSmokeEnvironment(root, filepath.Join(root, "ordinary-home"))
	for index, entry := range environment {
		if strings.HasPrefix(entry, "DEV_WORKSPACE_CODEX_HOME=") {
			home := filepath.Join(root, "naming-home")
			if missingHome {
				home = filepath.Join(root, "missing-native-home")
			}
			environment[index] = "DEV_WORKSPACE_CODEX_HOME=" + home
		}
	}
	args := []string{"--user", "--quiet", "--unit=" + unit.name, "--property=Type=simple", "--property=KillMode=mixed", "--property=TimeoutStopSec=150s", "--property=SendSIGKILL=yes", "--property=Restart=no", "--property=UMask=0077", "--", "/run/current-system/sw/bin/env", "-i"}
	args = append(args, environment...)
	args = append(args, filepath.Join(h.manifest.PackageRoot, "bin/workspace-portal"), "serve", "--unix-socket", unit.socket, "--workspace", filepath.Join(root, "workspace"), "--base-url", managedSmokeOrigin, "--dev-session", filepath.Join(root, "fail-dev-session"), "--authority-dir", filepath.Join(root, "authority"), "--user-state-root", filepath.Join(root, "state"), "--package-root", h.manifest.PackageRoot, "--workspace-name", "naming-smoke", "--registration-marker", filepath.Join(root, "runtime", "registration.json"), "--host-profile", filepath.Join(root, "profile"), "--transition-lock", filepath.Join(root, "transition.lock"), "--codex-socket", filepath.Join(root, "runtime", "ordinary.sock"), "--codex-version", "0.160.0", "--gh", filepath.Join(root, "fail-helper"), "--tmux", filepath.Join(root, "fail-helper"))
	managedSmokeCommand(h.t, 3*time.Second, "/run/current-system/sw/bin/systemd-run", args...)
	managedSmokeAwait(h.t, 4*time.Second, "packaged portal readiness failed", func() bool {
		status, _, err := h.http(unit.socket, "GET", "/healthz", nil)
		return err == nil && status == 200
	})
	data := managedSmokeCommand(h.t, time.Second, "/run/current-system/sw/bin/systemctl", "--user", "show", unit.name, "--property=MainPID", "--property=ControlGroup")
	var pid int
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MainPID=") {
			pid, _ = strconv.Atoi(strings.TrimPrefix(line, "MainPID="))
		}
		if strings.HasPrefix(line, "ControlGroup=") {
			unit.group = strings.TrimPrefix(line, "ControlGroup=")
		}
	}
	var ok bool
	unit.main, ok = managedSmokeProcessAt(pid)
	status, _ := managedSmokeRead(fmt.Sprintf("/proc/%d/status", pid), 8192)
	if !ok || !strings.HasPrefix(unit.group, "/user.slice/user-1000.slice/") || unit.main.group != unit.group || !strings.Contains(string(status), "Uid:\t1000\t1000\t1000\t1000") {
		managedSmokeFail(h.t, "managed MainPID or cgroup ownership invalid")
	}
	h.t.Logf("owned MainPID=%d start=%s cgroup=%s", unit.main.pid, unit.main.start, unit.group)
	if _, err := os.Lstat(filepath.Join(root, "runtime", "registration.json")); !os.IsNotExist(err) {
		managedSmokeFail(h.t, "registration marker unexpectedly created")
	}
	return unit
}

func (h *managedSmokeHarness) http(socket, method, path string, values url.Values) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	var body io.Reader
	if values != nil {
		body = strings.NewReader(values.Encode())
	}
	// TLS terminates outside the portal in production. The owned HTTP UDS is
	// plain HTTP while Host and the browser Origin retain the external identity.
	request, err := http.NewRequestWithContext(ctx, method, "http://managed-smoke.example.test"+path, body)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Origin", managedSmokeOrigin)
	request.Header.Set("Accept", "application/json")
	if values != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if len(data) > 2*1024*1024 {
		return 0, nil, errors.New("response bound")
	}
	return response.StatusCode, data, err
}

func (h *managedSmokeHarness) admit(socket, goal string) managedSmokePreparation {
	status, data, err := h.http(socket, "GET", "/", nil)
	if err != nil || status != 200 {
		managedSmokeFail(h.t, "actual new-session form unavailable")
	}
	document, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		managedSmokeFail(h.t, "actual form parse failed")
	}
	values := url.Values{"goal": {goal}, "name": {""}}
	var visit func(*html.Node, bool)
	visit = func(node *html.Node, inForm bool) {
		attrs := map[string]string{}
		for _, attr := range node.Attr {
			attrs[attr.Key] = attr.Val
		}
		if node.Type == html.ElementNode && node.Data == "form" {
			inForm = attrs["action"] == "/sessions"
		}
		if inForm && node.Type == html.ElementNode && node.Data == "input" && (attrs["name"] == "creation_date" || attrs["name"] == "catalogDigest") {
			values.Set(attrs["name"], attrs["value"])
		}
		if inForm && node.Type == html.ElementNode && node.Data == "select" && attrs["name"] == "team" {
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				if child.Type != html.ElementNode || child.Data != "option" {
					continue
				}
				var value string
				selected := false
				for _, attr := range child.Attr {
					if attr.Key == "value" {
						value = attr.Val
					}
					if attr.Key == "selected" {
						selected = true
					}
				}
				if selected {
					values.Set("team", value)
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child, inForm)
		}
	}
	visit(document, false)
	if values.Get("creation_date") == "" || values.Get("catalogDigest") == "" || values.Get("team") == "" {
		managedSmokeFail(h.t, "installed team form selection missing")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		managedSmokeFail(h.t, "request identity allocation failed")
	}
	raw[6] = (raw[6] & 15) | 64
	raw[8] = (raw[8] & 63) | 128
	id := fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
	values.Set("clientRequestId", id)
	status, data, err = h.http(socket, "POST", "/sessions", values)
	var result managedSmokePreparation
	if err != nil || status != http.StatusAccepted || json.Unmarshal(data, &result) != nil || result.RequestID != id || !managedSmokeHex(result.ReceiptID, 32) || result.Attempt != 1 {
		managedSmokeFail(h.t, "actual request admission identity failed")
	}
	return result
}

func (h *managedSmokeHarness) preparation(root string, accepted managedSmokePreparation) managedSmokePreparation {
	path := filepath.Join(userstate.WorkspaceDirectory(filepath.Join(root, "state"), "portal", filepath.Join(root, "workspace")), "session-preparations", accepted.RequestID+".json")
	var record managedSmokePreparation
	data, err := managedSmokeRead(path, 1024*1024)
	if os.IsNotExist(err) {
		data, err = managedSmokeRead(filepath.Join(filepath.Dir(filepath.Dir(path)), "session-preparation-mappings", accepted.RequestID+".json"), 1024*1024)
	}
	if err != nil || json.Unmarshal(data, &record) != nil || record.RequestID != accepted.RequestID || record.ReceiptID != accepted.ReceiptID || record.Attempt != accepted.Attempt {
		managedSmokeFail(h.t, "durable preparation identity changed")
	}
	return record
}

func (h *managedSmokeHarness) named(root string, accepted managedSmokePreparation, base, outcome string) {
	managedSmokeAwait(h.t, 6*time.Second, "expected native naming outcome missing", func() bool {
		record := h.preparation(root, accepted)
		if record.NamingOutcome == "" {
			return false
		}
		if record.Base != base || record.NamingOutcome != outcome {
			managedSmokeFail(h.t, "naming outcome or exact base mismatch")
		}
		return true
	})
}

func (h *managedSmokeHarness) waitProvider(count int, stopped bool) {
	channel := h.namingProvider.entered
	if stopped {
		channel = h.namingProvider.stopped
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for index := 0; index < count; index++ {
		select {
		case <-channel:
		case <-ctx.Done():
			managedSmokeFail(h.t, "actual provider reachability or cancellation missing")
		}
	}
	h.namingProvider.check(h.t)
}

func (h *managedSmokeHarness) emptyUnit(unit *managedSmokeUnit, child managedSmokeProcess) {
	managedSmokeAwait(h.t, 155*time.Second, "managed cgroup or captured PID survived main exit", func() bool {
		if managedSmokeAlive(unit.main) || managedSmokeAlive(child) {
			return false
		}
		return managedSmokeCgroupEmpty(unit.group)
	})
}

func (h *managedSmokeHarness) noRoute() {
	if h.namerRoutes.Load() != 0 || h.proxyFailure.Load() {
		managedSmokeFail(h.t, "ordinary proxy naming route or transport failure")
	}
}

func (h *managedSmokeHarness) close() bool {
	accounted := true
	// Stop only recorded units after recording an assertion failure. No user
	// slice, host session, profile, ordinary daemon or unrelated unit is stopped.
	for _, unit := range h.units {
		if !managedSmokeAlive(unit.main) && managedSmokeCgroupEmpty(unit.group) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 155*time.Second)
		command := exec.CommandContext(ctx, "/run/current-system/sw/bin/systemctl", "--user", "stop", unit.name)
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Run(); err != nil {
			h.t.Error("managed naming smoke: owned unit cleanup failed")
			accounted = false
		}
		cancel()
		if managedSmokeAlive(unit.main) || !managedSmokeCgroupEmpty(unit.group) {
			h.t.Error("managed naming smoke: owned cgroup retained processes")
			accounted = false
		}
	}
	if h.ordinary != nil {
		h.ordinary.Close()
	}
	if h.ordinaryCancel != nil {
		h.ordinaryCancel()
		<-h.ordinaryDone
	}
	if h.ordinaryProcess.pid != 0 && !managedSmokeGroupEmpty(h.ordinaryProcess) {
		h.t.Error("managed naming smoke: ordinary native group was not joined")
		accounted = false
	}
	if h.proxyStop != nil {
		h.proxyStop()
	}
	if h.proxy != nil {
		_ = h.proxy.Close()
		h.proxyWG.Wait()
	}
	for _, provider := range []*managedSmokeProvider{h.namingProvider, h.ordinaryProvider} {
		if provider != nil {
			provider.release()
			_ = provider.server.Close()
			provider.mu.Lock()
			if provider.failure != "" {
				h.t.Error("managed naming smoke: " + provider.failure)
			}
			provider.mu.Unlock()
		}
	}
	if h.canary != nil {
		_ = h.canary.Close()
	}
	if h.baseline != 0 && (h.counter() != h.baseline || h.canaryHits.Load() != 0) {
		h.t.Error("managed naming smoke: denied egress changed after fixed baseline")
	}
	if h.namerRoutes.Load() != 0 || h.proxyFailure.Load() {
		h.t.Error("managed naming smoke: ordinary routing isolation failed")
	}
	return accounted
}

func managedSmokeCgroupEmpty(group string) bool {
	if group == "" {
		return false
	}
	empty := true
	err := filepath.WalkDir(filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(group, "/")), func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			empty = false
			return nil
		}
		if !entry.IsDir() && entry.Name() == "cgroup.procs" {
			data, err := managedSmokeRead(path, 8192)
			if err != nil || len(bytes.TrimSpace(data)) != 0 {
				empty = false
			}
		}
		return nil
	})
	return empty && (err == nil || os.IsNotExist(err))
}

func TestManagedNamingRuntimeIntegration(t *testing.T) {
	h := newManagedSmokeHarness(t)
	parent := t
	run := func(name string, body func(*testing.T)) {
		t.Run(name, func(current *testing.T) { h.t = current; defer func() { h.t = parent }(); body(current) })
	}
	run("one child two simultaneous slots", func(t *testing.T) {
		unit := h.startUnit(false)
		child := h.probe(unit.root)
		if child.group != unit.group {
			managedSmokeFail(t, "ready child cgroup mismatch")
		}
		h.namingProvider.barrier()
		first := h.admit(unit.socket, "Managed first request")
		second := h.admit(unit.socket, "Managed second request")
		third := h.admit(unit.socket, "Managed third request")
		h.waitProvider(2, false)
		h.namingProvider.mu.Lock()
		active, posts, peak := h.namingProvider.active, h.namingProvider.posts, h.namingProvider.peak
		h.namingProvider.mu.Unlock()
		if active != 2 || posts != 2 || peak != 2 || h.nativeFor(unit.root, unit.group) != child {
			managedSmokeFail(t, "one-child concurrent-two-slot proof missing")
		}
		h.namingProvider.release()
		h.named(unit.root, first, "verify-managed-first", "model")
		h.named(unit.root, second, "verify-managed-second", "model")
		h.named(unit.root, third, "verify-managed-third", "model")
		h.waitProvider(1, false)
		h.namingProvider.mu.Lock()
		peak = h.namingProvider.peak
		h.namingProvider.mu.Unlock()
		if peak != 2 {
			managedSmokeFail(t, "native provider concurrency exceeded two")
		}
		h.noRoute()
		h.control()
	})
	// Each following case receives a fresh naming mock so no old request or
	// channel can satisfy its reachability/cancellation evidence.
	reset := func() {
		h.namingProvider.release()
		h.namingProvider.check(h.t)
		_ = h.namingProvider.server.Close()
		h.namingProvider = managedSmokeNewProvider(h.t, false)
	}
	run("missing home fallback", func(t *testing.T) {
		reset()
		unit := h.startUnit(true)
		accepted := h.admit(unit.socket, "Fallback smoke request")
		h.named(unit.root, accepted, "fallback-smoke-request", "unavailable")
		entries, err := os.ReadDir(filepath.Join(unit.root, "runtime"))
		if err != nil {
			managedSmokeFail(t, "unavailable runtime read failed")
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "naming-") {
				managedSmokeFail(t, "missing home launched naming child")
			}
		}
		h.namingProvider.mu.Lock()
		posts := h.namingProvider.posts
		h.namingProvider.mu.Unlock()
		if posts != 0 {
			managedSmokeFail(t, "missing home reached naming inference")
		}
		journal := managedSmokeCommand(t, time.Second, "/run/current-system/sw/bin/journalctl", "--user", "--unit="+unit.name, "--output=cat", "--no-pager", "--lines=32")
		if !bytes.Contains(journal, []byte("session naming runtime unavailable")) {
			managedSmokeFail(t, "startup unavailable category missing")
		}
		h.noRoute()
		h.control()
	})
	run("child loss fallback no restart", func(t *testing.T) {
		reset()
		unit := h.startUnit(false)
		child := h.probe(unit.root)
		if syscall.Kill(child.pid, syscall.SIGKILL) != nil {
			managedSmokeFail(t, "owned child fault failed")
		}
		managedSmokeAwait(t, 3*time.Second, "captured naming child survived fault", func() bool { return !managedSmokeAlive(child) })
		accepted := h.admit(unit.socket, "Fallback smoke request")
		h.named(unit.root, accepted, "fallback-smoke-request", "utility_error")
		entries, err := os.ReadDir(filepath.Join(unit.root, "runtime"))
		if err != nil {
			managedSmokeFail(t, "lost runtime directory unavailable")
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "naming-") {
				managedSmokeFail(t, "lost child runtime was adopted or restarted")
			}
		}
		h.noRoute()
		h.control()
	})
	for _, signal := range []string{"TERM", "KILL"} {
		run("managed MainPID "+signal, func(t *testing.T) {
			reset()
			unit := h.startUnit(false)
			child := h.probe(unit.root)
			h.namingProvider.barrier()
			first := h.admit(unit.socket, "Managed first request")
			second := h.admit(unit.socket, "Managed second request")
			h.waitProvider(2, false)
			managedSmokeCommand(t, time.Second, "/run/current-system/sw/bin/systemctl", "--user", "kill", "--kill-whom=main", "--signal="+signal, unit.name)
			h.emptyUnit(unit, child)
			h.waitProvider(2, true)
			for _, accepted := range []managedSmokePreparation{first, second} {
				record := h.preparation(unit.root, accepted)
				if record.Base != "" || record.Slug != "" || record.NamingOutcome != "" {
					managedSmokeFail(t, "cancelled request committed fallback or slug")
				}
			}
			if status, _, err := h.http(unit.socket, "GET", "/healthz", nil); err == nil && status == 200 {
				managedSmokeFail(t, "stopped main still admitted HTTP")
			}
			h.noRoute()
			h.control()
		})
	}
	// Naming success here ends before fixed-failure CLI initialization. The
	// existing TestSessionNameAdapterFallbackAndWorkspaceConcurrency supplies the owning
	// unit semaphore proof; this case adds real native/systemd observations.
}

func TestNativeNamingRuntimeHTTPFailure(t *testing.T) {
	h := newManagedSmokeHarness(t)
	root := h.privateCase()
	socket := filepath.Join(root, "runtime", "portal.sock")
	// These per-test environment changes match the managed clean environment;
	// the normal factory and real native child remain in use, without injection.
	for _, entry := range managedSmokeEnvironment(root, filepath.Join(root, "ordinary-home")) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	owner := startNamingRuntime(h.manifest.PackageRoot, socket, log.New(io.Discard, "", 0))
	if owner == nil {
		managedSmokeFail(t, "real native owner unavailable")
	}
	defer owner.Close()
	child := h.probe(root)
	ordinary := newCodexClient(filepath.Join(root, "runtime", "ordinary.sock"), filepath.Join(root, "workspace"))
	defer ordinary.Close()
	application, err := portalweb.New(portalweb.Config{ObserveActivity: true, CollectUploads: true, Workspace: filepath.Join(root, "workspace"), BaseURL: managedSmokeOrigin, DevSession: filepath.Join(root, "fail-dev-session"), HostProfile: filepath.Join(root, "profile"), GH: filepath.Join(root, "fail-helper"), Tmux: filepath.Join(root, "fail-helper"), AuthorityDir: filepath.Join(root, "authority"), TransitionLock: filepath.Join(root, "transition.lock"), UserStateRoot: filepath.Join(root, "state"), PackageRoot: h.manifest.PackageRoot, WorkspaceName: "naming-smoke", RegistrationMarker: filepath.Join(root, "runtime", "registration.json"), CodexSocket: filepath.Join(root, "runtime", "ordinary.sock"), CodexVersion: "0.160.0", NamingSocket: owner.socket, NamingModelCatalogFile: owner.catalog, Logger: log.New(io.Discard, "", 0), Codex: ordinary})
	if err != nil {
		managedSmokeFail(t, "owning HTTP constructor failed")
	}
	defer application.Close()
	listener, err := portalListener(socket)
	if err != nil {
		managedSmokeFail(t, "owning listener failed")
	}
	defer listener.Close()
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		done <- servePortalHTTP(ctx, application, listener, log.New(io.Discard, "", 0), managedSmokeOrigin)
	}()
	defer func() {
		cancel()
		_ = listener.Close()
		select {
		case <-joined:
		case <-time.After(12 * time.Second):
			t.Error("managed naming smoke: failed-case HTTP owner did not join")
		}
	}()
	h.namingProvider.barrier()
	accepted := h.admit(socket, "Managed first request")
	h.waitProvider(1, false)
	if listener.Close() != nil {
		managedSmokeFail(t, "owned listener fault failed")
	}
	select {
	case err := <-done:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			managedSmokeFail(t, "non-shutdown HTTP failure missing")
		}
	case <-time.After(3 * time.Second):
		managedSmokeFail(t, "HTTP failure did not join application workers")
	}
	if !managedSmokeAlive(child) {
		managedSmokeFail(t, "native child stopped before application worker cleanup")
	}
	h.waitProvider(1, true)
	record := h.preparation(root, accepted)
	if record.Base != "" || record.Slug != "" || record.NamingOutcome != "" {
		managedSmokeFail(t, "HTTP cancellation committed fallback or slug")
	}
	owner.Close()
	if !managedSmokeGroupEmpty(child) {
		managedSmokeFail(t, "owning native close retained owned processes")
	}
	h.noRoute()
	h.control()
	// Owning-function native integration, not externally forced packaged-CLI
	// HTTP failure. Packaged MainPID cases above cover the real service boundary.
}
