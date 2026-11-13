package piruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
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
	var status SubmissionStatus
	if !operationPattern.MatchString(id) {
		return status, ErrScope
	}
	if err := readPrivate(filepath.Join(e.root, id+".json"), &status); err != nil {
		return status, err
	}
	if status.Schema != 1 || status.Ref.OperationID != id || status.Ref.Digest != SubmissionDigest(status.Ref, status.Input, status.Settings) {
		return status, ErrConflict
	}
	switch status.Phase {
	case "Accepted":
		if status.Witness == nil || status.Witness.SessionID != status.Ref.Conversation.NativeID || status.Witness.EntryID == "" || status.Witness.EffectiveDigest != MessageDigest(status.Input.Message) {
			return status, ErrConflict
		}
	case "NotSubmitted":
		if status.Refusal == "" {
			return status, ErrConflict
		}
	case "prepared", "dispatching", "Unknown":
	default:
		return status, ErrConflict
	}
	return status, nil
}

func (e *receiptEngine) save(status SubmissionStatus) error {
	if !operationPattern.MatchString(status.Ref.OperationID) {
		return ErrScope
	}
	return writePrivate(filepath.Join(e.root, status.Ref.OperationID+".json"), status)
}

func (e *receiptEngine) lookup(ref SubmissionRef) (SubmissionStatus, error) {
	status, err := e.load(ref.OperationID)
	if err != nil {
		return status, err
	}
	if status.Ref != ref {
		return status, ErrConflict
	}
	if status.Phase == "prepared" || status.Phase == "dispatching" {
		status.Phase = "Unknown"
	}
	return status, nil
}

func (e *receiptEngine) held() (bool, error) {
	entries, err := os.ReadDir(e.root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if len(entries) > 4096 {
		return true, errors.New("Pi receipt capacity exceeded")
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return true, ErrConflict
		}
		status, err := e.load(entry.Name()[:len(entry.Name())-5])
		if err != nil {
			return true, err
		}
		if status.Phase != "Accepted" && status.Phase != "NotSubmitted" {
			return true, nil
		}
	}
	return false, nil
}

func (e *receiptEngine) send(ctx context.Context, ref SubmissionRef, input SendInput, settings EffectiveSettings, generation string,
	dispatch func(context.Context) (nativeResult, error), abort func()) (SubmissionStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !operationPattern.MatchString(ref.OperationID) || ref.Action != "send" || ref.Digest != SubmissionDigest(ref, input, settings) {
		return SubmissionStatus{}, ErrScope
	}
	old, err := e.load(ref.OperationID)
	if err == nil {
		if old.Ref != ref || old.Input != input || old.Settings != settings {
			return old, ErrConflict
		}
		if old.Phase == "prepared" || old.Phase == "dispatching" {
			old.Phase = "Unknown"
		}
		return old, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return old, err
	}
	held, err := e.held()
	if err != nil {
		return old, err
	}
	if held {
		return old, ErrHeld
	}
	now := time.Now().UTC()
	status := SubmissionStatus{Schema: 1, Ref: ref, Input: input, Settings: settings, Generation: generation,
		Phase: "prepared", CreatedAt: now, UpdatedAt: now}
	if err := e.save(status); err != nil {
		return status, err
	}
	if err := e.point("intent"); err != nil {
		return status, err
	}
	status.Phase = "dispatching"
	if err := e.save(status); err != nil {
		return status, err
	}
	if err := e.point("dispatch"); err != nil {
		return status, err
	}
	result, dispatchErr := dispatch(ctx)
	if err := e.point("native"); err != nil {
		abort()
		return status, err
	}
	status.Phase = "Unknown"
	if dispatchErr == nil {
		switch result.Outcome {
		case "Accepted":
			w := result.Witness
			if w != nil && w.SessionID == ref.Conversation.NativeID && w.EntryID != "" && w.EffectiveMessage == input.Message {
				status.Phase = "Accepted"
				status.Witness = &NativeWitness{SessionID: w.SessionID, EntryID: w.EntryID, ParentID: w.ParentID,
					PreviousLeafID: w.PreviousLeafID, EffectiveDigest: MessageDigest(w.EffectiveMessage)}
			}
		case "NotSubmitted":
			if result.Refusal != "" {
				status.Phase = "NotSubmitted"
				status.Refusal = result.Refusal
			}
		}
	}
	status.UpdatedAt = time.Now().UTC()
	if status.Phase == "Unknown" {
		abort()
	}
	if err := e.point("witness"); err != nil {
		abort()
		return status, err
	}
	if err := e.save(status); err != nil {
		abort()
		return status, fmt.Errorf("Pi receipt commit unresolved: %w", err)
	}
	if err := e.point("receipt"); err != nil {
		return status, err
	}
	// The only successful return/ACK follows the fsynced receipt publication.
	return status, nil
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
