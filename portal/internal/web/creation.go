package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"golang.org/x/sys/unix"
)

// Match dev-session read_goal's Ruby String#strip boundary exactly. Captured
// plan text keeps its original bytes; only the derived CLI goal is normalized.
func planCreationGoal(source, plan string) string {
	return normalizedCreationGoal("Implement the following approved plan from session " + source + ".\n\n" + plan)
}

func normalizedCreationGoal(goal string) string {
	return strings.Trim(goal, " \t\n\v\f\r\x00")
}

func creationGoalDigest(goal string) string {
	// Deployed receipts may retain boundary whitespace already stripped by the
	// CLI. Compare their canonical goal without rewriting the accepted snapshot.
	return planDigest(normalizedCreationGoal(goal))
}

// Keep acceptance independent of App Server, tmux, Git and cluster discovery.
func (s *Server) withCreationMutation(w http.ResponseWriter, r *http.Request, mutate func()) {
	ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
	defer cancel()
	unlock, err := s.lockTransitionContext(ctx)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Workspace runtime is changing. Retry shortly."})
		return
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	mutate()
}

func creationDestination(name, date string) (string, error) {
	if !session.ValidSlug(name) || len(name) > 48 {
		return "", errors.New("session name is invalid")
	}
	parsed, err := time.Parse(time.DateOnly, date)
	if err != nil || parsed.Format(time.DateOnly) != date {
		return "", errors.New("session creation date is invalid")
	}
	return date + "-" + name, nil
}

func validateCreationSettings(model, effort string) error {
	for _, value := range []string{model, effort} {
		if err := validateCreationSettingValue(value); err != nil {
			return err
		}
	}
	if model == "" && effort != "" {
		return errors.New("select a Codex model before choosing a reasoning effort")
	}
	return nil
}

// Submitted managed lead overrides are independently optional. The resolver
// combines them with the selected lead, while resolved settings still require
// a model before an effort through validateCreationSettings above.
func validateCreationSettingValue(value string) error {
	if len(value) > session.MaxAgentTeamPersistedScalarBytes || strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("Codex model settings are invalid")
	}
	return nil
}

// resolveManagedCreation is the browser's one policy-bearing acceptance gate.
// It runs before a creation receipt, root thread, worktree, or retained state
// exists. The rendered catalog is only a hint; this function resolves the
// submitted selection from the installed catalog and current launch evidence.
func (s *Server) resolveManagedCreation(_ context.Context, request creationRequest, input agentteams.CreationSelectionInput) (creationRequest, error) {
	if !input.Team.Present && !input.CatalogDigest.Present {
		return request, nil
	}
	if !input.Team.Present || !input.CatalogDigest.Present || input.Team.Value == "" || input.CatalogDigest.Value == "" {
		return creationRequest{}, errors.New("select a starting team and reload the form")
	}
	if input.Model.Present != input.ReasoningEffort.Present {
		return creationRequest{}, errors.New("select a Codex model and reasoning effort together")
	}
	if s.installedTeams == nil || !s.installedTeams.Managed || s.installedTeams.Catalog == nil {
		return creationRequest{}, errors.New("direct team selection is unavailable in this workspace package")
	}
	preset, err := teamruntime.FindCatalogPreset(*s.installedTeams.Catalog, input.Team.Value)
	if err != nil {
		return creationRequest{}, errors.New("selected starting team is unavailable")
	}
	if input.CatalogDigest.Value != preset.CatalogDigest {
		return creationRequest{}, errors.New("catalog digest is stale; reload the form")
	}
	if input.Model.Present {
		if s.config.Codex == nil {
			return creationRequest{}, errors.New("Codex model catalog is unavailable")
		}
		models, err := s.config.Codex.ListModels(context.Background())
		if err != nil {
			return creationRequest{}, fmt.Errorf("load Codex models: %w", err)
		}
		settings, err := workspacecodex.ResolveNewThreadSettings(models, codex.ThreadSettings{
			Model: input.Model.Value, ReasoningEffort: input.ReasoningEffort.Value,
		})
		if err != nil {
			return creationRequest{}, err
		}
		preset.LeadModel, preset.LeadEffort = settings.Model, settings.ReasoningEffort
	}
	if err := validDirectTeamPreset(preset); err != nil {
		return creationRequest{}, fmt.Errorf("resolve direct team preset: %w", err)
	}
	request.Team, request.CatalogDigest = preset.ID, preset.CatalogDigest
	request.directTeam = &preset
	return request, nil
}

func validDirectTeamPreset(preset teamruntime.Preset) error {
	if !validManagedCreationTeam(preset.ID) || preset.Name == "" || len(preset.Name) > 256 ||
		preset.Description == "" || len(preset.Description) > 4096 ||
		!utf8.ValidString(preset.Name) || !utf8.ValidString(preset.Description) ||
		strings.ContainsAny(preset.Name+preset.Description, "\x00\r\n") ||
		!messageDigestPattern.MatchString(preset.CatalogDigest) ||
		!messageDigestPattern.MatchString(preset.TeamDigest) ||
		validateCreationSettingValue(preset.LeadModel) != nil || validateCreationSettingValue(preset.LeadEffort) != nil ||
		preset.LeadModel == "" || preset.LeadEffort == "" || len(preset.Roles) > 64 || len(preset.Members) > 64 {
		return errors.New("direct team preset is malformed")
	}
	withInstructions := preset.LeadInstructions != ""
	if withInstructions && !validDirectInstructions(preset.LeadInstructions) {
		return errors.New("direct team preset has invalid lead instructions")
	}
	addresses := make(map[string]struct{}, len(preset.Members))
	expectedRoles := make([]string, 0, len(preset.Members)+1)
	expectedRoles = append(expectedRoles, "lead")
	for _, member := range preset.Members {
		if !validDirectMemberRole(member.Role) || member.Role == "lead" ||
			member.Address != member.Role+"0" || member.Model == "" || member.Effort == "" ||
			!validDirectMemberPolicy(member.Role, member.Behavior, member.Access, member.Purpose, member.Instructions, withInstructions) ||
			validateCreationSettingValue(member.Model) != nil || validateCreationSettingValue(member.Effort) != nil {
			return errors.New("direct team preset has an invalid member")
		}
		if _, duplicate := addresses[member.Address]; duplicate {
			return errors.New("direct team preset has duplicate member addresses")
		}
		addresses[member.Address] = struct{}{}
		expectedRoles = append(expectedRoles, member.Address)
	}
	if len(preset.Roles) != len(expectedRoles) {
		return errors.New("direct team preset has an invalid role summary")
	}
	for index, role := range expectedRoles {
		if preset.Roles[index] != role {
			return errors.New("direct team preset has an invalid role summary")
		}
	}
	return nil
}

func validDirectMemberRole(role string) bool {
	if len(role) == 0 || len(role) > 32 || role[0] < 'a' || role[0] > 'z' {
		return false
	}
	for _, character := range role[1:] {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

func validDirectInstructions(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validDirectMemberPolicy(role, behavior, access, purpose, instructions string, withInstructions bool) bool {
	if withInstructions {
		if !validDirectInstructions(instructions) {
			return false
		}
		switch behavior {
		case "designer":
			return purpose == "design" && (access == "read_only" || access == "workspace_write")
		case "implementer":
			return purpose == "implementation" && access == "workspace_write"
		case "reviewer":
			return purpose == "review" && access == "read_only"
		case "general":
			return purpose == "general" && (access == "read_only" || access == "workspace_write")
		default:
			return false
		}
	}
	if purpose != "" || instructions != "" {
		return false
	}
	switch role {
	case "architect":
		return behavior == "designer" && access == "read_only"
	case "implementer":
		return behavior == "implementer" && access == "workspace_write"
	case "reviewer":
		return behavior == "reviewer" && access == "read_only"
	default:
		return false
	}
}

func requireLiveUnixSocket(path string) error {
	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	return connection.Close()
}

func (s *Server) creationSource(slug string) (*session.Summary, string, error) {
	if receipt, ok := s.currentCreation(slug); ok && receipt.blocksSession() {
		return nil, "", errors.New("source session initialization has not finished")
	}
	lock, err := session.LockRuntimeShared(s.config.AuthorityDir, slug)
	if err != nil {
		return nil, "", err
	}
	defer lock.Close()
	source, err := session.Find(s.config.Workspace, slug)
	if err != nil {
		// A missing manifest means the immutable session selected at acceptance
		// no longer exists. Do not generalize this to other ENOENT values below:
		// runtime-authority files can disappear briefly during a healthy restart.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("%w: source session no longer exists", errSourceIdentityChanged)
		}
		return nil, "", err
	}
	owner, err := session.PendingLifecycle(s.config.Workspace, slug)
	if err != nil {
		return nil, "", err
	}
	if owner != "" {
		return nil, "", errors.New("source session is not ready for browser changes")
	}
	if source.Archived || source.Creation.State != "ready" ||
		(source.Creation.GoalSHA256 != "" && !source.Creation.InitialGoalSent) || source.Codex.ThreadID == "" ||
		source.Codex.SocketPath != s.config.CodexSocket {
		return nil, "", fmt.Errorf("%w: source session is not ready for browser changes", errSourceIdentityChanged)
	}
	authority, err := session.LoadRuntimeAuthority(s.config.AuthorityDir, slug, s.config.Workspace)
	if err != nil {
		return nil, "", err
	}
	if authority.State != "ready" || authority.CodexThreadID != source.Codex.ThreadID || authority.CodexSocketPath != s.config.CodexSocket {
		return nil, "", fmt.Errorf("%w: source session runtime identity changed", errSourceIdentityChanged)
	}
	identity, err := lifecycleTargetIdentity(source)
	return source, identity, err
}

func (s *Server) acceptCreation(request creationRequest) (creationReceipt, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if s.closing {
		return creationReceipt{}, errors.New("portal is stopping; retry shortly")
	}
	if err := s.retireAbsentCreation(request.Slug); err != nil {
		return creationReceipt{}, err
	}
	if previous, ok := s.creations[request.Slug]; ok {
		same := sameCreationRequest(previous.Request, request) &&
			previous.Schema != 2 &&
			(previous.Schema != 3 || (request.directTeam != nil && previous.DirectTeam != nil && reflect.DeepEqual(*previous.DirectTeam, *request.directTeam) &&
				previous.Model == request.directTeam.LeadModel && previous.Effort == request.directTeam.LeadEffort))
		if same {
			return previous, nil
		}
		return creationReceipt{}, errors.New("this session name belongs to a different creation request")
	}
	lock, err := session.LockRuntimeShared(s.config.AuthorityDir, request.Slug)
	if err != nil {
		return creationReceipt{}, err
	}
	defer lock.Close()
	for _, root := range []string{"work", "archive", "worktrees"} {
		if _, err := os.Lstat(filepath.Join(s.config.Workspace, root, request.Slug)); err == nil {
			return creationReceipt{}, errors.New("session name already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return creationReceipt{}, err
		}
	}
	owner, err := session.PendingLifecycle(s.config.Workspace, request.Slug)
	if err != nil {
		return creationReceipt{}, err
	}
	if owner != "" {
		return creationReceipt{}, errors.New("session has an unfinished lifecycle operation")
	}
	if err := s.retireReadyCreations(1); err != nil {
		return creationReceipt{}, err
	}
	history, err := session.CompletedRemovalHistory(s.config.Workspace, request.Slug, s.config.UserStateRoot)
	if err != nil {
		return creationReceipt{}, err
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return creationReceipt{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	schema := 1
	if request.directTeam != nil {
		if request.Kind == "fork" ||
			request.Team != request.directTeam.ID || request.CatalogDigest != request.directTeam.CatalogDigest {
			return creationReceipt{}, errors.New("direct team preset is incomplete")
		}
		if err := validDirectTeamPreset(*request.directTeam); err != nil {
			return creationReceipt{}, err
		}
		schema = 3
	}
	receipt := creationReceipt{Schema: schema, Workspace: s.config.Workspace, Request: request, ReceiptID: hex.EncodeToString(id),
		DeletionHistorySHA256: history, Attempt: 1, State: "running", Phase: "Checking session settings…", StartedAt: now, UpdatedAt: now}
	if schema == 3 {
		preset := *request.directTeam
		receipt.DirectTeam = &preset
		receipt.Model, receipt.Effort = preset.LeadModel, preset.LeadEffort
	}
	if err := s.saveCreation(receipt); err != nil {
		return creationReceipt{}, err
	}
	s.creations[request.Slug] = receipt
	s.operationWG.Add(1)
	go s.runCreation(receipt)
	return receipt, nil
}

func sameCreationRequest(left, right creationRequest) bool {
	left.directTeam = nil
	right.directTeam = nil
	left.teamSubmitted, left.digestSubmitted, left.modelSubmitted, left.effortSubmitted = false, false, false, false
	right.teamSubmitted, right.digestSubmitted, right.modelSubmitted, right.effortSubmitted = false, false, false, false
	return left == right
}

const creationConflictMessage = "This session was created separately. The earlier creation request could not use this name."
const creationArchivedMessage = "This session was archived. The earlier creation request cannot be retried."

// Source snapshot failures require the user to review the plan again before
// retrying. Transport and model lookup failures leave the accepted request
// unchanged so it can be retried.
var errStaleSourcePlan = errors.New("source plan selection is stale")

// errSourceIdentityChanged marks durable source provenance which contradicts
// an accepted plan. It is deliberately separate from temporary runtime and
// App Server failures.
var errSourceIdentityChanged = errors.New("source session identity changed")

func (receipt creationReceipt) blocksSession() bool {
	return receipt.State != "ready" && receipt.State != "conflict" && receipt.State != "cancelled"
}

func (s *Server) canonicalCreationConflict(receipt creationReceipt) string {
	lock, err := session.LockRuntimeShared(s.config.AuthorityDir, receipt.Request.Slug)
	if err != nil {
		return ""
	}
	defer lock.Close()
	pending, err := s.creationJournalPresent(receipt.Request.Slug, false)
	if err != nil || pending {
		return ""
	}
	summary, err := session.Find(s.config.Workspace, receipt.Request.Slug)
	if err != nil || summary.Codex.ThreadID == "" || summary.Creation.State != "ready" ||
		(summary.Creation.GoalSHA256 != "" && !summary.Creation.InitialGoalSent) {
		return ""
	}
	if summary.Archived {
		return creationArchivedMessage
	}
	// A cancelled managed plan is deliberately unvalidated: it proved only that
	// it never reached a destination effect. If a later direct CLI start has
	// created the canonical destination at the same slug, reconcile that fact
	// rather than leaving the obsolete terminal receipt visible forever.
	if receipt.State == "cancelled" {
		return creationConflictMessage
	}
	bound, err := s.readCreationBinding(receipt)
	if err != nil {
		return ""
	}
	if !bound {
		return creationConflictMessage
	}
	// A binding can precede every destination journal. The preceding package
	// ignores that private binding, so its independently completed session may
	// occupy the name after rollback. Preserve ambiguous matching state for CLI
	// recovery; reconcile only canonical state that contradicts this request.
	if receipt.Request.Kind == "fork" {
		// A successful receipt-bound fork publishes evidence before removing
		// its journal. Binding alone cannot resume an existing destination.
		if _, err := os.Lstat(s.creationEvidencePath(receipt)); errors.Is(err, os.ErrNotExist) {
			return creationConflictMessage
		}
		if summary.ForkedFrom != receipt.Request.Source || summary.Creation.GoalSHA256 != "" {
			return creationConflictMessage
		}
		return ""
	}
	if summary.Creation.GoalSHA256 != creationGoalDigest(receipt.Goal) || summary.ForkedFrom != "" {
		return creationConflictMessage
	}
	var journal map[string]any
	path := filepath.Join(s.config.Workspace, "worktrees", ".locks", receipt.Request.Slug+".creation.json")
	if readCreationJSON(path, &journal) == nil && journal["schema"] == float64(1) &&
		journal["slug"] == receipt.Request.Slug && journal["state"] == "ready" &&
		(journal["model"] != receipt.Model || journal["effort"] != receipt.Effort) {
		return creationConflictMessage
	}
	return ""
}

func (s *Server) currentCreation(slug string) (creationReceipt, bool) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if err := s.retireAbsentCreation(slug); err != nil {
		s.config.Logger.Printf("retire absent creation %s: %v", slug, err)
	}
	receipt, ok := s.creations[slug]
	if !ok || receipt.State == "running" || receipt.State == "ready" || receipt.State == "conflict" {
		return receipt, ok
	}
	if receipt.State == "cancelled" {
		if message := s.canonicalCreationConflict(receipt); message != "" {
			receipt.State = "conflict"
			receipt.Phase = message
			receipt.Error = "Choose another name to start the earlier request."
		} else {
			return receipt, true
		}
	} else if s.proveCreation(receipt) == nil {
		if err := s.bindCreationUploads(s.operationContext, receipt); err != nil {
			return receipt, true
		}
		receipt.State = "ready"
		receipt.Phase = "Session is ready."
		receipt.Error = ""
	} else if message := s.canonicalCreationConflict(receipt); message != "" {
		receipt.State = "conflict"
		receipt.Phase = message
		receipt.Error = "Choose another name to start the earlier request."
	} else {
		return receipt, true
	}
	receipt.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.saveCreation(receipt); err != nil {
		// Canonical tracking remains usable even when recording the conflict
		// fails. This in-memory state is reconciled again after a restart.
		s.config.Logger.Printf("reconcile creation %s: %v", slug, err)
		if receipt.State != "conflict" {
			receipt.State = "paused"
			receipt.Error = err.Error()
		}
	}
	s.creations[slug] = receipt
	return receipt, true
}

func (s *Server) creationPage(w http.ResponseWriter, r *http.Request, slug string) bool {
	receipt, ok := s.currentCreation(slug)
	if !ok || (!receipt.blocksSession() && receipt.State != "cancelled") {
		return false
	}
	s.render(w, "creation", pageData{InitialRequest: receipt.status().InitialRequest, Session: &session.Summary{Manifest: session.Manifest{Slug: slug}}})
	return true
}

func (s *Server) creationAPI(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if parts[1] != "creation" {
		return false
	}
	if len(parts) == 2 && r.Method == http.MethodGet {
		receipt, ok := s.currentCreation(parts[0])
		if !ok {
			http.NotFound(w, r)
		} else {
			s.writeJSON(w, http.StatusOK, receipt.status())
		}
		return true
	}
	if len(parts) == 3 && parts[2] == "retry" && r.Method == http.MethodPost {
		s.withCreationMutation(w, r, func() { s.retryCreation(w, r, parts[0]) })
		return true
	}
	http.NotFound(w, r)
	return true
}

func (s *Server) retryCreation(w http.ResponseWriter, r *http.Request, slug string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		ReceiptID string `json:"receiptId"`
		Attempt   int    `json:"attempt"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	receipt, ok := s.currentCreation(slug)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	current := s.creations[slug]
	if body.ReceiptID != current.ReceiptID || body.Attempt != current.Attempt || receipt.ReceiptID != current.ReceiptID {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Creation status changed. Reload before retrying."})
		return
	}
	if current.State == "conflict" || current.State == "cancelled" {
		s.writeJSON(w, http.StatusConflict, current.status())
		return
	}
	if current.State == "running" || current.State == "ready" {
		s.writeJSON(w, http.StatusAccepted, current.status())
		return
	}
	if s.closing {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Portal is stopping. Retry shortly."})
		return
	}
	current.Attempt++
	current.State = "running"
	current.Error = ""
	current.Phase = "Checking recorded request…"
	current.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.saveCreation(current); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.creations[slug] = current
	s.operationWG.Add(1)
	go s.runCreation(current)
	s.writeJSON(w, http.StatusAccepted, current.status())
}

// A receipt alone is never evidence that a CLI operation completed. The CLI
// writes this exact receipt-bound evidence under its existing creation lock,
// before deleting a fork journal or returning successful start metadata.
type creationEvidence struct {
	Schema                 int    `json:"schema"`
	Workspace              string `json:"workspace"`
	Slug                   string `json:"slug"`
	ReceiptID              string `json:"receiptId"`
	ThreadID               string `json:"threadId"`
	SourceThreadID         string `json:"sourceThreadId"`
	GoalSHA256             string `json:"goalSha256"`
	Model                  string `json:"model"`
	Effort                 string `json:"effort"`
	TrackingDevice         uint64 `json:"trackingDevice"`
	TrackingInode          uint64 `json:"trackingInode"`
	AgentTeamBindingDigest string `json:"agentTeamBindingDigest,omitempty"`
	CatalogDigest          string `json:"catalogDigest,omitempty"`
	TeamDigest             string `json:"teamDigest,omitempty"`
	Team                   string `json:"team,omitempty"`
	CreationIdentity       string `json:"creationIdentity,omitempty"`
	RemovalEpoch           string `json:"removalEpoch,omitempty"`
}

func (s *Server) proveCreation(receipt creationReceipt) error {
	var proof creationEvidence
	if err := readCreationJSON(s.creationEvidencePath(receipt), &proof); err != nil {
		return err
	}
	if proof.Schema != receipt.Schema || (proof.Schema != 1 && proof.Schema != 2 && proof.Schema != 3) || proof.Workspace != s.config.Workspace || proof.Slug != receipt.Request.Slug ||
		proof.ReceiptID != receipt.ReceiptID || proof.ThreadID == "" || proof.SourceThreadID != receipt.Request.SourceThreadID ||
		proof.Model != receipt.Model || proof.Effort != receipt.Effort || !receipt.Validated {
		return errors.New("creation completion evidence does not match the recorded request")
	}
	if receipt.Request.Kind != "fork" && proof.GoalSHA256 != creationGoalDigest(receipt.Goal) {
		return errors.New("creation completion goal changed")
	}
	summary, err := session.Find(s.config.Workspace, receipt.Request.Slug)
	if err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Lstat(filepath.Join(s.config.Workspace, summary.Root, summary.Slug), &stat); err != nil {
		return err
	}
	if stat.Dev != proof.TrackingDevice || stat.Ino != proof.TrackingInode ||
		summary.Codex.ThreadID != proof.ThreadID || summary.Creation.State != "ready" ||
		(summary.Creation.GoalSHA256 != "" && !summary.Creation.InitialGoalSent) {
		return errors.New("completed session identity changed")
	}
	if receipt.Request.Kind != "fork" && summary.Creation.GoalSHA256 != proof.GoalSHA256 {
		return errors.New("completed session goal does not match the recorded request")
	}
	if receipt.Request.Kind == "fork" && summary.ForkedFrom != receipt.Request.Source {
		return errors.New("fork source changed")
	}
	pending, err := s.creationJournalPresent(receipt.Request.Slug, false)
	if err != nil {
		return err
	}
	if pending {
		return errors.New("creation completion is recorded; retry to finish its journal")
	}
	if receipt.Schema == 2 {
		return errors.New("virtual team creation cannot resume after the direct-team cutover")
	}
	if receipt.Schema == 3 {
		if receipt.DirectTeam == nil || proof.CatalogDigest != receipt.DirectTeam.CatalogDigest ||
			proof.TeamDigest != receipt.DirectTeam.TeamDigest || proof.Team != receipt.DirectTeam.ID {
			return errors.New("direct team creation completion evidence does not match the recorded request")
		}
		store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
		if err != nil {
			return err
		}
		roster, err := store.Load(receipt.Request.Slug, proof.ThreadID)
		if err != nil {
			return fmt.Errorf("load direct team roster: %w", err)
		}
		if err := rosterMatchesDirectTeam(*roster, *receipt.DirectTeam); err != nil {
			return err
		}
	}

	return nil
}

func rosterMatchesDirectTeam(roster teamruntime.Roster, preset teamruntime.Preset) error {
	if roster.PresetID != preset.ID || roster.CatalogDigest != preset.CatalogDigest || roster.TeamDigest != preset.TeamDigest ||
		roster.LeadModel != preset.LeadModel || roster.LeadEffort != preset.LeadEffort ||
		roster.LeadInstructions != preset.LeadInstructions || len(roster.Members) != len(preset.Members) {
		return errors.New("direct team roster does not match the creation snapshot")
	}
	members := make(map[string]teamruntime.Member, len(roster.Members))
	for _, member := range roster.Members {
		members[member.Address] = member
	}
	for _, expected := range preset.Members {
		member, ok := members[expected.Address]
		if !ok || member.Role != expected.Role || member.Model != expected.Model || member.Effort != expected.Effort ||
			member.Behavior != expected.Behavior || member.Access != expected.Access ||
			member.Purpose != expected.Purpose || member.Instructions != expected.Instructions ||
			member.Thread == "" || member.State != "ready" {
			return errors.New("direct team roster does not prove all configured member threads")
		}
	}
	return nil
}

func (s *Server) updateCreation(receipt creationReceipt) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	current := s.creations[receipt.Request.Slug]
	if current.ReceiptID != receipt.ReceiptID || current.Attempt != receipt.Attempt {
		return errors.New("creation attempt changed")
	}
	receipt.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.saveCreation(receipt); err != nil {
		return err
	}
	s.creations[receipt.Request.Slug] = receipt
	return nil
}

func (s *Server) runCreation(receipt creationReceipt) {
	defer s.operationWG.Done()
	err := s.initializeCreation(&receipt)
	if err == nil {
		receipt.State = "ready"
		receipt.Phase = "Session is ready."
		receipt.Error = ""
	} else {
		receipt.State = "failed"
		receipt.Phase = "Session initialization stopped."
		receipt.Error = err.Error()
		if len(receipt.Error) > 4096 {
			receipt.Error = receipt.Error[:4096]
		}
		if s.operationContext.Err() != nil {
			receipt.State = "paused"
			receipt.Phase = "Initialization was interrupted. Retry to continue."
		}
	}
	if err := s.updateCreation(receipt); err != nil {
		s.config.Logger.Printf("save creation outcome for %s: %v", receipt.Request.Slug, err)
		// A finished goroutine must not leave the page claiming that work is
		// still running when persistence failed. Keep retry available; a later
		// status read can also prove and durably reconcile completed work.
		s.operationMu.Lock()
		current := s.creations[receipt.Request.Slug]
		if current.ReceiptID == receipt.ReceiptID && current.Attempt == receipt.Attempt {
			receipt.State = "paused"
			receipt.Phase = "Unable to save initialization status. Retry to check the result."
			receipt.Error = err.Error()
			s.creations[receipt.Request.Slug] = receipt
		}
		s.operationMu.Unlock()
	}
}

func (s *Server) initializeCreation(receipt *creationReceipt) error {
	if receipt.Schema == 2 {
		return errors.New("virtual team creation cannot resume after the direct-team cutover")
	}
	ctx, cancel := context.WithTimeout(s.operationContext, 4*time.Minute)
	defer cancel()
	transition, unlock, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		return err
	}
	if receipt.Validated && s.proveCreation(*receipt) == nil {
		return s.bindCreationUploads(ctx, *receipt)
	}
	request := receipt.Request
	var unlockSnapshot func()
	defer func() {
		if unlockSnapshot != nil {
			unlockSnapshot()
		}
	}()
	if request.Source != "" && !receipt.Validated {
		lock := s.messageLock(request.Source)
		if err := lock.Lock(ctx); err != nil {
			return err
		}
		unlockSnapshot = lock.Unlock
	}
	if request.Source != "" && !receipt.Validated {
		source, identity, err := s.creationSource(request.Source)
		if err != nil {
			if errors.Is(err, errSourceIdentityChanged) {
				return fmt.Errorf("%w: %v", errStaleSourcePlan, err)
			}
			return err
		}
		if identity != request.SourceIdentity || source.Codex.ThreadID != request.SourceThreadID {
			return fmt.Errorf("%w: source session changed; the recorded request cannot use its replacement", errStaleSourcePlan)
		}
		s.normalizeInteractivity(ctx, source)
		if !source.Interactive {
			return errors.New("source session is not ready for browser changes")
		}
		if !receipt.Validated {
			if s.config.Codex == nil {
				return errors.New("Codex is unavailable")
			}
			transcript, err := s.config.Codex.ReadThread(ctx, request.SourceThreadID)
			if err != nil {
				return err
			}
			if request.Kind == "plan" {
				plan, ok := completedPlan(transcript)
				if !ok || plan.TurnID != request.PlanTurnID || planDigest(plan.Text) != request.PlanSHA256 || (request.PlanText != "" && plan.Text != request.PlanText) {
					return fmt.Errorf("%w: the displayed plan is stale; review the latest plan before implementing it", errStaleSourcePlan)
				}
				if transcript.Status == "active" || transcript.CollaborationMode != "plan" {
					return fmt.Errorf("%w: the source conversation is no longer ready to implement this plan", errStaleSourcePlan)
				}
				if receipt.Schema == 1 && request.Model != "" && (transcript.Model != request.Model || transcript.ReasoningEffort != request.Effort) {
					return errors.New("source model settings changed; review the plan before implementing it")
				}
				receipt.Goal = planCreationGoal(request.Source, plan.Text)
				if receipt.Schema == 1 {
					receipt.Model = transcript.Model
					receipt.Effort = transcript.ReasoningEffort
				}
			} else {
				models, err := s.config.Codex.ListModels(ctx)
				if err != nil {
					return err
				}
				settings, err := codex.ResolveForkThreadSettings(models,
					codex.ThreadSettings{Model: transcript.Model, ReasoningEffort: transcript.ReasoningEffort},
					codex.ThreadSettings{Model: request.Model, ReasoningEffort: request.Effort})
				if err != nil {
					return err
				}
				receipt.Model = settings.Model
				receipt.Effort = settings.ReasoningEffort
			}
		}
	}
	if !receipt.Validated {
		if request.Kind == "new" {
			receipt.Goal = request.Goal
			settings := codex.ThreadSettings{Model: request.Model, ReasoningEffort: request.Effort}
			if receipt.Schema == 2 || receipt.Schema == 3 {
				settings = codex.ThreadSettings{Model: receipt.Model, ReasoningEffort: receipt.Effort}
			}
			if receipt.Schema == 1 && settings.Model != "" {
				if s.config.Codex == nil {
					return errors.New("Codex model catalog is unavailable")
				}
				models, err := s.config.Codex.ListModels(ctx)
				if err != nil {
					return err
				}
				settings, err = workspacecodex.ResolveNewThreadSettings(models, settings)
				if err != nil {
					return err
				}
			}
			if receipt.Schema == 1 {
				receipt.Model = settings.Model
				receipt.Effort = settings.ReasoningEffort
			}
		}
		if request.Kind != "fork" && (receipt.Goal == "" || len(receipt.Goal) > session.MaxMessageBytes) {
			return errors.New("initial request is too large or empty")
		}
		if err := s.validateModelSettings(ctx, codex.ThreadSettings{Model: receipt.Model, ReasoningEffort: receipt.Effort}, true); err != nil {
			return err
		}
		receipt.Validated = true
		receipt.Phase = "Initializing the conversation and terminal…"
		if err := s.updateCreation(*receipt); err != nil {
			return err
		}
	}
	if unlockSnapshot != nil {
		unlockSnapshot()
		unlockSnapshot = nil
	}
	// Revalidate availability on retries without changing the captured settings.
	allowCapturedDefault := request.Kind == "new" &&
		request.Model == "" && request.Effort == "" &&
		receipt.Model == "" && receipt.Effort == ""
	if err := s.validateModelSettings(ctx, codex.ThreadSettings{Model: receipt.Model, ReasoningEffort: receipt.Effort}, allowCapturedDefault); err != nil {
		return err
	}
	args := []string{}
	if request.Kind == "fork" {
		args = []string{"fork", request.Source, request.Slug, "--as-is", "--json"}
	} else {
		file, err := os.CreateTemp(s.creationDirectory(), ".goal-*.txt")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if _, err = file.WriteString(receipt.Goal); err != nil {
			file.Close()
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
		args = []string{"start", request.Slug, "--as-is", "--exclusive", "--no-attach", "--goal-file", file.Name(), "--json"}
	}
	if receipt.Schema == 3 {
		if receipt.DirectTeam == nil {
			return errors.New("direct creation receipt has no team snapshot")
		}
		presetPath, err := writeDirectTeamPreset(s.creationDirectory(), *receipt.DirectTeam)
		if err != nil {
			return err
		}
		defer os.Remove(presetPath)
		args = append(args, "--team-preset", presetPath)
	}
	if receipt.Model != "" {
		args = append(args, "--model", receipt.Model)
	}
	if receipt.Effort != "" {
		args = append(args, "--effort", receipt.Effort)
	}
	args = append(args, "--creation-receipt-id", receipt.ReceiptID, "--creation-evidence", s.creationEvidencePath(*receipt))
	if request.Source != "" {
		args = append(args, "--expected-source", request.Source, "--expected-source-thread", request.SourceThreadID, "--expected-source-identity", request.SourceIdentity)
	}
	stdout, stderr, err := s.runDevSessionWithTransition(ctx, 210*time.Second, transition, args...)
	if err != nil {
		if s.proveCreation(*receipt) == nil {
			return s.bindCreationUploads(ctx, *receipt)
		}
		return commandFailure("initialize session", stdout, stderr, err)
	}
	var result struct {
		Slug string `json:"slug"`
	}
	if json.Unmarshal([]byte(stdout), &result) != nil || result.Slug != request.Slug {
		return errors.New("dev-session returned invalid session metadata")
	}
	if err := s.proveCreation(*receipt); err != nil {
		return err
	}
	return s.bindCreationUploads(ctx, *receipt)
}

func writeDirectTeamPreset(directory string, preset teamruntime.Preset) (string, error) {
	if err := validDirectTeamPreset(preset); err != nil {
		return "", err
	}
	data, err := json.Marshal(preset)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(directory, ".direct-team-*.json")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Chmod(0o600); err == nil {
		_, err = file.Write(append(data, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// Peek only the bounded action field. The same-session path retains its full
// runtime locking and transcript checks in sessionAPIForSummary.
func (s *Server) earlyCreationRequest(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if len(parts) != 2 || r.Method != http.MethodPost || (parts[1] != "fork" && parts[1] != "implement-plan") {
		return false
	}
	if parts[1] == "implement-plan" {
		r.Body = http.MaxBytesReader(w, r.Body, session.MaxFormRequestBodyBytes)
		var payload json.RawMessage
		if !s.decodeJSON(w, r, &payload) {
			return true
		}
		var action struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(payload, &action); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return true
		}
		r.Body = io.NopCloser(strings.NewReader(string(payload)))
		if strings.TrimSpace(action.Action) != "new" {
			return false
		}
	}
	s.withCreationMutation(w, r, func() {
		source, _, err := s.creationSource(parts[0])
		if err != nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if parts[1] == "fork" {
			s.forkSession(w, r, source)
		} else {
			s.implementPlan(w, r, source)
		}
	})
	return true
}

func (s *Server) acceptPlanCreation(w http.ResponseWriter, r *http.Request, source *session.Summary, request creationRequest) {
	current, identity, err := s.creationSource(source.Slug)
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	request.Source = current.Slug
	request.SourceThreadID = current.Codex.ThreadID
	request.SourceIdentity = identity
	if !messageDigestPattern.MatchString(request.PlanSHA256) || request.PlanTurnID == "" || len(request.PlanTurnID) > 256 ||
		(request.PlanText != "" && planDigest(request.PlanText) != request.PlanSHA256) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "approved plan identity is invalid"})
		return
	}
	if len(request.PlanText)+len(request.Source)+70 > session.MaxMessageBytes {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the approved plan is too large for a new session"})
		return
	}
	input := agentteams.CreationSelectionInput{
		Team:            agentteams.StringPresence{Present: request.teamSubmitted, Value: request.Team},
		CatalogDigest:   agentteams.StringPresence{Present: request.digestSubmitted, Value: request.CatalogDigest},
		Model:           agentteams.StringPresence{Present: request.modelSubmitted && request.Model != "", Value: request.Model},
		ReasoningEffort: agentteams.StringPresence{Present: request.effortSubmitted && request.Effort != "", Value: request.Effort},
	}
	resolved, resolveErr := s.resolveManagedCreation(r.Context(), request, input)
	if resolveErr != nil {
		status := http.StatusBadRequest
		if strings.Contains(resolveErr.Error(), "catalog digest is stale") {
			status = http.StatusConflict
		} else if strings.Contains(resolveErr.Error(), "launch evidence") || strings.Contains(resolveErr.Error(), "Codex socket") {
			status = http.StatusServiceUnavailable
		}
		s.writeJSON(w, status, map[string]string{"error": resolveErr.Error()})
		return
	}
	request = resolved
	settingsModel, settingsEffort := request.Model, request.Effort
	if request.directTeam != nil {
		settingsModel, settingsEffort = request.directTeam.LeadModel, request.directTeam.LeadEffort
	}
	if err := validateCreationSettings(settingsModel, settingsEffort); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	receipt, err := s.acceptCreation(request)
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusAccepted, receipt.status())
}

func (s *Server) readCreationBinding(receipt creationReceipt) (bool, error) {
	var binding struct {
		Schema                 int                 `json:"schema"`
		Workspace              string              `json:"workspace"`
		Slug                   string              `json:"slug"`
		Kind                   string              `json:"kind"`
		ReceiptID              string              `json:"receipt_id"`
		DeletionHistorySHA256  string              `json:"deletion_history_sha256"`
		SourceThread           string              `json:"source_thread"`
		SourceIdentity         string              `json:"source_identity"`
		GoalSHA256             *string             `json:"goal_sha256"`
		Model                  string              `json:"model"`
		Effort                 string              `json:"effort"`
		AgentTeamBinding       string              `json:"agent_team_binding,omitempty"`
		AgentTeamBindingDigest string              `json:"agent_team_binding_digest,omitempty"`
		DirectTeam             *teamruntime.Preset `json:"direct_team,omitempty"`
	}
	if err := readCreationJSON(s.creationEvidencePath(receipt)+".request", &binding); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	kind, goalMatches := "start", binding.GoalSHA256 != nil && *binding.GoalSHA256 == creationGoalDigest(receipt.Goal)
	if receipt.Request.Kind == "fork" {
		kind, goalMatches = "fork", binding.GoalSHA256 == nil
	}
	if !receipt.Validated || binding.Schema != receipt.Schema || binding.Workspace != s.config.Workspace || binding.Slug != receipt.Request.Slug || binding.Kind != kind ||
		binding.ReceiptID != receipt.ReceiptID || binding.SourceThread != receipt.Request.SourceThreadID || binding.SourceIdentity != receipt.Request.SourceIdentity ||
		!goalMatches || binding.Model != receipt.Model || binding.Effort != receipt.Effort || binding.DeletionHistorySHA256 != receipt.DeletionHistorySHA256 {
		return false, errors.New("creation binding does not match the recorded request")
	}
	if receipt.Schema == 2 {
		if binding.AgentTeamBinding != receipt.AgentTeamBinding ||
			binding.AgentTeamBindingDigest != receipt.AgentTeamBindingDigest {
			return false, errors.New("managed creation binding does not match the recorded request")
		}
	}
	if receipt.Schema == 3 {
		if receipt.DirectTeam == nil || binding.DirectTeam == nil || !reflect.DeepEqual(*binding.DirectTeam, *receipt.DirectTeam) {
			return false, errors.New("direct creation binding does not match the recorded request")
		}
	}
	return true, nil
}
