package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aither64/vpsfree-cz-workspace/portal/internal/codex"
	"github.com/aither64/vpsfree-cz-workspace/portal/internal/session"
	portalweb "github.com/aither64/vpsfree-cz-workspace/portal/internal/web"
)

const version = "0.1.0"

type threadRuntime struct {
	Slug, Workspace, WorkDir, WorktreesDir, PortalBaseURL, PortalURL  string
	AuthorityDir, TmuxSocket, CodexCommand, CodexSocket, CodexVersion string
	PortalCommand                                                     string
}

func (runtime threadRuntime) complete() bool {
	return runtime.Slug != "" && runtime.Workspace != "" && runtime.WorkDir != "" &&
		runtime.WorktreesDir != "" && runtime.PortalBaseURL != "" && runtime.PortalURL != "" &&
		runtime.AuthorityDir != "" && runtime.TmuxSocket != "" && runtime.CodexCommand != "" &&
		runtime.CodexSocket != "" && runtime.CodexVersion != "" && runtime.PortalCommand != ""
}

func (runtime threadRuntime) environment() map[string]string {
	return map[string]string{
		"VPSFREE_DEV_SESSION_SLUG":            runtime.Slug,
		"VPSFREE_DEV_SESSION_WORKSPACE":       runtime.Workspace,
		"VPSFREE_DEV_SESSION_WORK_DIR":        runtime.WorkDir,
		"VPSFREE_DEV_SESSION_WORKTREES_DIR":   runtime.WorktreesDir,
		"VPSFREE_DEV_SESSION_PORTAL_BASE_URL": runtime.PortalBaseURL,
		"VPSFREE_DEV_SESSION_URL":             runtime.PortalURL,
		"VPSFREE_DEV_SESSION_AUTHORITY_DIR":   runtime.AuthorityDir,
		"VPSFREE_DEV_SESSION_TMUX_SOCKET":     runtime.TmuxSocket,
		"VPSFREE_DEV_SESSION_CODEX":           runtime.CodexCommand,
		"VPSFREE_DEV_SESSION_CODEX_SOCKET":    runtime.CodexSocket,
		"VPSFREE_DEV_SESSION_CODEX_VERSION":   runtime.CodexVersion,
		"VPSFREE_DEV_SESSION_PORTAL_COMMAND":  runtime.PortalCommand,
		"VPSFREE_DEV_SESSION_REQUIRE_RUNTIME": "1",
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "workspace-portal:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: workspace-portal serve|thread|validate|version")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "thread":
		return threadCommand(args[1:])
	case "validate":
		return validateCommand(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type serveOptions struct {
	unixSocket, workspace, baseURL, devSession, authorityDir string
	codexSocket, codexVersion                                string
	gh, tmux                                                 string
	vpsadminCluster, vpsadminOSCluster                       string
}

func newServeFlagSet() (*flag.FlagSet, *serveOptions) {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	options := &serveOptions{}
	flags.StringVar(&options.unixSocket, "unix-socket", "", "required HTTP Unix socket")
	flags.StringVar(&options.workspace, "workspace", "/home/aither/workspace/ai/vpsfree.cz", "workspace root")
	flags.StringVar(&options.baseURL, "base-url", "https://vpsfree-cz-workspace.aitherdev.int.vpsfree.cz", "external base URL")
	flags.StringVar(&options.devSession, "dev-session", "", "absolute installed dev-session command")
	flags.StringVar(&options.authorityDir, "authority-dir", "", "host-only runtime session authority directory")
	flags.StringVar(&options.codexSocket, "codex-socket", codex.DefaultSocket(), "Codex App Server Unix socket")
	flags.StringVar(&options.codexVersion, "codex-version", "", "Codex client version used by attached terminal sessions")
	flags.StringVar(&options.gh, "gh", "gh", "GitHub CLI executable")
	flags.StringVar(&options.tmux, "tmux", "tmux", "tmux executable")
	flags.StringVar(&options.vpsadminCluster, "vpsadmin-cluster", "", "absolute vpsAdmin development cluster helper")
	flags.StringVar(&options.vpsadminOSCluster, "vpsadminos-cluster", "", "absolute vpsAdminOS development cluster helper")
	return flags, options
}

func serve(args []string) error {
	flags, options := newServeFlagSet()
	if err := flags.Parse(args); err != nil {
		return err
	}
	logger := log.New(os.Stderr, "workspace-portal: ", log.LstdFlags|log.LUTC)
	codexClient := codex.New(options.codexSocket)
	defer codexClient.Close()
	application, err := portalweb.New(portalweb.Config{
		Workspace: options.workspace, BaseURL: options.baseURL, DevSession: options.devSession,
		GH: options.gh, Tmux: options.tmux, AuthorityDir: options.authorityDir,
		CodexSocket:     options.codexSocket,
		CodexVersion:    options.codexVersion,
		VpsadminCluster: options.vpsadminCluster, VpsadminOSCluster: options.vpsadminOSCluster,
		Logger: logger, Codex: codexClient,
	})
	if err != nil {
		return err
	}
	defer application.Close()
	listener, err := portalListener(options.unixSocket)
	if err != nil {
		return err
	}
	defer listener.Close()
	httpServer := &http.Server{Handler: application.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 * 1024}
	stopContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan error, 1)
	go func() {
		<-stopContext.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 130*time.Second)
		defer cancel()
		httpShutdown := make(chan error, 1)
		go func() { httpShutdown <- httpServer.Shutdown(shutdownContext) }()
		application.Close()
		shutdownDone <- <-httpShutdown
	}()
	logger.Printf("listening on %s for %s", listener.Addr(), options.baseURL)
	err = httpServer.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return <-shutdownDone
	}
	return err
}

func portalListener(socketPath string) (net.Listener, error) {
	if socketPath == "" {
		return nil, errors.New("--unix-socket is required")
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("Unix socket path already exists and is not a socket: %s", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("remove stale Unix socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect Unix socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o660); err != nil {
		listener.Close()
		return nil, fmt.Errorf("set Unix socket permissions: %w", err)
	}
	return listener, nil
}

func threadCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: workspace-portal thread create|fork|set-name|ensure-initial|require-idle")
	}
	command := args[0]
	flags := flag.NewFlagSet("thread "+command, flag.ContinueOnError)
	socket := flags.String("socket", codex.DefaultSocket(), "Codex App Server Unix socket")
	cwd := flags.String("cwd", "", "thread working directory")
	workspace := flags.String("workspace", "", "development workspace root")
	sessionSlug := flags.String("session-slug", "", "development session slug")
	worktreesDir := flags.String("worktrees-dir", "", "development session worktree directory")
	portalBaseURL := flags.String("portal-base-url", "", "workspace portal base URL")
	portalURL := flags.String("portal-url", "", "development session portal URL")
	portalCommand := flags.String("portal-command", "", "stable workspace-portal executable")
	authorityDir := flags.String("authority-dir", "", "host-only runtime authority directory")
	tmuxSocket := flags.String("tmux-socket", "", "dedicated tmux socket")
	codexCommand := flags.String("codex-command", "", "absolute Codex executable")
	codexVersion := flags.String("codex-version", "", "Codex client version")
	name := flags.String("name", "", "thread name")
	threadID := flags.String("thread-id", "", "thread id")
	model := flags.String("model", "", "Codex model")
	effort := flags.String("effort", "", "Codex reasoning effort")
	inputFile := flags.String("input-file", "", "file containing a message")
	requireRuntime := flags.Bool("require-runtime", false, "require complete deployed runtime provenance")
	recoverCreating := flags.Bool("recover-creating", false, "reconcile a creating thread by working directory")
	startUnmaterialized := flags.Bool("start-unmaterialized", false, "allow the first turn on a proven fresh thread")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	client := codex.New(*socket)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	switch command {
	case "create":
		runtime := threadRuntime{
			Slug: *sessionSlug, Workspace: *workspace, WorkDir: *cwd,
			WorktreesDir: *worktreesDir, PortalBaseURL: *portalBaseURL, PortalURL: *portalURL,
			AuthorityDir: *authorityDir, TmuxSocket: *tmuxSocket,
			CodexCommand: *codexCommand, CodexSocket: *socket, CodexVersion: *codexVersion,
			PortalCommand: *portalCommand,
		}
		if !*requireRuntime || !runtime.complete() {
			return errors.New("thread create requires complete workspace, lifecycle, tmux and Codex provenance")
		}
		var id string
		var err error
		settings := codex.ThreadSettings{Model: *model, ReasoningEffort: *effort}
		if *threadID == "" || *recoverCreating {
			models, listErr := client.ListModels(ctx)
			if listErr != nil {
				return fmt.Errorf("load Codex models: %w", listErr)
			}
			settings, listErr = codex.ResolveNewThreadSettings(models, settings)
			if listErr != nil {
				return listErr
			}
		}
		if *recoverCreating {
			id, err = client.RecoverCreatingThreadWithSettings(ctx, *threadID, *cwd, runtime.environment(), settings)
		} else {
			id, err = client.OpenThreadWithSettings(ctx, *threadID, *cwd, runtime.environment(), settings)
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"threadId": id})
	case "fork":
		runtime := threadRuntime{
			Slug: *sessionSlug, Workspace: *workspace, WorkDir: *cwd,
			WorktreesDir: *worktreesDir, PortalBaseURL: *portalBaseURL, PortalURL: *portalURL,
			AuthorityDir: *authorityDir, TmuxSocket: *tmuxSocket,
			CodexCommand: *codexCommand, CodexSocket: *socket, CodexVersion: *codexVersion,
			PortalCommand: *portalCommand,
		}
		if !*requireRuntime || !runtime.complete() || *threadID == "" {
			return errors.New("thread fork requires a source thread and complete runtime provenance")
		}
		id, err := client.RecoverForkThread(
			ctx, *threadID, *cwd, runtime.environment(),
			codex.ThreadSettings{Model: *model, ReasoningEffort: *effort},
		)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"threadId": id})
	case "set-name":
		if *threadID == "" || *name == "" {
			return errors.New("thread set-name requires --thread-id and --name")
		}
		return client.SetName(ctx, *threadID, *name)
	case "ensure-initial":
		if *threadID == "" || *cwd == "" || *inputFile == "" {
			return errors.New("thread ensure-initial requires --thread-id, --cwd and --input-file")
		}
		input, err := os.ReadFile(*inputFile)
		if err != nil {
			return err
		}
		if len(input) == 0 || len(input) > session.MaxMessageBytes {
			return fmt.Errorf(
				"thread input must contain between 1 and %s bytes",
				session.FormattedMaxMessageBytes(),
			)
		}
		message := bytes.TrimSpace(input)
		if len(message) == 0 {
			return errors.New("thread input must not be blank")
		}
		return client.EnsureInitialMessage(ctx, *threadID, *cwd, string(message), *startUnmaterialized)
	case "require-idle":
		if *threadID == "" || *cwd == "" {
			return errors.New("thread require-idle requires --thread-id and --cwd")
		}
		return client.RequireThreadIdle(ctx, *threadID, *cwd)
	default:
		return fmt.Errorf("unknown thread command %q", command)
	}
}

func validateCommand(args []string) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	workspace := flags.String("workspace", "/home/aither/workspace/ai/vpsfree.cz", "workspace root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("validate accepts no positional arguments")
	}
	summaries, err := session.List(*workspace)
	if err != nil {
		return err
	}
	fmt.Printf("validated %d portal manifest(s)\n", len(summaries))
	return nil
}
