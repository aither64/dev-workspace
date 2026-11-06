package turnretry

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type evidenceMarker struct {
	Schema int    `json:"schema"`
	Scope  Scope  `json:"scope"`
	Source string `json:"source"`
	Ready  bool   `json:"ready"`
}

func (s Store) databasePath(scope Scope) string { return s.path(scope) + ".sqlite" }

func atomicPrivateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".retry-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func decodeMarker(scope Scope, data []byte) (evidenceMarker, error) {
	var m evidenceMarker
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, err
	}
	if m.Schema != Schema || m.Scope != scope || (m.Source != "empty" && m.Source != "legacy") {
		return m, errors.New("retry evidence marker identity changed")
	}
	return m, nil
}

// Cutover is fail-closed for old writers: the incompatible marker is durable
// before database publication. Recovery uses the immutable source backup.
// The caller holds the original per-thread admission lock throughout.
func (s Store) migrateEvidence(scope Scope, create bool) error {
	data, err := readPrivateRecord(s.path(scope))
	var state State
	var marker evidenceMarker
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return err
		}
		marker = evidenceMarker{Schema: Schema, Scope: scope, Source: "empty"}
		state = New(scope)
	} else if err != nil {
		return err
	} else {
		var head struct {
			Schema int `json:"schema"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			return err
		}
		if head.Schema == Schema {
			marker, err = decodeMarker(scope, data)
			if err != nil {
				return err
			}
			if _, err := os.Stat(s.databasePath(scope)); err == nil {
				if marker.Ready {
					return nil
				}
				if _, err := s.loadEvidence(scope); err != nil {
					return err
				}
				marker.Ready = true
				encoded, _ := json.Marshal(marker)
				return atomicPrivateFile(s.path(scope), append(encoded, '\n'))
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if marker.Ready {
				return errors.New("retry evidence database is missing")
			}
			if marker.Source == "empty" {
				state = New(scope)
			} else {
				backup, err := readPrivateRecord(s.path(scope) + ".legacy")
				if err != nil {
					return err
				}
				state, err = decodeState(scope, backup)
				if err == nil && state.Schema != 1 && state.Schema != 2 {
					err = errors.New("unsupported retry migration source")
				}
				if err != nil {
					return err
				}
			}
		} else {
			state, err = decodeState(scope, data)
			if err != nil {
				return err
			}
			if state.Schema != 1 && state.Schema != 2 {
				return errors.New("unsupported retry migration source")
			}
			if err := atomicPrivateFile(s.path(scope)+".legacy", data); err != nil {
				return err
			}
			marker = evidenceMarker{Schema: Schema, Scope: scope, Source: "legacy"}
		}
	}
	encoded, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if err := atomicPrivateFile(s.path(scope), append(encoded, '\n')); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.path(scope)), ".retry-db-*")
	if err != nil {
		return err
	}
	path := file.Name()
	file.Close()
	defer os.Remove(path)
	defer os.Remove(path + "-journal")
	db, err := openEvidence(path, true)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`PRAGMA synchronous=FULL;
	 CREATE TABLE retry_state(id INTEGER PRIMARY KEY CHECK(id=1),revision INTEGER NOT NULL,body BLOB NOT NULL);
	 CREATE TABLE retry_settings(id TEXT PRIMARY KEY,body BLOB NOT NULL);
	 CREATE TABLE retry_snapshots(id TEXT PRIMARY KEY,turn TEXT NOT NULL,proven INTEGER NOT NULL,
	 epoch TEXT NOT NULL,origin TEXT NOT NULL,text_digest TEXT NOT NULL,
	 settings_id TEXT NOT NULL REFERENCES retry_settings(id),body BLOB NOT NULL);
	 CREATE INDEX retry_snapshots_turn ON retry_snapshots(turn,proven,epoch);
	 CREATE INDEX retry_snapshots_initial ON retry_snapshots(origin,text_digest);
 CREATE INDEX retry_snapshots_settings ON retry_snapshots(settings_id);`)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = importEvidence(tx, state); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = db.Close(); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return err
	}
	if err = os.Rename(path, s.databasePath(scope)); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	marker.Ready = true
	encoded, err = json.Marshal(marker)
	if err != nil {
		return err
	}
	return atomicPrivateFile(s.path(scope), append(encoded, '\n'))
}

func importEvidence(tx *sql.Tx, state State) error {
	for _, p := range state.Snapshots {
		if err := writeSnapshot(tx, p); err != nil {
			return err
		}
	}
	state.Schema = Schema
	state.Snapshots = []Snapshot{}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO retry_state(id,revision,body) VALUES(1,0,?)", data)
	return err
}
