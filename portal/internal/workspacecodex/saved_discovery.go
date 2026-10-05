package workspacecodex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"

	"github.com/aither64/codex-web/codex"
	"golang.org/x/sys/unix"
)

// Selected 0.160's bare state-only list can turn an unavailable DB into empty.
// Explicit project selectors and project/list propagate those errors instead.
// Saved headers independently account for imports that have never been indexed.
func (c *Client) RequireSavedConversationAbsence(ctx context.Context, cwd string) error {
	if !canonicalObservationCWD(cwd) {
		return errors.New("conversation absence requires an exact canonical directory")
	}
	projects, err := c.absenceProjects(ctx)
	if err != nil {
		return observationFailure("absence_unavailable", err.Error())
	}
	selectors := []*string{nil}
	for _, project := range projects {
		selectors = append(selectors, &project)
	}
	for _, project := range selectors {
		for _, archived := range []bool{false, true} {
			var page struct {
				Data       *[]codex.ThreadMetadata `json:"data"`
				NextCursor *string                 `json:"nextCursor"`
			}
			err := c.Request(ctx, "thread/list", map[string]any{
				"cwd": cwd, "projectId": project, "archived": archived,
				"sourceKinds": ArchiveDiscoverySourceKinds(), "modelProviders": []string{},
				"useStateDbOnly": true, "sortDirection": "asc", "limit": 1,
			}, &page)
			if err != nil || page.Data == nil {
				return observationFailure("absence_unavailable", "Indexed conversation absence cannot be verified.")
			}
			if len(*page.Data) != 0 || page.NextCursor != nil {
				return observationFailure("conversation_residue", "A conversation or incomplete index contradicts threadless tracking.")
			}
		}
	}
	files, err := c.savedConversationScopes(ctx, cwd)
	if err != nil {
		return err
	}
	again, err := c.absenceProjects(ctx)
	if err != nil || !slices.Equal(projects, again) {
		return observationFailure("absence_unavailable", "Native projects changed during conversation absence proof.")
	}
	return files.recheck(ctx, c)
}

func (c *Client) absenceProjects(ctx context.Context) ([]string, error) {
	ids, cursors := map[string]bool{}, map[string]bool{}
	cursor := ""
	for pages := 0; pages < 64; pages++ {
		var page struct {
			Data *[]struct {
				ID string `json:"id"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		params := map[string]any{"limit": 100, "sortKey": "position", "sortDirection": "asc"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := c.Request(ctx, "project/list", params, &page); err != nil {
			return nil, fmt.Errorf("enumerate native projects: %w", err)
		}
		if page.Data == nil || len(*page.Data) > 100 {
			return nil, errors.New("native project discovery has an invalid data array")
		}
		for _, project := range *page.Data {
			if !observationID(project.ID, true) || strings.TrimSpace(project.ID) == "" || ids[project.ID] {
				return nil, errors.New("native project identity is invalid or repeated")
			}
			ids[project.ID] = true
		}
		if page.NextCursor == nil {
			result := make([]string, 0, len(ids))
			for id := range ids {
				result = append(result, id)
			}
			slices.Sort(result)
			return result, nil
		}
		if !observationID(*page.NextCursor, true) || len(*page.NextCursor) > 4096 || cursors[*page.NextCursor] {
			return nil, errors.New("native project discovery has an invalid or repeated cursor")
		}
		cursors[*page.NextCursor], cursor = true, *page.NextCursor
	}
	return nil, errors.New("native project discovery exceeded its page bound")
}

type savedScopeFile struct {
	info     os.FileInfo
	prefix   []byte
	metadata *codex.ThreadMetadata // exact public fallback, only when the header cannot prove scope
}
type savedScopeDirectory struct {
	info  os.FileInfo
	names []string
}
type savedScopes struct {
	directories map[string]savedScopeDirectory
	files       map[string]savedScopeFile
}

func ownedSavedInfo(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid()) && info.Mode()&os.ModeSymlink == 0
}

func (c *Client) savedConversationScopes(ctx context.Context, cwd string) (*savedScopes, error) {
	home, err := filepath.EvalSymlinks(c.CodexHome)
	info, statErr := os.Lstat(c.CodexHome)
	if err != nil || statErr != nil || !canonicalArchivePath(c.CodexHome) || home != c.CodexHome || !info.IsDir() || !ownedSavedInfo(info) {
		return nil, observationFailure("absence_unavailable", "Saved conversation home cannot be verified.")
	}
	proof := &savedScopes{directories: map[string]savedScopeDirectory{}, files: map[string]savedScopeFile{}}
	proof.directories[home] = savedScopeDirectory{info: info} // home identity, not unrelated home contents
	for _, root := range []string{filepath.Join(home, "sessions"), filepath.Join(home, "archived_sessions")} {
		if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
			proof.directories[root] = savedScopeDirectory{}
			continue
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return walkErr
			}
			info, err := os.Lstat(path)
			if err != nil || !ownedSavedInfo(info) {
				return errors.New("saved path ownership or identity is unavailable")
			}
			if info.IsDir() {
				entries, err := os.ReadDir(path)
				if err != nil {
					return err
				}
				names := make([]string, 0, len(entries))
				for _, child := range entries {
					names = append(names, child.Name())
				}
				proof.directories[path] = savedScopeDirectory{info: info, names: names}
				return nil
			}
			if !info.Mode().IsRegular() {
				return errors.New("saved rollout is not a regular file")
			}
			match := archiveRolloutPattern.FindStringSubmatch(strings.TrimSuffix(filepath.Base(path), ".zst"))
			if len(match) != 2 {
				return errors.New("saved rollout filename has no exact thread identity")
			}
			file := savedScopeFile{info: info}
			if strings.HasSuffix(path, ".zst") {
				plain := strings.TrimSuffix(path, ".zst")
				if sibling, err := os.Lstat(plain); err == nil {
					if !sibling.Mode().IsRegular() || !ownedSavedInfo(sibling) {
						return errors.New("plain rollout sibling is invalid")
					}
					proof.files[path] = file
					return nil // native plain-first convention; WalkDir also proves that sibling
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			} else {
				header, prefix, err := savedHeader(path, info)
				file.prefix = prefix
				if len(prefix) > archiveHeaderLimit {
					return errors.New("saved first metadata record exceeds its bound")
				}
				if err == nil && header.ID != match[1] {
					return errors.New("saved header contradicts its filename identity")
				}
				if err == nil && header.Cwd != nil && canonicalObservationCWD(*header.Cwd) {
					if *header.Cwd == cwd {
						return observationFailure("conversation_residue", "A saved conversation uses the session directory.")
					}
					proof.files[path] = file
					return nil
				}
			}
			metadata, err := c.ReadThreadMetadata(ctx, match[1], false)
			if err != nil || metadata.ID != match[1] || !canonicalObservationCWD(metadata.Cwd) || metadata.Path == nil || !sameSavedSpelling(path, *metadata.Path) {
				return errors.New("saved conversation scope cannot be positively verified")
			}
			if metadata.Cwd == cwd {
				return observationFailure("conversation_residue", "A saved conversation uses the session directory.")
			}
			file.metadata = &metadata
			proof.files[path] = file
			return nil
		})
		if err != nil {
			var refused *ObservationError
			if errors.As(err, &refused) {
				return nil, err
			}
			return nil, observationFailure("absence_unavailable", err.Error())
		}
	}
	return proof, nil
}

func sameSavedSpelling(path, metadataPath string) bool {
	return canonicalArchivePath(metadataPath) && strings.TrimSuffix(path, ".zst") == strings.TrimSuffix(metadataPath, ".zst")
}

func savedHeader(path string, before os.FileInfo) (archiveHeader, []byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return archiveHeader{}, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return archiveHeader{}, nil, errors.New("saved rollout changed while opening")
	}
	return readArchiveHeaderRecord(file)
}

func (proof *savedScopes) recheck(ctx context.Context, c *Client) error {
	unknown := func() error {
		return observationFailure("absence_unavailable", "Saved conversation scope changed during absence proof.")
	}
	for path, directory := range proof.directories {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if directory.info == nil {
			if !errors.Is(err, os.ErrNotExist) {
				return unknown()
			}
			continue
		}
		if err != nil || !info.IsDir() || !ownedSavedInfo(info) || !os.SameFile(info, directory.info) {
			return unknown()
		}
		if directory.names != nil {
			entries, err := os.ReadDir(path)
			if err != nil {
				return unknown()
			}
			names := make([]string, 0, len(entries))
			for _, child := range entries {
				names = append(names, child.Name())
			}
			if !slices.Equal(names, directory.names) {
				return unknown()
			}
		}
	}
	for path, file := range proof.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || !ownedSavedInfo(info) || !os.SameFile(info, file.info) {
			return unknown()
		}
		if !strings.HasSuffix(path, ".zst") {
			_, prefix, _ := savedHeader(path, info)
			if !bytes.Equal(prefix, file.prefix) {
				return unknown()
			}
		}
		if file.metadata != nil {
			metadata, err := c.ReadThreadMetadata(ctx, file.metadata.ID, false)
			if err != nil || !reflect.DeepEqual(metadata, *file.metadata) {
				return unknown()
			}
		}
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(info, current) {
			return unknown()
		}
	}
	return nil
}
