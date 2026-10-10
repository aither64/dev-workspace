package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Separate additive records leave the recovery and roster schemas readable by
// preceding generations. Records contain settings or counters, never messages.
func (s *Server) threadRecordPath(kind, thread string) string {
	digest := sha256.Sum256([]byte(thread))
	return filepath.Join(s.operationStore.directory, kind+"-"+hex.EncodeToString(digest[:])+".json")
}

func (s *Server) loadThreadRecord(kind, thread string, value any) error {
	file, err := os.Open(s.threadRecordPath(kind, thread))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16384 {
		return errors.New("invalid private thread record")
	}
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func (s *Server) saveThreadRecord(kind, thread string, value any) error {
	if err := s.operationStore.ensureDirectory(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 16384 {
		return errors.New("thread record exceeds limit")
	}
	file, err := os.CreateTemp(s.operationStore.directory, ".thread-record-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), s.threadRecordPath(kind, thread)); err != nil {
		return err
	}
	dir, err := os.Open(s.operationStore.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
