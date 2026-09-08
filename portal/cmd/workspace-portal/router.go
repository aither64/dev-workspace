package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var workspaceNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62})$`)
var hostnameLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

const maxWorkspaceRegistryBytes = 1024 * 1024

type workspaceRegistry struct {
	Schema     int                `json:"schema"`
	Workspaces []workspaceBinding `json:"workspaces"`
}

type workspaceBinding struct {
	Name     string   `json:"name"`
	Root     string   `json:"root"`
	Hostname string   `json:"hostname"`
	Aliases  []string `json:"aliases,omitempty"`
}

func routeWorkspaces(args []string) error {
	flags := flag.NewFlagSet("router", flag.ContinueOnError)
	registryPath := flags.String("registry", "", "workspace registry JSON file")
	runtimeDir := flags.String("runtime-dir", "", "private workspace runtime directory")
	unixSocket := flags.String("unix-socket", "", "HTTP Unix socket shared with the HTTPS proxy")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *registryPath == "" || *runtimeDir == "" || *unixSocket == "" {
		return errors.New("router requires --registry, --runtime-dir and --unix-socket")
	}

	logger := log.New(os.Stderr, "workspace-router: ", log.LstdFlags|log.LUTC)
	handler := workspaceRouterHandler(*registryPath, *runtimeDir, logger)
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    32 * 1024,
	}
	stopContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-stopContext.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	listener, err := portalListener(*unixSocket)
	if err != nil {
		return err
	}
	defer listener.Close()
	logger.Printf("listening on %s", listener.Addr())
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func workspaceRouterHandler(registryPath, runtimeDir string, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		registry, err := loadWorkspaceRegistry(registryPath)
		if err != nil {
			logger.Printf("load registry: %v", err)
			http.Error(w, "workspace registry is unavailable", http.StatusServiceUnavailable)
			return
		}
		host := strings.ToLower(request.Host)
		if parsedHost, _, splitErr := net.SplitHostPort(host); splitErr == nil {
			host = parsedHost
		}
		for _, workspace := range registry.Workspaces {
			if host == workspace.Hostname {
				proxyWorkspace(w, request, runtimeDir, workspace, logger)
				return
			}
			for _, alias := range workspace.Aliases {
				if host == alias {
					target := "https://" + workspace.Hostname + request.URL.RequestURI()
					http.Redirect(w, request, target, http.StatusPermanentRedirect)
					return
				}
			}
		}
		http.NotFound(w, request)
	})
}

func proxyWorkspace(w http.ResponseWriter, request *http.Request, runtimeDir string, workspace workspaceBinding, logger *log.Logger) {
	socket := filepath.Join(runtimeDir, workspace.Name, "portal.sock")
	target, _ := url.Parse("http://workspace-portal")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, err error) {
		logger.Printf("proxy %s: %v", workspace.Name, err)
		http.Error(writer, "workspace portal is unavailable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, request)
}

func loadWorkspaceRegistry(path string) (*workspaceRegistry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("registry is not a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("registry is not owned by the current user")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("registry must not be accessible by group or others")
	}
	if info.Size() > maxWorkspaceRegistryBytes {
		return nil, errors.New("registry exceeds 1 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var registry workspaceRegistry
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registry); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("registry must contain exactly one JSON value")
	}
	if registry.Schema != 1 || registry.Workspaces == nil {
		return nil, errors.New("registry must use schema 1 and contain a workspace list")
	}
	seenNames := make(map[string]struct{})
	seenHosts := make(map[string]struct{})
	seenRoots := make(map[string]struct{})
	for index := range registry.Workspaces {
		workspace := &registry.Workspaces[index]
		workspace.Hostname = strings.ToLower(workspace.Hostname)
		if !workspaceNamePattern.MatchString(workspace.Name) {
			return nil, fmt.Errorf("invalid workspace name %q", workspace.Name)
		}
		if _, exists := seenNames[workspace.Name]; exists {
			return nil, fmt.Errorf("duplicate workspace name %q", workspace.Name)
		}
		seenNames[workspace.Name] = struct{}{}
		if !filepath.IsAbs(workspace.Root) || filepath.Clean(workspace.Root) != workspace.Root {
			return nil, fmt.Errorf("workspace %q has an invalid root", workspace.Name)
		}
		if _, exists := seenRoots[workspace.Root]; exists {
			return nil, fmt.Errorf("duplicate workspace root %q", workspace.Root)
		}
		seenRoots[workspace.Root] = struct{}{}
		hosts := append([]string{workspace.Hostname}, workspace.Aliases...)
		for hostIndex, candidate := range hosts {
			candidate = strings.ToLower(candidate)
			if !validWorkspaceHostname(candidate) {
				return nil, fmt.Errorf("workspace %q has an invalid hostname %q", workspace.Name, candidate)
			}
			if _, exists := seenHosts[candidate]; exists {
				return nil, fmt.Errorf("duplicate workspace hostname %q", candidate)
			}
			seenHosts[candidate] = struct{}{}
			if hostIndex > 0 {
				workspace.Aliases[hostIndex-1] = candidate
			}
		}
	}
	return &registry, nil
}

func validWorkspaceHostname(hostname string) bool {
	labels := strings.Split(hostname, ".")
	if len(hostname) > 253 || len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !hostnameLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}
