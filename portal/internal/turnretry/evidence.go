package turnretry

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// This is a read batch, never a limit on retained or accepted inputs.
const snapshotReadBatch = 64

type evidenceView struct {
	path     string
	revision int64
	original map[string]string
}

func openEvidence(path string, writable bool) (*sql.DB, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("retry evidence database must be private")
	}
	mode := "ro"
	if writable {
		mode = "rw"
	}
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?mode="+mode+"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func snapshotEncoding(p Snapshot) string {
	data, _ := json.Marshal(p)
	return string(data)
}

// Loaded rows form a bounded working set. Changes to that set are committed
// together with the state; unloaded rows are never treated as deletions.
func (s *State) cacheSnapshots(values []Snapshot) {
	if s.evidence == nil {
		return
	}
	v := *s.evidence
	v.original = make(map[string]string, len(s.evidence.original)+len(values))
	for id, value := range s.evidence.original {
		v.original[id] = value
	}
	s.Snapshots = append([]Snapshot{}, s.Snapshots...)
	for _, p := range values {
		if _, loaded := v.original[p.ID]; loaded {
			continue
		}
		v.original[p.ID] = snapshotEncoding(p)
		s.Snapshots = append(s.Snapshots, p)
	}
	s.evidence = &v
}

func decodeEvidence(body, settings []byte) (Snapshot, error) {
	var p, row Snapshot
	if err := json.Unmarshal(settings, &p); err != nil {
		return p, err
	}
	if err := json.Unmarshal(body, &row); err != nil {
		return p, err
	}
	p.ID, p.Turn, p.Proven, p.TextDigest, p.Digest = row.ID, row.Turn, row.Proven, row.TextDigest, row.Digest
	digest, err := p.Hash()
	if err != nil || digest != p.Digest {
		return Snapshot{}, errors.New("retry evidence snapshot is invalid")
	}
	return p, nil
}

func (s *State) readEvidence(where string, args []any, limit int) ([]Snapshot, error) {
	return s.readEvidenceOrdered(where, args, limit, "p.rowid")
}

func (s *State) readEvidenceOrdered(where string, args []any, limit int, order string) ([]Snapshot, error) {
	if s.evidence == nil {
		return nil, nil
	}
	db, err := openEvidence(s.evidence.path, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRow("SELECT revision FROM retry_state WHERE id=1").Scan(&revision); err != nil {
		return nil, err
	}
	if revision != s.evidence.revision {
		return nil, errors.New("retry evidence revision changed")
	}
	ids := make([]string, 0, len(s.evidence.original))
	for id := range s.evidence.original {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		where += " AND p.id NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
		for _, id := range ids {
			args = append(args, id)
		}
	}
	args = append(args, limit)
	rows, err := tx.Query("SELECT p.body,c.body FROM retry_snapshots p JOIN retry_settings c ON c.id=p.settings_id WHERE "+where+" ORDER BY "+order+" LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []Snapshot{}
	for rows.Next() {
		var body, settings []byte
		if err := rows.Scan(&body, &settings); err != nil {
			return nil, err
		}
		p, err := decodeEvidence(body, settings)
		if err != nil {
			return nil, err
		}
		values = append(values, p)
	}
	return values, rows.Err()
}

func (s *State) Snapshot(id string) (*Snapshot, error) {
	for _, p := range s.Snapshots {
		if p.ID == id {
			return &p, nil
		}
	}
	values, err := s.readEvidence("p.id=?", []any{id}, 1)
	if err != nil || len(values) == 0 {
		return nil, err
	}
	s.cacheSnapshots(values)
	return &values[0], nil
}

func (s *State) initialSnapshot(text string) (*Snapshot, error) {
	values, err := s.readEvidence("p.origin='initial' AND p.text_digest=?", []any{text}, 2)
	if err != nil {
		return nil, err
	}
	s.cacheSnapshots(values)
	var found *Snapshot
	for _, p := range s.Snapshots {
		if p.Origin == "initial" && p.TextDigest == text {
			if found != nil {
				return nil, ErrMissingProof
			}
			copy := p
			found = &copy
		}
	}
	return found, nil
}

func (s *State) HasUnprovenSnapshots(turn string) (bool, error) {
	for _, p := range s.Snapshots {
		if p.Turn == turn && !p.Proven {
			return true, nil
		}
	}
	values, err := s.readEvidence("p.turn=? AND p.proven=0", []any{turn}, 1)
	return len(values) > 0, err
}

// SnapshotCount includes persisted rows outside the working set. It is useful
// for diagnostics without materializing all retained settings.
func (s *State) SnapshotCount() (int, error) {
	if s.evidence == nil {
		return len(s.Snapshots), nil
	}
	db, err := openEvidence(s.evidence.path, false)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var count int
	var revision int64
	err = db.QueryRow("SELECT revision,(SELECT count(*) FROM retry_snapshots) FROM retry_state WHERE id=1").Scan(&revision, &count)
	if err != nil {
		return 0, err
	}
	if revision != s.evidence.revision {
		return 0, errors.New("retry evidence revision changed")
	}
	return count - len(s.evidence.original) + len(s.Snapshots), nil
}

func writeSnapshot(tx *sql.Tx, p Snapshot) error {
	settings := p
	settings.ID, settings.Turn, settings.Proven, settings.TextDigest, settings.Digest = "", "", false, "", ""
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	id := hash(string(data))
	if _, err := tx.Exec("INSERT INTO retry_settings(id,body) VALUES(?,?) ON CONFLICT(id) DO NOTHING", id, data); err != nil {
		return err
	}
	row, err := json.Marshal(Snapshot{ID: p.ID, Turn: p.Turn, Proven: p.Proven, TextDigest: p.TextDigest, Digest: p.Digest})
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO retry_snapshots(id,turn,proven,epoch,origin,text_digest,settings_id,body)
	 VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET turn=excluded.turn,proven=excluded.proven,
	 epoch=excluded.epoch,origin=excluded.origin,text_digest=excluded.text_digest,settings_id=excluded.settings_id,body=excluded.body`,
		p.ID, p.Turn, p.Proven, strconv.FormatUint(p.Epoch, 10), p.Origin, p.TextDigest, id, row)
	return err
}

func (t *Transaction) saveEvidence() error {
	candidate := t.State
	candidate.Schema = Schema
	if candidate.evidenceErr != nil {
		return candidate.evidenceErr
	}
	if err := validate(t.scope, candidate); err != nil {
		return err
	}
	metadata := candidate
	metadata.Snapshots = []Snapshot{}
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	if len(data) > MaxBytes {
		return errors.New("retry state metadata exceeds 1 MiB")
	}
	db, err := openEvidence(t.store.databasePath(t.scope), true)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRow("SELECT revision FROM retry_state WHERE id=1").Scan(&revision); err != nil {
		return err
	}
	if candidate.evidence == nil || revision != candidate.evidence.revision {
		return errors.New("retry evidence revision changed")
	}
	present := map[string]string{}
	retired := map[string]bool{}
	for _, p := range candidate.Snapshots {
		encoded := snapshotEncoding(p)
		present[p.ID] = encoded
		if candidate.evidence.original[p.ID] != encoded {
			var old string
			err := tx.QueryRow("SELECT settings_id FROM retry_snapshots WHERE id=?", p.ID).Scan(&old)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if old != "" {
				retired[old] = true
			}
			if err := writeSnapshot(tx, p); err != nil {
				return err
			}
		}
	}
	for id := range candidate.evidence.original {
		if _, exists := present[id]; !exists {
			var old string
			if err := tx.QueryRow("DELETE FROM retry_snapshots WHERE id=? RETURNING settings_id", id).Scan(&old); err != nil {
				return err
			}
			retired[old] = true
		}
	}
	for id := range retired {
		if _, err := tx.Exec("DELETE FROM retry_settings WHERE id=? AND NOT EXISTS (SELECT 1 FROM retry_snapshots WHERE settings_id=?)", id, id); err != nil {
			return err
		}
	}
	if revision == int64(^uint64(0)>>1) {
		return errors.New("retry evidence revision overflow")
	}
	if _, err := tx.Exec("UPDATE retry_state SET revision=?,body=? WHERE id=1", revision+1, data); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	t.State.Schema = Schema
	t.State.evidence = &evidenceView{path: t.store.databasePath(t.scope), revision: revision + 1, original: present}
	return nil
}

func (s Store) loadEvidence(scope Scope) (State, error) {
	db, err := openEvidence(s.databasePath(scope), false)
	if err != nil {
		return State{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return State{}, err
	}
	defer tx.Rollback()
	var revision int64
	var data []byte
	if err := tx.QueryRow("SELECT revision,body FROM retry_state WHERE id=1").Scan(&revision, &data); err != nil {
		return State{}, err
	}
	state, err := decodeState(scope, data)
	if err != nil {
		return State{}, fmt.Errorf("invalid retry evidence metadata: %w", err)
	}
	if len(data) > MaxBytes || state.Schema != Schema || len(state.Snapshots) != 0 || revision < 0 {
		return State{}, errors.New("invalid retry evidence metadata")
	}
	state.evidence = &evidenceView{path: s.databasePath(scope), revision: revision, original: map[string]string{}}
	rows, err := tx.Query("SELECT p.body,c.body FROM retry_snapshots p JOIN retry_settings c ON c.id=p.settings_id ORDER BY p.rowid LIMIT ?", snapshotReadBatch)
	if err != nil {
		return State{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var body, settings []byte
		if err := rows.Scan(&body, &settings); err != nil {
			return State{}, err
		}
		p, err := decodeEvidence(body, settings)
		if err != nil {
			return State{}, err
		}
		state.Snapshots = append(state.Snapshots, p)
		state.evidence.original[p.ID] = snapshotEncoding(p)
	}
	if err := rows.Err(); err != nil {
		return State{}, err
	}
	return state, validate(scope, state)
}

// BindUnboundSnapshots runs only after an explicit native load confirmed this
// binding. Walk outstanding admissions in bounded batches, including queued
// inputs outside the initial working set. The per-thread lock stays held.
func (t *Transaction) BindUnboundSnapshots(b Binding) error {
	if err := t.Save(); err != nil {
		return err
	}
	cursor := ""
	for {
		state, err := t.store.loadEvidence(t.scope)
		if err != nil {
			return err
		}
		// Start with an empty cache so pagination also visits the initial batch.
		state.Snapshots = []Snapshot{}
		state.evidence.original = map[string]string{}
		values, err := state.readEvidenceOrdered("p.id>? AND p.epoch=?", []any{cursor, strconv.FormatUint(b.Epoch, 10)}, snapshotReadBatch, "p.id")
		if err != nil {
			return err
		}
		if len(values) == 0 {
			return nil
		}
		state.cacheSnapshots(values)
		changed := false
		for i := range state.Snapshots {
			p := &state.Snapshots[i]
			if p.BindingDigest == "" && p.SocketIdentity == b.SocketIdentity && p.Provider == b.Provider && p.CommonInstructions == b.CommonInstructions && reflect.DeepEqual(p.Options.ThreadPolicy, b.Policy) {
				p.BindingDigest = b.Digest
				p.Digest, err = p.Hash()
				if err != nil {
					return err
				}
				changed = true
			}
		}
		t.State = state
		if changed {
			if err := t.Save(); err != nil {
				return err
			}
		}
		cursor = values[len(values)-1].ID
	}
}

// EvidenceRevision invalidates disposable idle hints even when a change touches
// only snapshots outside the loaded batch. It grants no execution authority.
func (s State) EvidenceRevision() int64 {
	if s.evidence == nil {
		return -1
	}
	return s.evidence.revision
}
