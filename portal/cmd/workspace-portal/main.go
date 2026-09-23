package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/cluster"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

const sessionLifecycleDeveloperInstructions = "Completing work, preparing a handoff, or setting " +
	"lifecycle state does not authorize archiving, deleting, stopping, finalizing, removing, " +
	"invoking private lifecycle helpers, or scheduling delayed or background cleanup for this " +
	"session. Perform a session lifecycle action only when the user explicitly requests that " +
	"exact action for this exact session in the current conversation; otherwise leave the " +
	"session open."

func newCodexClient(socket, workspace string) *workspacecodex.Client {
	return workspacecodex.NewWithOptions(socket, workspace, codex.ClientOptions{
		ClientInfo: codex.ClientInfo{
			Name: "dev-workspace", Title: "Development Workspace", Version: "0.1.0",
		},
		DeveloperInstructions:              sessionLifecycleDeveloperInstructions,
		PreserveThreadInstructionsOnResume: true,
		// Keep the deployed retry identity while making storage ownership explicit.
		SubmissionLedgerPath: socket + ".submission-attempts-v3.json",
	})
}

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
		"DEV_SESSION_SLUG":            runtime.Slug,
		"DEV_SESSION_WORKSPACE":       runtime.Workspace,
		"DEV_SESSION_WORK_DIR":        runtime.WorkDir,
		"DEV_SESSION_WORKTREES_DIR":   runtime.WorktreesDir,
		"DEV_SESSION_PORTAL_BASE_URL": runtime.PortalBaseURL,
		"DEV_SESSION_URL":             runtime.PortalURL,
		"DEV_SESSION_AUTHORITY_DIR":   runtime.AuthorityDir,
		"DEV_SESSION_TMUX_SOCKET":     runtime.TmuxSocket,
		"DEV_SESSION_CODEX":           runtime.CodexCommand,
		"DEV_SESSION_CODEX_SOCKET":    runtime.CodexSocket,
		"DEV_SESSION_CODEX_VERSION":   runtime.CodexVersion,
		"DEV_SESSION_PORTAL_COMMAND":  runtime.PortalCommand,
		"DEV_SESSION_REQUIRE_RUNTIME": "1",
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
		return errors.New("usage: workspace-portal serve|router|thread|team|team-mcp|team-preset|agent-teams|validate|version")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "router":
		return routeWorkspaces(args[1:])
	case "thread":
		return threadCommand(args[1:])
	case "team":
		return teamCommand(args[1:])
	case "team-mcp":
		return teamMCPCommand(args[1:], os.Stdin, os.Stdout, os.Stderr, teamMCPDependencies{})
	case "team-preset":
		return teamPresetCommand(args[1:])
	case "agent-teams":
		return agentTeamsCommand(args[1:])
	case "capture-comparison":
		return captureComparisonCommand(args[1:])
	case "uploads":
		return uploadCommand(args[1:])
	case "validate":
		return validateCommand(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// teamPresetCommand resolves one package-provided, direct-thread preset. It
// is deliberately read-only: callers persist the returned snapshot in their
// creation receipt and replay that snapshot rather than consulting a later
// package generation.
func teamPresetCommand(args []string) error {
	flags := flag.NewFlagSet("team-preset", flag.ContinueOnError)
	packageRoot := flags.String("package-root", "", "absolute installed package root")
	id := flags.String("team", "", "package team identifier")
	model := flags.String("model", "", "explicit lead Codex model")
	effort := flags.String("effort", "", "explicit lead reasoning effort")
	socket := flags.String("socket", "", "Codex App Server Unix socket for lead override validation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !canonicalAbsolutePath(*packageRoot) || *id == "" {
		return errors.New("team-preset requires --package-root and --team")
	}
	if (*model == "") != (*effort == "") {
		return errors.New("team-preset requires --model and --effort together")
	}
	if *model != "" && !canonicalAbsolutePath(*socket) {
		return errors.New("team-preset lead override requires --socket")
	}
	installed, err := agentteams.LoadInstalled(*packageRoot)
	if err != nil {
		return err
	}
	if !installed.Managed || installed.Catalog == nil {
		return errors.New("this workspace package has no direct team catalog")
	}
	preset, err := teamruntime.FindCatalogPreset(*installed.Catalog, *id)
	if err != nil {
		return err
	}
	if *model != "" {
		client := newCodexClient(*socket, "")
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		models, err := client.ListModels(ctx)
		if err != nil {
			return fmt.Errorf("load live Codex models: %w", err)
		}
		settings, err := workspacecodex.ResolveNewThreadSettings(models, codex.ThreadSettings{
			Model: *model, ReasoningEffort: *effort,
		})
		if err != nil {
			return err
		}
		preset.LeadModel, preset.LeadEffort = settings.Model, settings.ReasoningEffort
	}
	return json.NewEncoder(os.Stdout).Encode(preset)
}

type serveOptions struct {
	unixSocket, workspace, baseURL, devSession, authorityDir string
	userStateRoot                                            string
	packageRoot, workspaceName, registrationMarker           string
	displayLabel, hostLabel, sshHost                         string
	hostProfile, transitionLock                              string
	codexSocket, codexVersion                                string
	gh, tmux                                                 string
	trustedOrigins                                           trustedOriginValues
	clusterProviders                                         clusterProviderValues
}

var clusterProviderName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type clusterProviderValues []cluster.Provider

func (values *clusterProviderValues) String() string { return "" }

func (values *clusterProviderValues) Set(value string) error {
	parts := strings.SplitN(value, "=", 3)
	if len(parts) != 3 || !clusterProviderName.MatchString(parts[0]) || parts[1] == "" ||
		!filepath.IsAbs(parts[2]) {
		return errors.New("cluster provider must be ID=LABEL=/absolute/helper")
	}
	for _, existing := range *values {
		if existing.Name == parts[0] {
			return fmt.Errorf("duplicate cluster provider %q", parts[0])
		}
	}
	*values = append(*values, cluster.Provider{Name: parts[0], Label: parts[1], Helper: parts[2]})
	return nil
}

// trustedOriginValues keeps the service boundary explicit: workspace-host
// supplies only the complete origins of aliases already recorded in its private
// registry. The web package validates every value before use.
type trustedOriginValues []string

func (values *trustedOriginValues) String() string { return strings.Join(*values, ",") }

func (values *trustedOriginValues) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func newServeFlagSet() (*flag.FlagSet, *serveOptions) {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	options := &serveOptions{}
	flags.StringVar(&options.unixSocket, "unix-socket", "", "required HTTP Unix socket")
	flags.StringVar(&options.workspace, "workspace", "", "required workspace root")
	flags.StringVar(&options.baseURL, "base-url", "", "required external base URL")
	flags.Var(&options.trustedOrigins, "trusted-origin", "registered alias HTTPS origin (repeatable)")
	flags.StringVar(&options.displayLabel, "display-label", "Development workspace", "workspace label shown in the portal")
	flags.StringVar(&options.hostLabel, "host-label", "this host", "host label shown in local commands")
	flags.StringVar(&options.sshHost, "ssh-host", "", "optional SSH host for remote attach commands")
	flags.StringVar(&options.devSession, "dev-session", "", "absolute installed dev-session command")
	flags.StringVar(&options.authorityDir, "authority-dir", "", "host-only runtime session authority directory")
	flags.StringVar(&options.userStateRoot, "user-state-root", "", "absolute package-selected user state root")
	flags.StringVar(&options.packageRoot, "package-root", "", "absolute current workspace package root")
	flags.StringVar(&options.workspaceName, "workspace-name", "", "registered workspace name")
	flags.StringVar(&options.registrationMarker, "registration-marker", "", "private host registration marker")
	flags.StringVar(&options.hostProfile, "host-profile", "", "selected workspace host profile")
	flags.StringVar(&options.transitionLock, "transition-lock", "", "host-wide runtime transition lock")
	flags.StringVar(&options.codexSocket, "codex-socket", codex.DefaultSocket(), "Codex App Server Unix socket")
	flags.StringVar(&options.codexVersion, "codex-version", "", "Codex client version used by attached terminal sessions")
	flags.StringVar(&options.gh, "gh", "gh", "GitHub CLI executable")
	flags.StringVar(&options.tmux, "tmux", "tmux", "tmux executable")
	flags.Var(&options.clusterProviders, "cluster-provider", "ID=LABEL=/absolute/helper (repeatable)")
	return flags, options
}

func serve(args []string) error {
	flags, options := newServeFlagSet()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("serve accepts no positional arguments")
	}
	if options.userStateRoot == "" || options.packageRoot == "" || options.workspaceName == "" || options.registrationMarker == "" {
		return errors.New("serve requires --user-state-root, --package-root, --workspace-name and --registration-marker")
	}
	if !workspaceNamePattern.MatchString(options.workspaceName) {
		return errors.New("serve workspace name is invalid")
	}
	for flagName, value := range map[string]string{
		"--user-state-root": options.userStateRoot, "--package-root": options.packageRoot,
		"--registration-marker": options.registrationMarker,
	} {
		if value != "" && (!filepath.IsAbs(value) || filepath.Clean(value) != value) {
			return fmt.Errorf("%s must be absolute and canonical", flagName)
		}
	}
	logger := log.New(os.Stderr, "workspace-portal: ", log.LstdFlags|log.LUTC)
	codexClient := newCodexClient(options.codexSocket, options.workspace)
	defer codexClient.Close()
	application, err := portalweb.New(portalweb.Config{
		ObserveActivity: true,
		CollectUploads:  true,
		Workspace:       options.workspace, BaseURL: options.baseURL, DisplayLabel: options.displayLabel,
		TrustedOrigins: options.trustedOrigins,
		HostLabel:      options.hostLabel, SSHHost: options.sshHost, DevSession: options.devSession,
		HostProfile: options.hostProfile, GH: options.gh, Tmux: options.tmux, AuthorityDir: options.authorityDir,
		TransitionLock: options.transitionLock, UserStateRoot: options.userStateRoot,
		PackageRoot: options.packageRoot, WorkspaceName: options.workspaceName, RegistrationMarker: options.registrationMarker,
		CodexSocket: options.codexSocket, CodexVersion: options.codexVersion,
		ClusterProviders: options.clusterProviders,
		Logger:           logger, Codex: codexClient,
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

type teamModelCatalog interface {
	ListModels(context.Context) ([]codex.Model, error)
}

// resolveTeamMemberSettings keeps add/configure from falling through to the
// App Server default model. Existing members can only be changed to an exact
// live model/effort pair, matching browser-side validation.
func resolveTeamMemberSettings(ctx context.Context, client teamModelCatalog, command, model, effort string) (codex.ThreadSettings, error) {
	if command != "add" && command != "configure" {
		return codex.ThreadSettings{Model: model, ReasoningEffort: effort}, nil
	}
	if model == "" || effort == "" {
		return codex.ThreadSettings{}, fmt.Errorf("team %s requires --model and --effort", command)
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		return codex.ThreadSettings{}, fmt.Errorf("load live Codex models: %w", err)
	}
	settings, err := workspacecodex.ResolveNewThreadSettings(models, codex.ThreadSettings{
		Model: model, ReasoningEffort: effort,
	})
	if err != nil {
		return codex.ThreadSettings{}, err
	}
	return settings, nil
}

// teamCommand is the CLI counterpart of the portal's team endpoint. Both use
// the same private roster and direct App Server thread operations.
func teamCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: workspace-portal team list|preset|apply-preset|add|configure|remove|assign|require-idle|archive|retire|revive|fork")
	}
	command := args[0]
	flags := flag.NewFlagSet("team "+command, flag.ContinueOnError)
	stateRoot := flags.String("user-state-root", "", "private user state root")
	packageRoot := flags.String("package-root", "", "installed workspace package root")
	workspace := flags.String("workspace", "", "development workspace root")
	slug := flags.String("session-slug", "", "development session slug")
	rootThread := flags.String("root-thread-id", "", "lead Codex thread id")
	sourceSlug := flags.String("source-session-slug", "", "source development session slug")
	sourceRootThread := flags.String("source-root-thread-id", "", "source lead Codex thread id")
	socket := flags.String("socket", codex.DefaultSocket(), "Codex App Server Unix socket")
	cwd := flags.String("cwd", "", "session working directory")
	preset := flags.String("preset", "", "team preset")
	presetFile := flags.String("preset-file", "", "immutable team preset JSON file")
	role := flags.String("role", "", "member role")
	address := flags.String("address", "", "team member address")
	from := flags.String("from", "lead", "sender address")
	to := flags.String("to", "", "recipient address")
	message := flags.String("message", "", "assignment message")
	messageID := flags.String("message-id", "", "stable assignment retry ID")
	inputFile := flags.String("input-file", "", "file containing an assignment message")
	model := flags.String("model", "", "Codex model")
	effort := flags.String("effort", "", "Codex reasoning effort")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *stateRoot == "" || *workspace == "" || !session.ValidSlug(*slug) || *rootThread == "" {
		return errors.New("team command requires --user-state-root, --workspace, --session-slug and --root-thread-id")
	}
	store, err := teamruntime.NewStore(*stateRoot, *workspace)
	if err != nil {
		return err
	}
	if command == "list" {
		presets := teamruntime.Presets()
		if *packageRoot != "" {
			if !canonicalAbsolutePath(*packageRoot) {
				return errors.New("team list requires a canonical absolute package root")
			}
			installed, err := agentteams.LoadInstalled(*packageRoot)
			if err != nil {
				return err
			}
			if installed.Managed && installed.Catalog != nil {
				presets, err = teamruntime.PresetsFromCatalog(*installed.Catalog)
				if err != nil {
					return err
				}
			}
		}
		roster, err := store.Load(*slug, *rootThread)
		if errors.Is(err, os.ErrNotExist) {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"roster": nil, "presets": presets})
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"roster": roster, "presets": presets})
	}
	if *cwd == "" {
		*cwd = filepath.Join(*workspace, "work", *slug)
	}
	var teamCatalog *agentteams.Catalog
	if *packageRoot != "" {
		if !canonicalAbsolutePath(*packageRoot) {
			return errors.New("team command requires a canonical absolute package root")
		}
		installed, loadErr := agentteams.LoadInstalled(*packageRoot)
		if loadErr != nil {
			return loadErr
		}
		if installed.Managed {
			if installed.Catalog == nil {
				return errors.New("installed team catalog is unavailable")
			}
			teamCatalog = installed.Catalog
		}
	}
	client := newCodexClient(*socket, *workspace)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	settings, err := resolveTeamMemberSettings(ctx, client, command, *model, *effort)
	if err != nil {
		return err
	}
	if command == "add" || command == "configure" {
		*model, *effort = settings.Model, settings.ReasoningEffort
	}
	environment := map[string]string{
		"DEV_SESSION_SLUG": *slug, "DEV_SESSION_WORKSPACE": *workspace, "DEV_SESSION_WORK_DIR": *cwd,
		"DEV_SESSION_REQUIRE_RUNTIME": "1", "DEV_SESSION_CODEX_SOCKET": *socket,
	}
	service := teamruntime.Service{Store: store, Client: client, Workspace: *workspace, Catalog: teamCatalog}
	var presetSpec teamruntime.Preset
	if command == "apply-preset" {
		if *presetFile == "" {
			return errors.New("team apply-preset requires --preset-file")
		}
		info, statErr := os.Lstat(*presetFile)
		if statErr != nil {
			return fmt.Errorf("inspect team preset: %w", statErr)
		}
		if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 {
			return errors.New("team preset must be a private regular file")
		}
		file, openErr := os.Open(*presetFile)
		if openErr != nil {
			return openErr
		}
		decoder := json.NewDecoder(io.LimitReader(file, 64*1024+1))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&presetSpec)
		if decodeErr == nil && decoder.Decode(&struct{}{}) != io.EOF {
			decodeErr = errors.New("team preset has trailing data")
		}
		if closeErr := file.Close(); decodeErr == nil {
			decodeErr = closeErr
		}
		if decodeErr != nil {
			return fmt.Errorf("decode team preset: %w", decodeErr)
		}
	}
	if *inputFile != "" {
		contents, readErr := os.ReadFile(*inputFile)
		if readErr != nil {
			return readErr
		}
		*message = string(contents)
	}
	var result any
	switch command {
	case "preset":
		result, err = service.ApplyPreset(ctx, *slug, *rootThread, *cwd, environment, *preset, *model, *effort)
	case "apply-preset":
		result, err = service.ApplyPresetSpec(ctx, *slug, *rootThread, *cwd, environment, presetSpec)
	case "add":
		result, err = service.Add(ctx, *slug, *rootThread, *cwd, environment, *role, *model, *effort)
	case "configure":
		result, err = service.Configure(ctx, *slug, *rootThread, *address, *model, *effort)
	case "remove":
		err = service.Remove(ctx, *slug, *rootThread, *address)
	case "assign":
		result, err = service.Assign(ctx, *slug, *rootThread, *from, *to, *message, *model, *effort, *messageID)
	case "require-idle":
		err = service.RequireIdleAll(ctx, *slug, *rootThread)
	case "archive":
		err = service.ArchiveAll(ctx, *slug, *rootThread)
	case "retire":
		err = service.RetireAll(ctx, *slug, *rootThread)
	case "revive":
		err = service.ReviveAll(ctx, *slug, *rootThread)
	case "fork":
		if !session.ValidSlug(*sourceSlug) || *sourceRootThread == "" {
			return errors.New("team fork requires --source-session-slug and --source-root-thread-id")
		}
		source, loadErr := store.Load(*sourceSlug, *sourceRootThread)
		if errors.Is(loadErr, os.ErrNotExist) {
			result = map[string]any{"roster": nil}
		} else if loadErr != nil {
			err = loadErr
		} else {
			result, err = service.Fork(ctx, source, *slug, *rootThread, *cwd, environment)
		}
	default:
		return fmt.Errorf("unknown team command %q", command)
	}
	if err != nil {
		return err
	}
	if result == nil {
		roster, loadErr := store.Load(*slug, *rootThread)
		if loadErr != nil {
			return loadErr
		}
		result = roster
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func threadCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: workspace-portal thread create|fork|set-name|models|resolve-fork-settings|ensure-initial|require-materialized|require-idle|retire")
	}
	command := args[0]
	flags := flag.NewFlagSet("thread "+command, flag.ContinueOnError)
	userStateRoot := flags.String("user-state-root", "", "package-selected user state root")
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
	recoverArchived := flags.Bool("recover-archived", false, "restore the exact thread retained by revived tracking")
	startUnmaterialized := flags.Bool("start-unmaterialized", false, "allow the first turn on a proven fresh thread")
	force := flags.Bool("force", false, "interrupt an active thread before retiring it")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	client := newCodexClient(*socket, *workspace)
	defer client.Close()
	// Directory discovery can repair a large persisted history. Keep ordinary
	// metadata commands short, but allow recovery to complete that scan.
	timeout := time.Minute
	if command == "create" || command == "fork" || command == "retire" {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	switch command {
	case "activity":
		if *threadID == "" || *cwd == "" {
			return errors.New("thread activity requires --thread-id and --cwd")
		}
		activities, err := client.ListThreadActivity(ctx, []workspacecodex.ThreadActivity{{ID: *threadID, Cwd: *cwd}})
		if err != nil {
			return err
		}
		if len(activities) != 1 || activities[0].ID != *threadID || activities[0].Cwd != *cwd {
			return errors.New("thread activity returned the wrong conversation")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"threadId": activities[0].ID, "cwd": activities[0].Cwd, "updatedAt": activities[0].UpdatedAt.Unix(),
		})
	case "models":
		models, err := client.ListModels(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(models)
	case "resolve-fork-settings":
		if *threadID == "" {
			return errors.New("thread resolve-fork-settings requires --thread-id")
		}
		settings, err := client.ResolveForkSettings(ctx, *threadID, codex.ThreadSettings{
			Model: *model, ReasoningEffort: *effort,
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"model": settings.Model, "reasoningEffort": settings.ReasoningEffort,
		})
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
		if *recoverCreating && *recoverArchived {
			return errors.New("thread create cannot combine creating and archived recovery")
		}
		if *recoverArchived && *threadID == "" {
			return errors.New("archived thread recovery requires --thread-id")
		}
		var id string
		var err error
		settings := codex.ThreadSettings{Model: *model, ReasoningEffort: *effort}
		resolveSettings := func() (codex.ThreadSettings, error) {
			if settings.Model == "" && settings.ReasoningEffort == "" {
				return settings, nil
			}
			models, listErr := client.ListModels(ctx)
			if listErr != nil {
				return codex.ThreadSettings{}, fmt.Errorf("load Codex models: %w", listErr)
			}
			return workspacecodex.ResolveNewThreadSettings(models, settings)
		}
		if *recoverArchived {
			id, err = client.RecoverArchivedThread(ctx, *threadID, *cwd, runtime.environment())
		} else if *recoverCreating {
			id, err = client.RecoverCreatingThreadWithSettingsResolver(
				ctx, *threadID, *cwd, runtime.environment(), resolveSettings,
			)
		} else {
			if *threadID == "" {
				settings, err = resolveSettings()
				if err != nil {
					return err
				}
			}
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
		if *userStateRoot != "" {
			store, err := uploads.ForWorkspace(*workspace, *userStateRoot)
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(store.Directory, "catalog.json")); err == nil {
				hasFiles, err := store.HasThread(ctx, *threadID)
				if err != nil {
					return err
				}
				if hasFiles {
					epoch, err := session.CompletedRemovalHistory(*workspace, *sessionSlug, *userStateRoot)
					if err != nil {
						return err
					}
					target, err := store.SessionScope(ctx, *sessionSlug, id, epoch)
					if err != nil {
						return err
					}
					transcript, err := client.ReadThread(ctx, id)
					if err != nil {
						return err
					}

					if err := store.Fork(ctx, *threadID, target, transcript.Entries); err != nil {
						return err
					}
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
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
	case "require-materialized":
		if *threadID == "" || *cwd == "" {
			return errors.New("thread require-materialized requires --thread-id and --cwd")
		}
		return client.RequireThreadMaterialized(ctx, *threadID, *cwd)
	case "retire":
		if *cwd == "" {
			return errors.New("thread retire requires --cwd")
		}
		return client.RetireThread(ctx, *threadID, *cwd, *force)
	default:
		return fmt.Errorf("unknown thread command %q", command)
	}
}

func validateCommand(args []string) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	workspace := flags.String("workspace", "", "workspace root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("validate accepts no positional arguments")
	}
	if *workspace == "" {
		return errors.New("validate requires --workspace")
	}
	summaries, err := session.List(*workspace)
	if err != nil {
		return err
	}
	fmt.Printf("validated %d portal manifest(s)\n", len(summaries))
	return nil
}

// uploadCommand is used by the session deletion journal after thread retirement
// and tracking preservation. Repetition completes interrupted file reclamation.
func uploadCommand(args []string) error {
	if len(args) == 0 || args[0] != "remove-session" {
		return errors.New("usage: workspace-portal uploads remove-session")
	}
	flags := flag.NewFlagSet("uploads remove-session", flag.ContinueOnError)
	workspace := flags.String("workspace", "", "workspace root")
	state := flags.String("user-state-root", "", "user state root")
	slug := flags.String("session-slug", "", "session slug")
	thread := flags.String("thread-id", "", "retired thread identity")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if !session.ValidSlug(*slug) || flags.NArg() != 0 {
		return errors.New("session slug and retired thread identity are required")
	}
	store, err := uploads.ForWorkspace(*workspace, *state)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(store.Directory, "catalog.json")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	epoch, err := session.CompletedRemovalHistory(*workspace, *slug, *state)
	if err != nil {
		return err
	}
	return store.RemoveSession(ctx, *slug, *thread, epoch)
}

func captureComparisonCommand(args []string) error {
	flags := flag.NewFlagSet("capture-comparison", flag.ContinueOnError)
	workspace := flags.String("workspace", "", "workspace root")
	stateRoot := flags.String("user-state-root", "", "private state root")
	slug := flags.String("slug", "", "session slug")
	name := flags.String("name", "", "registered repository name")
	base := flags.String("base", "", "exact historical base commit")
	head := flags.String("head", "", "exact historical head commit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *workspace == "" || *slug == "" || *name == "" || (*base == "") != (*head == "") {
		return errors.New("capture-comparison requires --workspace, --slug, --name and, optionally, both --base and --head")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pair, err := portalweb.CaptureRepositoryComparison(ctx, *workspace, *stateRoot, *slug, *name, *base, *head)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(pair)
}

// agentTeamsCommand is the bounded JSON bridge for host registration.
// Filesystem authorities remain explicit flags so callers cannot hide them
// inside policy JSON.
func agentTeamsCommand(args []string) error {
	return agentTeamsCommandIO(args, os.Stdin, os.Stdout)
}

func agentTeamsCommandIO(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: workspace-portal agent-teams registration")
	}
	command := args[0]
	if command != "registration" {
		return fmt.Errorf("unknown agent team command %q", command)
	}
	flags := flag.NewFlagSet("agent-teams "+command, flag.ContinueOnError)
	packageRoot := flags.String("package-root", "", "absolute installed or candidate package root")
	stateRoot := flags.String("state-root", "", "absolute private user-state root")
	workspace := flags.String("workspace", "", "absolute canonical workspace root")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("agent team helper accepts no positional arguments")
	}
	if !canonicalAbsolutePath(*packageRoot) || !canonicalAbsolutePath(*stateRoot) || !canonicalAbsolutePath(*workspace) {
		return errors.New("registration requires --package-root, --state-root and --workspace")
	}
	requestData, err := readAgentTeamsRequest(input)
	if err != nil {
		return err
	}
	if _, err := agentteams.DecodeRegistrationRequest(requestData); err != nil {
		return err
	}
	plan, err := agentteams.BuildRegistrationPlan(*packageRoot)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(plan)
}

func readAgentTeamsRequest(input io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(input, int64(agentteams.MaxHelperRequestBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("read agent team helper request: %w", err)
	}
	if len(data) > agentteams.MaxHelperRequestBytes {
		return nil, errors.New("agent team helper request exceeds bounds")
	}
	return data, nil
}

func canonicalAbsolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}
