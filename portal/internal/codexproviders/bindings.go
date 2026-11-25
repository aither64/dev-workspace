package codexproviders

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

const bindingStateLimit = 1 << 20
const bindingEvidenceLimit = 16 << 20
const bindingLineageLimit = 2048

// BindingKey contains origin identities supplied by the owning session/roster
// readers. Workspace is their canonical root, not the registration's name.
type BindingKey struct {
	Workspace        string `json:"workspace"`
	Slug             string `json:"slug"`
	Incarnation      string `json:"incarnation"`
	RootThreadID     string `json:"rootThreadId"`
	Member           string `json:"member"`
	OriginalThreadID string `json:"originalThreadId"`
}

type SessionBinding struct {
	RootThreadID  string                         `json:"rootThreadId"`
	Incarnation   string                         `json:"incarnation"`
	Revision      uint64                         `json:"revision"`
	Conversations map[string]ConversationBinding `json:"conversations"`
}

type ConversationBinding struct {
	Member         string `json:"member"`
	ThreadID       string `json:"threadId"`
	Provider       string `json:"provider"`
	OperationID    string `json:"operationId,omitempty"`
	EvidenceDigest string `json:"evidenceDigest,omitempty"`
}

// CurrentBinding carries validated caller preconditions, not authentication or
// proof of a live socket generation. Eligible includes current lead authority or
// ready, nonretired roster membership. R2 supplies these under its own locks.
type CurrentBinding struct {
	Key              BindingKey `json:"key"`
	BindingRequired  bool       `json:"bindingRequired"`
	ExpectedRevision uint64     `json:"expectedRevision"`
	Provider         string     `json:"provider"`
	Socket           string     `json:"socket"`
	Eligible         bool       `json:"eligible"`
}

type ResolvedBinding struct {
	Key      BindingKey `json:"key"`
	ThreadID string     `json:"threadId"`
	Provider string     `json:"provider"`
	Revision uint64     `json:"revision"`
	Socket   string     `json:"socket"`
}

// ReceiptTarget identifies an exact historical native receipt namespace. It
// neither authorizes current execution nor proves absence of a native receipt.
type ReceiptTarget struct {
	ThreadID       string `json:"threadId"`
	Provider       string `json:"provider"`
	OperationID    string `json:"operationId,omitempty"`
	EvidenceDigest string `json:"evidenceDigest,omitempty"`
}

type BindingEvidence struct {
	Schema                   int                  `json:"schema"`
	OperationID              string               `json:"operationId"`
	Workspace                string               `json:"workspace"`
	Root                     string               `json:"root"`
	Socket                   string               `json:"socket"`
	PreviousBindingRevision  uint64               `json:"previousBindingRevision"`
	CommittedBindingRevision uint64               `json:"committedBindingRevision"`
	SnapshotDigest           string               `json:"snapshotDigest"`
	Participants             []BindingParticipant `json:"participants"`
}

type BindingParticipant struct {
	Key                    BindingKey `json:"key"`
	OldThreadID            string     `json:"oldThreadId"`
	NewThreadID            string     `json:"newThreadId"`
	OldProvider            string     `json:"oldProvider"`
	NewProvider            string     `json:"newProvider"`
	PreviousOperationID    string     `json:"previousOperationId,omitempty"`
	PreviousEvidenceDigest string     `json:"previousEvidenceDigest,omitempty"`
}

// EvidenceReader returns immutable sealed bytes from the existing operation
// directory. Errors, including missing data, must be propagated as failures.
type EvidenceReader func(operationID string) ([]byte, error)

// MarshalJSON preserves schema-3's required empty map. Legacy encodings do
// not gain binding fields, and a nil map is not repaired into an empty one.
func (s State) MarshalJSON() ([]byte, error) {
	type wireState State
	if s.Schema != 3 {
		legacy := wireState(s)
		legacy.BindingRevision, legacy.Bindings = 0, nil
		return json.Marshal(legacy)
	}
	if s.BindingRevision == 0 || s.Bindings == nil {
		return nil, errors.New("invalid schema-3 binding revision or missing map")
	}
	return json.Marshal(struct {
		wireState
		BindingRevision uint64                    `json:"bindingRevision"`
		Bindings        map[string]SessionBinding `json:"bindings"`
	}{wireState(s), s.BindingRevision, s.Bindings})
}

// DecodeBindingState is the strict codec shared by ordinary and owning readers.
// Decoding state does not establish current execution authority.
func DecodeBindingState(data []byte, workspace, root, socket string, catalog Catalog) (State, error) {
	var state State
	if err := decodeBindingJSON(data, bindingStateLimit, &state); err != nil {
		return State{}, err
	}
	if state.Schema < 3 && hasBindingFields(data) {
		return State{}, errors.New("legacy state has binding fields")
	}
	if state.Workspace != workspace || state.Root != root || state.Socket != socket {
		return State{}, errors.New("binding workspace identity changed")
	}
	if err := ValidateBindings(state, catalog); err != nil {
		return State{}, err
	}
	return state, nil
}

func canonicalBindingPath(p string) bool {
	return len(p) <= 4096 && filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsAny(p, "\x00\r\n")
}
func validBindingKey(k BindingKey) bool {
	return canonicalBindingPath(k.Workspace) && ValidID(k.Slug) && digestID.MatchString(k.Incarnation) && threadIdentity(k.RootThreadID) && threadIdentity(k.OriginalThreadID) && ValidID(k.Member) && (k.Member != "lead" || k.OriginalThreadID == k.RootThreadID)
}
func validEvidencePair(op, digest string) bool {
	return op == "" && digest == "" || operationID.MatchString(op) && digestID.MatchString(digest)
}

func ValidateBindings(state State, catalog Catalog) error {
	if state.Schema < 1 || state.Schema > 3 || !ValidID(state.Workspace) || !canonicalBindingPath(state.Root) || !canonicalBindingPath(state.Socket) {
		return errors.New("invalid binding state identity")
	}
	routing := state
	if state.Operation != nil && state.Operation.Strategy == "fork-v1" {
		if err := ValidateForkOperation(state); err != nil {
			return err
		}
		routing.Operation = nil // Production's legacy validator stays unchanged.
	}
	if err := validateRoutingState(routing, catalog); err != nil {
		return err
	}
	if state.Schema != 3 {
		if state.BindingRevision != 0 || state.Bindings != nil {
			return errors.New("legacy state has binding fields")
		}
		return nil
	}
	if state.BindingRevision == 0 || state.Bindings == nil || len(state.Bindings) > 4096 {
		return errors.New("invalid binding revision or sessions")
	}
	origins, active := map[string]bool{}, map[string]bool{}
	for slug, session := range state.Bindings {
		if !ValidID(slug) || !threadIdentity(session.RootThreadID) || !digestID.MatchString(session.Incarnation) || session.Revision == 0 || session.Revision > state.BindingRevision || len(session.Conversations) == 0 {
			return errors.New("invalid session binding")
		}
		lead, ok := session.Conversations[session.RootThreadID]
		if !ok || lead.Member != "lead" {
			return errors.New("missing original lead binding")
		}
		for original, conversation := range session.Conversations {
			key := BindingKey{state.Root, slug, session.Incarnation, session.RootThreadID, conversation.Member, original}
			if !validBindingKey(key) || origins[original] || !threadIdentity(conversation.ThreadID) || active[conversation.ThreadID] || !validEvidencePair(conversation.OperationID, conversation.EvidenceDigest) || (conversation.ThreadID == original) != (conversation.OperationID == "") {
				return errors.New("invalid or duplicate conversation binding")
			}
			if _, err := catalog.Find(conversation.Provider); err != nil {
				return err
			}
			origins[original], active[conversation.ThreadID] = true, true
		}
	}
	// A fork may not take any other conversation's original receipt namespace.
	for _, session := range state.Bindings {
		for original, c := range session.Conversations {
			if c.ThreadID != original && origins[c.ThreadID] {
				return errors.New("cross-conversation alias")
			}
		}
	}
	b, err := json.Marshal(state)
	if err != nil || len(b) > bindingStateLimit {
		return errors.New("binding state exceeds bounds")
	}
	return nil
}

// ForkJournal records transaction progress, not lineage authority. Its source
// state is the exact validated origin reservation in the immutable snapshot.
type ForkJournal struct {
	Schema               int                       `json:"schema"`
	Strategy             string                    `json:"strategy"`
	OperationID          string                    `json:"operationId"`
	Workspace            string                    `json:"workspace"`
	Root                 string                    `json:"root"`
	Socket               string                    `json:"socket"`
	CatalogDigest        string                    `json:"catalogDigest"`
	SourcePackage        string                    `json:"sourcePackage"`
	Selection            SelectionChange           `json:"selection"`
	Sessions             []string                  `json:"sessions"`
	SourceSnapshotDigest string                    `json:"sourceSnapshotDigest"`
	ReservationRevision  uint64                    `json:"reservationRevision"`
	Stage                string                    `json:"stage"`
	Participants         []ForkParticipant         `json:"participants"`
	Projections          map[string]ForkProjection `json:"projections"`
}

type ForkParticipant struct {
	Current            CurrentBinding `json:"current"`
	SourceThreadID     string         `json:"sourceThreadId"`
	State              string         `json:"state"`
	ChildThreadID      string         `json:"childThreadId,omitempty"`
	VerificationDigest string         `json:"verificationDigest,omitempty"`
}

type ForkProjection struct {
	Manifest  bool `json:"manifest"`
	Authority bool `json:"authority"`
	Recovery  bool `json:"recovery"`
	Terminal  bool `json:"terminal"`
}

func (o Operation) MarshalJSON() ([]byte, error) {
	type wire Operation
	if o.Strategy != "fork-v1" {
		return json.Marshal(wire(o))
	}
	if o.Sessions == nil || o.Roots == nil {
		return nil, errors.New("fork operation requires explicit scope")
	}
	return json.Marshal(struct {
		wire
		Sessions []string          `json:"sessions"`
		Roots    map[string]string `json:"roots"`
	}{wire(o), o.Sessions, o.Roots})
}

func ValidateForkOperation(state State) error {
	o := state.Operation
	if o == nil || o.Strategy != "fork-v1" || state.Schema < 2 || state.Schema > 3 ||
		!operationID.MatchString(o.ID) || !ValidID(o.Target) || !digestID.MatchString(o.CatalogDigest) ||
		len(o.Sessions) > 4096 || len(o.Roots) != len(o.Sessions) || !slices.IsSorted(o.Sessions) ||
		o.Session != "" && (len(o.Sessions) != 1 || o.Sessions[0] != o.Session) || o.Inherit && o.Session == "" {
		return errors.New("invalid fork operation identity or scope")
	}
	for n, slug := range o.Sessions {
		if !ValidID(slug) || n > 0 && o.Sessions[n-1] == slug || !threadIdentity(o.Roots[slug]) {
			return errors.New("invalid fork operation roots")
		}
	}
	waiting := slices.Contains([]string{"accepted", "waiting-creation", "waiting-package", "draining"}, o.Stage)
	applying := slices.Contains([]string{"preparing", "forking", "verifying", "committing", "restoring-terminals"}, o.Stage)
	if !waiting && !applying || o.Phase == "waiting" && !waiting || o.Reserved() && !applying ||
		!slices.Contains([]string{"waiting", "applying", "recovery-required", "applied", "canceled", "failed"}, o.Phase) ||
		o.Phase == "applied" && o.Stage != "restoring-terminals" {
		return errors.New("invalid fork operation stage")
	}
	return nil
}

func DecodeForkJournal(data []byte, source State, catalog Catalog) (ForkJournal, error) {
	var j ForkJournal
	if err := decodeBindingJSON(data, bindingEvidenceLimit, &j); err != nil {
		return j, err
	}
	if err := ValidateBindings(source, catalog); err != nil {
		return j, err
	}
	o := source.Operation
	if o == nil || o.Strategy != "fork-v1" || j.Schema != 1 || j.Strategy != o.Strategy || j.OperationID != o.ID ||
		j.Workspace != source.Workspace || j.Root != source.Root || j.Socket != source.Socket || j.CatalogDigest != o.CatalogDigest ||
		!canonicalBindingPath(j.SourcePackage) || !digestID.MatchString(j.SourceSnapshotDigest) ||
		j.ReservationRevision == 0 || j.ReservationRevision != source.BindingRevision || !slices.Equal(j.Sessions, o.Sessions) ||
		j.Selection.Provider != o.Target || j.Selection.Session != o.Session || j.Selection.Inherit != o.Inherit ||
		(j.Selection.Scope == "session") != (o.Session != "") || !slices.Contains([]string{"session", "workspace"}, j.Selection.Scope) ||
		len(j.Participants) == 0 || len(j.Projections) != len(j.Sessions) {
		return j, errors.New("fork journal identity differs from source reservation")
	}
	stage := source
	operation := *o
	operation.Stage = j.Stage
	operation.Phase = "applying"
	stage.Operation = &operation
	if err := ValidateForkOperation(stage); err != nil {
		return j, err
	}
	keys, sources, children := map[BindingKey]bool{}, map[string]bool{}, map[string]int{}
	verifiedChildren := map[string]bool{}
	leads := map[string]bool{}
	for _, p := range j.Participants {
		r, err := source.ResolveCurrent(p.Current)
		if err != nil || keys[p.Current.Key] || !slices.Contains(j.Sessions, p.Current.Key.Slug) ||
			p.Current.Key.RootThreadID != o.Roots[p.Current.Key.Slug] || p.SourceThreadID != r.ThreadID || sources[p.SourceThreadID] {
			return j, errors.New("fork participant differs from frozen binding")
		}
		keys[p.Current.Key], sources[p.SourceThreadID] = true, true
		leads[p.Current.Key.Slug] = leads[p.Current.Key.Slug] || p.Current.Key.Member == "lead"
		switch p.State {
		case "not-attempted", "intent":
			if p.ChildThreadID != "" || p.VerificationDigest != "" {
				return j, errors.New("unknown fork outcome has child or proof")
			}
		case "known-child", "verified":
			if !threadIdentity(p.ChildThreadID) || (p.State == "verified") != digestID.MatchString(p.VerificationDigest) || p.State == "known-child" && p.VerificationDigest != "" {
				return j, errors.New("invalid known child or verification proof")
			}
			if p.State == "verified" {
				children[p.ChildThreadID]++
			}
			verifiedChildren[p.ChildThreadID] = verifiedChildren[p.ChildThreadID] || p.State == "verified"
		default:
			return j, errors.New("unknown fork participant state")
		}
		if j.Stage == "preparing" && p.State != "not-attempted" || slices.Contains([]string{"committing", "restoring-terminals"}, j.Stage) && p.State != "verified" {
			return j, errors.New("fork progress contradicts journal stage")
		}
	}
	for id := range verifiedChildren {
		if !verifiedChildren[id] {
			continue
		}
		if sources[id] || children[id] != 1 {
			return j, errors.New("fork child aliases a source")
		}
		for _, binding := range source.Bindings {
			for original, current := range binding.Conversations {
				if id == original || id == current.ThreadID {
					return j, errors.New("fork child aliases retained history")
				}
			}
		}
	}
	for _, slug := range j.Sessions {
		p, ok := j.Projections[slug]
		if !ok || !leads[slug] || p.Authority && !p.Manifest || p.Recovery && !p.Authority || p.Terminal && !p.Recovery || (p.Recovery || p.Terminal) && j.Stage != "restoring-terminals" {
			return j, errors.New("fork scope lacks lead or projection")
		}
	}
	return j, nil
}

func (s State) binding(k BindingKey) (ConversationBinding, uint64, bool, error) {
	if !validBindingKey(k) || k.Workspace != s.Root {
		return ConversationBinding{}, 0, false, errors.New("binding key identity changed")
	}
	session, exists := s.Bindings[k.Slug]
	if !exists {
		return ConversationBinding{}, 0, false, nil
	}
	if session.RootThreadID != k.RootThreadID || session.Incarnation != k.Incarnation {
		return ConversationBinding{}, 0, false, errors.New("session binding identity changed")
	}
	c, exists := session.Conversations[k.OriginalThreadID]
	if !exists || c.Member != k.Member {
		return ConversationBinding{}, 0, false, errors.New("conversation binding identity changed")
	}
	return c, session.Revision, true, nil
}

func (s State) ResolveCurrent(current CurrentBinding) (ResolvedBinding, error) {
	if !current.Eligible || current.Socket != s.Socket || !ValidID(current.Provider) || current.Provider != s.Provider(current.Key.Slug, current.Key.RootThreadID) {
		return ResolvedBinding{}, errors.New("current binding preconditions changed")
	}
	c, revision, exists, err := s.binding(current.Key)
	if err != nil {
		return ResolvedBinding{}, err
	}
	if !exists {
		if current.BindingRequired || current.ExpectedRevision != 0 || s.Provider(current.Key.Slug, current.Key.RootThreadID) != current.Provider {
			return ResolvedBinding{}, errors.New("required binding is missing")
		}
		c = ConversationBinding{Member: current.Key.Member, ThreadID: current.Key.OriginalThreadID, Provider: current.Provider}
	}
	if revision != current.ExpectedRevision || c.Provider != current.Provider {
		return ResolvedBinding{}, errors.New("stale binding revision or provider")
	}
	return ResolvedBinding{current.Key, c.ThreadID, c.Provider, revision, s.Socket}, nil
}

func (s State) decodeEvidence(data []byte, op, digest string) (BindingEvidence, error) {
	var evidence BindingEvidence
	if len(data) > bindingEvidenceLimit {
		return evidence, errors.New("sealed evidence exceeds bounds")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return evidence, errors.New("sealed evidence digest changed")
	}
	if err := decodeBindingJSON(data, bindingEvidenceLimit, &evidence); err != nil {
		return evidence, err
	}
	if evidence.Schema != 1 || evidence.OperationID != op || !operationID.MatchString(op) || evidence.Workspace != s.Workspace || evidence.Root != s.Root || evidence.Socket != s.Socket || !digestID.MatchString(evidence.SnapshotDigest) || evidence.PreviousBindingRevision == math.MaxUint64 || evidence.CommittedBindingRevision != evidence.PreviousBindingRevision+1 || evidence.CommittedBindingRevision > s.BindingRevision || len(evidence.Participants) == 0 {
		return evidence, errors.New("invalid sealed evidence identity or revision")
	}
	keys, oldIDs, newIDs := map[BindingKey]bool{}, map[string]bool{}, map[string]bool{}
	for _, p := range evidence.Participants {
		if !validBindingKey(p.Key) || p.Key.Workspace != s.Root || keys[p.Key] || !threadIdentity(p.OldThreadID) || !threadIdentity(p.NewThreadID) || p.OldThreadID == p.NewThreadID || oldIDs[p.OldThreadID] || newIDs[p.NewThreadID] || !ValidID(p.OldProvider) || !ValidID(p.NewProvider) || !validEvidencePair(p.PreviousOperationID, p.PreviousEvidenceDigest) || (p.OldThreadID == p.Key.OriginalThreadID) != (p.PreviousOperationID == "") {
			return evidence, errors.New("invalid sealed evidence participant")
		}
		keys[p.Key], oldIDs[p.OldThreadID], newIDs[p.NewThreadID] = true, true, true
	}
	for id := range newIDs {
		if oldIDs[id] {
			return evidence, errors.New("conflicting evidence targets")
		}
	}
	return evidence, nil
}

func (s State) ReceiptTargets(key BindingKey, bindingRequired bool, reader EvidenceReader) ([]ReceiptTarget, error) {
	c, revision, exists, err := s.binding(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		if bindingRequired {
			return nil, errors.New("receipt binding missing")
		}
		return []ReceiptTarget{{ThreadID: key.OriginalThreadID, Provider: s.Provider(key.Slug, key.RootThreadID)}}, nil
	}
	targets := []ReceiptTarget{}
	seen := map[string]bool{}
	for step := 0; ; step++ {
		if (step >= bindingLineageLimit && c.ThreadID != key.OriginalThreadID) || seen[c.ThreadID] {
			return nil, errors.New("binding lineage exceeds bounds or cycles")
		}
		seen[c.ThreadID] = true
		targets = append(targets, ReceiptTarget{c.ThreadID, c.Provider, c.OperationID, c.EvidenceDigest})
		if c.ThreadID == key.OriginalThreadID {
			if c.OperationID != "" || c.EvidenceDigest != "" {
				return nil, errors.New("original target has switch evidence")
			}
			return targets, nil
		}
		if reader == nil || !validEvidencePair(c.OperationID, c.EvidenceDigest) || c.OperationID == "" {
			return nil, errors.New("binding lineage missing")
		}
		data, err := reader(c.OperationID)
		if err != nil {
			return nil, err
		}
		e, err := s.decodeEvidence(data, c.OperationID, c.EvidenceDigest)
		if err != nil {
			return nil, err
		}
		if e.CommittedBindingRevision > revision {
			return nil, errors.New("lineage revisions do not decrease")
		}
		var found *BindingParticipant
		for i := range e.Participants {
			p := &e.Participants[i]
			if p.Key == key {
				found = p
			}
		}
		if found == nil || found.NewThreadID != c.ThreadID || found.NewProvider != c.Provider {
			return nil, errors.New("lineage target or key changed")
		}
		c = ConversationBinding{Member: key.Member, ThreadID: found.OldThreadID, Provider: found.OldProvider, OperationID: found.PreviousOperationID, EvidenceDigest: found.PreviousEvidenceDigest}
		revision = e.PreviousBindingRevision
	}
}

// BindingCommit freezes complete current participant preconditions and their
// one-to-one updates. Evidence is the exact sealed document, not mutable progress.
type BindingCommit struct {
	ExpectedRevision uint64           `json:"expectedRevision"`
	Participants     []CurrentBinding `json:"participants"`
	Updates          []BindingUpdate  `json:"updates"`
	Selection        SelectionChange  `json:"selection"`
	Evidence         json.RawMessage  `json:"evidence,omitempty"`
}
type BindingUpdate struct {
	Key      BindingKey `json:"key"`
	ThreadID string     `json:"threadId"`
	Provider string     `json:"provider"`
}
type SelectionChange struct {
	Scope    string `json:"scope"`
	Session  string `json:"session,omitempty"`
	Provider string `json:"provider"`
	Inherit  bool   `json:"inherit,omitempty"`
}

func (s State) PrepareBindingCommit(commit BindingCommit, catalog Catalog) (State, error) {
	fail := func() (State, error) { return State{}, errors.New("binding commit preconditions changed") }
	if ValidateBindings(s, catalog) != nil || commit.ExpectedRevision != s.BindingRevision || len(commit.Participants) == 0 || len(commit.Participants) != len(commit.Updates) {
		return fail()
	}
	selection := commit.Selection
	if _, err := catalog.Find(selection.Provider); err != nil {
		return fail()
	}
	if selection.Scope != "session" && selection.Scope != "workspace" || selection.Scope == "session" && !ValidID(selection.Session) || selection.Scope == "workspace" && (selection.Session != "" || selection.Inherit) || selection.Inherit && selection.Provider != s.Active {
		return fail()
	}
	current := map[BindingKey]ResolvedBinding{}
	updates := map[BindingKey]BindingUpdate{}
	slugs := map[string]bool{}
	changed := false
	for _, p := range commit.Participants {
		if _, ok := current[p.Key]; ok {
			return fail()
		}
		r, err := s.ResolveCurrent(p)
		if err != nil {
			return fail()
		}
		if selection.Scope == "session" && p.Key.Slug != selection.Session {
			return fail()
		}
		if selection.Scope == "workspace" {
			if explicit, ok := s.Sessions[p.Key.Slug]; ok && explicit.RootThreadID == p.Key.RootThreadID {
				return fail()
			}
		}
		current[p.Key] = r
		slugs[p.Key.Slug] = true
	}
	// The caller freezes the complete current roster under its locks. Retired
	// historical entries are not participants; one origin lead is required per scope.
	for slug := range slugs {
		found := false
		var identity BindingKey
		var revision uint64
		for key, r := range current {
			if key.Slug != slug {
				continue
			}
			if identity.Slug != "" && (identity.Incarnation != key.Incarnation || identity.RootThreadID != key.RootThreadID || revision != r.Revision) {
				return fail()
			}
			identity = key
			revision = r.Revision
			found = found || key.Member == "lead"
		}
		if !found {
			return fail()
		}
	}

	for _, u := range commit.Updates {
		r, ok := current[u.Key]
		if !ok || updates[u.Key].ThreadID != "" || !threadIdentity(u.ThreadID) || u.Provider != selection.Provider {
			return fail()
		}
		if u.ThreadID == r.ThreadID && u.Provider != r.Provider {
			return fail()
		}
		changed = changed || u.ThreadID != r.ThreadID
		updates[u.Key] = u
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fail()
	}
	var candidate State
	if json.Unmarshal(data, &candidate) != nil {
		return fail()
	}
	// Preserve present empty maps in the typed input as well as their entries.
	if s.Receipts != nil && candidate.Receipts == nil {
		candidate.Receipts = map[string]RequestReceipt{}
	}
	if s.Bindings != nil && candidate.Bindings == nil {
		candidate.Bindings = map[string]SessionBinding{}
	}
	if selection.Scope == "workspace" {
		candidate.Active = selection.Provider
	} else if selection.Inherit {
		delete(candidate.Sessions, selection.Session)
	} else {
		if candidate.Sessions == nil {
			candidate.Sessions = map[string]Selection{}
		}
		var root string
		for key := range current {
			root = key.RootThreadID
		}
		candidate.Sessions[selection.Session] = Selection{Provider: selection.Provider, RootThreadID: root}
	}
	if len(candidate.Sessions) > 0 && candidate.Schema == 1 {
		candidate.Schema = 2
	}
	if changed {
		if s.BindingRevision == math.MaxUint64 || len(commit.Evidence) == 0 {
			return fail()
		}
		candidate.Schema = 3
		candidate.BindingRevision = s.BindingRevision + 1
		if candidate.Bindings == nil {
			candidate.Bindings = map[string]SessionBinding{}
		}
		sum := sha256.Sum256(commit.Evidence)
		digest := hex.EncodeToString(sum[:])
		var declaration struct {
			OperationID string `json:"operationId"`
		}
		if json.Unmarshal(commit.Evidence, &declaration) != nil {
			return fail()
		}
		e, err := candidate.decodeEvidence(commit.Evidence, declaration.OperationID, digest)
		if err != nil || e.PreviousBindingRevision != s.BindingRevision || len(e.Participants) != len(current) {
			return fail()
		}
		for _, p := range e.Participants {
			r, ok := current[p.Key]
			u := updates[p.Key]
			if !ok || p.OldThreadID != r.ThreadID || p.NewThreadID != u.ThreadID || p.OldProvider != r.Provider || p.NewProvider != u.Provider {
				return fail()
			}
			previous := s.Bindings[p.Key.Slug].Conversations[p.Key.OriginalThreadID]
			if p.PreviousOperationID != previous.OperationID || p.PreviousEvidenceDigest != previous.EvidenceDigest {
				return fail()
			}
			session := candidate.Bindings[p.Key.Slug]
			if session.Conversations == nil {
				session = SessionBinding{RootThreadID: p.Key.RootThreadID, Incarnation: p.Key.Incarnation, Conversations: map[string]ConversationBinding{}}
			}
			session.Revision = candidate.BindingRevision
			session.Conversations[p.Key.OriginalThreadID] = ConversationBinding{p.Key.Member, p.NewThreadID, p.NewProvider, e.OperationID, digest}
			candidate.Bindings[p.Key.Slug] = session
		}
	} else if len(commit.Evidence) != 0 {
		return fail()
	}
	if ValidateBindings(candidate, catalog) != nil {
		return fail()
	}
	encoded, err := json.Marshal(candidate)
	if err != nil || len(encoded) > bindingStateLimit {
		return fail()
	}
	return candidate, nil
}

// The token pass rejects duplicates before ordinary typed decoding can discard
// them. Reflection requires exact wire keys and required fields (encoding/json
// alone accepts case-insensitive struct field aliases). No JSON null/coercion.
func decodeBindingJSON(data []byte, limit int, target any) error {
	if len(data) > limit || !utf8.Valid(data) {
		return errors.New("binding JSON exceeds bounds or invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) (any, error)
	value = func(depth int) (any, error) {
		if depth > 64 {
			return nil, errors.New("binding JSON nesting exceeds bounds")
		}
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				m := map[string]any{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return nil, e
					}
					key, ok := k.(string)
					if !ok {
						return nil, errors.New("invalid JSON key")
					}
					if _, ok := m[key]; ok {
						return nil, errors.New("duplicate binding JSON key")
					}
					v, e := value(depth + 1)
					if e != nil {
						return nil, e
					}
					m[key] = v
				}
				_, err = d.Token()
				return m, err
			case '[':
				a := []any{}
				for d.More() {
					v, e := value(depth + 1)
					if e != nil {
						return nil, e
					}
					a = append(a, v)
				}
				_, err = d.Token()
				return a, err
			}
			return nil, errors.New("invalid JSON delimiter")
		}
		if t == nil {
			return nil, errors.New("null binding field")
		}
		return t, nil
	}
	v, err := value(0)
	if err != nil {
		return err
	}
	if _, err = d.Token(); err != io.EOF {
		return errors.New("trailing binding JSON")
	}
	if err := bindingShape(v, reflect.TypeOf(target).Elem()); err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
func bindingShape(v any, t reflect.Type) error {
	if t.Kind() == reflect.Pointer {
		return bindingShape(v, t.Elem())
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("binding field must be object")
		}
		fields := map[string]reflect.StructField{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name := strings.Split(tag, ",")[0]
			fields[name] = f
			if !strings.Contains(tag, ",omitempty") {
				if _, ok := m[name]; !ok {
					return fmt.Errorf("missing binding field %s", name)
				}
			}
		}
		// Prospective wire validation retains presence here. The production
		// legacy validator intentionally continues treating zero values as before.
		var identities []string
		if t == reflect.TypeOf(RequestReceipt{}) {
			identities = []string{"session"}
		}
		if t == reflect.TypeOf(Selection{}) {
			identities = []string{"rootThreadId", "creationId"}
		}
		if t == reflect.TypeOf(Operation{}) && m["strategy"] == "fork-v1" {
			for _, name := range []string{"sessions", "roots", "stage"} {
				if _, ok := m[name]; !ok {
					return errors.New("fork operation scope is missing")
				}
			}
			if x, ok := m["session"]; ok && x == "" {
				return errors.New("empty fork session identity")
			}
		}
		for _, name := range identities {
			if x, present := m[name]; present && x == "" {
				return errors.New("empty optional binding identity")
			}
		}
		// Present switch references must be nonempty. Their absence, not an
		// empty-string sentinel, identifies an original conversation.
		if t == reflect.TypeOf(ConversationBinding{}) || t == reflect.TypeOf(BindingParticipant{}) {
			for _, name := range []string{"operationId", "evidenceDigest", "previousOperationId", "previousEvidenceDigest"} {
				if x, ok := m[name]; ok && x == "" {
					return errors.New("empty binding evidence reference")
				}
			}
		}
		if t == reflect.TypeOf(ForkParticipant{}) {
			for _, name := range []string{"childThreadId", "verificationDigest"} {
				if x, ok := m[name]; ok && x == "" {
					return errors.New("empty fork child or verification reference")
				}
			}
		}
		for k, x := range m {
			f, ok := fields[k]
			if !ok {
				return fmt.Errorf("unknown binding field %s", k)
			}
			if err := bindingShape(x, f.Type); err != nil {
				return err
			}
		}
	case reflect.Map:
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("binding map required")
		}
		for _, x := range m {
			if err := bindingShape(x, t.Elem()); err != nil {
				return err
			}
		}
	case reflect.Slice:
		a, ok := v.([]any)
		if !ok {
			return errors.New("binding array required")
		}
		for _, x := range a {
			if err := bindingShape(x, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasBindingFields(data []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return true
	}
	for name := range fields {
		if strings.EqualFold(name, "bindingRevision") || strings.EqualFold(name, "bindings") {
			return true
		}
	}
	return false
}
