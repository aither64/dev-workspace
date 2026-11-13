package monitoroperation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

var idPattern = regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

func validID(id string) bool         { return idPattern.MatchString(id) }
func canonicalPath(path string) bool { return filepath.IsAbs(path) && filepath.Clean(path) == path }
func bounded(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
func digest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func derivedID(operation, slot string) string {
	return digest([]string{"monitor", operation, slot})[:32]
}

// SameOwner compares live ownership; the originating turn and epoch remain
// immutable provenance. Launch additionally requires the original epoch.
func SameOwner(a, b Owner) bool {
	return a.Slug == b.Slug && a.RootThreadID == b.RootThreadID && a.Incarnation == b.Incarnation && a.Socket == b.Socket
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func validateStart(req StartRequest) error {
	if !validID(req.ID) || strings.TrimSpace(req.Brief) == "" || !bounded(req.Brief, 20_000) || !bounded(req.CancellationScope, 4096) {
		return errors.New("monitor start requires a stable ID and bounded nonempty brief")
	}
	source := req.Source
	encoded, err := json.Marshal(source)
	if err != nil || len(encoded) > 16*1024 {
		return errors.New("monitor source is too large")
	}
	for _, reference := range source.EnvironmentReferences {
		if reference == "" || strings.Contains(reference, "=") || !bounded(reference, 1024) {
			return errors.New("monitor environment references must name nonsecret inputs without values")
		}
	}
	switch source.Kind {
	case "local":
		if source.InvocationID != req.ID || !canonicalPath(source.Cwd) || len(source.Argv) == 0 ||
			strings.TrimSpace(source.InputIdentity) == "" || strings.TrimSpace(source.Revision) == "" ||
			source.RunID != "" || source.RunAttempt != 0 || source.Repository != "" || source.Provider != "" || source.WorkflowID != "" || source.Commit != "" {
			return errors.New("local monitor source requires exact argv, cwd, input, revision and invocation identity")
		}
		for _, arg := range source.Argv {
			if !bounded(arg, 4096) {
				return errors.New("invalid monitor argv")
			}
		}
		if source.Argv[0] == "" {
			return errors.New("monitor command is empty")
		}
	case "ci":
		if source.Provider == "" || source.Repository == "" || source.WorkflowID == "" || source.RunID == "" || source.RunAttempt < 1 ||
			source.Commit == "" || len(source.Argv) != 0 || source.Cwd != "" || source.InvocationID != "" {
			return errors.New("CI monitor source requires exact provider, repository, workflow, run, attempt and commit")
		}
	default:
		return errors.New("monitor source kind must be local or ci")
	}
	for _, field := range []string{source.InputIdentity, source.Revision, source.Provider, source.Repository, source.WorkflowID, source.RunID, source.Commit} {
		if !bounded(field, 4096) {
			return errors.New("invalid monitor source text")
		}
	}
	return nil
}

// validateReport retains the persisted report contract. New-event invariants
// belong in AddEvent after its exact immutable replay check.
func validateReport(req ReportRequest) error {
	if req.Slot != "escalation" && req.Slot != "terminal" {
		return errors.New("monitor report slot must be escalation or terminal")
	}
	if req.Outcome != "passed" && req.Outcome != "failed" && req.Outcome != "incomplete" {
		return errors.New("invalid monitor outcome")
	}
	if req.ElapsedMillis < 0 || !bounded(req.Detail, 4096) || !bounded(req.StillRunning, 4096) || !bounded(req.Conclusion, 256) || len(req.Evidence) > 16 {
		return errors.New("invalid monitor report evidence or elapsed time")
	}
	for _, evidence := range req.Evidence {
		if !bounded(evidence, 1024) {
			return errors.New("monitor evidence reference exceeds bound")
		}
	}
	if req.Slot == "escalation" && (req.Outcome != "incomplete" || req.Detail == "" || req.StillRunning == "") {
		return errors.New("monitor escalation requires incomplete outcome, reason and still-running identity")
	}
	if req.Outcome == "incomplete" && req.Detail == "" {
		return errors.New("incomplete monitor outcome requires a reason")
	}
	if req.Outcome != "incomplete" && req.ExitStatus == nil && req.Conclusion == "" {
		return errors.New("terminal monitor outcome requires authoritative exit status or CI conclusion")
	}
	if req.Outcome == "passed" && (req.ExitStatus != nil && *req.ExitStatus != 0 || req.Conclusion != "" && req.Conclusion != "success") {
		return errors.New("passed monitor outcome contradicts authoritative result")
	}
	if req.Outcome == "failed" && (req.ExitStatus != nil && *req.ExitStatus == 0 || req.Conclusion == "success") {
		return errors.New("failed monitor outcome contradicts authoritative result")
	}
	return nil
}

// AddEvent is shared with the cold utility observer. The slot payload is
// immutable; identical retries return the original event and receipt unchanged.
func AddEvent(op *Operation, req ReportRequest, at time.Time) error {
	if err := validateReport(req); err != nil {
		return err
	}
	for _, event := range op.Events {
		if event.Report.Slot != req.Slot {
			continue
		}
		if reflect.DeepEqual(event.Report, req) {
			return nil
		}
		return errors.New("monitor event slot already contains a different immutable outcome")
	}
	if req.Slot == "terminal" && req.Outcome != "incomplete" && req.StillRunning != "" {
		return errors.New("passed or failed monitor report requires empty or omitted stillRunning; use incomplete for unfinished or uncertain work and detail/evidence for completed work")
	}
	if req.Slot == "escalation" {
		for _, event := range op.Events {
			if event.Report.Slot == "terminal" {
				return errors.New("monitor operation already has a terminal outcome")
			}
		}
	}
	id := derivedID(op.ID, req.Slot)
	body, err := json.Marshal(struct {
		OperationID string        `json:"operationId"`
		Source      Source        `json:"source"`
		Report      ReportRequest `json:"report"`
	}{op.ID, op.Source, req})
	if err != nil {
		return err
	}
	status := "pending"
	if op.CanceledAt != nil || op.Disposition != "" {
		status = "canceled"
	}
	op.Events = append(op.Events, Event{ID: id, CreatedAt: at.UTC(), Report: req, Delivery: Delivery{
		Status: status, MessageID: id, Context: "monitor:" + op.ID + ":" + id,
		Envelope: "Monitoring operation outcome (runtime-owned):\n\n" + string(body),
	}})
	return nil
}

func validateOperation(op Operation) error {
	if op.Pi != nil {
		if op.Schema != 2 || op.ID != op.Pi.ID || op.Workspace != op.Pi.Workspace {
			return errors.New("Pi monitor projection differs")
		}
		return op.Pi.Validate()
	}
	if op.Disposition != "" && op.Disposition != "archiving" && op.Disposition != "archived" {
		return errors.New("invalid monitor lifecycle disposition")
	}
	if op.Schema != Schema || !canonicalPath(op.Workspace) || !session.ValidSlug(op.Owner.Slug) ||
		op.Owner.RootThreadID == "" || op.Owner.Incarnation == "" || op.Owner.OriginTurnID == "" ||
		!canonicalPath(op.Owner.Socket) || op.Owner.SocketEpoch == "" || op.Owner.RuntimeSchema < 1 || op.CreatedAt.IsZero() {
		return errors.New("invalid monitor operation owner or schema")
	}
	req := StartRequest{op.ID, op.Brief, op.Source, op.CancellationScope}
	if err := validateStart(req); err != nil {
		return err
	}
	if op.RequestDigest != digest(req) {
		return errors.New("monitor request digest differs")
	}
	p := op.UtilityPolicy
	if !canonicalPath(p.PackageRoot) || len(p.CatalogDigest) != 64 || p.NativeName == "" || p.Model == "" || p.Effort == "" ||
		!bounded(p.Instructions, 64*1024) || p.Instructions == "" || p.MaxConcurrent < 1 ||
		(p.Sandbox != "danger-full-access" && p.Sandbox != "workspace-write" && p.Sandbox != "read-only") || p.ApprovalPolicy != "never" {
		return errors.New("invalid retained monitor utility policy")
	}
	if op.Launch.MessageID != derivedID(op.ID, "launch") || !bounded(op.Launch.Envelope, 40*1024) || op.Launch.Envelope == "" ||
		op.Launch.CreateAttempted && op.Launch.ProjectID == "" || op.Launch.BootstrapAttempted && (op.Launch.ThreadID == "" || op.Launch.BootstrapMarker == "") ||
		op.Launch.Receipt != nil && (op.Launch.Receipt.ClientUserMessageID != op.Launch.MessageID || op.Launch.Receipt.TurnID == "") {
		return errors.New("invalid monitor launch identity")
	}
	seen := map[string]bool{}
	if op.Resolution != nil {
		r := op.Resolution
		if !validID(r.ID) || r.At.IsZero() || r.LeadThreadID != op.Owner.RootThreadID || validateResolution(ResolutionRequest{op.ID, r.ID, r.Evidence, r.Detail}) != nil {
			return errors.New("invalid monitor evidence resolution")
		}
	}
	if len(op.Events) > 2 {
		return errors.New("too many monitor events")
	}
	for _, event := range op.Events {
		if err := validateReport(event.Report); err != nil {
			return err
		}
		if seen[event.Report.Slot] || event.ID != derivedID(op.ID, event.Report.Slot) || event.Delivery.MessageID != event.ID ||
			event.Delivery.Context != "monitor:"+op.ID+":"+event.ID || event.CreatedAt.IsZero() || !bounded(event.Delivery.Envelope, 40*1024) {
			return errors.New("invalid monitor event identity")
		}
		seen[event.Report.Slot] = true
		switch event.Delivery.Status {
		case "pending", "held", "submitting", "accepted", "canceled":
		default:
			return errors.New("invalid monitor delivery state")
		}
		options, _ := json.Marshal(event.Delivery.Options)
		if len(options) > 64*1024 || len(event.Delivery.Detail) > 4096 {
			return errors.New("monitor delivery detail/options exceeds reserved capacity")
		}
		if event.Delivery.Receipt != nil && event.Delivery.Receipt.ClientUserMessageID != event.Delivery.MessageID {
			return errors.New("monitor delivery receipt identity differs")
		}
	}
	return nil
}

func validateResolution(req ResolutionRequest) error {
	if !validID(req.OperationID) || !validID(req.ResolutionID) || strings.TrimSpace(req.Detail) == "" || !bounded(req.Detail, 4096) || len(req.Evidence) == 0 || len(req.Evidence) > 16 {
		return errors.New("monitor resolution requires exact identities, detail and bounded evidence")
	}
	for _, item := range req.Evidence {
		if strings.TrimSpace(item) == "" || !bounded(item, 1024) {
			return errors.New("monitor resolution evidence is empty or exceeds its bound")
		}
	}
	return nil
}

// HasUnresolvedEffects is independent of native activity and launch uncertainty.
// A completed authoritative terminal result supersedes an earlier escalation.
func HasUnresolvedEffects(op Operation) bool {
	if op.Pi!=nil{return op.Pi.Settlement==nil||op.Pi.Settlement.State!="settled"}
	if op.Resolution != nil {
		return false
	}
	escalated := false
	for _, event := range op.Events {
		if event.Report.Slot == "terminal" {
			return event.Report.Outcome == "incomplete" || event.Report.StillRunning != ""
		}
		if event.Report.StillRunning != "" {
			escalated = true
		}
	}
	return escalated || op.Launch.Receipt != nil
}

func (s *Service) ResolveEffects(ctx context.Context, caller string, req ResolutionRequest) (Operation, error) {
	if err := validateResolution(req); err != nil {
		return Operation{}, err
	}
	owner, err := s.ResolveLead(ctx, caller)
	if err != nil {
		return Operation{}, err
	}
	unlock, err := s.LockSession(ctx, owner.Slug)
	if err != nil {
		return Operation{}, err
	}
	defer unlock()
	current, err := s.Resolve(owner.Slug)
	if err != nil || !SameOwner(owner, current) {
		return Operation{}, errors.New("monitor resolution owner changed")
	}
	tx, err := s.Store.Lock(ctx, req.OperationID)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Close()
	op := tx.Operation
	if !SameOwner(owner, op.Owner) || caller != op.Owner.RootThreadID {
		return Operation{}, errors.New("monitor evidence resolution is not owned by this lead")
	}
	if prior := op.Resolution; prior != nil {
		if prior.ID == req.ResolutionID && prior.Detail == req.Detail && reflect.DeepEqual(prior.Evidence, req.Evidence) {
			return *op, nil
		}
		return Operation{}, errors.New("monitor operation already has a different immutable resolution")
	}
	if err := s.reconcileLaunch(ctx, tx); err != nil {
		return *op, err
	}
	if s.CheckResolution == nil {
		return Operation{}, errors.New("monitor resolution requires native quiescence proof")
	}
	if err := s.CheckResolution(ctx, *op); err != nil {
		return *op, err
	}
	now := s.now()
	op.Resolution = &Resolution{ID: req.ResolutionID, At: now, LeadThreadID: caller, Evidence: req.Evidence, Detail: req.Detail}
	if op.Launch.TerminatedAt == nil {
		op.Launch.TerminatedAt = &now
		op.Launch.Termination = "quiescent"
	}
	if op.CanceledAt == nil {
		op.CanceledAt = &now
	}
	for i := range op.Events {
		d := &op.Events[i].Delivery
		if d.Status != "accepted" && d.Status != "submitting" {
			d.Status = "canceled"
		}
	}
	if err := tx.Save(); err != nil {
		return Operation{}, err
	}
	return *op, nil
}

func (s *Service) Start(ctx context.Context, caller string, req StartRequest) (Operation, error) {
	if err := validateStart(req); err != nil {
		return Operation{}, err
	}
	if s.Store == nil || s.Client == nil || s.ResolveLead == nil || s.Resolve == nil || s.LockSession == nil || s.Policy == nil || s.Activate == nil || s.Admit == nil {
		return Operation{}, errors.New("monitor launch is unavailable")
	}
	owner, err := s.ResolveLead(ctx, caller)
	if err != nil {
		return Operation{}, err
	}
	unlock, err := s.LockSession(ctx, owner.Slug)
	if err != nil {
		return Operation{}, err
	}
	defer unlock()
	current, err := s.Resolve(owner.Slug)
	if err != nil || !SameOwner(owner, current) || owner.SocketEpoch != current.SocketEpoch {
		return Operation{}, errors.New("monitor owner changed before start")
	}
	if owner.OriginTurnID == "" {
		return Operation{}, errors.New("monitor start requires an active originating lead turn")
	}
	// A retry uses its frozen snapshot; it does not resolve a new package policy.
	op, err := s.Store.Get(ctx, req.ID)
	if err == nil {
		if op.RequestDigest != digest(req) || !SameOwner(op.Owner, owner) {
			return Operation{}, errors.New("monitor start identity was reused for another request")
		}
	} else {
		if !errors.Is(err, os.ErrNotExist) {
			return Operation{}, err
		}
		if err := s.Admit(ctx, owner); err != nil {
			return Operation{}, err
		}
		policy, policyErr := s.Policy(ctx, owner)
		if policyErr != nil {
			return Operation{}, policyErr
		}
		op = Operation{Schema: Schema, ID: req.ID, Workspace: s.Store.workspace, Owner: owner, Source: req.Source, Brief: req.Brief, CancellationScope: req.CancellationScope, RequestDigest: digest(req), UtilityPolicy: policy, CreatedAt: s.now(), Events: []Event{}}
		op.Launch = initialLaunch(op)
		op, err = s.Store.reserve(ctx, op)
		if err != nil {
			return Operation{}, err
		}
	}
	if op.CanceledAt == nil && op.Disposition == "" {
		if err := s.Admit(ctx, owner); err != nil {
			return Operation{}, err
		}
	}
	tx, err := s.Store.Lock(ctx, op.ID)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Close()
	if err = s.launch(ctx, tx, current); err != nil {
		return *tx.Operation, err
	}
	return *tx.Operation, nil
}

func (s *Service) Report(ctx context.Context, caller string, req ReportRequest) (Operation, error) {
	if err := validateReport(req); err != nil {
		return Operation{}, err
	}
	op, err := s.Store.ResolveReporter(ctx, caller)
	if err != nil {
		return Operation{}, err
	}
	unlock, err := s.LockSession(ctx, op.Owner.Slug)
	if err != nil {
		return Operation{}, err
	}
	defer unlock()
	current, err := s.Resolve(op.Owner.Slug)
	if err != nil || !SameOwner(op.Owner, current) || op.Owner.SocketEpoch != current.SocketEpoch {
		return Operation{}, errors.New("monitor report owner or epoch changed")
	}
	tx, err := s.Store.Lock(ctx, op.ID)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Close()
	if tx.Operation.Launch.ThreadID != caller || tx.Operation.Disposition != "" {
		return Operation{}, errors.New("monitor utility binding changed")
	}
	if req.Outcome != "incomplete" && (op.Source.Kind == "local" && req.ExitStatus == nil || op.Source.Kind == "ci" && req.Conclusion == "") {
		return Operation{}, errors.New("monitor outcome lacks the source's authoritative result")
	}
	if err := AddEvent(tx.Operation, req, s.now()); err != nil {
		return Operation{}, err
	}
	if err := tx.Save(); err != nil {
		return Operation{}, err
	}
	return *tx.Operation, nil
}

func (s *Service) Status(ctx context.Context, caller, id string) (Operation, error) {
	if !validID(id) {
		op, err := s.Store.ResolveReporter(ctx, caller)
		if err != nil || id != "" {
			return Operation{}, errors.New("monitor status requires its owner or registered utility")
		}
		return op, nil
	}
	op, err := s.Store.Get(ctx, id)
	if err != nil {
		return Operation{}, err
	}
	if op.Launch.ThreadID == caller {
		return op, nil
	}
	owner, err := s.ResolveLead(ctx, caller)
	if err != nil || !SameOwner(owner, op.Owner) {
		return Operation{}, errors.New("monitor status is not owned by this lead")
	}
	unlock, err := s.LockSession(ctx, owner.Slug)
	if err != nil {
		return Operation{}, err
	}
	defer unlock()
	current, err := s.Resolve(owner.Slug)
	if err != nil || !SameOwner(owner, current) {
		return Operation{}, errors.New("monitor status owner changed before observation")
	}
	tx, err := s.Store.Lock(ctx, id)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Close()
	if !SameOwner(tx.Operation.Owner, owner) {
		return Operation{}, errors.New("monitor status owner changed")
	}
	if tx.Operation.CanceledAt != nil || tx.Operation.Launch.CreateAttempted && tx.Operation.Launch.Receipt == nil {
		if err := s.reconcileLaunch(ctx, tx); err != nil {
			tx.Operation.Launch.Detail = "Original launch evidence remains uncertain; no new native work was attempted"
			if saveErr := tx.Save(); saveErr != nil {
				return Operation{}, saveErr
			}
		}
	}
	return *tx.Operation, nil
}

func (s *Service) Cancel(ctx context.Context, caller, id string) (Operation, error) {
	owner, err := s.ResolveLead(ctx, caller)
	if err != nil {
		return Operation{}, err
	}
	unlock, err := s.LockSession(ctx, owner.Slug)
	if err != nil {
		return Operation{}, err
	}
	defer unlock()
	current, err := s.Resolve(owner.Slug)
	if err != nil || !SameOwner(owner, current) {
		return Operation{}, errors.New("monitor owner changed before cancellation")
	}
	tx, err := s.Store.Lock(ctx, id)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Close()
	if !SameOwner(owner, tx.Operation.Owner) {
		return Operation{}, errors.New("monitor operation is not owned by this lead")
	}
	if tx.Operation.CanceledAt == nil {
		now := s.now()
		tx.Operation.CanceledAt = &now
	}
	for i := range tx.Operation.Events {
		d := &tx.Operation.Events[i].Delivery
		if d.Status != "accepted" && d.Status != "submitting" {
			d.Status = "canceled"
			d.Detail = "Owner canceled continuation; external command execution is unchanged"
		}
	}
	if err := tx.Save(); err != nil {
		return Operation{}, err
	}
	return *tx.Operation, nil
}

func (s *Store) Inventory(ctx context.Context, slug, root string) (Inventory, error) {
	ops, err := s.List(ctx, slug)
	if err != nil {
		return Inventory{}, err
	}
	view := Inventory{Threads: []OwnedThread{}, Unresolved: []string{}, Blockers: []string{}}
	threads := map[string]bool{}
	projects := map[string]bool{}
	for _, op := range ops {
		if op.Owner.RootThreadID != root {
			if op.Disposition != "archived" {
				return Inventory{}, errors.New("monitor inventory has a different retained owner")
			}
			continue
		}
		if op.Disposition == "archived" {
			continue
		}
		if op.Launch.ThreadID != "" {
			if threads[op.Launch.ThreadID] || projects[op.Launch.ProjectID] {
				return Inventory{}, errors.New("monitor inventory has ambiguous native identities")
			}
			threads[op.Launch.ThreadID], projects[op.Launch.ProjectID] = true, true
			view.Threads = append(view.Threads, OwnedThread{OperationID: op.ID, ProjectID: op.Launch.ProjectID, ThreadID: op.Launch.ThreadID, Cwd: filepath.Join(s.workspace, "work", slug), Settings: utilitySettings(op), Disposition: op.Disposition})
		}
		if op.Disposition == "archiving" || op.Launch.CreateAttempted && op.Launch.ThreadID == "" || op.Launch.BootstrapAttempted && op.Launch.Receipt == nil && op.Launch.State != "canceled-unsent" && op.Launch.State != "unsubmitted" {
			view.Unresolved = append(view.Unresolved, op.ID)
		}
		if op.Launch.TerminatedAt == nil {
			view.Blockers = append(view.Blockers, op.ID+": utility execution is not proved terminated")
		}
		for _, event := range op.Events {
			if event.Delivery.Status == "submitting" {
				view.Unresolved = append(view.Unresolved, op.ID+":"+event.ID)
			}
		}
	}
	return view, nil
}
