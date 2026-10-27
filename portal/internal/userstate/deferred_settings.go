package userstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// DeferredSettings extends the existing private next-settings record. The
// request binding keeps other managed activation paths from changing its mode.
type DeferredSettings struct {
	Slug      string `json:"slug"`
	Root      string `json:"root"`
	Thread    string `json:"thread"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	Mode      string `json:"collaborationMode,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	Epoch     string `json:"epoch,omitempty"`
}

func ReadDeferredSettings(directory, slug, root, thread string) (*DeferredSettings, error) {
	digest := sha256.Sum256([]byte(thread))
	file, err := os.Open(filepath.Join(directory, "next-settings-"+hex.EncodeToString(digest[:])+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16384 {
		return nil, errors.New("invalid private thread record")
	}
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil {
		return nil, err
	}
	var record DeferredSettings
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	if record.Slug != slug || record.Root != root || record.Thread != thread || (record.Model == "") != (record.Effort == "") || (record.RequestID == "") != (record.Epoch == "") {
		return nil, errors.New("saved settings identity changed")
	}
	return &record, nil
}
