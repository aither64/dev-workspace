package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type TrackingSyncStatus struct {
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type ArchiveProgress struct {
	Schema      int    `json:"schema"`
	Workspace   string `json:"workspace"`
	Slug        string `json:"slug"`
	OperationID string `json:"operation_id"`
	Stage       string `json:"stage"`
	Path        string `json:"path,omitempty"`
	UpdatedAt   string `json:"updated_at"`
}

func ReadArchiveProgress(workspace, stateRoot, slug, operationID string) *ArchiveProgress {
	if stateRoot == "" || !slugPattern.MatchString(slug) || operationID == "" {
		return nil
	}
	identity := sha256.Sum256([]byte(workspace))
	data, err := os.ReadFile(filepath.Join(stateRoot, "archive-progress", hex.EncodeToString(identity[:]), slug+".json"))
	var progress ArchiveProgress
	if err != nil || len(data) > 64*1024 || json.Unmarshal(data, &progress) != nil || progress.Schema != 1 ||
		progress.Workspace != workspace || progress.Slug != slug || progress.OperationID != operationID {
		return nil
	}
	return &progress
}

func ReadTrackingSync(workspace, stateRoot, slug string) (*TrackingSyncStatus, error) {
	if !slugPattern.MatchString(slug) {
		return nil, errors.New("invalid tracking sync session")
	}
	if stateRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		stateHome := os.Getenv("XDG_STATE_HOME")
		if stateHome == "" {
			stateHome = filepath.Join(home, ".local", "state")
		}
		stateRoot = filepath.Join(stateHome, "dev-workspaces")
	}
	identity := sha256.Sum256([]byte(workspace))
	path := filepath.Join(stateRoot, "tracking-sync", hex.EncodeToString(identity[:]), slug+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt struct {
		Schema    int    `json:"schema"`
		Workspace string `json:"workspace"`
		Slug      string `json:"slug"`
		State     string `json:"state"`
		Error     string `json:"error"`
		UpdatedAt string `json:"updated_at"`
	}
	if len(data) > 64*1024 || json.Unmarshal(data, &receipt) != nil || receipt.Schema != 1 ||
		receipt.Workspace != workspace || receipt.Slug != slug || (receipt.State != "pending" && receipt.State != "synced") {
		return nil, errors.New("invalid tracking sync receipt")
	}
	return &TrackingSyncStatus{receipt.State, receipt.Error, receipt.UpdatedAt}, nil
}
