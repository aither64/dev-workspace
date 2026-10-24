package teamruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"gopkg.in/yaml.v3"
)

// PrepareDelete belongs only to the ordinary deletion owner, which keeps its
// lifecycle, generation and session locks across both calls and retirement.
// It abandons a managed reservation, without claiming its native start had no effect.
func (service Service) PrepareDelete(ctx context.Context, slug, rootThreadID, operationID string, checkOnly bool) error {
	if ctx == nil || service.Store == nil || service.Client == nil || !session.ValidSlug(slug) ||
		rootThreadID == "" || !digestPattern.MatchString(operationID) {
		return errors.New("invalid team deletion preparation")
	}
	return service.Store.withOperationLock(ctx, slug, func() error {
		context, err := service.deleteContext(slug, rootThreadID, operationID, checkOnly)
		if err != nil {
			return err
		}
		roster, err := service.Store.Load(slug, rootThreadID)
		if err != nil {
			return err
		}
		candidates, err := deletionCandidates(roster, context.unstarted)
		if err != nil {
			return err
		}
		if !context.force {
			cwd := filepath.Join(service.Store.workspace, "work", slug)
			for _, member := range roster.Members {
				if candidates[member.Address] {
					continue
				}
				if err := service.requireIdleMember(ctx, member, cwd); err != nil {
					return err
				}
			}
		}
		current, err := service.deleteContext(slug, rootThreadID, operationID, checkOnly)
		if err != nil || current != context {
			return errors.New("session deletion context changed during preparation")
		}
		if checkOnly || len(candidates) == 0 {
			return nil
		}
		_, err = service.Store.Update(ctx, slug, rootThreadID, false, func(updated *Roster) error {
			current, err := service.deleteContext(slug, rootThreadID, operationID, false)
			if err != nil || current != context || !reflect.DeepEqual(updated, roster) {
				return errors.New("session deletion context or roster changed before publication")
			}
			now := time.Now().UTC()
			for index := range updated.Members {
				member := &updated.Members[index]
				if candidates[member.Address] {
					member.State, member.RemovedAt = "removed", &now
				}
			}
			return nil
		})
		return err
	})
}

// Every member is validated before the first reservation can be changed.
func deletionCandidates(roster *Roster, unstarted bool) (map[string]bool, error) {
	candidates := make(map[string]bool)
	for _, member := range roster.Members {
		// Valid terminal records keep their existing no-op semantics.
		if member.State == "removed" || member.Thread != "" {
			continue
		}
		if !unstarted || roster.ForkSource != nil || !projectUUIDPattern.MatchString(member.ProjectID) ||
			!member.CreateAttempted || member.BootstrapAttempted || member.RetireIntent != "" {
			return nil, fmt.Errorf("member %s has no confirmed thread; resolve creation before deletion", member.Address)
		}
		switch member.State {
		case "creating":
			if member.RemovedAt != nil {
				return nil, fmt.Errorf("member %s has a conflicting removal marker", member.Address)
			}
			candidates[member.Address] = true
		default:
			return nil, fmt.Errorf("member %s has no confirmed thread", member.Address)
		}
	}
	return candidates, nil
}

type deletionContext struct {
	journal, manifest [32]byte
	force, unstarted  bool
}

// This is the deletion owner's schema-1 record, not the portal's presentation
// projection. Inventory fields stay owned and validated by the Ruby owner.
type deletionJournal struct {
	Schema          int             `json:"schema"`
	Slug            string          `json:"slug"`
	Workspace       string          `json:"workspace"`
	CreatedAt       string          `json:"created_at"`
	OperationID     string          `json:"operation_id"`
	State           string          `json:"state"`
	Phase           string          `json:"phase"`
	Force           *bool           `json:"force"`
	Tracking        string          `json:"tracking"`
	Recovery        string          `json:"recovery"`
	GitTracked      *bool           `json:"git_tracked,omitempty"`
	Worktrees       json.RawMessage `json:"worktrees,omitempty"`
	WorktreesSealed *bool           `json:"worktrees_sealed,omitempty"`
}

func (service Service) deleteContext(slug, rootThreadID, operationID string, checkOnly bool) (deletionContext, error) {
	workspace := service.Store.workspace
	if operation, err := session.PendingLifecycle(workspace, slug); err != nil || operation != "delete" {
		return deletionContext{}, errors.New("team deletion requires its live removal journal")
	}
	data, err := readDeletionFile(workspace, filepath.Join("worktrees", ".locks", slug+".removal.json"), 64*1024, true)
	if err != nil {
		return deletionContext{}, err
	}
	var journal deletionJournal
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return deletionContext{}, fmt.Errorf("decode session deletion journal: %w", err)
	}
	phase := "thread_retiring"
	if checkOnly {
		phase = "validated"
	}
	_, timeErr := time.Parse(time.RFC3339Nano, journal.CreatedAt)
	if decoder.Decode(new(any)) != io.EOF || journal.Schema != 1 || journal.Slug != slug ||
		journal.Workspace != workspace || journal.OperationID != operationID || journal.Force == nil ||
		journal.Phase != phase || journal.State != "prepared" || timeErr != nil ||
		(journal.Tracking != "work" && journal.Tracking != "archive") ||
		(journal.WorktreesSealed != nil && *journal.WorktreesSealed && len(journal.Worktrees) == 0) {
		return deletionContext{}, errors.New("session deletion journal identity, phase or force is invalid")
	}
	recoveryRoot := userstate.WorkspaceDirectory(service.Store.stateRoot, "removed", workspace)
	if filepath.Dir(journal.Recovery) != recoveryRoot || filepath.Clean(journal.Recovery) != journal.Recovery ||
		!strings.Contains(filepath.Base(journal.Recovery), "-"+slug+"-") {
		return deletionContext{}, errors.New("session deletion recovery identity is invalid")
	}
	info, err := os.Lstat(journal.Recovery)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return deletionContext{}, errors.New("session deletion recovery directory is unavailable or unsafe")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return deletionContext{}, errors.New("session deletion recovery directory has the wrong owner")
	}
	summary, err := session.Find(workspace, slug)
	if err != nil {
		return deletionContext{}, fmt.Errorf("validate session deletion manifest: %w", err)
	}
	if summary.Root != journal.Tracking || summary.Manifest.Codex.ThreadID != rootThreadID {
		return deletionContext{}, errors.New("session deletion manifest has the wrong retained root")
	}
	manifest, err := readDeletionFile(workspace, filepath.Join(summary.Root, slug, session.ManifestName), 1024*1024, false)
	if err != nil {
		return deletionContext{}, err
	}
	var presence struct {
		Codex struct {
			ThreadID string `yaml:"thread_id"`
		} `yaml:"codex"`
		Creation struct {
			State     string `yaml:"state"`
			Attempted *bool  `yaml:"initial_goal_attempted"`
			Sent      *bool  `yaml:"initial_goal_sent"`
		} `yaml:"creation"`
	}
	if err := yaml.Unmarshal(manifest, &presence); err != nil || presence.Codex.ThreadID != rootThreadID ||
		presence.Creation.State != summary.Manifest.Creation.State ||
		presence.Creation.Sent != nil && *presence.Creation.Sent != summary.Manifest.Creation.InitialGoalSent ||
		!reflect.DeepEqual(presence.Creation.Attempted, summary.Manifest.Creation.InitialGoalAttempted) {
		return deletionContext{}, errors.New("session deletion manifest changed during validation")
	}
	return deletionContext{journal: sha256.Sum256(data), manifest: sha256.Sum256(manifest), force: *journal.Force,
		unstarted: !summary.Archived && summary.Manifest.ForkedFrom == "" && presence.Creation.State == "creating" && presence.Creation.Attempted != nil &&
			!*presence.Creation.Attempted && presence.Creation.Sent != nil && !*presence.Creation.Sent}, nil
}

func readDeletionFile(workspace, relative string, limit int64, private bool) ([]byte, error) {
	file, info, err := session.OpenRegularFile(workspace, relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	owner, ok := info.Sys().(*syscall.Stat_t)
	if info.Size() > limit || private && (!ok || owner.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o600) {
		return nil, errors.New("session deletion record is unsafe or exceeds bounds")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("cannot read bounded session deletion record")
	}
	return data, nil
}
