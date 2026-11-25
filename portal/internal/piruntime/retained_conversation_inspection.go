package piruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type ConversationRetention struct {
	Conversation                   agentbackend.ConversationRef
	CreationID                     string
	Schema                         int
	HasInstructions                bool
	HasFork                        bool
	HasInputDelivery               bool
	HasInputRecovery               bool
	HasTeamConflictRecovery        bool
	HasTeamConflictTerminalRefusal bool
}

// InspectRetainedConversations uses only owning private mapping codecs. It
// never loads a worker, acquires locks or repairs missing state. Current runtime
// compatibility additionally verifies saved recovery history and receipts.
// Schema 3 is the strict fork-origin variant; it never upgrades legacy records.
func InspectRetainedConversations(ctx context.Context, userStateRoot string, workspaces []string) ([]ConversationRetention, error) {
	return inspectRetainedConversations(ctx, userStateRoot, workspaces, nil)
}

// InspectRetainedConversationsAndRuntimes joins compatibility metadata in the
// existing session traversal, including a reserved runtime with no members.
func InspectRetainedConversationsAndRuntimes(ctx context.Context, userStateRoot string, workspaces []string) ([]ConversationRetention, []RetainedRuntimeState, error) {
	runtimes := []RetainedRuntimeState{}
	conversations, err := inspectRetainedConversations(ctx, userStateRoot, workspaces, &runtimes)
	if err != nil {
		return nil, nil, err
	}
	return conversations, runtimes, nil
}

func inspectRetainedConversations(ctx context.Context, userStateRoot string, workspaces []string, runtimes *[]RetainedRuntimeState) ([]ConversationRetention, error) {
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
				var current *RetainedRuntimeState
				if runtimes != nil {
					workspace := owners[namespace.Name()]
					if workspace == "" {
						return nil, ErrScope
					}
					observed, err := InspectRetainedCurrentRuntime(ctx, userStateRoot, agentbackend.SessionRef{Workspace: workspace, Slug: slug.Name(), Incarnation: inc.Name()})
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						return nil, err
					}
					if err == nil {
						current = &observed
					}
				}
				teamRecoveries, err := inspectRetainedTeamConflict(ctx, userStateRoot, sessionDir, agentbackend.SessionRef{Workspace: owners[namespace.Name()], Slug: slug.Name(), Incarnation: inc.Name()}, current)
				if err != nil {
					return nil, err
				}
				if current != nil {
					*runtimes = append(*runtimes, *current)
					if len(*runtimes) > 65536 {
						return nil, ErrScope
					}
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
							} else if artifact.Name() == "agent" || artifact.Name() == "history" || artifact.Name() == "submissions" || artifact.Name() == "input-recoveries" {
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
						if record.Schema != 1 && record.Schema != 2 && record.Schema != 3 {
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
						hasInputDelivery, err := retainedInputDelivery(filepath.Join(root, "submissions"))
						if err != nil {
							return nil, err
						}
						recoveries, err := readInputRecoveries(root)
						if err != nil {
							return nil, err
						}
						result = append(result, ConversationRetention{Conversation: ref, CreationID: record.CreationID, Schema: record.Schema, HasInstructions: record.Instructions != "", HasFork: record.Fork != nil, HasInputDelivery: hasInputDelivery, HasInputRecovery: len(recoveries) != 0, HasTeamConflictRecovery: teamRecoveries[ref].Recovery, HasTeamConflictTerminalRefusal: teamRecoveries[ref].TerminalRefusal})
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

func retainedInputDelivery(root string) (bool, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || len(entries) > 4096 {
		return false, ErrScope
	}
	engine := &receiptEngine{root: root}
	required := false
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return false, ErrScope
		}
		record, err := engine.loadOwned(entry.Name()[:len(entry.Name())-5])
		if err != nil {
			return false, err
		}
		if strings.HasPrefix(record.Input.ActionContext, inputDeliveryPrefix) {
			if record.Input.ActionContext != inputDeliveryPrefix+"steer" && record.Input.ActionContext != inputDeliveryPrefix+"followUp" {
				return false, ErrScope
			}
			required = true
		}
	}
	return required, nil
}
