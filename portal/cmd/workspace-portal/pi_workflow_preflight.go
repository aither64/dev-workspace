package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

var workflowMemberPattern = regexp.MustCompile(`^(lead|[a-z][a-z0-9]{0,31}[0-9]+)$`)

func workflowEmptyLock(path string, info os.FileInfo) error {
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 0 || owner.Uid != uint32(os.Geteuid()) {
		return errors.New("invalid retained workflow lock")
	}
	return nil
}

func (s *piTeamScanner) workflowClaim(kind, id string, ref agentbackend.ConversationRef) error {
	scope := monitoroperation.PiSessionRecord{Workspace: ref.Session.Workspace, Slug: ref.Session.Slug, Incarnation: ref.Session.Incarnation}
	if ref.Member == "lead" {
		return s.claim(kind, id, scope, &piTeamLead{Incarnation: ref.Incarnation, NativeID: ref.NativeID})
	}
	return s.claim(kind, id, scope, nil, piTeamMember{Address: ref.Member, Incarnation: ref.Incarnation, NativeID: ref.NativeID})
}

// workflowRecords validates every present full record. No view/Store reader or
// native callback is used; empty and terminal records retain their scope claim.
func (s *piTeamScanner) workflowRecords(ctx context.Context, kind, name string) error {
	base := filepath.Join(s.root, kind)
	if _, err := os.Lstat(base); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	owners := map[string]string{}
	for _, w := range s.workspaces {
		owners[piTeamHash(w)] = w
	}
	visited := 0
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > 65536 {
			return errors.New("retained workflow traversal exceeds limit")
		}
		entries, err := piTeamDirectory(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if depth < 5 {
				valid := false
				switch depth {
				case 0:
					valid = piTeamHex.MatchString(entry.Name())
				case 1:
					valid = session.ValidSlug(entry.Name())
				case 2, 4:
					valid = piTeamHex.MatchString(entry.Name())
				case 3:
					valid = workflowMemberPattern.MatchString(entry.Name())
				}
				if !valid {
					return errors.New("invalid retained workflow directory")
				}
				if err := walk(path, depth+1); err != nil {
					return err
				}
				continue
			}
			if entry.Name() == name+".lock" {
				info, err := os.Lstat(path)
				if err != nil {
					return err
				}
				if err := workflowEmptyLock(path, info); err != nil {
					return err
				}
				continue
			}
			if entry.Name() != name {
				return errors.New("unexpected retained workflow artifact")
			}
			s.count++
			if s.count > 65536 {
				return errors.New("retained Pi record inventory exceeds limit")
			}
			data, err := piTeamRead(path, piquestions.StoreLimit)
			if err != nil {
				return err
			}
			var ref agentbackend.ConversationRef
			if kind == "pi-questions" {
				r, err := piquestions.DecodeRetainedRecord(data)
				if err != nil {
					return err
				}
				ref = r.Conversation
			} else {
				r, err := piplan.DecodeRetainedRecord(data)
				if err != nil {
					return err
				}
				ref = r.Conversation
			}
			expected, err := (piquestions.StateFile{Root: s.root, Kind: kind, Name: name, Ref: ref}).Path()
			if err != nil || expected != path || owners[piTeamHash(ref.Session.Workspace)] != ref.Session.Workspace {
				return errors.New("retained workflow scope/path differs")
			}
			rel, err := filepath.Rel(base, path)
			if err != nil {
				return err
			}
			source := "questions"
			if kind == "pi-plans" {
				source = "plans"
			}
			if err := s.workflowClaim(source, piTeamHash(filepath.ToSlash(rel)), ref); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(base, 0)
}

func (s *piTeamScanner) workflowConversations(rows []piruntime.ConversationRetention) error {
	for _, r := range rows {
		s.count++
		if s.count > 65536 {
			return errors.New("retained Pi record inventory exceeds limit")
		}
		if r.Schema != 1 && r.Schema != 2 {
			return errors.New("unsupported retained conversation schema")
		}
		if r.Schema == 1 {
			if r.HasInstructions {
				return errors.New("retained instructions require conversation schema 2")
			}
			continue
		}
		ref := r.Conversation
		scope := monitoroperation.PiSessionRecord{Workspace: ref.Session.Workspace, Slug: ref.Session.Slug, Incarnation: ref.Session.Incarnation}
		id := piTeamHash(piTeamHash(ref.Session.Workspace) + "/" + ref.Session.Slug + "/" + ref.Session.Incarnation + "/members/" + ref.Member + "/" + ref.Incarnation + "/conversation.json")
		if err := s.claim("tool_policy", id, scope, nil, piTeamMember{Address: ref.Member, Incarnation: ref.Incarnation, NativeID: ref.NativeID, CreationID: r.CreationID}); err != nil {
			return err
		}
		if r.HasInstructions {
			if err := s.claim("role_instructions", id, scope, nil, piTeamMember{Address: ref.Member, Incarnation: ref.Incarnation, NativeID: ref.NativeID, CreationID: r.CreationID}); err != nil {
				return err
			}
		}
	}
	return nil
}
