package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

var ErrTrackingFilesLimit = errors.New("session file listing exceeds its limit")

func trackingRoot(summary *Summary) (string, error) {
	if summary == nil || !ValidSlug(summary.Slug) || !filepath.IsAbs(summary.Workspace) ||
		(summary.Root != "work" && summary.Root != "archive") || summary.Archived != (summary.Root == "archive") {
		return "", fs.ErrPermission
	}
	return filepath.Join(summary.Root, summary.Slug), nil
}

func trackingPath(value string) (string, bool) {
	path, ok := NormalizeArtifactPath(value)
	if !ok || len(path) > 4096 || !utf8.ValidString(path) || strings.Contains(path, "\\") ||
		strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return "", false
	}
	for _, part := range strings.Split(path, "/") {
		if strings.EqualFold(part, ".git") {
			return "", false
		}
	}
	return path, true
}

// OpenTrackingFile is the shared session reader for source, previews and downloads.
// Legacy own-root prefixes are aliases only after literal ENOENT; a present
// symlink, directory or inaccessible literal must never fall through to a suffix.
func OpenTrackingFile(summary *Summary, value string) (*os.File, os.FileInfo, string, error) {
	root, err := trackingRoot(summary)
	if err != nil {
		return nil, nil, "", err
	}
	path, ok := trackingPath(value)
	if !ok {
		return nil, nil, "", fs.ErrPermission
	}
	file, info, err := OpenRegularFile(summary.Workspace, filepath.Join(root, path))
	if !errors.Is(err, fs.ErrNotExist) {
		return file, info, path, err
	}
	for _, prefix := range []string{"work/", "archive/"} {
		suffix, found := strings.CutPrefix(path, prefix+summary.Slug+"/")
		if !found {
			continue
		}
		file, info, suffixErr := OpenRegularFile(summary.Workspace, filepath.Join(root, suffix))
		if suffixErr == nil {
			return file, info, suffix, nil
		}
	}
	return nil, nil, path, err
}

// ListTrackingFiles walks nofollow directory descriptors in bounded batches.
// The byte budget bounds both traversal metadata and the complete JSON result;
// no partial list is returned on failure. It never reads file content or Git.
func ListTrackingFiles(ctx context.Context, summary *Summary, maxFiles, maxBytes int) ([]string, error) {
	root, err := trackingRoot(summary)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	traversalBytes, outputBytes := 0, len(`{"files":[]}`)
	pending := []string{""}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		directory, err := openConfined(summary.Workspace, filepath.Join(root, path), unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return nil, err
		}
		err = func() error {
			defer directory.Close()
			for {
				entries, err := directory.ReadDir(128)
				if err != nil && err != io.EOF {
					return err
				}
				for _, entry := range entries {
					if err := ctx.Err(); err != nil {
						return err
					}
					candidate := filepath.Join(path, entry.Name())
					traversalBytes += len(candidate) + 1
					if traversalBytes > maxBytes {
						return ErrTrackingFilesLimit
					}
					if _, ok := trackingPath(candidate); !ok || entry.Type()&os.ModeSymlink != 0 {
						continue
					}
					if entry.IsDir() {
						pending = append(pending, candidate)
						continue
					}
					if !entry.Type().IsRegular() {
						continue
					}
					file, _, err := OpenRegularFile(summary.Workspace, filepath.Join(root, candidate))
					if errors.Is(err, fs.ErrNotExist) {
						continue
					}
					if err != nil {
						return err
					}
					file.Close()
					encoded, _ := json.Marshal(candidate)
					outputBytes += len(encoded) + len(`{"path":,"registered":false},`)
					if len(paths) >= maxFiles || outputBytes > maxBytes {
						return ErrTrackingFilesLimit
					}
					paths = append(paths, candidate)
				}
				if err == io.EOF {
					return nil
				}
			}
		}()
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	return paths, nil
}
