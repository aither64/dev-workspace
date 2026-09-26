package teamruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
)

type testClient struct {
	mu                 sync.Mutex
	next               int
	sends              []string
	messageIDs         []string
	options            []codex.TurnOptions
	compactPrefixes    []string
	compactError       error
	clearedThreads     []string
	clearError         error
	ledgerEvents       []string
	unresolvedAttempts map[string]bool
	attemptChecks      []string
	starts             []codex.ThreadSettings
	projects           map[string]codex.ProjectMetadata
	projectCreates     int
	projectEntered     chan struct{}
	releaseProject     <-chan struct{}
	lostProjectCreate  bool
	projectReadError   error
	resumes            []codex.ThreadSettings
	forkEnvs           []map[string]string
	forkSettings       []codex.ThreadSettings
	threads            []codex.ThreadMetadata
	unmaterialized     map[string]bool
	bootstrapCalls     []string
	bootstrapError     error
	deleted            []string
	deleteError        error
	deleteResponseLost bool
	readError          error
	archived           map[string]bool
	archiveAfter       func()
	archiveResultError error
	listPageSize       int
	emptyNextCursor    bool
	ignoreListCwd      bool
	lostStart          bool
	lostFork           bool
	hideThreads        bool
	nameFailure        bool
	idleError          error
	idleChecks         []string
	verifyError        error
	verified           []string
	activeTurn         string
	interrupts         []string
	turnChecks         int
	busyChecks         int
	archives           []string
	sendEntered        chan struct{}
	releaseSend        chan struct{}
}

func (client *testClient) CreateProject(_ context.Context, name, key string) (codex.ProjectMetadata, error) {
	if client.projectEntered != nil {
		select {
		case client.projectEntered <- struct{}{}:
		default:
		}
		<-client.releaseProject
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	client.projectCreates++
	if client.projects == nil {
		client.projects = make(map[string]codex.ProjectMetadata)
	}
	project, ok := client.projects[key]
	if !ok {
		project = codex.ProjectMetadata{ID: fmt.Sprintf("00000000-0000-7000-8000-%012x", len(client.projects)+1), Name: name}
		client.projects[key] = project
	}
	if client.lostProjectCreate {
		client.lostProjectCreate = false
		return codex.ProjectMetadata{}, errors.New("lost project/create response")
	}
	return project, nil
}

func (client *testClient) ReadProject(_ context.Context, id string) (codex.ProjectMetadata, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.projectReadError != nil {
		return codex.ProjectMetadata{}, client.projectReadError
	}
	for _, project := range client.projects {
		if project.ID == id {
			return project, nil
		}
	}
	return codex.ProjectMetadata{}, &codex.ProjectNotFoundError{ProjectID: id}
}

func (client *testClient) StartThreadWithSettings(_ context.Context, cwd string, _ map[string]string, settings codex.ThreadSettings) (string, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.next++
	client.starts = append(client.starts, settings)
	id := fmt.Sprintf("thread-%d", client.next)
	client.threads = append(client.threads, codex.ThreadMetadata{ID: id, Cwd: cwd, ProjectID: &settings.ProjectID})
	if client.lostStart {
		return "", errors.New("lost response")
	}
	return id, nil
}
func (client *testClient) ListThreads(_ context.Context, options codex.ThreadListOptions) ([]codex.ThreadMetadata, *string, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.hideThreads {
		return nil, nil, nil
	}
	var found []codex.ThreadMetadata
	for _, thread := range client.threads {
		if !client.ignoreListCwd && options.Cwd != "" && thread.Cwd != options.Cwd {
			continue
		}
		if options.ProjectID != "" && (thread.ProjectID == nil || *thread.ProjectID != options.ProjectID) {
			continue
		}
		if options.Archived != nil && client.archived[thread.ID] != *options.Archived {
			continue
		}
		found = append(found, thread)
	}
	if client.listPageSize > 0 {
		start := 0
		if options.Cursor != "" {
			var err error
			start, err = strconv.Atoi(options.Cursor)
			if err != nil || start < 0 || start > len(found) {
				return nil, nil, errors.New("invalid test cursor")
			}
		}
		end := min(start+client.listPageSize, len(found))
		if end < len(found) {
			if client.emptyNextCursor {
				empty := ""
				return found[start:end], &empty, nil
			}
			next := strconv.Itoa(end)
			return found[start:end], &next, nil
		}
		return found[start:end], nil, nil
	}
	return found, nil, nil
}
func (client *testClient) ReadThreadMetadata(_ context.Context, threadID string, _ bool) (codex.ThreadMetadata, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.readError != nil {
		return codex.ThreadMetadata{}, client.readError
	}
	for _, thread := range client.threads {
		if thread.ID == threadID {
			return thread, nil
		}
	}
	return codex.ThreadMetadata{}, &codex.ThreadNotFoundError{ThreadID: threadID}
}
func (client *testClient) ResumeThreadWithSettings(_ context.Context, thread, _ string, _ map[string]string, settings codex.ThreadSettings) (string, error) {
	client.resumes = append(client.resumes, settings)
	return thread, nil
}
func (client *testClient) ForkThread(_ context.Context, source, cwd string, environment map[string]string, settings codex.ThreadSettings) (string, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.next++
	client.forkEnvs = append(client.forkEnvs, environment)
	client.forkSettings = append(client.forkSettings, settings)
	id := fmt.Sprintf("thread-%d", client.next)
	client.threads = append(client.threads, codex.ThreadMetadata{ID: id, Cwd: cwd, ForkedFromID: source})
	if client.lostFork {
		return "", errors.New("lost response")
	}
	return id, nil
}
func (client *testClient) ArchiveThread(_ context.Context, thread string) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.archived[thread] {
		return errors.New("thread is already archived")
	}
	if client.archived == nil {
		client.archived = make(map[string]bool)
	}
	client.archived[thread] = true
	client.archives = append(client.archives, thread)
	if client.archiveAfter != nil {
		client.archiveAfter()
	}
	return client.archiveResultError
}
func (client *testClient) RequireThreadIdle(_ context.Context, thread, cwd string) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.idleChecks = append(client.idleChecks, thread+":"+cwd)
	return client.idleError
}
func (client *testClient) RequireSubmissionAttemptsResolved(_ context.Context, thread string) error {
	client.attemptChecks = append(client.attemptChecks, thread)
	if client.unresolvedAttempts[thread] {
		return errors.New("unresolved message attempt")
	}
	return nil
}
func (client *testClient) VerifyThread(_ context.Context, thread, cwd string) error {
	client.verified = append(client.verified, thread+":"+cwd)
	return client.verifyError
}
func (client *testClient) ActiveTurnID(_ context.Context, _ string) (string, error) {
	return client.activeTurn, nil
}
func (client *testClient) Interrupt(_ context.Context, thread string) error {
	client.interrupts = append(client.interrupts, thread)
	client.activeTurn = ""
	return nil
}
func (client *testClient) RequireThreadTurnsIdle(_ context.Context, _ string) error {
	client.turnChecks++
	if client.busyChecks > 0 {
		client.busyChecks--
		return errors.New("turn still active")
	}
	return nil
}
func (client *testClient) UnarchiveThread(context.Context, string) (codex.ThreadMetadata, error) {
	return codex.ThreadMetadata{}, nil
}
func (client *testClient) SetName(context.Context, string, string) error {
	if client.nameFailure {
		client.nameFailure = false
		return errors.New("temporary name failure")
	}
	return nil
}
func (client *testClient) HeadlessThreadMaterialized(_ context.Context, thread, _, _ string) (bool, error) {
	for _, item := range client.threads {
		if item.ID == thread {
			return !client.unmaterialized[thread], nil
		}
	}
	return false, errors.New("thread not found")
}
func (client *testClient) ForkedHeadlessThreadMaterialized(_ context.Context, thread, _, _ string) (bool, error) {
	return !client.unmaterialized[thread], nil
}
func (client *testClient) BootstrapHeadlessThread(_ context.Context, thread, _, _, marker string) error {
	client.bootstrapCalls = append(client.bootstrapCalls, thread+":"+marker)
	if client.bootstrapError != nil {
		return client.bootstrapError
	}
	delete(client.unmaterialized, thread)
	return nil
}
func (client *testClient) BootstrapForkedHeadlessThread(ctx context.Context, thread, cwd, source, marker string) error {
	return client.BootstrapHeadlessThread(ctx, thread, cwd, source, marker)
}
func (client *testClient) VerifyHeadlessBootstrap(_ context.Context, thread, _, _, _ string) error {
	if client.unmaterialized[thread] {
		return errors.New("bootstrap marker is absent")
	}
	return nil
}
func (client *testClient) VerifyForkedHeadlessBootstrap(ctx context.Context, thread, cwd, source, marker string) error {
	return client.VerifyHeadlessBootstrap(ctx, thread, cwd, source, marker)
}
func (client *testClient) DeleteFreshHeadlessThread(_ context.Context, thread, _, _ string) error {
	if client.deleteError != nil {
		return client.deleteError
	}
	if !client.unmaterialized[thread] {
		return errors.New("thread has materialized history")
	}
	for index, item := range client.threads {
		if item.ID == thread {
			client.deleted = append(client.deleted, thread)
			client.threads = append(client.threads[:index], client.threads[index+1:]...)
			delete(client.unmaterialized, thread)
			if client.deleteResponseLost {
				return errors.New("lost thread/delete response")
			}
			return nil
		}
	}
	return &codex.ThreadNotFoundError{ThreadID: thread}
}
func (client *testClient) SendWithOptions(_ context.Context, thread, text, messageID string, _ string, options codex.TurnOptions) (codex.SendReceipt, error) {
	if client.sendEntered != nil {
		close(client.sendEntered)
		<-client.releaseSend
	}
	client.sends = append(client.sends, thread+"\n"+text)
	client.messageIDs = append(client.messageIDs, messageID)
	client.options = append(client.options, options)
	client.ledgerEvents = append(client.ledgerEvents, "send")
	return codex.SendReceipt{TurnID: "turn"}, nil
}

func (client *testClient) CompactAcceptedSendOptions(prefix string) error {
	client.compactPrefixes = append(client.compactPrefixes, prefix)
	client.ledgerEvents = append(client.ledgerEvents, "compact")
	return client.compactError
}

func (client *testClient) ClearRetiredThreadAttempts(threadID string) error {
	client.clearedThreads = append(client.clearedThreads, threadID)
	client.ledgerEvents = append(client.ledgerEvents, "clear")
	return client.clearError
}

func TestStoreUpdateRejectsOversizeForkSnapshotWithoutLosingRoster(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	makeSnapshot := func(target int) *ForkSourceSnapshot {
		snapshot := &ForkSourceSnapshot{Workspace: workspace, Slug: "source", RootThreadID: "root-source",
			Revision: 1, Digest: strings.Repeat("a", 64), Members: make([]ForkSourceMember, 70)}
		for i := range snapshot.Members {
			snapshot.Members[i] = ForkSourceMember{Address: fmt.Sprintf("analyst%d", i), Role: "analyst",
				Index: uint64(i), Thread: fmt.Sprintf("thread-%d", i), Model: "model", Effort: "high",
				Behavior: "general", Purpose: "general", Instructions: strings.Repeat("x", 4096),
				Access: "read_only", State: "ready"}
		}
		now := time.Now().UTC()
		candidate := Roster{Schema: schema, Workspace: workspace, Slug: "target", RootThreadID: "root-target",
			Revision: 2, CreatedAt: now, UpdatedAt: now, ForkSource: snapshot, Members: []Member{}}
		encoded, err := json.MarshalIndent(candidate, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		trim := len(encoded) + 1 - target
		if trim <= 0 {
			t.Fatal("fork snapshot fixture did not reach the roster size boundary")
		}
		for i := range snapshot.Members {
			available := len(snapshot.Members[i].Instructions) - 1
			if available > trim {
				available = trim
			}
			snapshot.Members[i].Instructions = snapshot.Members[i].Instructions[:len(snapshot.Members[i].Instructions)-available]
			trim -= available
			if trim == 0 {
				break
			}
		}
		if trim != 0 {
			t.Fatalf("fork snapshot fixture could not reach %d bytes", target)
		}
		return snapshot
	}
	_, err = store.Update(context.Background(), "target", "root-target", true, func(roster *Roster) error {
		roster.Members = []Member{}
		roster.ForkSource = makeSnapshot(maxRosterBytes - 128)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(store.path("target"))
	if err != nil || before.Size() > maxRosterBytes || before.Size() < maxRosterBytes-256 {
		t.Fatalf("near-boundary roster size = %v, %v", before, err)
	}
	_, err = store.Update(context.Background(), "target", "root-target", false, func(roster *Roster) error {
		roster.ForkSource = makeSnapshot(maxRosterBytes + 128)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds 256 KiB") {
		t.Fatalf("oversize fork snapshot update = %v", err)
	}
	after, err := os.Stat(store.path("target"))
	if err != nil || after.Size() != before.Size() {
		t.Fatalf("rejected update changed roster file: before %v, after %v, error %v", before, after, err)
	}
	if _, err := store.Load("target", "root-target"); err != nil {
		t.Fatalf("rejected update left unreadable roster: %v", err)
	}
}

func TestRosterRejectsMemberThreadSharedWithRootOrAnotherMember(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	roster := Roster{Schema: schema, Workspace: workspace, Slug: "one", RootThreadID: "root-one",
		Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Members: []Member{
			{Address: "architect0", Role: "architect", Thread: "thread-one", State: "ready", AddedAt: time.Now().UTC()},
			{Address: "reviewer0", Role: "reviewer", Thread: "thread-two", State: "ready", AddedAt: time.Now().UTC()},
		},
	}
	if err := roster.Validate(workspace, "one", "root-one"); err != nil {
		t.Fatal(err)
	}
	roster.Members[1].Thread = "root-one"
	if err := roster.Validate(workspace, "one", "root-one"); err == nil || !strings.Contains(err.Error(), "root thread") {
		t.Fatalf("root thread collision = %v", err)
	}
	roster.Members[1].Thread = "thread-one"
	if err := roster.Validate(workspace, "one", "root-one"); err == nil || !strings.Contains(err.Error(), "duplicate member thread") {
		t.Fatalf("member thread collision = %v", err)
	}
}

func TestAddSkipsAddressUsedByAnotherRoleEvenWhenRemoved(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = store.Update(context.Background(), "one", "root-one", true, func(roster *Roster) error {
		for i := 0; i < 10; i++ {
			roster.Members = append(roster.Members, Member{Address: fmt.Sprintf("analyst%d", i), Role: "analyst",
				Index: uint64(i), Behavior: "general", Purpose: "general", Instructions: "Analyze.",
				Access: "read_only", State: "removed", AddedAt: now})
		}
		roster.Members = append(roster.Members, Member{Address: "analyst10", Role: "analyst1", Index: 0,
			Behavior: "general", Purpose: "general", Instructions: "Analyze.", Access: "read_only",
			State: "removed", AddedAt: now})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog := &agentteams.Catalog{CatalogDigest: fmt.Sprintf("%064x", 1), Teams: map[string]agentteams.Team{
		"custom": {Roles: map[string]agentteams.Role{
			"analyst":  {Behavior: "general", Purpose: "general", Instructions: "Analyze.", Access: "read_only"},
			"analyst1": {Behavior: "general", Purpose: "general", Instructions: "Analyze.", Access: "read_only"},
		}},
	}}
	service := Service{Store: store, Client: &testClient{}, Workspace: workspace, Catalog: catalog}
	member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil,
		"analyst", "model", "high")
	if err != nil || member.Address != "analyst11" || member.Index != 11 {
		t.Fatalf("member after cross-role collision = %#v, %v", member, err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil || len(roster.Members) != 12 || roster.Members[10].Address != "analyst10" ||
		roster.Members[11].Address != "analyst11" {
		t.Fatalf("retained cross-role addresses = %#v, %v", roster, err)
	}
}

func TestAssignmentUsesConfiguredMemberSettings(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	if _, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "implementer", "gpt-5.6-sol", "xhigh"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Assign(context.Background(), "one", "root-one", "lead", "implementer0", "work", "", "", "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if len(client.options) != 1 || client.options[0].Model != "gpt-5.6-sol" || client.options[0].ReasoningEffort != "xhigh" {
		t.Fatalf("assignment settings = %#v", client.options)
	}
	bound := client.options[0].ThreadPolicy
	if client.starts[0].Policy.Sandbox != "workspace-write" || client.starts[0].Policy.MCPServer != nil ||
		bound.Sandbox != client.starts[0].Policy.Sandbox || bound.DeveloperInstructions != client.starts[0].Policy.DeveloperInstructions ||
		bound.MCPServer == nil || bound.MCPServer.Tool != "report_to_lead" {
		t.Fatalf("member report policy = starts %#v, assignment %#v", client.starts, client.options)
	}
	if got, want := bound.MCPServer.Args, []string{
		"team-mcp", "--user-state-root", store.stateRoot, "--workspace", workspace,
		"--session-slug", "one", "--root-thread-id", "root-one", "--member-address", "implementer0", "--member-thread-id", "thread-1",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("member report helper args = %#v, want %#v", got, want)
	}
	if len(client.sends) != 1 || !strings.Contains(client.sends[0], "report_to_lead") ||
		!strings.Contains(client.sends[0], "new message_id for each message") || strings.Contains(client.sends[0], "dev-session team assign") {
		t.Fatalf("assignment report instruction = %#v", client.sends)
	}
}

func TestAssignmentCompactsAcceptedTeamOptionsBeforeEachSend(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	if _, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil,
		"implementer", "gpt-6-sol", "high"); err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef0123456789abcdef"
	client.compactError = errors.New("ledger unavailable")
	if _, err := service.Assign(context.Background(), "one", "root-one", "lead", "implementer0", "work", "", "", id); err == nil ||
		!strings.Contains(err.Error(), "compact accepted team assignments") {
		t.Fatalf("compaction failure = %v", err)
	}
	if len(client.sends) != 0 {
		t.Fatalf("assignment sent before compaction: %#v", client.sends)
	}
	client.compactError = nil
	for i := 0; i < 2; i++ {
		if _, err := service.Assign(context.Background(), "one", "root-one", "lead", "implementer0", "work", "", "", id); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(client.compactPrefixes, []string{"team:", "team:", "team:"}) ||
		!reflect.DeepEqual(client.ledgerEvents, []string{"compact", "compact", "send", "compact", "send"}) ||
		!reflect.DeepEqual(client.messageIDs, []string{id, id}) || !reflect.DeepEqual(client.options[0], client.options[1]) {
		t.Fatalf("retry order and identity: prefixes=%#v events=%#v IDs=%#v options=%#v",
			client.compactPrefixes, client.ledgerEvents, client.messageIDs, client.options)
	}
}

func TestUpdateAccessRetainsThreadAndRebindsNextTurn(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	before, err := store.Update(context.Background(), "one", "root-one", true, func(roster *Roster) error {
		roster.Members = append(roster.Members, Member{Address: "architect0", Role: "architect", Index: 0,
			Thread: "thread-1", Model: "gpt-6-sol", Effort: "xhigh", Behavior: "designer", Purpose: "design",
			Instructions: legacyBehaviorInstructions["designer"], Access: "read_only",
			PolicyCatalogDigest: strings.Repeat("a", 64), State: "ready",
			AddedAt: time.Now().UTC()})
		roster.Members = append(roster.Members, Member{Address: "reviewer0", Role: "reviewer", Index: 0,
			Thread: "thread-2", Model: "gpt-6-sol", Effort: "xhigh", Behavior: "reviewer", Purpose: "review",
			Instructions: legacyBehaviorInstructions["reviewer"], Access: "read_only", State: "ready",
			AddedAt: time.Now().UTC()})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateAccess(context.Background(), "one", "root-one", "reviewer0", "workspace_write"); err == nil ||
		!strings.Contains(err.Error(), "incompatible access") {
		t.Fatalf("reviewer write access = %v", err)
	}
	updated, err := service.UpdateAccess(context.Background(), "one", "root-one", "architect0", "workspace_write")
	if err != nil {
		t.Fatal(err)
	}
	member := updated.Members[0]
	if updated.Revision != before.Revision+1 || member.Thread != "thread-1" || member.Address != "architect0" ||
		member.Model != "gpt-6-sol" || member.Effort != "xhigh" || member.Access != "workspace_write" ||
		member.Instructions != legacyBehaviorInstructions["designer"] || member.PolicyCatalogDigest != "" ||
		updated.Members[1].Access != "read_only" {
		t.Fatalf("updated roster = %#v", updated)
	}
	if want := "thread-1:" + filepath.Join(workspace, "work", "one"); !reflect.DeepEqual(client.idleChecks, []string{want}) {
		t.Fatalf("idle checks = %#v, want %q", client.idleChecks, want)
	}
	if _, err := service.Assign(context.Background(), "one", "root-one", "lead", "architect0", "draft design", "", "", "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if len(client.options) != 1 || client.options[0].ThreadPolicy.Sandbox != "workspace-write" ||
		!strings.Contains(client.options[0].ThreadPolicy.DeveloperInstructions, legacyBehaviorInstructions["designer"]) {
		t.Fatalf("next turn policy = %#v", client.options)
	}
	noChange, err := service.UpdateAccess(context.Background(), "one", "root-one", "architect0", "workspace_write")
	if err != nil || noChange.Revision != updated.Revision {
		t.Fatalf("unchanged access updated roster: %#v, %v", noChange, err)
	}
	client.idleError = errors.New("turn in progress")
	if _, err := service.UpdateAccess(context.Background(), "one", "root-one", "architect0", "read_only"); err == nil ||
		!strings.Contains(err.Error(), "not idle") {
		t.Fatalf("busy access update = %v", err)
	}
	retained, err := store.Load("one", "root-one")
	if err != nil || retained.Revision != updated.Revision {
		t.Fatalf("busy access update changed roster: %#v, %v", retained, err)
	}
	if _, err := service.UpdateAccess(context.Background(), "one", "root-one", "architect0", "invalid"); err == nil ||
		!strings.Contains(err.Error(), "read_only or workspace_write") {
		t.Fatalf("invalid access = %v", err)
	}
}

func TestMemberReportBindingChangesWithExactThreadIdentity(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Workspace: workspace}
	member := Member{Role: "reviewer", Behavior: "reviewer", Access: "read_only", Address: "reviewer0", Thread: "member-one"}
	first, err := service.memberTurnPolicy("one", "root-one", member)
	if err != nil || first.MCPServer == nil {
		t.Fatalf("first report binding = %#v, %v", first, err)
	}
	member.Thread = "member-two"
	second, err := service.memberTurnPolicy("one", "root-one", member)
	if err != nil || second.MCPServer == nil || first.MCPServer.Name == second.MCPServer.Name {
		t.Fatalf("thread replacement reused report binding: first %#v, second %#v, %v", first.MCPServer, second.MCPServer, err)
	}
	if !filepath.IsAbs(first.MCPServer.Command) || filepath.Clean(first.MCPServer.Command) != first.MCPServer.Command ||
		first.MCPServer.Tool != "report_to_lead" {
		t.Fatalf("invalid helper launch: %#v", first.MCPServer)
	}
	member.Thread = ""
	if _, err := service.memberTurnPolicy("one", "root-one", member); err == nil {
		t.Fatal("member report helper accepted an unbound thread")
	}
}

func TestAssignmentReplacesOnlyUniqueUnmaterializedMember(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{unmaterialized: make(map[string]bool)}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	member, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "architect", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	client.unmaterialized[member.Thread] = true
	messageID := "0123456789abcdef0123456789abcdef"
	if _, err := service.Assign(context.Background(), "one", "root-one", "lead", member.Address, "check", "", "", messageID); err != nil {
		t.Fatal(err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Members) != 1 || roster.Members[0].Address != member.Address ||
		roster.Members[0].ProjectID != member.ProjectID || roster.Members[0].Thread == member.Thread ||
		roster.Members[0].State != "ready" || len(client.deleted) != 1 || client.deleted[0] != member.Thread ||
		len(client.messageIDs) != 1 || client.messageIDs[0] != messageID ||
		len(client.bootstrapCalls) != 2 {
		t.Fatalf("recovered member = %#v, deleted = %#v, messages = %#v, bootstraps = %#v", roster.Members, client.deleted, client.messageIDs, client.bootstrapCalls)
	}
}

func TestAssignmentReplacesFreshMemberHiddenFromThreadList(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{unmaterialized: make(map[string]bool)}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	member, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "architect", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	client.unmaterialized[member.Thread] = true
	client.hideThreads = true // A no-rollout thread can be absent from thread/list.
	if _, err := service.Assign(context.Background(), "one", "root-one", "lead", member.Address,
		"check", "", "", "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	if roster.Members[0].Thread == member.Thread || len(client.deleted) != 1 ||
		client.deleted[0] != member.Thread || len(client.sends) != 1 {
		t.Fatalf("hidden fresh thread recovery = %#v, deleted = %#v, sends = %#v", roster.Members, client.deleted, client.sends)
	}
}

func TestAssignmentRefusesAmbiguousUnmaterializedMember(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{unmaterialized: make(map[string]bool)}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	member, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "architect", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	client.unmaterialized[member.Thread] = true
	client.threads = append(client.threads, codex.ThreadMetadata{ID: "other-thread", Cwd: cwd, ProjectID: &member.ProjectID})
	_, err = service.Assign(context.Background(), "one", "root-one", "lead", member.Address, "check", "", "", "0123456789abcdef0123456789abcdef")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || len(client.deleted) != 0 || len(client.sends) != 0 {
		t.Fatalf("ambiguous recovery = %v, deleted = %#v, sends = %#v", err, client.deleted, client.sends)
	}
}

func TestRetryCreatingReplacesUnmaterializedBootstrap(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{nameFailure: true, unmaterialized: make(map[string]bool)}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	if _, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "architect", "gpt-6-sol", "xhigh"); err == nil {
		t.Fatal("expected interrupted member creation")
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	old := roster.Members[0]
	if old.State != "creating" || !old.BootstrapAttempted {
		t.Fatalf("interrupted bootstrap = %#v", old)
	}
	client.unmaterialized[old.Thread] = true
	member, err := service.RetryCreating(context.Background(), "one", "root-one", cwd, nil, old.Address)
	if err != nil {
		t.Fatal(err)
	}
	if member.Thread == old.Thread || member.Address != old.Address || member.ProjectID != old.ProjectID ||
		member.State != "ready" || len(client.deleted) != 1 || client.deleted[0] != old.Thread {
		t.Fatalf("retry recovery = %#v, deleted = %#v", member, client.deleted)
	}
}

func TestArchiveReplacesUnmaterializedMemberBeforeArchiving(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{unmaterialized: make(map[string]bool)}
	service := Service{Store: store, Client: client, Workspace: workspace}
	member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "architect", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	client.unmaterialized[member.Thread] = true
	if err := service.RequireIdleAll(context.Background(), "one", "root-one"); err != nil {
		t.Fatal(err)
	}
	if err := service.ArchiveAll(context.Background(), "one", "root-one"); err != nil {
		t.Fatal(err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != member.Thread ||
		len(client.archives) != 1 || client.archives[0] == member.Thread ||
		roster.Members[0].Thread != client.archives[0] || roster.Members[0].State != "archived" {
		t.Fatalf("archive recovery = %#v, deleted = %#v, archives = %#v", roster.Members, client.deleted, client.archives)
	}
}

func TestRemoveDeletesOnlyEmptyUnmaterializedMember(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{unmaterialized: make(map[string]bool)}
	service := Service{Store: store, Client: client, Workspace: workspace}
	member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "architect", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	client.unmaterialized[member.Thread] = true
	if err := service.Remove(context.Background(), "one", "root-one", member.Address); err != nil {
		t.Fatal(err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != member.Thread || len(client.archives) != 0 ||
		roster.Members[0].State != "removed" || roster.Members[0].Thread != member.Thread ||
		!reflect.DeepEqual(client.clearedThreads, []string{member.Thread}) {
		t.Fatalf("remove recovery = %#v, deleted = %#v, archives = %#v", roster.Members, client.deleted, client.archives)
	}
	if err := service.Remove(context.Background(), "one", "root-one", member.Address); err != nil ||
		!reflect.DeepEqual(client.clearedThreads, []string{member.Thread, member.Thread}) {
		t.Fatalf("deleted member cleanup retry = %v, cleared %#v", err, client.clearedThreads)
	}
}

func TestMemberRemovalRetriesCleanupAfterExactArchive(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{clearError: errors.New("ledger write failed")}
	service := Service{Store: store, Client: client, Workspace: workspace}
	member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil,
		"implementer", "gpt-6-sol", "high")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Remove(context.Background(), "one", "root-one", member.Address); err == nil ||
		!strings.Contains(err.Error(), "clear retired") {
		t.Fatalf("cleanup failure = %v", err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil || roster.Members[0].State != "ready" || !client.archived[member.Thread] ||
		!reflect.DeepEqual(client.clearedThreads, []string{member.Thread}) {
		t.Fatalf("interrupted removal = %#v, error %v, archived %#v, cleared %#v",
			roster, err, client.archived, client.clearedThreads)
	}
	client.clearError = nil
	if err := service.Remove(context.Background(), "one", "root-one", member.Address); err != nil {
		t.Fatal(err)
	}
	roster, err = store.Load("one", "root-one")
	if err != nil || roster.Members[0].State != "removed" || len(client.archives) != 1 ||
		!reflect.DeepEqual(client.clearedThreads, []string{member.Thread, member.Thread}) {
		t.Fatalf("removal retry = %#v, error %v, archives %#v, cleared %#v",
			roster, err, client.archives, client.clearedThreads)
	}
	if err := service.Remove(context.Background(), "one", "root-one", member.Address); err != nil ||
		!reflect.DeepEqual(client.clearedThreads, []string{member.Thread, member.Thread, member.Thread}) {
		t.Fatalf("terminal removal retry = %v, cleared %#v", err, client.clearedThreads)
	}
	client.archived[member.Thread] = false
	if err := service.Remove(context.Background(), "one", "root-one", member.Address); err == nil ||
		len(client.clearedThreads) != 3 {
		t.Fatalf("active retired-thread identity = %v, cleared %#v", err, client.clearedThreads)
	}
}

func TestMemberRemovalDoesNotClearAttemptsWithoutArchiveProof(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil,
		"implementer", "gpt-6-sol", "high")
	if err != nil {
		t.Fatal(err)
	}
	client.hideThreads = true
	if err := service.Remove(context.Background(), "one", "root-one", member.Address); err == nil ||
		!strings.Contains(err.Error(), "not archived") {
		t.Fatalf("missing archive proof = %v", err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil || roster.Members[0].State != "ready" || len(client.clearedThreads) != 0 {
		t.Fatalf("unproved removal = %#v, error %v, cleared %#v", roster, err, client.clearedThreads)
	}
}

func TestAssignmentCompletesReservedDeletionAfterRosterWriteWasLost(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "architect", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(context.Background(), "one", "root-one", false, func(roster *Roster) error {
		roster.Members[0].RetireIntent = "replace"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	client.threads = nil // thread/delete committed before the roster replacement write.
	messageID := "0123456789abcdef0123456789abcdef"
	if _, err := service.Assign(context.Background(), "one", "root-one", "lead", member.Address, "check", "", "", messageID); err != nil {
		t.Fatal(err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	if roster.Members[0].Thread == member.Thread || roster.Members[0].RetireIntent != "" ||
		roster.Members[0].State != "ready" || len(client.messageIDs) != 1 || client.messageIDs[0] != messageID {
		t.Fatalf("lost roster write recovery = %#v, messages = %#v", roster.Members, client.messageIDs)
	}
}

func TestRemoveCompletesReservedDeletionAfterRosterWriteWasLost(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "architect", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(context.Background(), "one", "root-one", false, func(roster *Roster) error {
		roster.Members[0].RetireIntent = "remove"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	client.threads = nil
	if err := service.Remove(context.Background(), "one", "root-one", member.Address); err != nil {
		t.Fatal(err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	if roster.Members[0].State != "removed" || roster.Members[0].RetireIntent != "" ||
		roster.Members[0].Thread != member.Thread {
		t.Fatalf("lost removal write recovery = %#v", roster.Members)
	}
}

func TestRemoveReconcilesLostDeleteResponseByExactThreadRead(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{unmaterialized: make(map[string]bool)}
	service := Service{Store: store, Client: client, Workspace: workspace}
	member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil,
		"architect", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	client.unmaterialized[member.Thread] = true
	client.hideThreads = true
	client.deleteResponseLost = true
	if err := service.Remove(context.Background(), "one", "root-one", member.Address); err != nil {
		t.Fatal(err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	if roster.Members[0].State != "removed" || len(client.deleted) != 1 || client.deleted[0] != member.Thread {
		t.Fatalf("lost delete response = %#v, deleted = %#v", roster.Members, client.deleted)
	}
}

func TestRemoveAcceptsConfirmedDeleteWhenReadReportsNotLoaded(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{unmaterialized: make(map[string]bool)}
	service := Service{Store: store, Client: client, Workspace: workspace}
	member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil,
		"architect", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	client.unmaterialized[member.Thread] = true
	client.hideThreads = true
	client.readError = errors.New("Codex RPC -32600: thread not loaded: " + member.Thread)
	if err := service.Remove(context.Background(), "one", "root-one", member.Address); err != nil {
		t.Fatal(err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	if roster.Members[0].State != "removed" || len(client.deleted) != 1 || client.deleted[0] != member.Thread {
		t.Fatalf("confirmed delete = %#v, deleted = %#v", roster.Members, client.deleted)
	}
}

func TestRemoveDoesNotAcceptNotLoadedAfterLostDeleteResponse(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{unmaterialized: make(map[string]bool)}
	service := Service{Store: store, Client: client, Workspace: workspace}
	member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil,
		"architect", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	client.unmaterialized[member.Thread] = true
	client.hideThreads = true
	client.deleteResponseLost = true
	client.readError = errors.New("Codex RPC -32600: thread not loaded: " + member.Thread)
	if err := service.Remove(context.Background(), "one", "root-one", member.Address); err == nil ||
		!strings.Contains(err.Error(), "thread not loaded") {
		t.Fatalf("uncertain delete with not-loaded read = %v", err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	if roster.Members[0].State != "ready" || roster.Members[0].RetireIntent != "remove" {
		t.Fatalf("retained uncertain member = %#v", roster.Members)
	}
}

func TestRemoveRefusesWhenExactThreadStillExists(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{unmaterialized: make(map[string]bool)}
	service := Service{Store: store, Client: client, Workspace: workspace}
	member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil,
		"architect", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	client.unmaterialized[member.Thread] = true
	client.hideThreads = true
	client.deleteError = errors.New("lost response before deletion")
	if err := service.Remove(context.Background(), "one", "root-one", member.Address); err == nil ||
		!strings.Contains(err.Error(), "retired member thread still exists") {
		t.Fatalf("removal after failed deletion = %v", err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	if roster.Members[0].State != "ready" || len(client.deleted) != 0 {
		t.Fatalf("retained member = %#v, deleted = %#v", roster.Members, client.deleted)
	}
}

func TestRetryCreatingKeepsThreadWhenBootstrapMaterializesAfterRetireReservation(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{nameFailure: true, unmaterialized: make(map[string]bool)}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	if _, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "architect", "gpt-6-sol", "xhigh"); err == nil {
		t.Fatal("expected interrupted creation")
	}
	roster, err := store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	old := roster.Members[0]
	if _, err := store.Update(context.Background(), "one", "root-one", false, func(updated *Roster) error {
		updated.Members[0].RetireIntent = "replace"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The inject response was delayed; its exact marker appeared after the
	// retirement reservation but before deletion.
	member, err := service.RetryCreating(context.Background(), "one", "root-one", cwd, nil, old.Address)
	if err != nil {
		t.Fatal(err)
	}
	if member.Thread != old.Thread || member.State != "ready" || member.RetireIntent != "" ||
		len(client.deleted) != 0 || client.next != 1 {
		t.Fatalf("late bootstrap recovery = %#v, deleted = %#v", member, client.deleted)
	}
}

func TestMemberLifecycleRefusesBusyThreadBeforeArchiving(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	if _, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "implementer", "gpt-6-sol", "high"); err != nil {
		t.Fatal(err)
	}
	client.idleError = errors.New("pending request")
	if err := service.RequireIdleAll(context.Background(), "one", "root-one"); err == nil || !strings.Contains(err.Error(), "implementer0") {
		t.Fatalf("idle check = %v", err)
	}
	if err := service.ArchiveAll(context.Background(), "one", "root-one"); err == nil {
		t.Fatal("busy member was archived")
	}
	if len(client.archives) != 0 {
		t.Fatalf("archived busy threads: %#v", client.archives)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil || roster.Members[0].State != "ready" {
		t.Fatalf("busy roster = %#v, %v", roster, err)
	}
	client.idleError = nil
	if err := service.ArchiveAll(context.Background(), "one", "root-one"); err != nil {
		t.Fatal(err)
	}
	if len(client.archives) != 1 {
		t.Fatalf("archives = %#v", client.archives)
	}
}

func TestMemberArchiveRecoversWhenRosterUpdateWasLost(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "ordinary"
		if force {
			name = "forced"
		}
		t.Run(name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "workspace")
			store, err := NewStore(t.TempDir(), workspace)
			if err != nil {
				t.Fatal(err)
			}
			client := &testClient{listPageSize: 1}
			service := Service{Store: store, Client: client, Workspace: workspace}
			cwd := filepath.Join(workspace, "work", "one")
			if _, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "implementer", "gpt-6-sol", "high"); err != nil {
				t.Fatal(err)
			}
			// The unrelated archived thread fills the first page. A forked member
			// may have no project ID in App Server metadata.
			client.threads[0].ProjectID = nil
			client.threads = append([]codex.ThreadMetadata{{ID: "other", Cwd: cwd}}, client.threads...)
			client.archived = map[string]bool{"other": true}
			original, err := os.ReadFile(store.path("one"))
			if err != nil {
				t.Fatal(err)
			}
			client.archiveAfter = func() {
				if err := os.WriteFile(store.path("one"), []byte("{"), 0o600); err != nil {
					t.Errorf("simulate failed roster update: %v", err)
				}
			}
			archive := service.ArchiveAll
			if force {
				archive = service.RetireAll
			}
			if err := archive(context.Background(), "one", "root-one"); err == nil {
				t.Fatal("roster update unexpectedly succeeded")
			}
			if len(client.archives) != 1 || !client.archived["thread-1"] {
				t.Fatalf("App Server archive did not succeed: %#v", client.archives)
			}
			if err := os.WriteFile(store.path("one"), original, 0o600); err != nil {
				t.Fatal(err)
			}
			client.archiveAfter = nil
			client.idleError = errors.New("archived thread has no pending-request interface")
			if err := service.RequireIdleAll(context.Background(), "one", "root-one"); err != nil {
				t.Fatalf("already archived member should pass idle gate: %v", err)
			}
			idleChecks, verified, turnChecks := len(client.idleChecks), len(client.verified), client.turnChecks
			if err := archive(context.Background(), "one", "root-one"); err != nil {
				t.Fatalf("archive retry = %v", err)
			}
			if len(client.archives) != 1 || len(client.idleChecks) != idleChecks || len(client.verified) != verified || client.turnChecks != turnChecks {
				t.Fatalf("already archived thread was reprocessed: archives=%#v idle=%#v verified=%#v turnChecks=%d", client.archives, client.idleChecks, client.verified, client.turnChecks)
			}
			roster, err := store.Load("one", "root-one")
			if err != nil || roster.Members[0].State != "archived" {
				t.Fatalf("reconciled roster = %#v, %v", roster, err)
			}
		})
	}
}

func TestMemberArchiveReconcilesLostAppServerResponse(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{archiveResultError: errors.New("archive response lost")}
	service := Service{Store: store, Client: client, Workspace: workspace}
	if _, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "implementer", "gpt-6-sol", "high"); err != nil {
		t.Fatal(err)
	}
	if err := service.ArchiveAll(context.Background(), "one", "root-one"); err != nil {
		t.Fatalf("lost archive response was not reconciled: %v", err)
	}
	if len(client.archives) != 1 || !reflect.DeepEqual(client.clearedThreads, []string{"thread-1"}) {
		t.Fatalf("archive was repeated or cleanup missed: archives %#v, cleared %#v", client.archives, client.clearedThreads)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil || roster.Members[0].State != "archived" {
		t.Fatalf("reconciled roster = %#v, %v", roster, err)
	}
}

func TestMemberArchiveRetriesCleanupBeforeTerminalRosterState(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{clearError: errors.New("ledger write failed")}
	service := Service{Store: store, Client: client, Workspace: workspace}
	member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil,
		"implementer", "gpt-6-sol", "high")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ArchiveAll(context.Background(), "one", "root-one"); err == nil ||
		!strings.Contains(err.Error(), "clear retired") {
		t.Fatalf("cleanup failure = %v", err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil || roster.Members[0].State != "ready" || !client.archived[member.Thread] {
		t.Fatalf("interrupted archive = %#v, error %v", roster, err)
	}
	client.clearError = nil
	if err := service.ArchiveAll(context.Background(), "one", "root-one"); err != nil {
		t.Fatal(err)
	}
	roster, err = store.Load("one", "root-one")
	if err != nil || roster.Members[0].State != "archived" || len(client.archives) != 1 ||
		!reflect.DeepEqual(client.clearedThreads, []string{member.Thread, member.Thread}) {
		t.Fatalf("archive retry = %#v, error %v, archives %#v, cleared %#v",
			roster, err, client.archives, client.clearedThreads)
	}
	if err := service.ArchiveAll(context.Background(), "one", "root-one"); err != nil ||
		!reflect.DeepEqual(client.clearedThreads, []string{member.Thread, member.Thread, member.Thread}) {
		t.Fatalf("terminal archive retry = %v, cleared %#v", err, client.clearedThreads)
	}
	client.archived[member.Thread] = false
	if err := service.ArchiveAll(context.Background(), "one", "root-one"); err == nil ||
		len(client.clearedThreads) != 3 {
		t.Fatalf("active archived-thread identity = %v, cleared %#v", err, client.clearedThreads)
	}
}

func TestArchivedMemberKeepsUnresolvedAttemptsAcrossOrdinaryRetirement(t *testing.T) {
	cases := []struct {
		name       string
		startState string
		operation  string
		wantState  string
	}{
		{name: "archive ready", startState: "ready", operation: "archive", wantState: "archived"},
		{name: "remove ready", startState: "ready", operation: "remove", wantState: "removed"},
		{name: "archive terminal retry", startState: "archived", operation: "archive", wantState: "archived"},
		{name: "remove terminal retry", startState: "removed", operation: "remove", wantState: "removed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "workspace")
			store, err := NewStore(t.TempDir(), workspace)
			if err != nil {
				t.Fatal(err)
			}
			client := &testClient{unresolvedAttempts: make(map[string]bool)}
			service := Service{Store: store, Client: client, Workspace: workspace}
			member, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil,
				"implementer", "gpt-6-sol", "high")
			if err != nil {
				t.Fatal(err)
			}
			id := "0123456789abcdef0123456789abcdef"
			for i := 0; i < 2; i++ {
				if _, err := service.Assign(context.Background(), "one", "root-one", "lead", member.Address,
					"same assignment", "", "", id); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(client.messageIDs, []string{id, id}) || !reflect.DeepEqual(client.options[0], client.options[1]) {
				t.Fatalf("same-ID assignment changed: IDs %#v, options %#v", client.messageIDs, client.options)
			}
			if tc.startState != "ready" {
				if _, err := store.Update(context.Background(), "one", "root-one", false, func(roster *Roster) error {
					roster.Members[0].State = tc.startState
					if tc.startState == "removed" {
						now := time.Now().UTC()
						roster.Members[0].RemovedAt = &now
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			client.archived = map[string]bool{member.Thread: true}
			client.unresolvedAttempts[member.Thread] = true
			retire := func() error {
				if tc.operation == "remove" {
					return service.Remove(context.Background(), "one", "root-one", member.Address)
				}
				return service.ArchiveAll(context.Background(), "one", "root-one")
			}
			if err := retire(); err == nil || !strings.Contains(err.Error(), "unresolved message attempt") {
				t.Fatalf("unresolved retirement = %v", err)
			}
			roster, err := store.Load("one", "root-one")
			if err != nil || roster.Members[0].State != tc.startState || len(client.clearedThreads) != 0 ||
				len(client.archives) != 0 || len(client.attemptChecks) == 0 {
				t.Fatalf("unresolved attempts were lost: roster %#v, error %v, cleared %#v, archived %#v, checks %#v",
					roster, err, client.clearedThreads, client.archives, client.attemptChecks)
			}
			client.unresolvedAttempts[member.Thread] = false
			if err := retire(); err != nil {
				t.Fatal(err)
			}
			roster, err = store.Load("one", "root-one")
			if err != nil || roster.Members[0].State != tc.wantState ||
				!reflect.DeepEqual(client.clearedThreads, []string{member.Thread}) || len(client.archives) != 0 {
				t.Fatalf("resolved retirement = %#v, error %v, cleared %#v, archived %#v",
					roster, err, client.clearedThreads, client.archives)
			}
		})
	}
}

func TestArchivedMemberReconciliationRejectsWrongOrDuplicateIdentity(t *testing.T) {
	for _, wrongCwd := range []bool{false, true} {
		name := "duplicate"
		if wrongCwd {
			name = "wrong directory"
		}
		t.Run(name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "workspace")
			store, err := NewStore(t.TempDir(), workspace)
			if err != nil {
				t.Fatal(err)
			}
			client := &testClient{listPageSize: 1, ignoreListCwd: true}
			service := Service{Store: store, Client: client, Workspace: workspace}
			cwd := filepath.Join(workspace, "work", "one")
			if _, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "implementer", "gpt-6-sol", "high"); err != nil {
				t.Fatal(err)
			}
			client.archived = map[string]bool{"thread-1": true}
			if wrongCwd {
				client.threads[0].Cwd = filepath.Join(workspace, "work", "other")
			} else {
				client.threads = append(client.threads, client.threads[0])
			}
			if err := service.RequireIdleAll(context.Background(), "one", "root-one"); err == nil {
				t.Fatal("invalid archived identity passed the idle gate")
			}
			if err := service.RetireAll(context.Background(), "one", "root-one"); err == nil {
				t.Fatal("invalid archived identity passed forced retirement")
			}
			if len(client.archives) != 0 || len(client.interrupts) != 0 {
				t.Fatalf("invalid identity was touched: archives=%#v interrupts=%#v", client.archives, client.interrupts)
			}
		})
	}
}

func TestArchivedMemberReconciliationRejectsEmptyCursor(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{listPageSize: 1, emptyNextCursor: true}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	if _, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "implementer", "gpt-6-sol", "high"); err != nil {
		t.Fatal(err)
	}
	client.threads = append(client.threads, codex.ThreadMetadata{ID: "other", Cwd: cwd})
	client.archived = map[string]bool{"thread-1": true, "other": true}
	if err := service.RequireIdleAll(context.Background(), "one", "root-one"); err == nil || !strings.Contains(err.Error(), "empty cursor") {
		t.Fatalf("empty archived cursor = %v", err)
	}
}

func TestForcedMemberRetirementInterruptsAndWaitsBeforeArchiving(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{idleError: errors.New("pending request"), activeTurn: "turn-1", busyChecks: 2}
	service := Service{Store: store, Client: client, Workspace: workspace}
	if _, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "implementer", "gpt-6-sol", "high"); err != nil {
		t.Fatal(err)
	}
	if err := service.RetireAll(context.Background(), "one", "root-one"); err != nil {
		t.Fatal(err)
	}
	if len(client.interrupts) != 1 || client.interrupts[0] != "thread-1" || client.turnChecks != 3 || len(client.archives) != 1 || client.archives[0] != "thread-1" {
		t.Fatalf("retirement: interrupts=%#v checks=%d archives=%#v", client.interrupts, client.turnChecks, client.archives)
	}
	if len(client.idleChecks) != 0 || len(client.verified) != 1 || client.verified[0] != "thread-1:"+filepath.Join(workspace, "work", "one") {
		t.Fatalf("forced retirement identity checks: idle=%#v verified=%#v", client.idleChecks, client.verified)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil || roster.Members[0].State != "archived" {
		t.Fatalf("retired roster = %#v, %v", roster, err)
	}
	if err := service.RetireAll(context.Background(), "one", "root-one"); err != nil || len(client.archives) != 1 {
		t.Fatalf("retirement retry = %v, archives %#v", err, client.archives)
	}
}

func TestForcedMemberRetirementStopsAtContextDeadline(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{activeTurn: "turn-1", busyChecks: 100}
	service := Service{Store: store, Client: client, Workspace: workspace}
	if _, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "implementer", "gpt-6-sol", "high"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := service.RetireAll(ctx, "one", "root-one"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retirement deadline = %v", err)
	}
	if len(client.interrupts) != 1 || len(client.archives) != 0 {
		t.Fatalf("retirement after deadline: interrupts=%#v archives=%#v", client.interrupts, client.archives)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil || roster.Members[0].State != "ready" {
		t.Fatalf("unretired roster = %#v, %v", roster, err)
	}
}

func TestForcedMemberRetirementArchivesConfirmedCreatingThread(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{nameFailure: true}
	service := Service{Store: store, Client: client, Workspace: workspace}
	if _, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "implementer", "gpt-6-sol", "high"); err == nil {
		t.Fatal("member creation should stop after recording the confirmed thread")
	}
	roster, err := store.Load("one", "root-one")
	if err != nil || roster.Members[0].State != "creating" || roster.Members[0].Thread != "thread-1" {
		t.Fatalf("confirmed creating thread = %#v, %v", roster, err)
	}
	if err := service.RetireAll(context.Background(), "one", "root-one"); err != nil {
		t.Fatal(err)
	}
	if len(client.archives) != 1 || client.archives[0] != "thread-1" {
		t.Fatalf("archives = %#v", client.archives)
	}
}

func TestForcedMemberRetirementRejectsThreadFromAnotherDirectory(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{activeTurn: "turn-1", verifyError: errors.New("Codex thread does not match the trusted working directory")}
	service := Service{Store: store, Client: client, Workspace: workspace}
	if _, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "implementer", "gpt-6-sol", "high"); err != nil {
		t.Fatal(err)
	}
	if err := service.RetireAll(context.Background(), "one", "root-one"); err == nil || !strings.Contains(err.Error(), "verify member implementer0 thread") {
		t.Fatalf("wrong-directory retirement = %v", err)
	}
	if len(client.interrupts) != 0 || len(client.archives) != 0 {
		t.Fatalf("wrong-directory thread was touched: interrupts=%#v archives=%#v", client.interrupts, client.archives)
	}
}

func TestAssignmentSerializesWithMemberRemoval(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	if _, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "implementer", "gpt-6-sol", "high"); err != nil {
		t.Fatal(err)
	}
	client.sendEntered, client.releaseSend = make(chan struct{}), make(chan struct{})
	assigned := make(chan error, 1)
	go func() {
		_, err := service.Assign(context.Background(), "one", "root-one", "lead", "implementer0", "work", "", "", "0123456789abcdef0123456789abcdef")
		assigned <- err
	}()
	<-client.sendEntered
	removed := make(chan error, 1)
	go func() { removed <- service.Remove(context.Background(), "one", "root-one", "implementer0") }()
	select {
	case err := <-removed:
		t.Fatalf("removal raced active assignment: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(client.releaseSend)
	if err := <-assigned; err != nil {
		t.Fatal(err)
	}
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	if len(client.archives) != 1 {
		t.Fatalf("archives = %#v", client.archives)
	}
}

func TestRosterAddressesAndAssignmentsAreSessionScoped(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	environment := map[string]string{"DEV_SESSION_SLUG": "one"}
	first, err := service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), environment, "implementer", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Add(context.Background(), "two", "root-two", filepath.Join(workspace, "work", "two"), environment, "implementer", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Address != "implementer0" || second.Address != "implementer0" || first.Thread == second.Thread {
		t.Fatalf("session-scoped member identities = %#v, %#v", first, second)
	}
	if _, err := service.Assign(context.Background(), "one", "root-one", "lead", "implementer0", "work", "", "", "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if len(client.sends) != 1 || client.sends[0][:len(first.Thread)] != first.Thread {
		t.Fatalf("assignment did not resolve the current session member: %#v", client.sends)
	}
	if _, err := service.Assign(context.Background(), "one", "root-one", "implementer9", "implementer0", "work", "", "", "0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("outside sender was accepted")
	}
}

func TestRemovedMemberStaysRemovedAcrossLifecycleAndFork(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	if _, err := service.Add(context.Background(), "source", "root-source", filepath.Join(workspace, "work", "source"), nil, "implementer", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := service.Remove(context.Background(), "source", "root-source", "implementer0"); err != nil {
		t.Fatal(err)
	}
	if err := service.ArchiveAll(context.Background(), "source", "root-source"); err != nil {
		t.Fatal(err)
	}
	if err := service.ReviveAll(context.Background(), "source", "root-source"); err != nil {
		t.Fatal(err)
	}
	source, err := store.Load("source", "root-source")
	if err != nil || source.Members[0].State != "removed" {
		t.Fatalf("removed source member = %#v, %v", source, err)
	}
	destination, err := service.Fork(context.Background(), source, "target", "root-target", filepath.Join(workspace, "work", "target"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(destination.Members) != 1 || destination.Members[0].State != "removed" || destination.Members[0].Thread != "" {
		t.Fatalf("removed destination member = %#v", destination.Members)
	}
	if _, err := service.Add(context.Background(), "target", "root-target", filepath.Join(workspace, "work", "target"), nil, "implementer", "", ""); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Load("target", "root-target")
	if err != nil || updated.Members[1].Address != "implementer1" {
		t.Fatalf("replacement address = %#v, %v", updated, err)
	}
}

func TestPresetDoesNotAppendMembersToExistingTeam(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Client: &testClient{}, Workspace: workspace}
	if _, err := service.ApplyPreset(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "delivery", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplyPreset(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "full", "", ""); err == nil {
		t.Fatal("preset appended to an existing team")
	}
	cwd := filepath.Join(workspace, "work", "two")
	if _, err := service.Add(context.Background(), "two", "root-two", cwd, nil, "implementer", "model-1", "high"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplyPreset(context.Background(), "two", "root-two", cwd, nil, "delivery", "model-1", "high"); err == nil {
		t.Fatal("preset expanded an all-ready manual prefix")
	}
}

func TestUnmanagedPresetReservesAllMembersBeforeStartingThreads(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	client := &testClient{projectEntered: entered, releaseProject: release}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	type outcome struct {
		members []Member
		err     error
	}
	results := make(chan outcome, 2)
	apply := func() {
		members, err := service.ApplyPreset(context.Background(), "one", "root-one", cwd, nil, "delivery", "model-1", "high")
		results <- outcome{members: members, err: err}
	}
	go apply()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("preset did not start its first member")
	}
	roster, err := store.Load("one", "root-one")
	if err != nil || len(roster.Members) != 2 || roster.Members[0].Address != "implementer0" ||
		roster.Members[1].Address != "reviewer0" || roster.Members[0].State != "creating" || roster.Members[1].State != "creating" {
		t.Fatalf("preset reservation = %#v, %v", roster, err)
	}
	go apply()
	close(release)
	var succeeded int
	for range 2 {
		select {
		case result := <-results:
			if result.err == nil {
				succeeded++
				if len(result.members) != 2 {
					t.Fatalf("completed preset members = %#v", result.members)
				}
			} else if !strings.Contains(result.err.Error(), "a preset can only create an empty team") {
				t.Fatalf("concurrent preset = %v", result.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent preset did not finish")
		}
	}
	roster, err = store.Load("one", "root-one")
	if succeeded == 0 || err != nil || len(roster.Members) != 2 ||
		roster.Members[0].State != "ready" || roster.Members[1].State != "ready" || client.next != 2 {
		t.Fatalf("concurrent preset result = successes %d, roster %#v, starts %d, error %v", succeeded, roster, client.next, err)
	}
}

func TestUnmanagedPresetRetryResumesPartialCreation(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{nameFailure: true}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	if _, err := service.ApplyPreset(context.Background(), "one", "root-one", cwd, nil, "delivery", "model-1", "high"); err == nil {
		t.Fatal("expected injected member name failure")
	}
	partial, err := store.Load("one", "root-one")
	if err != nil || len(partial.Members) != 2 || partial.Members[0].State != "creating" ||
		partial.Members[0].Thread != "thread-1" || partial.Members[1].State != "creating" || partial.Members[1].Thread != "" {
		t.Fatalf("retained partial preset = %#v, %v", partial, err)
	}
	if _, err := service.ApplyPreset(context.Background(), "one", "root-one", cwd, nil, "full", "model-1", "high"); err == nil {
		t.Fatal("different preset changed the partial reservation")
	}
	if _, err := service.ApplyPreset(context.Background(), "one", "root-one", cwd, nil, "delivery", "model-1", "medium"); err == nil {
		t.Fatal("changed settings reused the partial reservation")
	}
	ready, err := service.ApplyPreset(context.Background(), "one", "root-one", cwd, nil, "delivery", "model-1", "high")
	if err != nil || len(ready) != 2 || ready[0].Address != "implementer0" || ready[1].Address != "reviewer0" || client.next != 2 {
		t.Fatalf("resumed preset = %#v, starts %d, error %v", ready, client.next, err)
	}
	roster, err := store.Load("one", "root-one")
	if err != nil || len(roster.Members) != 2 || roster.Members[0].State != "ready" || roster.Members[1].State != "ready" {
		t.Fatalf("ready preset roster = %#v, %v", roster, err)
	}
}

func TestUnmanagedPresetRejectsOldSequentialPrefix(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{nameFailure: true}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	if _, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "implementer", "model-1", "high"); err == nil {
		t.Fatal("expected injected member name failure")
	}
	partial, err := store.Load("one", "root-one")
	if err != nil || len(partial.Members) != 1 || partial.Members[0].State != "creating" {
		t.Fatalf("old sequential prefix = %#v, %v", partial, err)
	}
	if _, err := service.ApplyPreset(context.Background(), "one", "root-one", cwd, nil, "delivery", "model-1", "high"); err == nil {
		t.Fatal("preset expanded an old sequential prefix")
	}
	retained, err := store.Load("one", "root-one")
	if err != nil || len(retained.Members) != 1 || retained.Members[0].Address != "implementer0" || client.next != 1 {
		t.Fatalf("old prefix changed = %#v, starts %d, error %v", retained, client.next, err)
	}
	if _, err := service.ApplyPreset(context.Background(), "one", "root-one", cwd, nil, "solo", "", ""); err == nil {
		t.Fatal("solo accepted a nonempty team")
	}
}

func TestUnmanagedPresetRejectsManualCreatingPrefix(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	if _, err := store.Update(context.Background(), "one", "root-one", true, func(roster *Roster) error {
		roster.Members = append(roster.Members, Member{Address: "implementer0", Role: "implementer", Model: "model-1",
			Effort: "high", Behavior: "implementer", Access: "workspace_write", State: "creating", AddedAt: time.Now().UTC()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplyPreset(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil,
		"delivery", "model-1", "high"); err == nil {
		t.Fatal("preset expanded a manual creating prefix")
	}
	roster, err := store.Load("one", "root-one")
	if err != nil || len(roster.Members) != 1 || roster.Members[0].Address != "implementer0" || client.next != 0 {
		t.Fatalf("manual creating prefix changed = %#v, starts %d, error %v", roster, client.next, err)
	}
}

func TestUnmanagedSoloChecksRosterUnderOperationLock(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Client: &testClient{}, Workspace: workspace}
	release, err := store.LockOperation(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = service.ApplyPreset(ctx, "one", "root-one", "", nil, "solo", "", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("solo bypassed a concurrent team operation: %v", err)
	}
	release()
	if _, err := service.ApplyPreset(context.Background(), "one", "root-one", "", nil, "solo", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("one", "root-one"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("solo created a persistent reservation: %v", err)
	}
}

func TestCatalogPresetUsesExplicitSiteSettingsAndArchitectAddress(t *testing.T) {
	catalog := agentteams.Catalog{CatalogDigest: fmt.Sprintf("%064x", 1), Teams: map[string]agentteams.Team{
		"delegated": {Description: "Separate design and review", TeamDigest: fmt.Sprintf("%064x", 2), Roles: map[string]agentteams.Role{
			"team_lead":   {Model: "gpt-6-sol", Effort: "high", Purpose: "lead", Instructions: "Coordinate this team."},
			"designer":    {Model: "gpt-6-sol", Effort: "xhigh", Behavior: "designer", Purpose: "design", Instructions: "Design the change.", Access: "read_only"},
			"implementer": {Model: "gpt-6-sol", Effort: "xhigh", Behavior: "implementer", Purpose: "implementation", Instructions: "Implement the change.", Access: "workspace_write"},
			"reviewer":    {Model: "gpt-6-sol", Effort: "xhigh", Behavior: "reviewer", Purpose: "review", Instructions: "Review the change.", Access: "read_only"},
		}},
	}}
	preset, err := FindCatalogPreset(catalog, "delegated")
	if err != nil {
		t.Fatal(err)
	}
	if preset.Name != "Full team" || preset.LeadModel != "gpt-6-sol" || preset.LeadEffort != "high" ||
		preset.LeadInstructions != "Coordinate this team." ||
		len(preset.Members) != 3 || preset.Members[0].Address != "architect0" ||
		preset.Members[0].Model != "gpt-6-sol" || preset.Members[0].Effort != "xhigh" ||
		preset.Members[0].Purpose != "design" || preset.Members[0].Instructions != "Design the change." {
		t.Fatalf("catalog projection = %#v", preset)
	}
	if preset.MemberCount() != 4 || preset.RoleSummary() != "1 lead, 1 architect, 1 implementer, 1 reviewer" {
		t.Fatalf("catalog role summary = %d %q", preset.MemberCount(), preset.RoleSummary())
	}
}

func TestCatalogPresetProjectsCustomRole(t *testing.T) {
	catalog := agentteams.Catalog{CatalogDigest: fmt.Sprintf("%064x", 1), Teams: map[string]agentteams.Team{
		"custom": {TeamDigest: fmt.Sprintf("%064x", 2), Roles: map[string]agentteams.Role{
			"team_lead": {Model: "gpt-6-sol", Effort: "high", Purpose: "lead", Instructions: "Coordinate the work."},
			"analyst": {Model: "gpt-6-sol", Effort: "xhigh", Behavior: "general", Purpose: "general",
				Instructions: "Analyze the assigned evidence.", Access: "read_only"},
		}},
	}}
	preset, err := FindCatalogPreset(catalog, "custom")
	if err != nil {
		t.Fatal(err)
	}
	if len(preset.Members) != 1 || preset.Members[0].Address != "analyst0" ||
		preset.Members[0].Purpose != "general" || preset.Members[0].Instructions != "Analyze the assigned evidence." ||
		preset.RoleSummary() != "1 lead, 1 analyst" {
		t.Fatalf("custom role projection = %#v", preset)
	}
}

func TestMemberPolicyBindsSessionAndRetainsInstructions(t *testing.T) {
	member := Member{Role: "analyst", Address: "analyst0", Behavior: "general", Purpose: "general",
		Instructions: "Analyze the assigned evidence.", Access: "read_only"}
	policy, err := memberPolicy("/srv/workspace", "2026-09-24-example", member)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Sandbox != "read-only" || !strings.Contains(policy.DeveloperInstructions, "2026-09-24-example") ||
		!strings.Contains(policy.DeveloperInstructions, "/srv/workspace") ||
		!strings.Contains(policy.DeveloperInstructions, "analyst0") ||
		!strings.HasSuffix(policy.DeveloperInstructions, member.Instructions) {
		t.Fatalf("member policy = %#v", policy)
	}
	legacy := Member{Role: "reviewer", Address: "reviewer0", Behavior: "reviewer", Access: "read_only"}
	legacyPolicy, err := memberPolicy("/srv/workspace", "2026-09-24-example", legacy)
	if err != nil || !strings.HasSuffix(legacyPolicy.DeveloperInstructions, legacyBehaviorInstructions["reviewer"]) {
		t.Fatalf("legacy member instructions = %#v, %v", legacyPolicy, err)
	}
	for _, invalid := range []string{"", "bad\x00prompt", string([]byte{0xff})} {
		member.Instructions = invalid
		if _, err := memberPolicy("/srv/workspace", "2026-09-24-example", member); err == nil {
			t.Fatalf("accepted invalid custom instructions %q", invalid)
		}
	}
}

func TestPresetRoleSummaryCountsRepeatedRoles(t *testing.T) {
	preset := Preset{Members: []MemberSpec{{Role: "reviewer"}, {Role: "reviewer"}, {Role: "implementer"}}}
	if preset.MemberCount() != 4 || preset.RoleSummary() != "1 lead, 1 implementer, 2 reviewers" {
		t.Fatalf("role summary = %d %q", preset.MemberCount(), preset.RoleSummary())
	}
}

func TestUnmanagedPresetRoleSummaryUsesRoles(t *testing.T) {
	preset, ok := FindPreset("full")
	if !ok || preset.MemberCount() != 4 || preset.RoleSummary() != "1 lead, 1 architect, 1 implementer, 1 reviewer" {
		t.Fatalf("unmanaged preset summary = %d %q, found %t", preset.MemberCount(), preset.RoleSummary(), ok)
	}
}

func TestSoloCatalogPresetSerializesEmptyMembers(t *testing.T) {
	catalog := agentteams.Catalog{CatalogDigest: fmt.Sprintf("%064x", 1), Teams: map[string]agentteams.Team{
		"solo": {TeamDigest: fmt.Sprintf("%064x", 2), Roles: map[string]agentteams.Role{
			"team_lead": {Model: "gpt-6-sol", Effort: "high", Purpose: "lead", Instructions: "Lead the work."},
		}},
	}}
	preset, err := FindCatalogPreset(catalog, "solo")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(preset)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if members, ok := payload["members"].([]any); !ok || len(members) != 0 {
		t.Fatalf("solo preset members = %#v", payload["members"])
	}
}

func TestCatalogPresetRejectsInstructionDigestDrift(t *testing.T) {
	catalog := agentteams.Catalog{CatalogDigest: fmt.Sprintf("%064x", 1), Teams: map[string]agentteams.Team{
		"delegated": {TeamDigest: fmt.Sprintf("%064x", 2), Roles: map[string]agentteams.Role{
			"team_lead": {Model: "gpt-6-sol", Effort: "high", Purpose: "lead", Instructions: "Lead the work."},
			"reviewer":  {Model: "gpt-6-sol", Effort: "xhigh", Behavior: "reviewer", Purpose: "review", Instructions: "Review the work.", Access: "read_only"},
		}},
	}, NativeAgentConfigs: agentteams.NativeAgentConfigs{Roles: []agentteams.NativeRoleConfig{
		{Team: "delegated", Role: "reviewer", Effort: "xhigh", Identity: agentteams.NativeIdentity{BehaviorDigest: fmt.Sprintf("%064x", 9)}},
	}}}
	if _, err := FindCatalogPreset(catalog, "delegated"); err == nil {
		t.Fatal("catalog with different generated role instructions was accepted")
	}
}

func TestSoloCanAddCatalogSpecialistAfterCatalogUpdate(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	oldDigest := fmt.Sprintf("%064x", 1)
	preset := Preset{ID: "solo", CatalogDigest: oldDigest, TeamDigest: fmt.Sprintf("%064x", 2),
		LeadModel: "gpt-6-sol", LeadEffort: "high"}
	cwd := filepath.Join(workspace, "work", "one")
	if _, err := service.ApplyPresetSpec(context.Background(), "one", "root-one", cwd, nil, preset); err != nil {
		t.Fatal(err)
	}
	currentDigest := fmt.Sprintf("%064x", 3)
	service.Catalog = &agentteams.Catalog{CatalogDigest: currentDigest, Teams: map[string]agentteams.Team{
		"delegated": {Roles: map[string]agentteams.Role{
			"implementer": {Behavior: "implementer", Access: "workspace_write"},
		}},
	}}
	member, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "implementer", "gpt-6-sol", "xhigh")
	if err != nil || member.PolicyCatalogDigest != currentDigest || member.Behavior != "implementer" ||
		client.starts[0].Policy.Sandbox != "workspace-write" {
		t.Fatalf("added specialist = %#v, settings %#v, error %v", member, client.starts, err)
	}
	if _, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "custom", "gpt-6-sol", "xhigh"); err == nil {
		t.Fatal("custom role without pinned catalog policy was accepted")
	}
}

func TestCatalogPresetRetryReusesPersistedThreadAndDoesNotAppend(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{nameFailure: true}
	service := Service{Store: store, Client: client, Workspace: workspace}
	preset := Preset{ID: "delivery", CatalogDigest: fmt.Sprintf("%064x", 1), TeamDigest: fmt.Sprintf("%064x", 2),
		LeadModel: "gpt-6-sol", LeadEffort: "xhigh", LeadInstructions: "Coordinate the delivery.", Members: []MemberSpec{
			{Role: "implementer", Address: "implementer0", Model: "gpt-6-sol", Effort: "xhigh", Behavior: "implementer",
				Purpose: "implementation", Instructions: "Implement this assignment.", Access: "workspace_write"},
		}}
	cwd := filepath.Join(workspace, "work", "one")
	if _, err := service.ApplyPresetSpec(context.Background(), "one", "root-one", cwd, nil, preset); err == nil {
		t.Fatal("expected injected name failure")
	}
	partial, err := store.Load("one", "root-one")
	if err != nil || len(partial.Members) != 1 || partial.Members[0].State != "creating" || partial.Members[0].Thread != "thread-1" ||
		partial.LeadInstructions != preset.LeadInstructions || partial.Members[0].Instructions != preset.Members[0].Instructions {
		t.Fatalf("retained partial member = %#v, %v", partial, err)
	}
	ready, err := service.ApplyPresetSpec(context.Background(), "one", "root-one", cwd, nil, preset)
	if err != nil || len(ready.Members) != 1 || ready.Members[0].State != "ready" || client.next != 1 {
		t.Fatalf("resumed preset = %#v, starts %d, error %v", ready, client.next, err)
	}
	if len(client.starts) != 1 || client.starts[0].Model != "gpt-6-sol" || client.starts[0].ReasoningEffort != "xhigh" {
		t.Fatalf("member settings = %#v", client.starts)
	}
	if !strings.HasSuffix(client.starts[0].Policy.DeveloperInstructions, "Implement this assignment.") {
		t.Fatalf("member did not start with retained instructions: %#v", client.starts[0].Policy)
	}
	if len(client.resumes) != 1 || client.resumes[0].Policy.Sandbox != client.starts[0].Policy.Sandbox ||
		client.resumes[0].Policy.DeveloperInstructions != client.starts[0].Policy.DeveloperInstructions ||
		client.starts[0].Policy.MCPServer != nil || client.resumes[0].Policy.MCPServer == nil ||
		client.resumes[0].Policy.MCPServer.Args[len(client.resumes[0].Policy.MCPServer.Args)-1] != "thread-1" {
		t.Fatalf("retry did not restore retained policy: starts %#v, resumes %#v", client.starts, client.resumes)
	}
	service.Catalog = &agentteams.Catalog{CatalogDigest: fmt.Sprintf("%064x", 3), Teams: map[string]agentteams.Team{
		"delegated": {Roles: map[string]agentteams.Role{
			"implementer": {Behavior: "implementer", Access: "workspace_write"},
		}},
	}}
	if _, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "implementer", "gpt-6-sol", "xhigh"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplyPresetSpec(context.Background(), "one", "root-one", cwd, nil, preset); err == nil {
		t.Fatal("reapplication changed an edited team")
	}
}

func TestSoloPresetRetainsSelectionWithoutSpecialistThread(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	preset := Preset{ID: "solo", CatalogDigest: fmt.Sprintf("%064x", 1), TeamDigest: fmt.Sprintf("%064x", 2),
		LeadModel: "gpt-6-sol", LeadEffort: "high"}
	roster, err := service.ApplyPresetSpec(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, preset)
	if err != nil || roster.PresetID != "solo" || len(roster.Members) != 0 || client.next != 0 {
		t.Fatalf("solo roster = %#v, starts %d, error %v", roster, client.next, err)
	}
}

func TestPresetRejectsInvalidRetainedLeadInstructions(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Client: &testClient{}, Workspace: workspace}
	preset := Preset{ID: "solo", CatalogDigest: fmt.Sprintf("%064x", 1), TeamDigest: fmt.Sprintf("%064x", 2),
		LeadModel: "gpt-6-sol", LeadEffort: "high", LeadInstructions: "bad\x00prompt"}
	if _, err := service.ApplyPresetSpec(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, preset); err == nil {
		t.Fatal("invalid lead instructions were retained")
	}
}

func TestRetryCreatingReconcilesLostResponseWithoutSecondStart(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{lostStart: true, hideThreads: true}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	if _, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "implementer", "gpt-6-sol", "xhigh"); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("lost start response = %v", err)
	}
	partial, err := store.Load("one", "root-one")
	if err != nil || partial.Members[0].Thread != "" || !partial.Members[0].CreateAttempted || partial.Members[0].ProjectID == "" {
		t.Fatalf("durable start attempt = %#v, %v", partial, err)
	}
	if err := service.Remove(context.Background(), "one", "root-one", "implementer0"); err == nil {
		t.Fatal("removed member with an unresolved start")
	}
	if err := service.ArchiveAll(context.Background(), "one", "root-one"); err == nil {
		t.Fatal("archived member with an unresolved start")
	}
	if _, err := service.RetryCreating(context.Background(), "one", "root-one", cwd, nil, "implementer0"); err == nil {
		t.Fatal("zero-result reconciliation allocated or accepted another start")
	}
	client.hideThreads = false
	member, err := service.RetryCreating(context.Background(), "one", "root-one", cwd, nil, "implementer0")
	if err != nil || member.Thread != "thread-1" || member.State != "ready" || client.next != 1 {
		t.Fatalf("reconciled member = %#v, starts %d, error %v", member, client.next, err)
	}
	if client.starts[0].ProjectID != member.ProjectID ||
		member.ProjectID == memberProjectKey(workspace, "one", "different-root", "implementer0") {
		t.Fatalf("project identity was not bound to the root: %#v", member)
	}
}

func TestRetryCreatingReplaysLostProjectRegistrationBeforeStarting(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{lostProjectCreate: true}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	if _, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "architect", "gpt-6-sol", "xhigh"); err == nil {
		t.Fatal("lost project/create response unexpectedly completed member start")
	}
	partial, err := store.Load("one", "root-one")
	if err != nil || partial.Members[0].ProjectID != "" || partial.Members[0].CreateAttempted || len(client.starts) != 0 {
		t.Fatalf("project reservation = %#v, starts %d, error %v", partial, len(client.starts), err)
	}
	member, err := service.RetryCreating(context.Background(), "one", "root-one", cwd, nil, "architect0")
	if err != nil || member.State != "ready" || len(client.projects) != 1 || client.projectCreates != 2 || len(client.starts) != 1 ||
		client.starts[0].ProjectID != member.ProjectID {
		t.Fatalf("registered member = %#v, projects %#v, creates %d, starts %#v, error %v",
			member, client.projects, client.projectCreates, client.starts, err)
	}
}

func TestRetryCreatingRequiresRegisteredProject(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{projectReadError: errors.New("project not found")}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	if _, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "reviewer", "gpt-6-sol", "xhigh"); err == nil ||
		!strings.Contains(err.Error(), "verify reviewer0 project") {
		t.Fatalf("missing project verification = %v", err)
	}
	partial, err := store.Load("one", "root-one")
	if err != nil || !projectUUIDPattern.MatchString(partial.Members[0].ProjectID) || partial.Members[0].CreateAttempted || len(client.starts) != 0 {
		t.Fatalf("unstarted member = %#v, starts %d, error %v", partial, len(client.starts), err)
	}
	client.projectReadError = nil
	if _, err := service.RetryCreating(context.Background(), "one", "root-one", cwd, nil, "reviewer0"); err != nil || len(client.starts) != 1 {
		t.Fatalf("verified project retry = %v, starts %d", err, len(client.starts))
	}
}

func TestRetryCreatingRejectsProjectKeyBoundToDifferentName(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	key := memberProjectKey(workspace, "one", "root-one", "reviewer0")
	client := &testClient{projects: map[string]codex.ProjectMetadata{
		key: {ID: "00000000-0000-7000-8000-000000000001", Name: "another member"},
	}}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	if _, err := service.Add(context.Background(), "one", "root-one", cwd, nil, "reviewer", "gpt-6-sol", "xhigh"); err == nil ||
		!strings.Contains(err.Error(), "different identity") || len(client.starts) != 0 {
		t.Fatalf("wrong-name project = %v, starts %d", err, len(client.starts))
	}
}

func TestRetryCreatingRepairsOnlyMissingLegacyProject(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	_, err = store.Update(context.Background(), "one", "root-one", true, func(roster *Roster) error {
		roster.Members = append(roster.Members, Member{Address: "architect0", Role: "architect", State: "creating",
			Model: "gpt-6-sol", Effort: "xhigh", Behavior: "designer", Access: "read_only",
			ProjectID: memberProjectKey(workspace, "one", "root-one", "architect0"), CreateAttempted: true, AddedAt: time.Now().UTC()})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	client.projectReadError = errors.New("project lookup unavailable")
	if _, err := service.RetryCreating(context.Background(), "one", "root-one", cwd, nil, "architect0"); err == nil ||
		!strings.Contains(err.Error(), "needs project reconciliation") || len(client.starts) != 0 || client.projectCreates != 0 {
		t.Fatalf("uncertain legacy reservation = %v, starts %d, projects %d", err, len(client.starts), client.projectCreates)
	}
	client.projectReadError = nil
	member, err := service.RetryCreating(context.Background(), "one", "root-one", cwd, nil, "architect0")
	if err != nil || member.State != "ready" || !projectUUIDPattern.MatchString(member.ProjectID) ||
		len(client.starts) != 1 || client.projectCreates != 1 {
		t.Fatalf("reconciled legacy reservation = %#v, starts %d, projects %d, error %v",
			member, len(client.starts), client.projectCreates, err)
	}
}

func TestRetryCreatingRejectsAmbiguousProjectIdentity(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{lostStart: true, hideThreads: true}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	_, _ = service.Add(context.Background(), "one", "root-one", cwd, nil, "reviewer", "gpt-6-sol", "xhigh")
	client.hideThreads = false
	client.threads = append(client.threads, codex.ThreadMetadata{ID: "duplicate", Cwd: cwd, ProjectID: client.threads[0].ProjectID})
	if _, err := service.RetryCreating(context.Background(), "one", "root-one", cwd, nil, "reviewer0"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous start recovery = %v", err)
	}
	if client.next != 1 {
		t.Fatalf("started %d threads", client.next)
	}
}

func TestConcurrentRetryCreatingStartsOnce(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	cwd := filepath.Join(workspace, "work", "one")
	_, err = store.Update(context.Background(), "one", "root-one", true, func(roster *Roster) error {
		roster.Members = append(roster.Members, Member{Address: "reviewer0", Role: "reviewer", State: "creating",
			Model: "gpt-6-sol", Effort: "xhigh", Behavior: "reviewer", Access: "read_only",
			AddedAt: time.Now().UTC()})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := service.RetryCreating(context.Background(), "one", "root-one", cwd, nil, "reviewer0")
			results <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if client.next != 1 {
		t.Fatalf("concurrent retries started %d threads", client.next)
	}
}

func TestAssignRequiresStableCallerMessageID(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	_, err = service.Add(context.Background(), "one", "root-one", filepath.Join(workspace, "work", "one"), nil, "reviewer", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "random", "ABCDEF0123456789ABCDEF0123456789"} {
		if _, err := service.Assign(context.Background(), "one", "root-one", "lead", "reviewer0", "check", "", "", bad); err == nil {
			t.Fatalf("accepted invalid message ID %q", bad)
		}
	}
	id := "12345678-1234-1234-1234-123456789abc"
	for i := 0; i < 2; i++ {
		if _, err := service.Assign(context.Background(), "one", "root-one", "lead", "reviewer0", "check", "", "", id); err != nil {
			t.Fatal(err)
		}
	}
	if len(client.messageIDs) != 2 || client.messageIDs[0] != id || client.messageIDs[1] != id {
		t.Fatalf("assignment IDs = %#v", client.messageIDs)
	}
}

func TestForkUsesDestinationMemberAddressInEachThreadEnvironment(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	sourceCwd := filepath.Join(workspace, "work", "source")
	for _, role := range []string{"architect", "reviewer"} {
		if _, err := service.Add(context.Background(), "source", "root-source", sourceCwd, nil, role, "gpt-6-sol", "xhigh"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Update(context.Background(), "source", "root-source", false, func(roster *Roster) error {
		roster.Members[0].Purpose = "design"
		roster.Members[0].Instructions = "Design only this branch."
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	source, err := store.Load("source", "root-source")
	if err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"DEV_SESSION_SLUG": "target", "DEV_SESSION_MEMBER_ADDRESS": "lead"}
	if _, err := service.Fork(context.Background(), source, "target", "root-target", filepath.Join(workspace, "work", "target"), environment); err != nil {
		t.Fatal(err)
	}
	if len(client.forkEnvs) != 2 || client.forkEnvs[0]["DEV_SESSION_MEMBER_ADDRESS"] != "architect0" ||
		client.forkEnvs[1]["DEV_SESSION_MEMBER_ADDRESS"] != "reviewer0" {
		t.Fatalf("fork member environments = %#v", client.forkEnvs)
	}
	for _, forkEnvironment := range client.forkEnvs {
		if forkEnvironment["DEV_SESSION_SLUG"] != "target" {
			t.Fatalf("fork lost shared destination environment: %#v", forkEnvironment)
		}
	}
	if environment["DEV_SESSION_MEMBER_ADDRESS"] != "lead" {
		t.Fatalf("fork changed caller environment: %#v", environment)
	}
	if len(client.bootstrapCalls) != 4 || !strings.Contains(client.bootstrapCalls[2], "thread-3") ||
		!strings.Contains(client.bootstrapCalls[3], "thread-4") {
		t.Fatalf("forked members were not bootstrapped: %#v", client.bootstrapCalls)
	}
	if len(client.forkSettings) != 2 || client.forkSettings[0].Policy.MCPServer != nil || client.forkSettings[1].Policy.MCPServer != nil {
		t.Fatalf("fork exposed a report tool before destination thread identity: %#v", client.forkSettings)
	}
	if !strings.Contains(client.forkSettings[0].Policy.DeveloperInstructions, "session \"target\"") ||
		!strings.HasSuffix(client.forkSettings[0].Policy.DeveloperInstructions, "Design only this branch.") {
		t.Fatalf("fork did not retain instructions with destination identity: %#v", client.forkSettings[0].Policy)
	}
	destination, err := store.Load("target", "root-target")
	if err != nil || destination.Members[0].Purpose != "design" || destination.Members[0].Instructions != "Design only this branch." {
		t.Fatalf("forked retained policy = %#v, %v", destination, err)
	}
	if _, err := service.Assign(context.Background(), "target", "root-target", "lead", "architect0", "Review the design", "", "", "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if len(client.options) != 1 || client.options[0].ThreadPolicy.MCPServer == nil ||
		client.options[0].ThreadPolicy.MCPServer.Args[len(client.options[0].ThreadPolicy.MCPServer.Args)-1] != "thread-3" {
		t.Fatalf("forked member assignment did not bind its destination thread: %#v", client.options)
	}
}

func TestForkRejectsUnmaterializedSourceBeforeDestinationReservation(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{unmaterialized: make(map[string]bool)}
	service := Service{Store: store, Client: client, Workspace: workspace}
	member, err := service.Add(context.Background(), "source", "root-source", filepath.Join(workspace, "work", "source"), nil, "architect", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	client.unmaterialized[member.Thread] = true
	source, err := store.Load("source", "root-source")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Fork(context.Background(), source, "target", "root-target", filepath.Join(workspace, "work", "target"), nil)
	if err == nil || !strings.Contains(err.Error(), "no persisted rollout") || len(client.forkEnvs) != 0 {
		t.Fatalf("unmaterialized source fork = %v, forks = %#v", err, client.forkEnvs)
	}
	if _, err := store.Load("target", "root-target"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fork reserved destination despite missing source rollout: %v", err)
	}
}

func TestForkRejectsCreatingSourceBeforeDestinationRosterOrThread(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	if _, err := service.Add(context.Background(), "source", "root-source", filepath.Join(workspace, "work", "source"), nil,
		"architect", "gpt-6-sol", "xhigh"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(context.Background(), "source", "root-source", false, func(roster *Roster) error {
		roster.Members = append(roster.Members, Member{Address: "reviewer0", Role: "reviewer", State: "creating",
			Model: "gpt-6-sol", Effort: "xhigh", Behavior: "reviewer", Access: "read_only", AddedAt: time.Now().UTC()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	source, err := store.Load("source", "root-source")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Fork(context.Background(), source, "target", "root-target", filepath.Join(workspace, "work", "target"), nil)
	if err == nil || !strings.Contains(err.Error(), "reviewer0 is still creating") {
		t.Fatalf("creating source member was forked: %v", err)
	}
	if _, err := store.Load("target", "root-target"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fork reserved a destination roster: %v", err)
	}
	if client.next != 1 || len(client.forkEnvs) != 0 {
		t.Fatalf("fork created destination threads: starts=%d forks=%d", client.next, len(client.forkEnvs))
	}
}

func TestForkRetryUsesFrozenSourceAndRejectsMutation(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	_, err = service.Add(context.Background(), "source", "root-source", filepath.Join(workspace, "work", "source"), nil, "reviewer", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Load("source", "root-source")
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(workspace, "work", "target")
	client.lostFork, client.hideThreads = true, true
	if _, err := service.Fork(context.Background(), source, "target", "root-target", cwd, nil); err == nil {
		t.Fatal("accepted uncertain fork")
	}
	partial, err := store.Load("target", "root-target")
	if err != nil || partial.ForkSource == nil || partial.ForkSource.Members[0].Thread != source.Members[0].Thread || !partial.Members[0].CreateAttempted {
		t.Fatalf("frozen fork = %#v, %v", partial, err)
	}
	client.hideThreads = false // Even a plausible list match is not unique across slug recreation.
	if _, err := service.Fork(context.Background(), source, "target", "root-target", cwd, nil); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("uncertain fork retry was not refused: %v", err)
	}
	if _, err := store.Update(context.Background(), "source", "root-source", false, func(roster *Roster) error {
		roster.Members[0].Model = "changed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	client.hideThreads = false
	if _, err := service.Fork(context.Background(), source, "target", "root-target", cwd, nil); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("source mutation was not refused: %v", err)
	}
	if client.next != 2 {
		t.Fatalf("fork retry made another thread: %d", client.next)
	}
}

func TestForkKnownThreadRetryUsesFrozenSnapshot(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	store, err := NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{}
	service := Service{Store: store, Client: client, Workspace: workspace}
	_, err = service.Add(context.Background(), "source", "root-source", filepath.Join(workspace, "work", "source"), nil, "reviewer", "gpt-6-sol", "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	client.nameFailure = true
	source, err := store.Load("source", "root-source")
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(workspace, "work", "target")
	if _, err := service.Fork(context.Background(), source, "target", "root-target", cwd, nil); err == nil {
		t.Fatal("expected name failure")
	}
	partial, err := store.Load("target", "root-target")
	if err != nil || partial.Members[0].Thread != "thread-2" || partial.Members[0].State != "creating" {
		t.Fatalf("partial fork = %#v, %v", partial, err)
	}
	ready, err := service.Fork(context.Background(), source, "target", "root-target", cwd, nil)
	if err != nil || ready.Members[0].State != "ready" || client.next != 2 || ready.ForkSource.Members[0].Model != "gpt-6-sol" {
		t.Fatalf("known-thread fork retry = %#v, starts %d, error %v", ready, client.next, err)
	}
}
