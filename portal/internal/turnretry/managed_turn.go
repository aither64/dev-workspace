package turnretry

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/aither64/codex-web/codex"
)

// ManagedTurn retains authority once, plus a constant-size commitment to the
// accepted native user-item prefix. Opaque pagination cursors are never durable.
type ManagedTurn struct {
	Version           int                                     `json:"version"`
	Turn              string                                  `json:"turn"`
	Epoch             uint64                                  `json:"epoch"`
	BindingDigest     string                                  `json:"bindingDigest"`
	Count             uint64                                  `json:"count"`
	Digest            string                                  `json:"digest"`
	LastUserItemID    string                                  `json:"lastUserItemId"`
	AdditionalContext map[string]codex.AdditionalContextEntry `json:"additionalContext"`
}

func ManagedTurnSeed(scope Scope, epoch uint64, binding, turn string) string {
	data, _ := json.Marshal([]any{"managed-turn/v1", scope, epoch, binding, turn})
	return hash(string(data))
}

func ManagedInputDigest(previous, item, client, text string) string {
	data, _ := json.Marshal([]any{"managed-turn/input/v1", previous, item, client, text})
	return hash(string(data))
}

func validDigest(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == 32 && hex.EncodeToString(data) == value
}

func (s *State) validateManagedTurn() error {
	m := s.ManagedTurn
	if m == nil {
		return nil
	}
	if s.Schema != Schema || m.Version != 1 || m.Turn == "" || len(m.Turn) > 4096 || m.Epoch != s.Epoch || s.Binding == nil ||
		m.BindingDigest != s.Binding.Digest || !validDigest(m.Digest) || m.AdditionalContext == nil || len(m.LastUserItemID) > 4096 {
		return errors.New("managed turn authority is invalid")
	}
	if m.Count == 0 && (m.LastUserItemID != "" || len(m.AdditionalContext) != 0 || m.Digest != ManagedTurnSeed(s.Scope, m.Epoch, m.BindingDigest, m.Turn)) || m.Count > 0 && m.LastUserItemID == "" {
		return errors.New("managed turn prefix is invalid")
	}
	data, err := json.Marshal(m)
	if err != nil || len(data) > MaxSnapshotBytes {
		return errors.New("managed turn context exceeds its bounds")
	}
	return nil
}

// CompactManagedInput is called only for a positively observed native item in
// chronological order. The caller saves this prefix and source removal together.
// Pending is deliberately untouched, including its immutable settings snapshot.
func (s *State) CompactManagedInput(turn, item, client, text string) error {
	b := s.Binding
	if b == nil || b.Epoch != s.Epoch || item == "" || len(item) > 4096 || turn == "" || s.Handled == turn {
		return ErrMissingProof
	}
	index := -1
	for i, p := range s.Snapshots {
		matches := client != "" && p.ID == client || client == "" && p.Origin == "initial" && p.TextDigest == text
		if matches {
			if index >= 0 {
				return ErrMissingProof
			}
			index = i
		}
	}
	if index < 0 {
		return ErrMissingProof
	}
	p := s.Snapshots[index]
	if p.TextDigest != text || p.Turn != "" && p.Turn != turn || p.Proven || p.Epoch != b.Epoch || p.BindingDigest != b.Digest ||
		p.SocketIdentity != b.SocketIdentity || p.Provider != b.Provider || p.CommonInstructions != b.CommonInstructions ||
		!reflect.DeepEqual(p.Options.ThreadPolicy, b.Policy) || p.Origin == "queue" && len(p.Options.AdditionalContext) != 0 {
		return ErrMissingProof
	}
	candidate := *s
	candidate.Schema = Schema
	m := ManagedTurn{Version: 1, Turn: turn, Epoch: s.Epoch, BindingDigest: b.Digest, Digest: ManagedTurnSeed(s.Scope, s.Epoch, b.Digest, turn)}
	if s.ManagedTurn != nil && s.ManagedTurn.Turn == turn {
		m = *s.ManagedTurn
	}
	m.AdditionalContext = make(map[string]codex.AdditionalContextEntry, len(m.AdditionalContext))
	if s.ManagedTurn != nil && s.ManagedTurn.Turn == turn {
		for key, value := range s.ManagedTurn.AdditionalContext {
			m.AdditionalContext[key] = value
		}
	}
	for key, value := range p.Options.AdditionalContext {
		if old, exists := m.AdditionalContext[key]; exists && old != value {
			return ErrMissingProof
		}
		m.AdditionalContext[key] = value
	}
	if m.Count == ^uint64(0) {
		return errors.New("managed turn prefix count overflow")
	}
	m.Count++
	m.Digest, m.LastUserItemID = ManagedInputDigest(m.Digest, item, client, text), item
	candidate.ManagedTurn = &m
	candidate.Snapshots = make([]Snapshot, 0, len(s.Snapshots)-1)
	for i, snapshot := range s.Snapshots {
		if i != index && !(snapshot.Turn == turn && snapshot.Origin == "executed") {
			candidate.Snapshots = append(candidate.Snapshots, snapshot)
		}
	}
	if err := validate(candidate.Scope, candidate); err != nil {
		return err
	}
	*s = candidate
	return nil
}
