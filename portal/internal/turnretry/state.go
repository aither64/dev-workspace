// Package turnretry owns durable automatic-continuation policy. It has no
// authority to load a thread, execute native work, or infer a turn's settings.
package turnretry

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aither64/codex-web/codex"
)

const Schema = 1
const MaxBytes = 1024 * 1024
const MaxSnapshotBytes = 256 * 1024
const CyberLimit = 30
const MaxSnapshots = 64

const Continuation = "Continue the interrupted task from the saved conversation. Inspect the current state before repeating actions."

type Scope struct {
	Workspace string `json:"workspace"`
	Slug      string `json:"slug"`
	Root      string `json:"root"`
	Thread    string `json:"thread"`
	Socket    string `json:"socket"`
}

// Snapshot is captured at managed admission, never reconstructed from defaults
// after failure. Turn is populated only by an exact accepted receipt.
type Snapshot struct {
	Epoch              uint64            `json:"epoch"`
	Version            int               `json:"version"`
	ID                 string            `json:"id"`
	Turn               string            `json:"turn"`
	Origin             string            `json:"origin"`
	Provider           string            `json:"provider"`
	Mode               string            `json:"mode"`
	CommonInstructions string            `json:"commonInstructions"`
	Options            codex.TurnOptions `json:"options"`
	Digest             string            `json:"digest"`
	Proven             bool              `json:"proven"`
	TextDigest         string            `json:"textDigest"`
	SocketIdentity     string            `json:"socketIdentity"`
	NativeContext      json.RawMessage   `json:"nativeContext,omitempty"`
	BindingDigest      string            `json:"bindingDigest"`
}

type Binding struct {
	Epoch              uint64             `json:"epoch"`
	SocketIdentity     string             `json:"socketIdentity"`
	Provider           string             `json:"provider"`
	Policy             codex.ThreadPolicy `json:"policy"`
	CommonInstructions string             `json:"commonInstructions"`
	Digest             string             `json:"digest"`
}

func (b Binding) Hash() (string, error) {
	b.Digest = ""
	data, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return hash(string(data)), nil
}

func (s Snapshot) Hash() (string, error) {
	s.Digest, s.Turn, s.Proven = "", "", false
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	if len(data) > MaxSnapshotBytes {
		return "", errors.New("retry settings snapshot exceeds 256 KiB")
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

type Pending struct {
	ID          string   `json:"id"`
	Action      string   `json:"action"`
	Failure     string   `json:"failure"`
	Snapshot    Snapshot `json:"snapshot"`
	ChargedAtMS int64    `json:"chargedAtMs"`
	ResultTurn  string   `json:"resultTurn"`
}

type Charge struct {
	ID       string `json:"id"`
	AtMS     int64  `json:"atMs"`
	Accepted bool   `json:"accepted"`
}

type State struct {
	Epoch          uint64     `json:"epoch"`
	Schema         int        `json:"schema"`
	Scope          Scope      `json:"scope"`
	Enabled        bool       `json:"enabled"`
	Enrolled       bool       `json:"enrolled"`
	Boundary       string     `json:"boundary"`
	Baseline       string     `json:"baseline"`
	BaselineActive bool       `json:"baselineActive"`
	Handled        string     `json:"handled"`
	Chain          string     `json:"chain"`
	Class          string     `json:"class"`
	Phase          string     `json:"phase"`
	Reason         string     `json:"reason"`
	Attempt        uint64     `json:"attempt"`
	Capacity       uint64     `json:"capacity"`
	DeadlineMS     int64      `json:"deadlineMs"`
	ClockMS        int64      `json:"clockMs"`
	Cyber          []Charge   `json:"cyber"`
	Cancelled      bool       `json:"cancelled"`
	Pending        *Pending   `json:"pending"`
	Snapshots      []Snapshot `json:"snapshots"`
	Binding        *Binding   `json:"binding,omitempty"`
}

type Observation struct{ Turn, Status, Code string }

func New(scope Scope) State {
	return State{Schema: Schema, Epoch: 1, Scope: scope, Enabled: true, Phase: "idle", Cyber: []Charge{}, Snapshots: []Snapshot{}}
}

// Enroll consumes a terminal baseline. An active baseline is eligible only if
// still active at the completed enrollment boundary supplied by the caller.
func (s *State) Enroll(o Observation) {
	if s.Enrolled {
		return
	}
	s.Enrolled, s.Baseline = true, o.Turn
	s.BaselineActive = o.Status == "inProgress"
}

func (s *State) SetEnabled(enabled bool, baseline Observation) {
	if s.Enabled == enabled {
		return
	}
	s.Enabled = enabled
	s.Cancel("disabled")
	if enabled {
		s.Baseline, s.BaselineActive, s.Handled = baseline.Turn, baseline.Status == "inProgress", baseline.Turn
		if s.BaselineActive {
			s.Handled = ""
		}
	}
}

func (s *State) Uncertain() bool {
	return s.Pending != nil && (s.Phase == "submitting" || s.Phase == "reconciling")
}
func (s *State) Owned() bool { return s.Phase == "scheduled" || s.Uncertain() }

func (s *State) Cancel(reason string) {
	s.Cancelled, s.DeadlineMS, s.Reason = true, 0, reason
	if s.Pending != nil && s.Pending.ResultTurn != "" {
		s.Handled = s.Pending.ResultTurn
	}
	if !s.Uncertain() {
		s.Phase, s.Pending = "cancelled", nil
	}
}

// Settings intent invalidates every earlier admission, including queues that
// can execute natively while the settings RPC is in flight. Old tuples remain
// immutable for reconciliation and can never acquire the new epoch.
func (s *State) InvalidateSettings(turn string) error {
	if s.Epoch == 0 || s.Epoch == ^uint64(0) {
		return errors.New("retry provenance epoch is unavailable")
	}
	s.Epoch++
	s.Cancel("settings_changed")
	if turn != "" {
		s.Handled = turn
	}
	s.Binding = nil
	return nil
}

func (s *State) Now(now time.Time) int64 {
	// A backward clock jump cannot expire charges or make a deadline due early.
	value := now.UnixMilli()
	if value < s.ClockMS {
		return s.ClockMS
	}
	s.ClockMS = value
	return value
}

func (s *State) CyberUsed(now int64) int {
	count := 0
	for _, charge := range s.Cyber {
		if charge.AtMS > now-300000 {
			count++
		}
	}
	return count
}

func CapacityDelay(attempt uint64, random float64) time.Duration {
	seconds := [...]int64{15, 30, 60, 120, 240, 300}
	index := min(attempt, uint64(len(seconds)-1))
	base := time.Duration(seconds[index]) * time.Second
	if random < 0 {
		random = 0
	}
	if random > 1 {
		random = 1
	}
	jitter := time.Duration(float64(min(3*time.Second, base/10)) * random)
	return min(300*time.Second, base+jitter)
}

func Class(code string) string {
	switch code {
	case "serverOverloaded":
		return "capacity"
	case "cyberPolicy":
		return "cybersecurity"
	}
	return ""
}

func (s *State) Proof(turn string) *Snapshot {
	for i := len(s.Snapshots) - 1; i >= 0; i-- {
		if s.Snapshots[i].Turn == turn && s.Snapshots[i].Proven && s.Snapshots[i].Epoch == s.Epoch {
			v := s.Snapshots[i]
			return &v
		}
	}
	return nil
}

// Observe records each terminal failure once, including unsupported failures.
// Quota exhaustion and cancel remain terminal until an independent turn.
func (s *State) Observe(o Observation, now time.Time, random float64) error {
	clock := s.Now(now)
	if !s.Enrolled {
		s.Enroll(o)
		return nil
	}
	if s.Uncertain() {
		return nil
	}
	if s.Pending != nil && s.Pending.ResultTurn == "" && o.Turn != s.Pending.Failure && o.Turn != "" {
		s.Cancel("superseded")
	}
	if s.Pending != nil && s.Pending.ResultTurn != "" && o.Turn == s.Pending.ResultTurn && o.Status == "inProgress" {
		return nil
	}
	if o.Status == "inProgress" {
		return nil
	}
	if o.Turn == s.Handled || (o.Turn == s.Baseline && !s.BaselineActive) {
		return nil
	}
	if s.Pending != nil && s.Pending.ResultTurn != "" && o.Turn != s.Pending.ResultTurn {
		s.Cancel("superseded")
	}
	if o.Status != "failed" {
		if o.Turn != "" {
			s.Handled = o.Turn
		}
		if !s.Uncertain() {
			s.Phase, s.Pending, s.DeadlineMS, s.Class = "idle", nil, 0, ""
		}
		return nil
	}
	s.Handled = o.Turn
	class := Class(o.Code)
	if class == "" || !s.Enabled {
		return nil
	}
	continuing := s.Pending != nil && s.Pending.ResultTurn == o.Turn && !s.Cancelled
	if !continuing {
		s.Chain, s.Attempt, s.Capacity, s.Cancelled = o.Turn, 0, 0, false
	}
	s.Class = class
	proof := s.Proof(o.Turn)
	if proof == nil {
		s.Phase, s.Reason, s.Pending, s.DeadlineMS = "blocked", "settings_unproven", nil, 0
		return nil
	}
	if class == "cybersecurity" && s.CyberUsed(clock) >= CyberLimit {
		s.Phase, s.Reason, s.Pending, s.DeadlineMS = "exhausted", "cyber_limit", nil, 0
		return nil
	}
	id, err := NewID()
	if err != nil {
		return err
	}
	s.Pending = &Pending{ID: id, Failure: o.Turn, Snapshot: *proof, Action: "turn-retry:" + o.Turn + ":" + proof.Digest + ":" + id}
	s.Phase, s.Reason, s.DeadlineMS = "scheduled", "", clock
	if class == "capacity" {
		s.DeadlineMS += CapacityDelay(s.Capacity, random).Milliseconds()
	}
	return nil
}

func (s *State) Begin(now time.Time) error {
	clock := s.Now(now)
	if !s.Enabled || s.Cancelled || s.Phase != "scheduled" || s.Pending == nil || clock < s.DeadlineMS {
		return errors.New("retry is not due")
	}
	if err := s.ReserveUnsubmitted(now); err != nil {
		return err
	}
	if s.Pending == nil {
		return nil
	}
	s.Attempt++
	if s.Class == "capacity" {
		s.Capacity++
	}
	s.Phase, s.DeadlineMS, s.Reason = "submitting", 0, ""
	return nil
}

// ReserveUnsubmitted requires positive proof that this exact pending ID has
// never been sent. Its old reservation cannot authorize a later outbound call.
func (s *State) ReserveUnsubmitted(now time.Time) error {
	if s.Class != "cybersecurity" {
		return nil
	}
	if s.Pending == nil {
		return errors.New("retry reservation has no intent")
	}
	clock := s.Now(now)
	kept := []Charge{}
	for _, charge := range s.Cyber {
		if charge.ID != s.Pending.ID && charge.AtMS > clock-300000 {
			kept = append(kept, charge)
		}
	}
	s.Cyber = kept
	if len(kept) >= CyberLimit {
		s.Phase, s.Reason, s.DeadlineMS, s.Pending = "exhausted", "cyber_limit", 0, nil
		return nil
	}
	s.Cyber = append(s.Cyber, Charge{ID: s.Pending.ID, AtMS: clock})
	s.Pending.ChargedAtMS = clock
	return nil
}

func (s *State) Accepted(turn string, now time.Time) error {
	if s.Pending == nil || turn == "" {
		return errors.New("retry receipt has no exact turn")
	}
	if s.Pending.ResultTurn != "" && s.Pending.ResultTurn != turn {
		return errors.New("retry receipt identity changed")
	}
	if s.Class == "cybersecurity" {
		found := false
		for i := range s.Cyber {
			charge := &s.Cyber[i]
			if charge.ID != s.Pending.ID {
				continue
			}
			found = true
			if !charge.Accepted {
				charge.AtMS = max(charge.AtMS, s.Now(now))
				charge.Accepted = true
				s.Pending.ChargedAtMS = charge.AtMS
			}
		}
		if !found {
			return errors.New("retry receipt has no quota reservation")
		}
	}
	s.Pending.ResultTurn = turn
	proof := s.Pending.Snapshot
	proof.ID, proof.Turn, proof.Origin, proof.Proven = s.Pending.ID, turn, "automatic", false
	proof.TextDigest = hash(Continuation)
	proof.NativeContext = nil
	proof.Digest, _ = proof.Hash()
	s.AddSnapshot(proof)
	s.Phase, s.Reason = "running", ""
	if s.Cancelled {
		s.Phase, s.Handled = "cancelled", turn
	}
	return nil
}

func (s *State) AddSnapshot(snapshot Snapshot) {
	for i := range s.Snapshots {
		if s.Snapshots[i].ID == snapshot.ID {
			s.Snapshots[i] = snapshot
			return
		}
	}
	// Never evict queued or uncertain admissions to make room. Save enforces
	// the bounded record and the caller refuses before submitting new work.
	s.Snapshots = append(s.Snapshots, snapshot)
}

func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&15|64, b[8]&63|128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
