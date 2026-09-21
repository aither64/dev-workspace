package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"golang.org/x/sys/unix"
)

// Creation requests are intentionally separate from strict lifecycle journals.
// Old packages ignore these private files; the existing CLI journals retain
// their original format and continue to own runtime initialization.
type creationRequest struct {
	Kind           string `json:"kind"`
	Slug           string `json:"slug"`
	Goal           string `json:"goal,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	Source         string `json:"source,omitempty"`
	SourceThreadID string `json:"sourceThreadId,omitempty"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
	PlanTurnID     string `json:"planTurnId,omitempty"`
	PlanSHA256     string `json:"planSha256,omitempty"`
	PlanText       string `json:"planText,omitempty"`
	// Team and CatalogDigest identify a schema-2 virtual receipt or a schema-3
	// direct-thread receipt. Schema 3 retains its exact resolved roster below.
	// Empty advanced model/effort fields remain absent via omitempty.
	Team          string `json:"team,omitempty"`
	CatalogDigest string `json:"catalogDigest,omitempty"`

	directTeam      *teamruntime.Preset
	teamSubmitted   bool
	digestSubmitted bool
	modelSubmitted  bool
	effortSubmitted bool
}

type creationReceipt struct {
	Schema                int             `json:"schema"`
	Workspace             string          `json:"workspace"`
	Request               creationRequest `json:"request"`
	ReceiptID             string          `json:"receiptId"`
	DeletionHistorySHA256 string          `json:"deletionHistorySha256"`
	Attempt               int             `json:"attempt"`
	State                 string          `json:"state"`
	Phase                 string          `json:"phase"`
	Error                 string          `json:"error,omitempty"`
	StartedAt             string          `json:"startedAt"`
	UpdatedAt             string          `json:"updatedAt"`
	// Resolved settings and goal are immutable once validation has succeeded.
	Validated bool   `json:"validated"`
	Goal      string `json:"goal,omitempty"`
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"`
	// Schema 2 binds the receipt to the canonical, opaque selection token.
	// The token never appears in completion evidence.
	AgentTeamBinding       string              `json:"agentTeamBinding,omitempty"`
	AgentTeamBindingDigest string              `json:"agentTeamBindingDigest,omitempty"`
	DirectTeam             *teamruntime.Preset `json:"directTeam,omitempty"`
}

type creationStatus struct {
	InitialRequest string `json:"initialRequest,omitempty"`
	StartedAt      string `json:"startedAt"`
	UpdatedAt      string `json:"updatedAt"`
	SourceURL      string `json:"sourceUrl,omitempty"`
	CanonicalURL   string `json:"canonicalUrl,omitempty"`

	Slug      string `json:"slug"`
	URL       string `json:"url"`
	ReceiptID string `json:"receiptId"`
	Attempt   int    `json:"attempt"`
	State     string `json:"state"`
	Phase     string `json:"phase"`
	Error     string `json:"error,omitempty"`
}

func (receipt creationReceipt) status() creationStatus {
	sourceURL := ""
	if receipt.Request.Source != "" {
		sourceURL = "/" + receipt.Request.Source + "/"
	}
	canonicalURL := ""
	// Conflict means a separate canonical destination exists. Cancelled means
	// pre-effect plan validation stopped before any destination existed.
	if receipt.State == "conflict" {
		canonicalURL = "/" + receipt.Request.Slug + "/"
	}
	goal := receipt.Goal
	if goal == "" {
		goal = receipt.Request.Goal
	}
	if goal == "" && receipt.Request.Kind == "plan" && receipt.Request.PlanText != "" {
		goal = planCreationGoal(receipt.Request.Source, receipt.Request.PlanText)
	}
	return creationStatus{InitialRequest: goal, CanonicalURL: canonicalURL, Slug: receipt.Request.Slug, URL: "/" + receipt.Request.Slug + "/", ReceiptID: receipt.ReceiptID,
		Attempt: receipt.Attempt, State: receipt.State, Phase: receipt.Phase, Error: receipt.Error,
		StartedAt: receipt.StartedAt, UpdatedAt: receipt.UpdatedAt, SourceURL: sourceURL}
}

func (s *Server) creationDirectory() string {
	return filepath.Join(s.operationStore.directory, "creations")
}
func (s *Server) creationPath(slug string) string {
	return filepath.Join(s.creationDirectory(), slug+".json")
}
func (s *Server) creationEvidencePath(receipt creationReceipt) string {
	return filepath.Join(s.creationDirectory(), receipt.Request.Slug+"."+receipt.ReceiptID+".complete.json")
}

// The accepted and resolved message snapshots may each use six JSON bytes
// per input byte (control characters or HTML escaping).
const maxCreationReceiptBytes = 12*session.MaxMessageBytes + 16384
const maxCreationReceipts = 512

// Terminal receipts are a bounded retry cache. Conflict records describe a
// separate canonical session; they never prove the request was completed.
func (s *Server) retireReadyCreations(reserve int) error {
	if len(s.creations)+reserve <= maxCreationReceipts {
		return nil
	}
	// A CLI deletion may finish without another request for that slug.
	// Reclaim those receipts before treating the remaining ones as unfinished.
	for slug, receipt := range s.creations {
		if receipt.State == "paused" || receipt.State == "failed" {
			if err := s.retireAbsentCreation(slug); err != nil {
				return err
			}
		}
	}
	var terminal []creationReceipt
	for _, receipt := range s.creations {
		if receipt.State != "ready" && receipt.State != "conflict" && receipt.State != "cancelled" {
			continue
		}
		terminal = append(terminal, receipt)
	}
	sort.Slice(terminal, func(i, j int) bool {
		if terminal[i].UpdatedAt == terminal[j].UpdatedAt {
			return terminal[i].Request.Slug < terminal[j].Request.Slug
		}
		return terminal[i].UpdatedAt < terminal[j].UpdatedAt
	})
	for _, receipt := range terminal {
		if len(s.creations)+reserve <= maxCreationReceipts {
			break
		}
		if err := s.retireIdleCreation(receipt); err != nil {
			return err
		}
	}
	if len(s.creations)+reserve > maxCreationReceipts {
		return errors.New("too many unfinished session creations")
	}
	return nil
}

func (s *Server) retireIdleCreation(receipt creationReceipt) error {
	lock, err := session.LockRuntimeShared(s.config.AuthorityDir, receipt.Request.Slug)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return nil
	} else if err != nil {
		return err
	}
	defer lock.Close()
	pending, err := s.creationJournalPresent(receipt.Request.Slug, false)
	if err != nil || pending {
		return err
	}
	return s.retireCreation(receipt)
}

// Call with operationMu held. Removing the receipt last keeps an interrupted
// retirement recoverable; a ready receipt no longer needs CLI binding evidence.
func (s *Server) retireCreation(receipt creationReceipt) error {
	for _, path := range []string{s.creationEvidencePath(receipt) + ".request", s.creationEvidencePath(receipt), s.creationPath(receipt.Request.Slug)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("retire creation receipt: %w", err)
		}
	}
	directory, err := os.Open(s.creationDirectory())
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return err
	}
	delete(s.creations, receipt.Request.Slug)
	return nil
}

// Completed deletion releases a slug even when the retry cache is not full.
// Existing tracking, authority and journals preserve the old identity until
// their owning lifecycle operation has finished.
func (s *Server) retireAbsentCreation(slug string) error {
	receipt, ok := s.creations[slug]
	// A cancelled schema-2 plan has no destination to keep this receipt alive,
	// but it remains the user-visible record of the failed request. Capacity
	// eviction through retireReadyCreations reclaims it later.
	if !ok || receipt.State == "running" || receipt.State == "cancelled" {
		return nil
	}
	lock, err := session.LockRuntimeShared(s.config.AuthorityDir, slug)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return nil
	} else if err != nil {
		return err
	}
	defer lock.Close()
	paths := []string{filepath.Join(s.config.AuthorityDir, slug+".json")}
	for _, root := range []string{"work", "archive", "worktrees"} {
		paths = append(paths, filepath.Join(s.config.Workspace, root, slug))
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	pending, err := s.creationJournalPresent(slug, true)
	if err != nil || pending {
		return err
	}
	if receipt.State != "ready" && receipt.State != "conflict" {
		history, err := session.CompletedRemovalHistory(s.config.Workspace, slug, s.config.UserStateRoot)
		if err != nil || history == receipt.DeletionHistorySHA256 {
			return err
		}
	}

	return s.retireCreation(receipt)
}

func (s *Server) creationJournalPresent(slug string, includeCompleted bool) (bool, error) {
	owner, err := session.PendingLifecycle(s.config.Workspace, slug)
	if err != nil || owner != "" {
		return owner != "", err
	}
	for _, suffix := range []string{".start.json", ".fork.json"} {
		_, err := os.Lstat(filepath.Join(s.config.Workspace, "worktrees", ".locks", slug+suffix))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	journal, present, err := agentteams.ReadCreationJournal(s.config.Workspace, slug)
	if err != nil {
		return true, err
	}
	if !present {
		return false, nil
	}
	return includeCompleted || journal.State != "ready", nil
}

// completedCreationJournal keeps the existing receipt tests on the shared
// strict journal decoder. A malformed record cannot look completed, while a
// ready record remains terminal for every supported schema.
func completedCreationJournal(raw map[string]json.RawMessage, slug string) (bool, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return false, errors.New("creation journal has an invalid shape")
	}
	journal, err := agentteams.DecodeCreationJournal(data, slug)
	if err != nil {
		return false, err
	}
	return journal.State == "ready", nil
}

func readCreationJSON(path string, target any) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		stat.Uid != uint32(os.Geteuid()) || info.Size() < 1 || info.Size() > maxCreationReceiptBytes {
		return errors.New("creation state must be a private bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) > maxCreationReceiptBytes {
		return errors.New("creation state exceeds bounds")
	}
	if err := rejectDuplicateCreationJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("creation state has trailing data")
	}
	return nil
}

func rejectDuplicateCreationJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("creation state contains a duplicate JSON key")
				}
				seen[name] = true
				if err := value(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := value(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		default:
			return errors.New("creation state has an invalid JSON delimiter")
		}
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("creation state has trailing data")
	}
	return nil
}

func rawCreationObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("creation state value must be a JSON object")
	}
	return object, nil
}

func rejectCreationNulls(object map[string]json.RawMessage, fields ...string) error {
	for _, field := range fields {
		if value, ok := object[field]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("creation state %s must be omitted instead of null", field)
		}
	}
	return nil
}

func validateCreationReceiptShape(raw map[string]json.RawMessage) error {
	allowed := map[string]bool{}
	for _, field := range []string{
		"schema", "workspace", "request", "receiptId", "deletionHistorySha256", "attempt", "state", "phase", "error", "startedAt", "updatedAt", "validated", "goal", "model", "effort", "agentTeamBinding", "agentTeamBindingDigest", "directTeam",
	} {
		allowed[field] = true
	}
	for field := range raw {
		if !allowed[field] {
			return fmt.Errorf("creation receipt has an unknown field %q", field)
		}
	}
	for _, field := range []string{"schema", "workspace", "request", "receiptId", "deletionHistorySha256", "attempt", "state", "phase", "startedAt", "updatedAt", "validated"} {
		if _, ok := raw[field]; !ok {
			return fmt.Errorf("creation receipt lacks %q", field)
		}
	}
	if err := rejectCreationNulls(raw, "schema", "workspace", "request", "receiptId", "deletionHistorySha256", "attempt", "state", "phase", "error", "startedAt", "updatedAt", "validated", "goal", "model", "effort", "agentTeamBinding", "agentTeamBindingDigest", "directTeam"); err != nil {
		return err
	}
	var schema int
	if err := json.Unmarshal(raw["schema"], &schema); err != nil || (schema != 1 && schema != 2 && schema != 3) {
		return errors.New("creation receipt has an unsupported schema")
	}
	request, err := rawCreationObject(raw["request"])
	if err != nil {
		return err
	}
	if schema == 1 {
		requestAllowed := map[string]bool{}
		for _, field := range []string{"kind", "slug", "goal", "model", "effort", "source", "sourceThreadId", "sourceIdentity", "planTurnId", "planSha256", "planText", "team", "catalogDigest"} {
			requestAllowed[field] = true
		}
		for field := range request {
			if !requestAllowed[field] {
				return fmt.Errorf("creation receipt request has an unknown field %q", field)
			}
		}
		for _, field := range []string{"kind", "slug"} {
			if _, ok := request[field]; !ok {
				return fmt.Errorf("creation receipt request lacks %q", field)
			}
		}
		if err := rejectCreationNulls(request, "kind", "slug", "goal", "model", "effort", "source", "sourceThreadId", "sourceIdentity", "planTurnId", "planSha256", "planText", "team", "catalogDigest"); err != nil {
			return err
		}
		if _, ok := raw["agentTeamBinding"]; ok {
			return errors.New("schema-1 receipt has a managed binding")
		}
		if _, ok := raw["agentTeamBindingDigest"]; ok {
			return errors.New("schema-1 receipt has a managed binding digest")
		}
		if _, ok := raw["directTeam"]; ok {
			return errors.New("schema-1 receipt has a direct team")
		}
		if _, ok := request["team"]; ok {
			return errors.New("schema-1 receipt has a team")
		}
		if _, ok := request["catalogDigest"]; ok {
			return errors.New("schema-1 receipt has a catalog digest")
		}
		return nil
	}
	if schema == 3 {
		if _, ok := raw["agentTeamBinding"]; ok {
			return errors.New("schema-3 receipt has a virtual binding")
		}
		if _, ok := raw["agentTeamBindingDigest"]; ok {
			return errors.New("schema-3 receipt has a virtual binding digest")
		}
		presetRaw, ok := raw["directTeam"]
		if !ok {
			return errors.New("schema-3 receipt lacks a direct team")
		}
		var preset teamruntime.Preset
		decoder := json.NewDecoder(bytes.NewReader(presetRaw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&preset); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
			return errors.New("schema-3 receipt has an invalid direct team")
		}
		if err := validDirectTeamPreset(preset); err != nil {
			return fmt.Errorf("schema-3 receipt has an invalid direct team: %w", err)
		}
		for _, field := range []string{"team", "catalogDigest"} {
			if _, ok := request[field]; !ok {
				return fmt.Errorf("schema-3 request lacks %q", field)
			}
		}
		if err := validateManagedCreationRequestShape(request); err != nil {
			return err
		}
		return nil
	}
	if _, ok := raw["directTeam"]; ok {
		return errors.New("schema-2 receipt has a direct team")
	}
	for _, field := range []string{"agentTeamBinding", "agentTeamBindingDigest"} {
		if _, ok := raw[field]; !ok {
			return fmt.Errorf("schema-2 receipt lacks %q", field)
		}
	}
	for _, field := range []string{"team", "catalogDigest"} {
		if _, ok := request[field]; !ok {
			return fmt.Errorf("schema-2 request lacks %q", field)
		}
	}
	return validateManagedCreationRequestShape(request)
}

// Schema-2 request records are a durable, policy-bearing replay contract. They
// intentionally do not share a permissive union: a new creation must never
// acquire source or plan provenance, and a plan cannot acquire a new-session
// goal. Schema 1 retains its existing unmanaged reader above.
func validateManagedCreationRequestShape(request map[string]json.RawMessage) error {
	kind, err := creationRequestString(request, "kind", 16)
	if err != nil {
		return err
	}

	var required, optional []string
	switch kind {
	case "new":
		required = []string{"kind", "slug", "goal", "team", "catalogDigest"}
		optional = []string{"model", "effort"}
	case "plan":
		required = []string{"kind", "slug", "source", "sourceThreadId", "sourceIdentity", "planTurnId", "planSha256", "team", "catalogDigest"}
		optional = []string{"planText", "model", "effort"}
	default:
		return errors.New("schema-2 creation request has an invalid kind")
	}
	if err := exactCreationRequestFields(request, required, optional); err != nil {
		return err
	}

	slug, err := creationRequestString(request, "slug", 256)
	if err != nil || !session.ValidSlug(slug) {
		return errors.New("schema-2 creation request has an invalid slug")
	}
	team, err := creationRequestString(request, "team", 63)
	if err != nil || !validManagedCreationTeam(team) {
		return errors.New("schema-2 creation request has an invalid team")
	}
	catalogDigest, err := creationRequestString(request, "catalogDigest", 64)
	if err != nil || !messageDigestPattern.MatchString(catalogDigest) {
		return errors.New("schema-2 creation request has an invalid catalog digest")
	}
	model, err := optionalCreationRequestString(request, "model", session.MaxAgentTeamPersistedScalarBytes)
	if err != nil {
		return err
	}
	effort, err := optionalCreationRequestString(request, "effort", session.MaxAgentTeamPersistedScalarBytes)
	if err != nil {
		return err
	}
	for _, value := range []string{model, effort} {
		if err := validateCreationSettingValue(value); err != nil {
			return fmt.Errorf("schema-2 creation request has invalid model settings: %w", err)
		}
	}

	if kind == "new" {
		goal, err := creationRequestString(request, "goal", session.MaxMessageBytes)
		if err != nil || !validCreationRequestText(goal, session.MaxMessageBytes) {
			return errors.New("schema-2 new creation request has an invalid goal")
		}
		return nil
	}

	source, err := creationRequestString(request, "source", 256)
	if err != nil || !session.ValidSlug(source) {
		return errors.New("schema-2 plan creation request has an invalid source")
	}
	for _, field := range []string{"sourceThreadId", "planTurnId"} {
		value, err := creationRequestString(request, field, 256)
		if err != nil || !validCreationOpaqueID(value, 256) {
			return fmt.Errorf("schema-2 plan creation request has an invalid %s", field)
		}
	}
	sourceIdentity, err := creationRequestString(request, "sourceIdentity", 64)
	if err != nil || !messageDigestPattern.MatchString(sourceIdentity) {
		return errors.New("schema-2 plan creation request has an invalid source identity")
	}
	planDigest, err := creationRequestString(request, "planSha256", 64)
	if err != nil || !messageDigestPattern.MatchString(planDigest) {
		return errors.New("schema-2 plan creation request has an invalid plan digest")
	}
	if plan, err := optionalCreationRequestString(request, "planText", session.MaxMessageBytes); err != nil ||
		(plan != "" && !validCreationRequestText(plan, session.MaxMessageBytes)) {
		return errors.New("schema-2 plan creation request has an invalid plan text")
	}
	return nil
}

func exactCreationRequestFields(request map[string]json.RawMessage, required, optional []string) error {
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, field := range append(append([]string{}, required...), optional...) {
		allowed[field] = true
	}
	for field := range request {
		if !allowed[field] {
			return fmt.Errorf("schema-2 creation request has an invalid field %q", field)
		}
	}
	for _, field := range required {
		if _, ok := request[field]; !ok {
			return fmt.Errorf("schema-2 creation request lacks %q", field)
		}
	}
	return rejectCreationNulls(request, append(required, optional...)...)
}

func creationRequestString(request map[string]json.RawMessage, field string, maximum int) (string, error) {
	raw, ok := request[field]
	if !ok {
		return "", fmt.Errorf("schema-2 creation request lacks %q", field)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("schema-2 creation request %q must be omitted instead of null", field)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || !validCreationRequestText(value, maximum) {
		return "", fmt.Errorf("schema-2 creation request has an invalid %q", field)
	}
	return value, nil
}

func optionalCreationRequestString(request map[string]json.RawMessage, field string, maximum int) (string, error) {
	if _, ok := request[field]; !ok {
		return "", nil
	}
	return creationRequestString(request, field, maximum)
}

func validCreationRequestText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validCreationOpaqueID(value string, maximum int) bool {
	if !validCreationRequestText(value, maximum) {
		return false
	}
	for _, character := range value {
		if character <= 0x1f || character == 0x7f {
			return false
		}
	}
	return true
}

func validManagedCreationTeam(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_') {
			return false
		}
	}
	return true
}

func (s *Server) loadCreations() error {
	s.creations = make(map[string]creationReceipt)
	entries, err := os.ReadDir(s.creationDirectory())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		slug := strings.TrimSuffix(entry.Name(), ".json")
		if !strings.HasSuffix(entry.Name(), ".json") || !session.ValidSlug(slug) {
			continue
		}
		var raw map[string]json.RawMessage
		if err := readCreationJSON(s.creationPath(slug), &raw); err != nil {
			return fmt.Errorf("load creation %s: %w", slug, err)
		}
		if err := validateCreationReceiptShape(raw); err != nil {
			return fmt.Errorf("invalid session creation receipt for %s: %w", slug, err)
		}
		var receipt creationReceipt
		if err := readCreationJSON(s.creationPath(slug), &receipt); err != nil {
			return fmt.Errorf("load creation %s: %w", slug, err)
		}
		if (receipt.Schema != 1 && receipt.Schema != 2 && receipt.Schema != 3) || receipt.Workspace != s.config.Workspace || receipt.Request.Slug != slug ||
			!messageDigestPattern.MatchString(receipt.ReceiptID) || !messageDigestPattern.MatchString(receipt.DeletionHistorySHA256) || receipt.Attempt < 1 ||
			(receipt.Request.Kind != "new" && receipt.Request.Kind != "fork" && receipt.Request.Kind != "plan") ||
			(receipt.State != "running" && receipt.State != "paused" && receipt.State != "failed" && receipt.State != "ready" && receipt.State != "conflict" && receipt.State != "cancelled") ||
			(receipt.State == "cancelled" && (receipt.Schema != 2 || receipt.Request.Kind != "plan" || receipt.Validated)) ||
			(receipt.Schema == 1 && (receipt.Request.Team != "" || receipt.Request.CatalogDigest != "" || receipt.AgentTeamBinding != "" || receipt.AgentTeamBindingDigest != "" || receipt.DirectTeam != nil)) ||
			(receipt.Schema == 2 && ((receipt.Request.Kind != "new" && receipt.Request.Kind != "plan") || receipt.Request.Team == "" || !messageDigestPattern.MatchString(receipt.Request.CatalogDigest) || receipt.AgentTeamBinding == "" || !messageDigestPattern.MatchString(receipt.AgentTeamBindingDigest) || receipt.Model == "" || receipt.Effort == "")) {
			return fmt.Errorf("invalid session creation receipt for %s", slug)
		}
		if receipt.Schema == 2 {
			if err := validateCreationSettings(receipt.Model, receipt.Effort); err != nil ||
				!validCreationRequestText(receipt.Model, session.MaxAgentTeamPersistedScalarBytes) || !validCreationRequestText(receipt.Effort, session.MaxAgentTeamPersistedScalarBytes) {
				return fmt.Errorf("invalid managed session creation settings for %s", slug)
			}
			// Schema-2 receipts are retained only for an explicit failure on
			// retry. Their virtual binding no longer grants launch authority.
		}
		if receipt.Schema == 3 {
			if (receipt.Request.Kind != "new" && receipt.Request.Kind != "plan") || receipt.DirectTeam == nil ||
				receipt.AgentTeamBinding != "" || receipt.AgentTeamBindingDigest != "" || receipt.Request.Team != receipt.DirectTeam.ID ||
				receipt.Request.CatalogDigest != receipt.DirectTeam.CatalogDigest || receipt.Model != receipt.DirectTeam.LeadModel ||
				receipt.Effort != receipt.DirectTeam.LeadEffort || validDirectTeamPreset(*receipt.DirectTeam) != nil ||
				validateCreationSettings(receipt.Model, receipt.Effort) != nil {
				return fmt.Errorf("invalid direct session creation receipt for %s", slug)
			}
		}
		if receipt.State == "running" {
			receipt.State = "paused"
			receipt.Phase = "Initialization was interrupted. Retry to continue."
		}
		s.creations[slug] = receipt
		if receipt.State == "paused" || receipt.State == "failed" {
			if err := s.retireAbsentCreation(slug); err != nil {
				return err
			}
		}
		if err := s.retireReadyCreations(0); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) saveCreation(receipt creationReceipt) error {
	if err := os.MkdirAll(s.creationDirectory(), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if len(data) > maxCreationReceiptBytes {
		return errors.New("session creation receipt is too large")
	}
	file, err := os.CreateTemp(s.creationDirectory(), ".creation-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(file.Name(), s.creationPath(receipt.Request.Slug)); err != nil {
		return err
	}
	directory, err := os.Open(s.creationDirectory())
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
