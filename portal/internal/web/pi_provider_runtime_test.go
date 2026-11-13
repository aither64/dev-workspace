package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type piChangedStartFixture struct{ *piCreationRuntimeFixture }

func (f piChangedStartFixture) ReadStart(ctx context.Context, ref agentbackend.SessionRef) (piruntime.StartRequest, error) {
	saved, err := f.piCreationRuntimeFixture.ReadStart(ctx, ref)
	saved.AppStorePath += "-changed"
	return saved, err
}

func TestPiProviderManagerConfigurationUsesExactRootsAndNoOverrides(t *testing.T) {
	configuration := Config{UserStateRoot: "/explicit/user", PiHostStateRoot: "/explicit/host", PiAuthDirectory: "/explicit/auth"}
	accounts := []piruntime.AccountBinding{{Provider: "native-a", AccountRef: "account-a"}, {Provider: "native-b", AccountRef: "account-b"}}
	got := piManagerConfiguration(configuration, accounts)
	if got.UserStateRoot != configuration.UserStateRoot || got.HostStateRoot != configuration.PiHostStateRoot || got.AuthDirectory != configuration.PiAuthDirectory || !reflect.DeepEqual(got.AccountBindings, accounts) || got.Host != nil || got.Dial != nil || got.HostHelper != "" {
		t.Fatalf("production Manager configuration changed: %#v", got)
	}
	accounts[0].AccountRef = "changed"
	if got.AccountBindings[0].AccountRef != "account-a" {
		t.Fatal("caller mutation changed startup map")
	}
	// Actual NewManager must refuse missing explicit roots without making them.
	root := t.TempDir()
	configuration.UserStateRoot = filepath.Join(root, "missing-user")
	configuration.PiHostStateRoot = filepath.Join(root, "missing-host")
	configuration.PiAuthDirectory = filepath.Join(root, "missing-auth")
	if manager, err := piruntime.NewManager(piManagerConfiguration(configuration, got.AccountBindings)); err == nil || manager != nil {
		t.Fatal("real Manager adopted missing roots")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("constructor created runtime/auth/host state")
	}
}

func TestPiProviderOriginalStartPreservesInitialRequestAndHistoricalRevision(t *testing.T) {
	_, receipt, _ := newPiCreationFixture(t)
	intent, err := receipt.piIntent()
	if err != nil {
		t.Fatal(err)
	}
	config := Config{PackageRoot: "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-dev-workspace", HostProfile: "/fixture/profile"}
	profile := hostProfileIdentity{device: 1, inode: 2, ctimeSec: 3, ctimeNSec: 4, mtimeSec: 5, mtimeNSec: 6, target: "original-target"}
	journal := session.PiCreationJournal{PiCreationIntent: intent, State: "creating"}
	binding := intent.Binding()
	start, err := piOriginalStart(config, profile, intent, binding, journal, false, piruntime.StartRequest{}, os.ErrNotExist)
	if err != nil || start.RequestID != intent.InitialInputID || start.Session != intent.SessionRef() || start.BindingRevision != 1 || start.AppStorePath != config.PackageRoot || start.ProfilePath != config.HostProfile {
		t.Fatalf("first request lost reserved identity: %#v %v", start, err)
	}
	wire, _ := json.Marshal([]any{1, uint64(1), uint64(2), int64(3), int64(4), int64(5), int64(6), "original-target"})
	digest := sha256.Sum256(wire)
	if start.ProfileGeneration != "pi-profile-v1:"+hex.EncodeToString(digest[:]) {
		t.Fatal("profile token changed its exact encoding")
	}
	ready := binding
	ready.Revision = 2
	ready.State = "ready"
	ready.Conversations = append([]session.BackendConversation{}, binding.Conversations...)
	ready.Conversations[0].State = "ready"
	ready.Conversations[0].NativeID = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	journal.NativeCreateStarted = true
	journal.NativeGeneration = strings.Repeat("8", 64)
	journal.Native = &session.PiCreationNative{NativeID: ready.Conversations[0].NativeID, HistoryRef: strings.Repeat("9", 64)}
	again, err := piOriginalStart(config, profile, intent, ready, journal, true, start, nil)
	if err != nil || again != start || again.BindingRevision == ready.Revision {
		t.Fatalf("saved launch was rewritten after binding advancement: %#v %v", again, err)
	}
	for name, edit := range map[string]func(*piruntime.StartRequest){
		"new_request":       func(s *piruntime.StartRequest) { s.RequestID = "22222222-2222-4222-8222-222222222222" },
		"current_revision":  func(s *piruntime.StartRequest) { s.BindingRevision = ready.Revision },
		"zero_revision":     func(s *piruntime.StartRequest) { s.BindingRevision = 0 },
		"different_package": func(s *piruntime.StartRequest) { s.AppStorePath += "-next" },
		"different_profile": func(s *piruntime.StartRequest) { s.ProfilePath += "-next" },
		"different_token":   func(s *piruntime.StartRequest) { s.ProfileGeneration += "-next" },
		"wrong_incarnation": func(s *piruntime.StartRequest) { s.Session.Incarnation = strings.Repeat("f", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := start
			edit(&changed)
			if _, err := piOriginalStart(config, profile, intent, ready, journal, true, changed, nil); err == nil {
				t.Fatal("conflicting saved start accepted")
			}
		})
	}
	for _, readErr := range []error{os.ErrNotExist, piruntime.ErrHeld, piruntime.ErrScope} {
		if _, err := piOriginalStart(config, profile, intent, ready, journal, true, piruntime.StartRequest{}, readErr); err == nil {
			t.Fatal("missing/malformed retained start reconstructed after native evidence")
		}
	}
	freshJournal := session.PiCreationJournal{PiCreationIntent: intent, State: "creating"}
	if _, err := piOriginalStart(config, profile, intent, binding, freshJournal, true, piruntime.StartRequest{}, os.ErrNotExist); !errors.Is(err, piruntime.ErrHeld) {
		t.Fatal("retained authority treated as first launch", err)
	}
	freshJournal.NativeCreateStarted = true
	freshJournal.NativeGeneration = strings.Repeat("8", 64)
	if _, err := piOriginalStart(config, profile, intent, binding, freshJournal, false, piruntime.StartRequest{}, os.ErrNotExist); !errors.Is(err, piruntime.ErrHeld) {
		t.Fatal("native intent without authority treated as first launch", err)
	}
}

func TestPiProviderRealConstructorAndProfileDriftRemainUnavailable(t *testing.T) {
	configuration := piProviderPackageFixture(t)
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	if err := os.Symlink(root, profile); err != nil {
		t.Fatal(err)
	}
	config := Config{Workspace: root, UserStateRoot: root, PiAuthDirectory: root, PiHostStateRoot: root, HostProfile: profile, PackageRoot: root, PiProviderConfig: &configuration}
	identity, err := readHostProfileIdentity(profile)
	if err != nil {
		t.Fatal(err)
	}
	if services, err := NewPiCreationServices(config); err == nil || services != nil {
		t.Fatal("real factory accepted a package without runtime contracts")
	}
	if err := os.Remove(profile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, profile); err != nil {
		t.Fatal(err)
	}
	if err := validatePiPackage(config, identity); !errors.Is(err, piruntime.ErrHeld) {
		t.Fatal("same-target replaced profile retained old identity", err)
	}
	legacy := newTestServer(t)
	serving := legacy.config
	serving.Codex = &browserContractCodex{}
	serving.PiProviderConfig = &configuration
	serving.PiHostStateRoot, serving.PiAuthDirectory = root, root
	application, err := NewContext(context.Background(), serving)
	if err != nil {
		t.Fatal("Pi-only static refusal prevented legacy server construction", err)
	}
	defer application.Close()
	if application.config.Codex != serving.Codex || application.config.PiCreation != nil || application.config.PiProviderConfigError == nil {
		t.Fatal("Pi-only static failure changed Codex or enabled partial runtime")
	}
}

func TestPiProviderReadyReplayUsesOriginalLaunchAndCurrentAuthority(t *testing.T) {
	s, receipt, runtime := newPiCreationFixture(t)
	s.piCreationCheckpoint = func(point string) error {
		if point == "ready_authority" {
			return errors.New("fixture interrupted before final tracking")
		}
		return nil
	}
	if err := s.initializePiCreation(receipt); err == nil {
		t.Fatal("fixture did not stop after ready publication")
	}
	if runtime.start == nil || runtime.start.BindingRevision != 1 {
		t.Fatal("first launch did not retain original binding revision")
	}
	original := *runtime.start
	s.piCreationCheckpoint = nil
	s.config.PiCreation.Runtime = piChangedStartFixture{runtime}
	if err := s.initializePiCreation(receipt); !errors.Is(err, piruntime.ErrConflict) {
		t.Fatal("historical revision accepted conflicting saved request", err)
	}
	if runtime.creates != 1 || runtime.sends != 1 {
		t.Fatal("conflicting request repeated native effects")
	}
	s.config.PiCreation.Runtime = runtime
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	if *runtime.start != original || runtime.creates != 1 || runtime.sends != 1 {
		t.Fatal("tracking recovery changed launch request or repeated native effects")
	}
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(receipt.Request.Slug)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, binding.Slug, binding.Workspace)
	if err != nil || binding.Revision != 2 || authority.BindingRevision != binding.Revision || authority.Generation != runtime.runtime.Generation || authority.State != "ready" {
		t.Fatalf("live publication used historical launch revision: %#v %v", authority, err)
	}
	if err := s.provePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
}
