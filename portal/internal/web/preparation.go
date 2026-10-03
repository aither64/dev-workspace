package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"golang.org/x/sys/unix"
)

type preparationStatus struct {
	RequestID      string `json:"requestId"`
	ReceiptID      string `json:"receiptId"`
	Attempt        int    `json:"attempt"`
	URL            string `json:"url"`
	Slug           string `json:"slug,omitempty"`
	CanonicalURL   string `json:"canonicalUrl,omitempty"`
	State          string `json:"state"`
	Phase          string `json:"phase"`
	Detail         string `json:"detail,omitempty"`
	Error          string `json:"error,omitempty"`
	InitialRequest string `json:"initialRequest,omitempty"`
	StartedAt      string `json:"startedAt"`
	UpdatedAt      string `json:"updatedAt"`
}

// A pending launch is repaired once in this process. A dispatched worker that
// stops needs explicit retry; confirmation must never dispatch it again.
type preparationWork struct {
	attempt    int
	dispatched bool
	stopped    bool
}

// Published records replace their immutable input snapshots. Capture the record,
// work and existence together so response projection never rereads preparation
// state after confirmation releases its locks.
type preparationStatusSnapshot struct {
	record sessionPreparation
	work   preparationWork
	exists bool
}

func (s *Server) writePreparationError(w http.ResponseWriter, r *http.Request, id string, status int, err error) {
	if errors.Is(err, errPreparationPersistenceUnconfirmed) || errors.Is(err, uploads.ErrPersistenceUnconfirmed) {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"requestId": id, "code": "preparation_persistence_unconfirmed",
			"error": "The portal could not confirm the saved request. Try again.",
		})
		return
	}
	s.writeError(w, r, status, err.Error())
}

func (s *Server) confirmPreparationStatus(ctx context.Context, id string) (preparationStatusSnapshot, error) {
	_, unlock, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return preparationStatusSnapshot{}, err
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		return preparationStatusSnapshot{}, err
	}
	if err := s.uploadCreationMu.Lock(ctx); err != nil {
		return preparationStatusSnapshot{}, err
	}
	defer s.uploadCreationMu.Unlock()
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if err := s.confirmPreparationLocked(id); err != nil {
		return preparationStatusSnapshot{}, err
	}
	record, exists := s.preparations[id]
	if exists && record.State == "accepting" {
		// An intent is not acceptance. Only its identical POST may finish the
		// upload claim and pending launch before browser storage is cleared.
		return preparationStatusSnapshot{}, errPreparationPersistenceUnconfirmed
	}
	return preparationStatusSnapshot{record: record, work: s.preparationWork[id], exists: exists}, nil
}

func looksLikePreparationPath(value *url.URL) bool {
	for _, prefix := range []string{"/creations", "/api/session-creations"} {
		if strings.HasPrefix(value.Path, prefix) || strings.HasPrefix(value.EscapedPath(), prefix) || strings.HasPrefix(path.Clean(value.Path), prefix) {
			return true
		}
	}
	return false
}

func preparationPathID(value *url.URL) (string, bool, bool) {
	if value.RawPath != "" || value.EscapedPath() != value.Path {
		return "", false, false
	}
	if strings.HasPrefix(value.Path, "/creations/") {
		id := strings.TrimSuffix(strings.TrimPrefix(value.Path, "/creations/"), "/")
		return id, false, validPreparationID(id) && value.Path == "/creations/"+id+"/"
	}
	rest, ok := strings.CutPrefix(value.Path, "/api/session-creations/")
	if !ok {
		return "", false, false
	}
	id := strings.TrimSuffix(rest, "/retry")
	retry := rest != id
	return id, retry, validPreparationID(id) && (rest == id || rest == id+"/retry")
}

func validPreparationPath(value *url.URL) bool { _, _, valid := preparationPathID(value); return valid }

func (s *Server) preparationRoute(w http.ResponseWriter, r *http.Request) {
	id, retry, valid := preparationPathID(r.URL)
	if !valid {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost && retry {
		s.withCreationMutation(w, r, func() { s.retryPreparation(w, r, id) })
		return
	}
	if r.Method != http.MethodGet || retry {
		http.NotFound(w, r)
		return
	}
	snapshot, err := s.confirmPreparationStatus(r.Context(), id)
	if err != nil {
		s.writePreparationError(w, r, id, http.StatusServiceUnavailable, err)
		return
	}
	status, ok := s.preparationStatusFromSnapshot(id, snapshot)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.writeJSON(w, http.StatusOK, status)
		return
	}
	// Browser endpoint wiring is a separate implementation unit. The existing
	// progress template already renders accepted text safely before JS loads.
	s.render(w, "creation", pageData{InitialRequest: status.InitialRequest, Session: &session.Summary{Manifest: session.Manifest{Slug: id}}})
}

func (s *Server) preparationStatus(id string) (preparationStatus, bool) {
	s.operationMu.Lock()
	record, ok := s.preparations[id]
	work := s.preparationWork[id]
	s.operationMu.Unlock()
	return s.preparationStatusFromSnapshot(id, preparationStatusSnapshot{record: record, work: work, exists: ok})
}

func (s *Server) preparationStatusFromSnapshot(id string, snapshot preparationStatusSnapshot) (preparationStatus, bool) {
	if !snapshot.exists {
		return preparationStatus{}, false
	}
	record, work := snapshot.record, snapshot.work
	status := preparationStatus{RequestID: id, ReceiptID: record.ReceiptID, Attempt: record.Attempt,
		URL: "/creations/" + id + "/", State: record.State, Phase: record.Phase, Error: record.Error,
		StartedAt: record.StartedAt, UpdatedAt: record.UpdatedAt}
	if record.Snapshot != nil {
		status.InitialRequest = record.Snapshot.Input.RawPrompt
	}
	if record.State == "running" && work.attempt == record.Attempt && (!work.dispatched || work.stopped) {
		status.State, status.Phase = "paused", "stopped"
		status.Error = "Session preparation stopped. Retry to continue."
	}
	if record.Slug == "" {
		return status, true
	}
	if record.State == "terminal" {
		epoch, err := session.CompletedRemovalHistory(s.config.Workspace, record.Slug, s.config.UserStateRoot)
		status.State = record.Terminal
		if err != nil || epoch != record.Epoch {
			status.State = "gone"
			status.Error = "The original session is no longer available."
			return status, true
		}
		if record.Terminal == "ready" {
			if _, err := session.Find(s.config.Workspace, record.Slug); err != nil {
				status.State, status.Error = "gone", "The original session is no longer available."
				return status, true
			}
			s.operationMu.Lock()
			current, exists := s.creations[record.Slug]
			s.operationMu.Unlock()
			if exists && current.ReceiptID != record.ReceiptID {
				status.State = "gone"
				status.Error = "The recorded session was replaced."
				return status, true
			}
			status.Slug, status.CanonicalURL = record.Slug, "/"+record.Slug+"/"
		}
		return status, true
	}
	receipt, exists := s.currentCreation(record.Slug)
	if exists && preparationReceiptMatches(record, receipt) {
		status.Slug, status.CanonicalURL = record.Slug, "/"+record.Slug+"/"
		status.State, status.Phase, status.Detail = receipt.State, "initializing", receipt.Phase
		status.Attempt, status.UpdatedAt, status.Error = receipt.Attempt, receipt.UpdatedAt, receipt.Error
		if receipt.State == "conflict" || receipt.State == "cancelled" {
			status.CanonicalURL = ""
		}
	} else if record.State == "handed_off" {
		status.State, status.Error = "conflict", "The recorded session creation identity changed."
	}
	return status, true
}

func preparationCreationRequest(record sessionPreparation) creationRequest {
	input := record.Snapshot.Input
	request := creationRequest{Kind: "new", Slug: record.Slug, Goal: record.Snapshot.Goal,
		Model: strings.TrimSpace(input.Model.Value), Effort: strings.TrimSpace(input.Effort.Value)}
	if record.Snapshot.Preset != nil {
		request.Team, request.CatalogDigest = record.Snapshot.Preset.ID, record.Snapshot.Preset.CatalogDigest
		request.directTeam = record.Snapshot.Preset
	}
	return request
}

func preparationReceiptMatches(record sessionPreparation, receipt creationReceipt) bool {
	if record.Snapshot == nil || record.Handoff == nil {
		return false
	}
	request := preparationCreationRequest(record)
	expectedSchema := 1
	if request.directTeam != nil {
		expectedSchema = 3
	}
	return receipt.Schema == expectedSchema && receipt.Workspace == record.Workspace && receipt.ReceiptID == record.ReceiptID &&
		receipt.DeletionHistorySHA256 == record.Epoch && sameCreationRequest(receipt.Request, request) &&
		reflect.DeepEqual(receipt.DirectTeam, request.directTeam) &&
		(request.directTeam == nil || (receipt.Model == request.directTeam.LeadModel && receipt.Effort == request.directTeam.LeadEffort))
}

func (s *Server) freezePreparationPreset(input preparationInput) (*teamruntime.Preset, error) {
	if !input.Team.Present && !input.Catalog.Present {
		if s.installedTeams != nil && s.installedTeams.Managed {
			return nil, errors.New("select a starting team and reload the form")
		}
		if err := validateCreationSettings(strings.TrimSpace(input.Model.Value), strings.TrimSpace(input.Effort.Value)); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if !input.Team.Present || !input.Catalog.Present || input.Model.Present != input.Effort.Present ||
		s.installedTeams == nil || !s.installedTeams.Managed || s.installedTeams.Catalog == nil {
		return nil, errors.New("invalid starting team selection")
	}
	preset, err := teamruntime.FindCatalogPreset(*s.installedTeams.Catalog, strings.TrimSpace(input.Team.Value))
	if err != nil {
		return nil, err
	}
	if input.Catalog.Value != preset.CatalogDigest {
		return nil, errors.New("catalog digest is stale; reload the form")
	}
	model, effort := strings.TrimSpace(input.Model.Value), strings.TrimSpace(input.Effort.Value)
	if model != "" || effort != "" {
		if model == "" || effort == "" || validateCreationSettings(model, effort) != nil {
			return nil, errors.New("select a Codex model and reasoning effort together")
		}
		preset.LeadModel, preset.LeadEffort = model, effort
	}
	if err := validDirectTeamPreset(preset); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(preset)
	if err != nil {
		return nil, err
	}
	var frozen teamruntime.Preset
	if err := json.Unmarshal(encoded, &frozen); err != nil {
		return nil, err
	}
	return &frozen, nil
}

func preparationForm(r *http.Request) (string, preparationInput, error) {
	var input preparationInput
	for key, values := range r.Form {
		if key != "attachmentIds" && len(values) != 1 {
			return "", input, errors.New("duplicate session request field")
		}
		switch key {
		case "clientRequestId", "goal", "creation_date", "name", "uploadScope", "attachmentIds", "team", "catalogDigest", "model", "effort":
		default:
			return "", input, errors.New("unknown session request field")
		}
	}
	id := r.FormValue("clientRequestId")
	if !validPreparationID(id) {
		return "", input, errors.New("invalid session request ID")
	}
	present := func(key string) preparationValue { _, ok := r.Form[key]; return preparationValue{ok, r.FormValue(key)} }
	input = preparationInput{RawPrompt: r.FormValue("goal"), Date: r.FormValue("creation_date"), CustomName: strings.TrimSpace(r.FormValue("name")),
		UploadScope: r.FormValue("uploadScope"), Attachments: append([]string{}, r.Form["attachmentIds"]...),
		Team: present("team"), Catalog: present("catalogDigest"), Model: present("model"), Effort: present("effort")}
	return id, input, nil
}

func (s *Server) createPreparation(w http.ResponseWriter, r *http.Request) {
	id, input, err := preparationForm(r)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.uploadCreationMu.Lock(r.Context()); err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer s.uploadCreationMu.Unlock()
	s.operationMu.Lock()
	record, exists := s.preparations[id]
	digest := inputPreparationDigest(input)
	if exists {
		if record.InputDigest != digest {
			s.operationMu.Unlock()
			s.writeError(w, r, http.StatusConflict, "Session request ID was reused with different input.")
			return
		}
		err = s.confirmPreparationLocked(id)
		if work := s.preparationWork[id]; err == nil && s.closing && work.attempt == record.Attempt && !work.dispatched {
			err = errors.New("portal is stopping")
		}
		if err == nil && record.State == "accepting" {
			if s.closing {
				err = errors.New("portal is stopping")
			} else {
				err = s.finishAcceptingPreparationLocked(r.Context(), &record)
			}
		}
		if err == nil && !s.closing {
			s.launchPreparationLocked(s.preparations[id])
		}
	} else {
		if s.closing {
			err = errors.New("portal is stopping")
		} else {
			err = s.preparationCapacity()
		}
		var preset *teamruntime.Preset
		if err == nil {
			preset, err = s.freezePreparationPreset(input)
		}
		if err == nil {
			var raw [32]byte
			_, err = rand.Read(raw[:])
			now := time.Now().UTC().Format(time.RFC3339Nano)
			record = sessionPreparation{Schema: 1, Workspace: s.config.Workspace, RequestID: id, InputVersion: 1, InputDigest: digest,
				ReceiptID: hex.EncodeToString(raw[:]), Attempt: 1, State: "accepting", Phase: "accepting", StartedAt: now, UpdatedAt: now,
				Snapshot: &preparationSnapshot{Input: input, Preset: preset}}
			record.SnapshotDigest = preparationDigest(record.Snapshot)
			if err == nil {
				err = validatePreparation(record, s.config.Workspace)
			}
			if err == nil {
				err = s.uploadStore.ValidatePreparation(r.Context(), id, input.UploadScope, input.RawPrompt, input.Attachments)
			}
			if err == nil {
				err = s.savePreparationForDispatchLocked(record)
			}
			if err == nil {
				err = s.finishAcceptingPreparationLocked(r.Context(), &record)
			}
			if err == nil {
				s.launchPreparationLocked(record)
			}
		}
	}
	s.operationMu.Unlock()
	if err != nil {
		s.writePreparationError(w, r, id, http.StatusConflict, err)
		return
	}
	status, _ := s.preparationStatus(id)
	if r.Header.Get("Accept") == "application/json" {
		s.writeJSON(w, http.StatusAccepted, status)
	} else {
		http.Redirect(w, r, status.URL, http.StatusSeeOther)
	}
}

func (s *Server) finishAcceptingPreparationLocked(ctx context.Context, record *sessionPreparation) error {
	if err := s.confirmPreparationLocked(record.RequestID); err != nil {
		return err
	}
	input := record.Snapshot.Input
	goal, err := s.uploadStore.ClaimPreparation(ctx, record.RequestID, input.UploadScope, input.RawPrompt, input.Attachments)
	if err != nil {
		return err
	}
	if record.Snapshot.Goal != "" && record.Snapshot.Goal != goal {
		return errors.New("Accepted attachment goal changed.")
	}
	snapshot := *record.Snapshot
	snapshot.Goal = goal
	record.Snapshot, record.SnapshotDigest = &snapshot, preparationDigest(&snapshot)
	record.State, record.Phase = "running", "naming"
	record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return s.savePreparationForDispatchLocked(*record)
}

func (s *Server) savePreparationForDispatchLocked(record sessionPreparation) error {
	err := s.savePreparation(record)
	if published, ok := s.preparations[record.RequestID]; ok && published.Attempt == record.Attempt &&
		(published.State == "accepting" || published.State == "running") {
		s.pendingPreparationLocked(published)
	}
	return err
}

func (s *Server) pendingPreparationLocked(record sessionPreparation) {
	if s.preparationWork == nil {
		s.preparationWork = make(map[string]preparationWork)
	}
	work := s.preparationWork[record.RequestID]
	if work.attempt != record.Attempt {
		s.preparationWork[record.RequestID] = preparationWork{attempt: record.Attempt}
	}
}

func (s *Server) launchPreparationLocked(record sessionPreparation) {
	work := s.preparationWork[record.RequestID]
	if s.closing || record.State != "running" || work.attempt != record.Attempt || work.dispatched || s.preparationUnconfirmed[record.RequestID] != "" {
		return
	}
	work.dispatched = true
	s.preparationWork[record.RequestID] = work
	s.operationWG.Add(1)
	go s.runPreparation(record.RequestID, record.Attempt)
}

// Every worker mutation reacquires the package-transition lock, then upload
// coordination and operationMu. No model call runs under these locks.
func (s *Server) mutatePreparation(ctx context.Context, id string, attempt int, mutate func(*sessionPreparation) error) error {
	_, unlock, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		return err
	}
	if err := s.uploadCreationMu.Lock(ctx); err != nil {
		return err
	}
	defer s.uploadCreationMu.Unlock()
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	record, ok := s.preparations[id]
	if !ok || record.Attempt != attempt || record.State == "terminal" || s.closing {
		return errors.New("session preparation attempt changed or stopped")
	}
	if err := s.confirmPreparationLocked(id); err != nil {
		return err
	}
	if err := mutate(&record); err != nil {
		return err
	}
	record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return s.savePreparation(record)
}

func (s *Server) runPreparation(id string, attempt int) {
	defer s.operationWG.Done()
	defer func() {
		s.operationMu.Lock()
		work := s.preparationWork[id]
		if work.attempt == attempt {
			work.stopped = true
			s.preparationWork[id] = work
		}
		s.operationMu.Unlock()
	}()
	s.operationMu.Lock()
	record := s.preparations[id]
	s.operationMu.Unlock()
	var err error
	if record.Snapshot.Goal == "" || len(record.Snapshot.Input.Attachments) > 0 {
		err = s.mutatePreparation(s.operationContext, id, attempt, func(current *sessionPreparation) error {
			return s.finishAcceptingPreparationLocked(s.operationContext, current)
		})
	}
	if err == nil && record.Base == "" {
		base, outcome := record.Snapshot.Input.CustomName, "custom"
		if base == "" {
			base, outcome, err = s.namePreparation(s.operationContext, record.Snapshot.Input.RawPrompt)
		}
		if err == nil {
			err = s.mutatePreparation(s.operationContext, id, attempt, func(current *sessionPreparation) error {
				current.Base, current.NamingOutcome, current.Phase = base, outcome, "reserving"
				return nil
			})
		}
	}
	if err == nil {
		err = s.mutatePreparation(s.operationContext, id, attempt, s.reservePreparationLocked)
	}
	if err != nil {
		// Shutdown leaves the last durable running record for startup to pause.
		// A superseded package never gets a separate unguarded outcome write.
		if s.operationContext.Err() != nil {
			return
		}
		_ = s.mutatePreparation(s.operationContext, id, attempt, func(current *sessionPreparation) error {
			current.State, current.Phase, current.Error = "failed", "stopped", err.Error()
			if len(current.Error) > 4096 {
				current.Error = current.Error[:4096]
			}
			return nil
		})
	}
}

func (s *Server) destinationOccupiedLocked(slug, owner string) (bool, error) {
	if receipt, ok := s.creations[slug]; ok && receipt.ReceiptID != owner {
		return true, nil
	}
	for _, record := range s.preparations {
		if record.State != "terminal" && record.Slug == slug && record.ReceiptID != owner {
			return true, nil
		}
	}
	for _, root := range []string{"work", "archive", "worktrees"} {
		if _, err := os.Lstat(filepath.Join(s.config.Workspace, root, slug)); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	pending, err := s.creationJournalPresent(slug, true)
	if err != nil || pending {
		return pending, err
	}
	for _, file := range []string{filepath.Join(s.config.AuthorityDir, slug+".json"), filepath.Join(s.config.AuthorityDir, slug+".start.json"),
		filepath.Join(userstate.WorkspaceDirectory(s.config.UserStateRoot, "codex-teams", s.config.Workspace), slug+".json")} {
		if _, err := os.Lstat(file); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func (s *Server) reservePreparationLocked(record *sessionPreparation) error {
	for number := 1; number <= maxPreparationMappings+1; number++ {
		slug := record.Slug
		if slug == "" {
			slug = record.Snapshot.Input.Date + "-" + suffixedSessionName(record.Base, number)
		}
		creationLock, err := session.LockCreation(s.config.AuthorityDir, slug)
		if err != nil {
			return err
		}
		lock, err := session.LockRuntimeExclusive(s.config.AuthorityDir, slug)
		if err != nil {
			creationLock.Close()
			return err
		}
		err = s.handoffPreparationLocked(record, slug)
		lock.Close()
		creationLock.Close()
		if errors.Is(err, errPreparationOccupied) && record.Slug == "" && record.Snapshot.Input.CustomName == "" {
			continue
		}
		return err
	}
	return errors.New("session name collision limit reached")
}

var errPreparationOccupied = errors.New("session name already exists")

func (s *Server) handoffPreparationLocked(record *sessionPreparation, slug string) error {
	if previous, ok := s.creations[slug]; ok && record.Slug != "" {
		if !preparationReceiptMatches(*record, previous) {
			return errors.New("session receipt does not match the accepted reservation")
		}
	} else {
		occupied, err := s.destinationOccupiedLocked(slug, record.ReceiptID)
		if err != nil {
			return err
		}
		if occupied {
			return errPreparationOccupied
		}
	}
	if record.Slug == "" {
		epoch, err := session.CompletedRemovalHistory(s.config.Workspace, slug, s.config.UserStateRoot)
		if err != nil {
			return err
		}
		record.Slug, record.Epoch = slug, epoch
		request := preparationCreationRequest(*record)
		record.Handoff = &request
		if err := s.savePreparation(*record); err != nil {
			return err
		}
	}
	input := record.Snapshot.Input
	if len(input.Attachments) > 0 {
		if err := s.uploadStore.BindPreparation(s.operationContext, record.RequestID, input.UploadScope, record.Snapshot.Goal, record.Slug, record.Epoch); err != nil {
			return err
		}
	}
	request := preparationCreationRequest(*record)
	if _, err := s.installCreationLocked(request, record.ReceiptID, record.Epoch, true); err != nil {
		return err
	}
	record.State, record.Phase, record.Error = "handed_off", "initializing", ""
	return nil
}

func (s *Server) retryPreparation(w http.ResponseWriter, r *http.Request, id string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		ReceiptID string `json:"receiptId"`
		Attempt   int    `json:"attempt"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	if err := s.uploadCreationMu.Lock(r.Context()); err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer s.uploadCreationMu.Unlock()
	s.operationMu.Lock()
	record, ok := s.preparations[id]
	if !ok {
		s.operationMu.Unlock()
		http.NotFound(w, r)
		return
	}
	if err := s.confirmPreparationLocked(id); err != nil {
		s.operationMu.Unlock()
		s.writePreparationError(w, r, id, http.StatusServiceUnavailable, err)
		return
	}
	if record.State == "handed_off" {
		receipt, exists := s.creations[record.Slug]
		if !exists || !preparationReceiptMatches(record, receipt) {
			s.operationMu.Unlock()
			s.writeError(w, r, http.StatusConflict, "Session creation identity changed.")
			return
		}
		s.operationMu.Unlock()
		// The ordinary endpoint retains sole ownership of attempts after handoff.
		_, statusCode, err := s.retryCreationReceipt(record.Slug, body.ReceiptID, body.Attempt)
		if err != nil {
			s.writeError(w, r, statusCode, err.Error())
			return
		}
		status, _ := s.preparationStatus(id)
		s.writeJSON(w, statusCode, status)
		return
	}
	if body.ReceiptID != record.ReceiptID || body.Attempt != record.Attempt {
		s.operationMu.Unlock()
		s.writeError(w, r, http.StatusConflict, "Creation status changed. Reload before retrying.")
		return
	}
	work := s.preparationWork[id]
	if s.closing && work.attempt == record.Attempt && !work.dispatched {
		s.operationMu.Unlock()
		s.writeError(w, r, http.StatusServiceUnavailable, "Portal is stopping.")
		return
	}
	if record.State == "terminal" || record.State == "running" && !(work.attempt == record.Attempt && work.stopped) {
		if !s.closing {
			s.launchPreparationLocked(record)
		}
		s.operationMu.Unlock()
		status, _ := s.preparationStatus(id)
		s.writeJSON(w, http.StatusAccepted, status)
		return
	}
	if s.closing {
		s.operationMu.Unlock()
		s.writeError(w, r, http.StatusServiceUnavailable, "Portal is stopping.")
		return
	}
	record.Attempt++
	record.State, record.Error = "running", ""
	if record.Snapshot.Goal == "" {
		record.Phase = "accepting"
	} else {
		record.Phase = "naming"
	}
	err := s.savePreparationForDispatchLocked(record)
	if err == nil {
		s.launchPreparationLocked(record)
	}
	s.operationMu.Unlock()
	if err != nil {
		s.writePreparationError(w, r, id, http.StatusInternalServerError, err)
		return
	}
	status, _ := s.preparationStatus(id)
	s.writeJSON(w, http.StatusAccepted, status)
}

func (s *Server) reconcilePreparations() error {
	_, unlock, err := s.acquireTransitionContext(s.operationContext, unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		return err
	}
	if err := s.uploadCreationMu.Lock(s.operationContext); err != nil {
		return err
	}
	defer s.uploadCreationMu.Unlock()
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	return s.reconcilePreparationsLocked(s.operationContext)
}

func (s *Server) reconcilePreparationsLocked(ctx context.Context) error {
	for _, record := range s.preparations {
		if err := s.confirmPreparationLocked(record.RequestID); err != nil {
			return err
		}
		work := s.preparationWork[record.RequestID]
		if work.attempt == record.Attempt && !work.dispatched {
			// A published intent alone does not retain attachments. Finish the
			// claim (or abort collection) without consuming its pending launch.
			if record.State == "accepting" || record.State == "running" && record.Snapshot.Goal == "" {
				if err := s.finishAcceptingPreparationLocked(ctx, &record); err != nil {
					return err
				}
			}
			continue
		}
		if record.State == "terminal" {
			if _, err := os.Lstat(s.preparationPath(record.RequestID, false)); err == nil {
				if err := s.savePreparation(record); err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if record.State == "paused" {
			if err := s.savePreparation(record); err != nil {
				return err
			}
		}
		if record.State == "accepting" {
			if err := s.finishAcceptingPreparationLocked(ctx, &record); err != nil {
				if errors.Is(err, errPreparationPersistenceUnconfirmed) || errors.Is(err, uploads.ErrPersistenceUnconfirmed) {
					return err
				}
				record.Error = err.Error()
			}
			record.State, record.Phase = "paused", "stopped"
			if err := s.savePreparation(record); err != nil {
				return err
			}
		}
		if record.State != "accepting" && record.Snapshot.Goal != "" && len(record.Snapshot.Input.Attachments) > 0 {
			input := record.Snapshot.Input
			goal, err := s.uploadStore.ClaimPreparation(ctx, record.RequestID, input.UploadScope, input.RawPrompt, input.Attachments)
			if errors.Is(err, uploads.ErrPersistenceUnconfirmed) {
				return err
			}
			if err != nil || goal != record.Snapshot.Goal {
				record.State, record.Phase, record.Error = "failed", "stopped", "Accepted attachment ownership changed."
				if err := s.savePreparation(record); err != nil {
					return err
				}
				continue
			}
		}
		if record.Handoff != nil {
			if receipt, exists := s.creations[record.Slug]; exists {
				if !preparationReceiptMatches(record, receipt) {
					record.State, record.Phase, record.Error = "failed", "stopped", "Session receipt identity changed."
				} else {
					record.State, record.Phase = "handed_off", "initializing"
				}
				if err := s.savePreparation(record); err != nil {
					return err
				}
				if err := s.compactPreparationsLocked(receipt); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Server) compactPreparationsLocked(receipt creationReceipt) error {
	if receipt.State != "ready" && receipt.State != "conflict" && receipt.State != "cancelled" {
		return nil
	}
	related := false
	for _, record := range s.preparations {
		if record.ReceiptID == receipt.ReceiptID && record.Slug == receipt.Request.Slug {
			related = true
			break
		}
	}
	if !related {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, unlock, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	cancel()
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		return err
	}
	for _, record := range s.preparations {
		if record.ReceiptID == receipt.ReceiptID && record.Slug == receipt.Request.Slug {
			if err := s.confirmPreparationLocked(record.RequestID); err != nil {
				return err
			}
		}
		if record.State == "terminal" || !preparationReceiptMatches(record, receipt) {
			continue
		}
		if receipt.State == "ready" {
			if s.uploadStore == nil {
				return errors.New("preparation compaction waits for upload reconciliation")
			}
			if err := s.proveCreation(receipt); err != nil {
				return err
			}
			if err := s.bindCreationUploads(s.operationContext, receipt); err != nil {
				return err
			}
		}
		record.State, record.Terminal, record.Phase = "terminal", receipt.State, "initializing"
		record.Attempt, record.Error = receipt.Attempt, receipt.Error
		if len(record.Error) > 512 {
			record.Error = record.Error[:512]
		}
		record.Snapshot, record.Handoff, record.Base, record.SnapshotDigest = nil, nil, "", ""
		record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := s.savePreparation(record); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) pausePreparationsOnClose() {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, unlock, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return
	}
	defer unlock()
	if s.requireCurrentHostProfile() != nil {
		return
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	for _, record := range s.preparations {
		if record.State != "running" {
			continue
		}
		record.State, record.Phase = "paused", "stopped"
		record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := s.savePreparation(record); err != nil {
			s.config.Logger.Printf("pause preparation %s: %v", record.RequestID, err)
		}
	}
}
