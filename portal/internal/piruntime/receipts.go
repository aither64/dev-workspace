package piruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

func digestValue(value any) string {
	data, _ := json.Marshal(value)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// MessageDigest remains distinct from the full immutable operation digest.
func MessageDigest(message string) string {
	hash := sha256.Sum256([]byte(message))
	return hex.EncodeToString(hash[:])
}

func SubmissionDigest(ref SubmissionRef, input SendInput, settings EffectiveSettings) string {
	ref.Digest = ""
	return digestValue(struct {
		Version  int
		Ref      SubmissionRef
		Input    SendInput
		Settings EffectiveSettings
	}{1, ref, input, settings})
}

func writePrivate(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writePrivateBytes(path, append(data, '\n'))
}

func writePrivateBytes(path string, data []byte) error {
	if len(data) > recordLimit {
		return errors.New("Pi private record too large")
	}
	dir := filepath.Dir(path)
	if err := makePrivateDirectory(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".publish-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	folder, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer folder.Close()
	return folder.Sync()
}

func readPrivate(path string, out any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	owner, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 || stat.Size() > recordLimit {
		return errors.New("invalid Pi private record")
	}
	data, err := io.ReadAll(io.LimitReader(file, recordLimit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) != stat.Size() {
		return errors.New("Pi record changed while reading")
	}
	return strictDecode(data, out, recordLimit)
}

type nativeResult struct {
	Outcome string         `json:"outcome"`
	Refusal string         `json:"refusal,omitempty"`
	Witness *workerWitness `json:"witness,omitempty"`
}
type workerWitness struct {
	SessionID        string `json:"sessionId"`
	EntryID          string `json:"entryId"`
	ParentID         string `json:"parentId"`
	PreviousLeafID   string `json:"previousLeafId"`
	EffectiveMessage string `json:"effectiveMessage"`
}

// receiptEngine has one writer in the verified supervisor. Incomplete records
// left by any prior writer are observational Unknown; they never dispatch again.
type receiptEngine struct {
	root       string
	ownerKind  string
	mu         sync.Mutex
	checkpoint func(string) error // in-process fixture fault injection, never wire input
}

func (e *receiptEngine) point(name string) error {
	if e.checkpoint != nil {
		return e.checkpoint(name)
	}
	return nil
}

func (e *receiptEngine) load(id string) (SubmissionStatus, error) {
	if e.expectedOwner() != "retained" {
		return SubmissionStatus{}, ErrConflict
	}
	r, err := e.loadOwned(id)
	if err != nil {
		return SubmissionStatus{}, err
	}
	return r.retained(), nil
}
func (e *receiptEngine) save(status SubmissionStatus) error {
	if status.Schema != 1 {
		return ErrConflict
	}
	return e.saveOwned(fromRetained(status))
}
func (e *receiptEngine) lookup(ref SubmissionRef) (SubmissionStatus, error) {
	r, err := e.lookupOwned(receiptReference{retained: &ref})
	if err != nil {
		return SubmissionStatus{}, err
	}
	return r.retained(), nil
}
func (e *receiptEngine) send(ctx context.Context, ref SubmissionRef, input SendInput, settings EffectiveSettings, generation string, dispatch func(context.Context) (nativeResult, error), abort func()) (SubmissionStatus, error) {
	r, err := e.sendOwned(ctx, receiptReference{retained: &ref}, input, settings, generation, dispatch, abort)
	return r.retained(), err
}

// Sync each newly created directory's parent before a durable record can rely
// on it. A file rename alone does not persist a freshly created ancestor.
func makePrivateDirectory(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return ErrScope
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return ErrScope
	}
	if err := makePrivateDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	folder, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer folder.Close()
	return folder.Sync()
}
