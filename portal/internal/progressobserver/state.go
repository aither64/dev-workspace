// Package progressobserver keeps advisory progress separate from lifecycle state.
package progressobserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const Interval = 15 * time.Minute
const Deadline = 3 * time.Minute

// Pending retains the original request so uncertain delivery reuses its identity.
type Pending struct {
	ID        string    `json:"id"`
	Thread    string    `json:"thread"`
	Address   string    `json:"address"`
	Model     string    `json:"model"`
	Effort    string    `json:"effort"`
	Message   string    `json:"message"`
	StartedAt time.Time `json:"startedAt"`
	Turn      string    `json:"turn,omitempty"`
}
type Concern struct {
	Key            string `json:"key"`
	Summary        string `json:"summary"`
	Recommendation string `json:"recommendation"`
	Count          int    `json:"count"`
	Response       string `json:"response,omitempty"`
}
type Notice struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Message string `json:"message"`
}
type State struct {
	Schema      int       `json:"schema"`
	Root        string    `json:"root"`
	Paused      bool      `json:"paused"`
	LastCheck   time.Time `json:"lastCheck"`
	Pending     *Pending  `json:"pending,omitempty"`
	Concern     *Concern  `json:"concern,omitempty"`
	Notice      *Notice   `json:"notice,omitempty"`
	Unavailable string    `json:"unavailable,omitempty"`
}
type Result struct {
	Status         string `json:"status"`
	Key            string `json:"key,omitempty"`
	Summary        string `json:"summary,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
}

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func ParseResult(text string) (Result, error) {
	var r Result
	d := json.NewDecoder(strings.NewReader(strings.TrimSpace(text)))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return r, errors.New("observer did not return a progress result")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return r, errors.New("observer returned extra output")
	}
	if r.Status == "on_track" && r.Key == "" && r.Summary == "" && r.Recommendation == "" {
		return r, nil
	}
	if r.Status != "concern" || !keyPattern.MatchString(r.Key) || strings.TrimSpace(r.Summary) == "" || strings.TrimSpace(r.Recommendation) == "" || len(strings.Fields(r.Summary+" "+r.Recommendation)) > 150 || len(r.Summary)+len(r.Recommendation) > 3000 {
		return r, errors.New("observer returned an invalid or oversized concern")
	}
	return r, nil
}

// Due requires current development work. Recent activity in an idle session
// cannot start inference; long-running active work can still be observed.
func (s State) Due(now time.Time, active bool) bool {
	return !s.Paused && s.Pending == nil && !now.Before(s.LastCheck.Add(Interval)) && active
}
func (s *State) Apply(r Result) bool {
	s.Pending, s.Unavailable = nil, ""
	if r.Status == "on_track" {
		s.Concern = nil
		return false
	}
	if s.Concern != nil && s.Concern.Key == r.Key {
		s.Concern.Count++
		s.Concern.Summary, s.Concern.Recommendation = r.Summary, r.Recommendation
		return false
	}
	s.Concern = &Concern{Key: r.Key, Summary: r.Summary, Recommendation: r.Recommendation, Count: 1}
	return true
}
func (s State) Warning() bool { return s.Concern != nil && s.Concern.Count >= 2 }

// Store is optional, private and ignored by earlier package generations.
type Store struct{ Directory string }

func NewStore(root, workspace string) Store {
	h := sha256.Sum256([]byte(workspace))
	return Store{filepath.Join(root, "progress-observers", hex.EncodeToString(h[:]))}
}
func (s Store) path(root string) string {
	h := sha256.Sum256([]byte(root))
	return filepath.Join(s.Directory, hex.EncodeToString(h[:])+".json")
}
func (s Store) Load(root string) (State, error) {
	state := State{Schema: 1, Root: root}
	f, err := os.Open(s.path(root))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return state, err
	}
	if len(b) > 65536 {
		return state, errors.New("progress state exceeds limit")
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return state, err
	}
	if state.Schema != 1 || state.Root != root {
		return state, errors.New("progress state identity changed")
	}
	return state, nil
}
func (s Store) Save(state State) error {
	if state.Schema != 1 || state.Root == "" {
		return errors.New("invalid progress state")
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(b) > 65536 {
		return errors.New("progress state exceeds limit")
	}
	if err := os.MkdirAll(s.Directory, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Directory, ".progress-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), s.path(state.Root)); err != nil {
		return err
	}
	dir, err := os.Open(s.Directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func ID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%x", h[:16])
}
