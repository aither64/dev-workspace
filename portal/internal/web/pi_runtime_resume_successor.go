package web

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type piSuccessorStartResumer interface {
	ReadStart(context.Context, agentbackend.SessionRef) (piruntime.StartRequest, error)
	ResumeSuccessorStart(context.Context, piruntime.UpgradeRequest) (piruntime.RuntimeStatus, error)
}

// This finite cold admission reads the selected profile configuration without
// constructing a server or starting background work.
func resumePiSuccessorStart(ctx context.Context, config Config, slug, request, expected string, services *PiCreationServices) (piruntime.RuntimeStatus, error) {
	if err := ctx.Err(); err != nil {
		return piruntime.RuntimeStatus{}, err
	}
	if !session.ValidSlug(slug) || !queueClientMessageIDPattern.MatchString(request) || len(request) != 36 || !messageDigestPattern.MatchString(expected) || !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62})$`).MatchString(config.WorkspaceName) {
		return piruntime.RuntimeStatus{}, piruntime.ErrScope
	}
	for _, path := range []string{config.Workspace, config.UserStateRoot, config.AuthorityDir, config.HostProfile, config.TransitionLock, config.RegistrationMarker, config.CodexSocket, config.PiAuthDirectory, config.PiHostStateRoot} {
		if !explicitPiPath(path) {
			return piruntime.RuntimeStatus{}, piruntime.ErrScope
		}
	}
	workspace, err := filepath.EvalSymlinks(config.Workspace)
	if err != nil || workspace != config.Workspace || filepath.Base(config.RegistrationMarker) != "registration.json" || filepath.Dir(config.RegistrationMarker) != filepath.Dir(config.CodexSocket) {
		return piruntime.RuntimeStatus{}, piruntime.ErrScope
	}
	profile, err := readHostProfileIdentity(config.HostProfile)
	if err != nil {
		return piruntime.RuntimeStatus{}, err
	}
	selected, err := filepath.EvalSymlinks(config.HostProfile)
	if err != nil {
		return piruntime.RuntimeStatus{}, err
	}
	config.PackageRoot = selected
	installed, err := agentteams.LoadInstalled(selected)
	if err != nil {
		return piruntime.RuntimeStatus{}, err
	}
	configuration, err := piproviders.LoadInstalled(selected)
	if err != nil {
		return piruntime.RuntimeStatus{}, err
	}
	config.PiProviderConfig = &configuration
	if err := validatePiPackage(config, profile); err != nil {
		return piruntime.RuntimeStatus{}, err
	}
	if services == nil {
		services, err = NewPiCreationServices(config)
	} else {
		runtime := services.Runtime
		services, err = newPiSelectionServices(config)
		if err == nil && services != nil {
			services.Runtime = runtime
		}
	}
	if err != nil {
		return piruntime.RuntimeStatus{}, err
	}
	if services == nil {
		return piruntime.RuntimeStatus{}, piruntime.ErrUnavailable
	}
	config.PiCreation = services
	store, err := newLifecycleOperationStore(config.Workspace, config.UserStateRoot)
	if err != nil {
		return piruntime.RuntimeStatus{}, err
	}
	s := &Server{config: config, hostProfile: profile, installedTeams: &installed, operationStore: store}
	binding, err := (session.BackendBindingStore{Root: config.UserStateRoot, Workspace: config.Workspace}).Load(slug)
	if err != nil || binding.Backend != agentbackend.Pi {
		return piruntime.RuntimeStatus{}, errors.Join(agentbackend.ErrBindingConflict, err)
	}
	ref := agentbackend.SessionRef{Workspace: binding.Workspace, Slug: binding.Slug, Incarnation: binding.SessionIncarnation}
	current, release, err := s.acquirePiSessionAdmissionFor(ctx, ref, true, true)
	if err != nil {
		return piruntime.RuntimeStatus{}, err
	}
	defer release()
	runtime, ok := services.Runtime.(piSuccessorStartResumer)
	if !ok {
		return piruntime.RuntimeStatus{}, piruntime.ErrUnavailable
	}
	start, err := runtime.ReadStart(ctx, ref)
	if err != nil || start.Session != ref || start.RequestID != request || start.ReplacesRequestID == "" || start.BindingRevision != current.Binding.Revision || start.AppStorePath != selected || start.ProfilePath != config.HostProfile || start.ProfileGeneration != piProfileToken(profile) || current.Authority.Generation != expected {
		return piruntime.RuntimeStatus{}, errors.Join(piruntime.ErrConflict, err)
	}
	members := make([]agentbackend.ConversationRef, 0, len(current.Binding.Conversations))
	for _, member := range current.Binding.Conversations {
		bound, err := current.Binding.Resolve(member.Address)
		if err != nil {
			return piruntime.RuntimeStatus{}, err
		}
		members = append(members, bound)
	}
	if err := s.requireCurrentHostProfile(); err != nil {
		return piruntime.RuntimeStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return piruntime.RuntimeStatus{}, err
	}
	return runtime.ResumeSuccessorStart(ctx, piruntime.UpgradeRequest{Start: start, ExpectedGeneration: expected, Members: members})
}

// RunPiRuntimeResumeSuccessor is shared by the finite command and its joined
// fixture. Config carries only an optional existing in-process runtime port.
func RunPiRuntimeResumeSuccessor(ctx context.Context, args []string, output io.Writer, fixture Config) error {
	flags := flag.NewFlagSet("resume-successor-start", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := Config{PiCreation: fixture.PiCreation}
	values := []struct {
		name   string
		target *string
	}{
		{"workspace", &config.Workspace}, {"workspace-name", &config.WorkspaceName},
		{"user-state-root", &config.UserStateRoot}, {"authority-dir", &config.AuthorityDir},
		{"host-profile", &config.HostProfile}, {"transition-lock", &config.TransitionLock},
		{"registration-marker", &config.RegistrationMarker}, {"codex-socket", &config.CodexSocket},
		{"pi-auth-dir", &config.PiAuthDirectory}, {"pi-host-state-root", &config.PiHostStateRoot},
	}
	for _, value := range values {
		flags.StringVar(value.target, value.name, "", "explicit serving configuration")
	}
	slug := flags.String("slug", "", "existing ready session")
	request := flags.String("request", "", "retained successor request UUID")
	expected := flags.String("expected-generation", "", "exact predecessor generation")
	if len(args) != 26 {
		return piruntime.ErrScope
	}
	seen := map[string]bool{}
	for i := 0; i < len(args); i += 2 {
		if len(args[i]) < 3 || args[i][:2] != "--" || seen[args[i]] || args[i+1] == "" {
			return piruntime.ErrScope
		}
		seen[args[i]] = true
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return piruntime.ErrScope
	}
	controller, err := os.Executable()
	if err != nil {
		return err
	}
	controller, err = filepath.EvalSymlinks(controller)
	if err != nil {
		return err
	}
	status, err := resumePiSuccessorStart(ctx, config, *slug, *request, *expected, config.PiCreation)
	if err != nil {
		return fmt.Errorf("successor request %s remains unconfirmed: %w", *request, err)
	}
	selected, err := filepath.EvalSymlinks(config.HostProfile)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(output).Encode(struct {
		ControllerExecutable string
		SelectedPackage      string
		Status               piruntime.RuntimeStatus
	}{controller, selected, status}); err != nil {
		return err
	}
	if status.Phase != "ready" {
		return fmt.Errorf("successor request %s remains %s; inspect the same retained request", *request, status.Phase)
	}
	return nil
}
