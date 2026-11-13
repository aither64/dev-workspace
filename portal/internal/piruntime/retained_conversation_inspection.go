package piruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type ConversationRetention struct {
	Conversation agentbackend.ConversationRef
	CreationID   string
	Schema       int
}

// InspectRetainedConversations uses only owning private mapping codecs. It
// never loads a worker, parses history, acquires locks or repairs missing state.
// Schemas 1/2 are an explicit compatibility boundary even if native learns 3.
func InspectRetainedConversations(ctx context.Context, userStateRoot string, workspaces []string) ([]ConversationRetention, error) {
	if !filepath.IsAbs(userStateRoot) || filepath.Clean(userStateRoot) != userStateRoot || len(workspaces) == 0 || len(workspaces) > 1024 {
		return nil, ErrScope
	}
	visited := 0
	readDir := func(path string, absent bool) ([]os.DirEntry, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		visited++
		if visited > 65536 {
			return nil, ErrScope
		}
		info, err := os.Lstat(path)
		if absent && errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || owner.Uid != uint32(os.Geteuid()) {
			return nil, ErrScope
		}
		return os.ReadDir(path)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	visited++
	// The generic state parent can predate Pi with safe shared-read modes.
	// Private Pi traversal below it still requires exact 0700 directories.
	info, err := os.Lstat(userStateRoot)
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || owner.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0700 != 0700 || info.Mode().Perm()&0022 != 0 {
		return nil, ErrScope
	}
	canonical, err := filepath.EvalSymlinks(userStateRoot)
	if err != nil {
		return nil, err
	}
	if canonical != userStateRoot {
		return nil, ErrScope
	}
	if err := syscall.Access(userStateRoot, 7); err != nil {
		return nil, err
	}
	owners := map[string]string{}
	for _, workspace := range workspaces {
		canonical, err := filepath.EvalSymlinks(workspace)
		if err != nil || !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace || canonical != workspace {
			return nil, ErrScope
		}
		info, err := os.Stat(workspace)
		if err != nil || !info.IsDir() {
			return nil, ErrScope
		}
		h := sha256.Sum256([]byte(workspace))
		key := hex.EncodeToString(h[:])
		if owners[key] != "" {
			return nil, ErrScope
		}
		owners[key] = workspace
	}
	base := filepath.Join(userStateRoot, "pi-runtime")
	namespaces, err := readDir(base, true)
	if err != nil {
		return nil, err
	}
	result := []ConversationRetention{}
	for _, namespace := range namespaces {
		if !hexPattern.MatchString(namespace.Name()) {
			return nil, ErrScope
		}
		ns := filepath.Join(base, namespace.Name())
		slugs, err := readDir(ns, false)
		if err != nil {
			return nil, err
		}
		for _, slug := range slugs {
			if !slugPattern.MatchString(slug.Name()) {
				return nil, ErrScope
			}
			slugDir := filepath.Join(ns, slug.Name())
			incarnations, err := readDir(slugDir, false)
			if err != nil {
				return nil, err
			}
			for _, inc := range incarnations {
				if !hexPattern.MatchString(inc.Name()) {
					return nil, ErrScope
				}
				sessionDir := filepath.Join(slugDir, inc.Name())
				if _, err := readDir(sessionDir, false); err != nil {
					return nil, err
				}
				members, err := readDir(filepath.Join(sessionDir, "members"), true)
				if err != nil {
					return nil, err
				}
				for _, member := range members {
					if !memberPattern.MatchString(member.Name()) {
						return nil, ErrScope
					}
					memberDir := filepath.Join(sessionDir, "members", member.Name())
					instances, err := readDir(memberDir, false)
					if err != nil {
						return nil, err
					}
					for _, instance := range instances {
						if !hexPattern.MatchString(instance.Name()) || owners[namespace.Name()] == "" {
							return nil, ErrScope
						}
						root := filepath.Join(memberDir, instance.Name())
						artifacts, err := readDir(root, false)
						if err != nil {
							return nil, err
						}
						for _, artifact := range artifacts {
							path := filepath.Join(root, artifact.Name())
							if artifact.Name() == "conversation.json" {
								info, err := os.Lstat(path)
								if err != nil || !info.Mode().IsRegular() {
									return nil, ErrConflict
								}
							} else if artifact.Name() == "agent" || artifact.Name() == "history" || artifact.Name() == "submissions" {
								if _, err := readDir(path, false); err != nil {
									return nil, err
								}
							} else {
								return nil, ErrConflict
							}
						}
						var record conversationRecord
						if err := readPrivate(filepath.Join(root, "conversation.json"), &record); err != nil {
							return nil, err
						}
						if record.Schema != 1 && record.Schema != 2 {
							return nil, ErrConflict
						}
						ref := record.Conversation
						expected, err := stateDirectory(userStateRoot, ref.Session)
						if err != nil || ref.Session.Workspace != owners[namespace.Name()] || ref.Session.Slug != slug.Name() || ref.Session.Incarnation != inc.Name() || ref.Member != member.Name() || ref.Incarnation != instance.Name() || filepath.Join(expected, "members", ref.Member, ref.Incarnation) != root || validateHandle(agentbackend.ExecutionHandle{Conversation: ref, Generation: record.Generation}) != nil {
							return nil, ErrScope
						}
						if err := validateConversationRecord(record, ref, root); err != nil {
							return nil, err
						}
						result = append(result, ConversationRetention{Conversation: ref, CreationID: record.CreationID, Schema: record.Schema})
						if len(result) > 65536 {
							return nil, ErrScope
						}
					}
				}
			}
		}
	}
	return result, nil
}
