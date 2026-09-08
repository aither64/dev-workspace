package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceRouterRoutesCanonicalHostAndRedirectsAlias(t *testing.T) {
	directory := t.TempDir()
	registry := filepath.Join(directory, "registry.json")
	writeRegistry(t, registry)
	runtime, err := os.MkdirTemp("/tmp", "workspace-router-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtime) })
	instance := filepath.Join(runtime, "vpsfree-cz")
	if err := os.MkdirAll(instance, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(instance, "portal.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	backend := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "portal")
	})}
	defer backend.Close()
	go backend.Serve(listener)

	handler := workspaceRouterHandler(registry, runtime, log.New(io.Discard, "", 0))
	request := httptest.NewRequest(http.MethodGet, "http://canonical/session", nil)
	request.Host = "vpsfree-cz.workspace.aitherdev.int.vpsfree.cz"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "portal" {
		t.Fatalf("canonical response = %d %q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "http://alias/session?q=1", nil)
	request.Host = "vpsfree-cz-workspace.aitherdev.int.vpsfree.cz"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPermanentRedirect ||
		response.Header().Get("Location") != "https://vpsfree-cz.workspace.aitherdev.int.vpsfree.cz/session?q=1" {
		t.Fatalf("alias response = %d %q", response.Code, response.Header().Get("Location"))
	}
}

func TestWorkspaceRouterRegistryMatchesManagementBoundaries(t *testing.T) {
	directory := t.TempDir()
	registry := filepath.Join(directory, "registry.json")
	if err := os.WriteFile(registry, []byte(`{"schema":1,"workspaces":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadWorkspaceRegistry(registry)
	if err != nil || len(loaded.Workspaces) != 0 {
		t.Fatalf("empty registry = %#v, %v", loaded, err)
	}

	duplicateRoot := `{"schema":1,"workspaces":[` +
		`{"name":"first","root":"/workspace","hostname":"first.example.test"},` +
		`{"name":"second","root":"/workspace","hostname":"second.example.test"}]}`
	if err := os.WriteFile(registry, []byte(duplicateRoot), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkspaceRegistry(registry); err == nil || !strings.Contains(err.Error(), "duplicate workspace root") {
		t.Fatalf("duplicate root result = %v", err)
	}

	if err := os.WriteFile(registry, make([]byte, maxWorkspaceRegistryBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkspaceRegistry(registry); err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("oversized registry result = %v", err)
	}
}

func TestWorkspaceRouterRejectsUnknownHostAndUnsafeRegistry(t *testing.T) {
	directory := t.TempDir()
	registry := filepath.Join(directory, "registry.json")
	writeRegistry(t, registry)
	handler := workspaceRouterHandler(registry, directory, log.New(io.Discard, "", 0))
	request := httptest.NewRequest(http.MethodGet, "http://unknown/", nil)
	request.Host = "unknown.workspace.aitherdev.int.vpsfree.cz"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown response = %d", response.Code)
	}

	if err := os.Chmod(registry, 0o644); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unsafe registry response = %d", response.Code)
	}
}

func writeRegistry(t *testing.T, path string) {
	t.Helper()
	data := []byte(`{"schema":1,"workspaces":[{"name":"vpsfree-cz","root":"/workspace","hostname":"vpsfree-cz.workspace.aitherdev.int.vpsfree.cz","aliases":["vpsfree-cz-workspace.aitherdev.int.vpsfree.cz"]}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
