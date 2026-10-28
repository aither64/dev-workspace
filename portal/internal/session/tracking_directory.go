package session

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	TrackingDirectoryPageSize = 200
	TrackingDirectoryMaxBytes = 256 * 1024
	trackingCursorMaxBytes    = 4096
)

var (
	ErrTrackingDirectoryChanged = errors.New("session directory changed")
	ErrTrackingDirectoryInput   = errors.New("invalid session directory request")
)

type TrackingDirectoryEntry struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type TrackingDirectoryPage struct {
	Path       string                   `json:"path"`
	Entries    []TrackingDirectoryEntry `json:"entries"`
	NextCursor *string                  `json:"nextCursor"`
}

type directoryCursor struct {
	Version int    `json:"v"`
	Binding string `json:"binding"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
}

func ValidTrackingDirectoryPath(path string) bool {
	if path == "" {
		return true
	}
	canonical, ok := trackingPath(path)
	return ok && canonical == path
}

func validDirectoryName(name string) bool {
	return name != "" && !strings.Contains(name, "/") && ValidTrackingDirectoryPath(name)
}

func parseDirectoryCursor(value string) (directoryCursor, error) {
	var cursor directoryCursor
	if value == "" || len(value) > trackingCursorMaxBytes {
		return cursor, ErrTrackingDirectoryInput
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || !utf8.Valid(data) {
		return cursor, ErrTrackingDirectoryInput
	}
	// Decode fields separately to reject duplicates as well as unknown fields.
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return cursor, ErrTrackingDirectoryInput
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return cursor, ErrTrackingDirectoryInput
		}
		seen[key] = true
		switch key {
		case "v":
			err = decoder.Decode(&cursor.Version)
		case "binding":
			err = decoder.Decode(&cursor.Binding)
		case "kind":
			err = decoder.Decode(&cursor.Kind)
		case "name":
			err = decoder.Decode(&cursor.Name)
		default:
			return cursor, ErrTrackingDirectoryInput
		}
		if err != nil {
			return cursor, ErrTrackingDirectoryInput
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return cursor, ErrTrackingDirectoryInput
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || len(seen) != 4 || cursor.Version != 1 ||
		(cursor.Kind != "directory" && cursor.Kind != "file") || !validDirectoryName(cursor.Name) {
		return cursor, ErrTrackingDirectoryInput
	}
	digest, err := hex.DecodeString(cursor.Binding)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != cursor.Binding {
		return cursor, ErrTrackingDirectoryInput
	}
	return cursor, nil
}

type directoryGeneration struct {
	Dev, Ino     uint64
	MTime, CTime unix.Timespec
}

func trackingDirectoryGeneration(file *os.File) (directoryGeneration, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return directoryGeneration{}, err
	}
	return directoryGeneration{stat.Dev, stat.Ino, stat.Mtim, stat.Ctim}, nil
}

func directoryBinding(summary *Summary, root, path string, generation directoryGeneration) string {
	encoded, _ := json.Marshal(struct {
		Workspace, Slug, Root, Path string
		Generation                  directoryGeneration
	}{summary.Workspace, summary.Slug, root, path, generation})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// A max-heap retains only the smallest page plus one lookahead entry. Each page
// rescans immediate children, trading scan time for bounded memory and no cache.
type directoryCandidates []TrackingDirectoryEntry

func directoryEntryLess(a, b TrackingDirectoryEntry) bool {
	if a.Kind != b.Kind {
		return a.Kind == "directory"
	}
	return a.Path < b.Path
}
func (h directoryCandidates) Len() int           { return len(h) }
func (h directoryCandidates) Less(i, j int) bool { return directoryEntryLess(h[j], h[i]) }
func (h directoryCandidates) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *directoryCandidates) Push(value any)    { *h = append(*h, value.(TrackingDirectoryEntry)) }
func (h *directoryCandidates) Pop() any {
	last := (*h)[len(*h)-1]
	*h = (*h)[:len(*h)-1]
	return last
}

// ListTrackingDirectory never opens descendants or file contents. All directory
// descriptors are request-local; a cursor supplies only an untrusted position.
func ListTrackingDirectory(ctx context.Context, summary *Summary, path, continuation string) (TrackingDirectoryPage, error) {
	page := TrackingDirectoryPage{Path: path, Entries: []TrackingDirectoryEntry{}}
	if !ValidTrackingDirectoryPath(path) {
		return page, ErrTrackingDirectoryInput
	}
	var cursor directoryCursor
	if continuation != "" {
		var err error
		cursor, err = parseDirectoryCursor(continuation)
		if err != nil {
			return page, err
		}
	}
	root, err := trackingRoot(summary)
	if err != nil {
		return page, err
	}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	relative := filepath.Join(root, path)
	directory, err := openConfined(summary.Workspace, relative, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return page, err
	}
	defer directory.Close()
	generation, err := trackingDirectoryGeneration(directory)
	if err != nil {
		return page, err
	}
	binding := directoryBinding(summary, root, path, generation)
	if continuation != "" && cursor.Binding != binding {
		return page, ErrTrackingDirectoryChanged
	}
	after := TrackingDirectoryEntry{Path: filepath.Join(path, cursor.Name), Kind: cursor.Kind}
	candidates := make(directoryCandidates, 0, TrackingDirectoryPageSize+1)
	for {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		entries, readErr := directory.ReadDir(128)
		if readErr != nil && readErr != io.EOF {
			return page, readErr
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return page, err
			}
			candidate := TrackingDirectoryEntry{Path: filepath.Join(path, entry.Name())}
			if !validDirectoryName(entry.Name()) || !ValidTrackingDirectoryPath(candidate.Path) {
				continue
			}
			var stat unix.Stat_t
			if err := unix.Fstatat(int(directory.Fd()), entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return page, ErrTrackingDirectoryChanged
				}
				return page, err
			}
			switch stat.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				candidate.Kind = "directory"
			case unix.S_IFREG:
				candidate.Kind = "file"
			default:
				continue
			}
			if continuation != "" && !directoryEntryLess(after, candidate) {
				continue
			}
			if len(candidates) < TrackingDirectoryPageSize+1 {
				heap.Push(&candidates, candidate)
			} else if directoryEntryLess(candidate, candidates[0]) {
				candidates[0] = candidate
				heap.Fix(&candidates, 0)
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	final, err := trackingDirectoryGeneration(directory)
	if err != nil {
		return page, err
	}
	if final != generation {
		return page, ErrTrackingDirectoryChanged
	}
	// Detect rename/replacement of this path or its work/archive root as well as
	// changes observed through the original open descriptor.
	current, err := openConfined(summary.Workspace, relative, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return page, ErrTrackingDirectoryChanged
	}
	currentGeneration, err := trackingDirectoryGeneration(current)
	current.Close()
	if err != nil {
		return page, err
	}
	if currentGeneration != generation {
		return page, ErrTrackingDirectoryChanged
	}
	sort.Slice(candidates, func(i, j int) bool { return directoryEntryLess(candidates[i], candidates[j]) })
	count := min(len(candidates), TrackingDirectoryPageSize)
	for {
		page.Entries = candidates[:count]
		page.NextCursor = nil
		if count < len(candidates) {
			last := candidates[count-1]
			encoded, _ := json.Marshal(directoryCursor{1, binding, last.Kind, filepath.Base(last.Path)})
			value := base64.RawURLEncoding.EncodeToString(encoded)
			page.NextCursor = &value
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			return TrackingDirectoryPage{}, err
		}
		if len(encoded)+1 <= TrackingDirectoryMaxBytes {
			break
		}
		count--
		if count == 0 {
			return TrackingDirectoryPage{}, ErrTrackingDirectoryInput
		}
	}
	return page, ctx.Err()
}
