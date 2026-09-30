package repository

// Mutable repository state is read only during capture. Published previews never
// refer back to the index or worktree, and no synthetic objects are written.
import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	MaxWorktreeSnapshotBytes = 64 << 20
	MaxWorktreeStoreBytes    = 256 << 20
	maxWorktreeIndexOutput   = 32 << 20
)

var (
	ErrWorktreeChanged = errors.New("the repository changed while the snapshot was being captured; try again")
	ErrWorktreeState   = errors.New("the repository index or a working path cannot be reviewed safely")
)

type WorktreeCapture struct {
	Kind                 string
	SourceHead           string
	CapturedAt           time.Time
	Files                []ReviewFile
	UnverifiedSubmodules []WorktreeSubmodule
	Contents             map[string]ReviewContent
	RawBytes             int64
	RootID               string
}

// A gitlink's index identity is frozen without inspecting its nested worktree.
type WorktreeSubmodule struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Object string `json:"object"`
}

type treeEntry struct{ mode, object string }
type workStamp struct {
	dev, ino, mode, size, mtimeSec, mtimeNsec, ctimeSec, ctimeNsec uint64
}
type workEntry struct {
	treeEntry
	blob    ReviewBlob
	stamp   workStamp
	warning string
}

// Account for previews before retaining them; the final capture also counts
// the index-side previews against the same snapshot limit.
func admitWorkingEntry(working map[string]workEntry, changed map[string]struct{}, unverified int, retained *int64, path string, item workEntry) error {
	if _, exists := changed[path]; exists {
		return ErrWorktreeState
	}
	if len(changed)+unverified >= maxReviewFiles || int64(len(item.blob.Text)) > MaxWorktreeSnapshotBytes-*retained {
		return ErrReviewLimit
	}
	working[path] = item
	changed[path] = struct{}{}
	*retained += int64(len(item.blob.Text))
	return nil
}

type indexStamp struct {
	ctimeSec, ctimeNsec, mtimeSec, mtimeNsec, dev, ino, size uint64
	flags                                                    uint64
}

var indexDebugRecord = regexp.MustCompile(`^  ctime: ([0-9]+):([0-9]+)\n  mtime: ([0-9]+):([0-9]+)\n  dev: ([0-9]+)\tino: ([0-9]+)\n  uid: [0-9]+\tgid: [0-9]+\n  size: ([0-9]+)\tflags: ([0-9a-fA-F]+)\n`)

func parseIndexDebug(raw []byte) (map[string]indexStamp, error) {
	result := make(map[string]indexStamp)
	for len(raw) > 0 {
		boundary := bytes.IndexByte(raw, 0)
		if boundary < 0 || !validReviewPath(string(raw[:boundary])) {
			return nil, ErrWorktreeState
		}
		path := string(raw[:boundary])
		raw = raw[boundary+1:]
		fields := indexDebugRecord.FindSubmatchIndex(raw)
		if fields == nil || fields[0] != 0 {
			return nil, ErrWorktreeState
		}
		var numbers [7]uint64
		for i := range numbers {
			value, err := strconv.ParseUint(string(raw[fields[2+i*2]:fields[3+i*2]]), 10, 64)
			if err != nil {
				return nil, ErrWorktreeState
			}
			numbers[i] = value
		}
		flags, err := strconv.ParseUint(string(raw[fields[16]:fields[17]]), 16, 64)
		if err != nil {
			return nil, ErrWorktreeState
		}
		result[path] = indexStamp{numbers[0], numbers[1], numbers[2], numbers[3], numbers[4], numbers[5], numbers[6], flags}
		raw = raw[fields[1]:]
	}
	return result, nil
}

func indexMatchesWorking(index indexStamp, actual workStamp, mode string, indexWrite time.Time) bool {
	if actual == (workStamp{}) || index.ctimeSec == 0 || index.mtimeSec == 0 {
		return false
	}
	// Git treats an entry whose working mtime could coincide with the index
	// write as racy. Open it even when the cached stat tuple still matches.
	workingTime := time.Unix(int64(actual.mtimeSec), int64(actual.mtimeNsec))
	if !workingTime.Before(indexWrite) {
		return false
	}
	if index.ctimeSec != actual.ctimeSec || index.ctimeNsec != actual.ctimeNsec ||
		index.mtimeSec != actual.mtimeSec || index.mtimeNsec != actual.mtimeNsec ||
		uint32(index.dev) != uint32(actual.dev) || uint32(index.ino) != uint32(actual.ino) ||
		uint32(index.size) != uint32(actual.size) {
		return false
	}
	if mode == "120000" {
		return actual.mode&unix.S_IFMT == unix.S_IFLNK
	}
	if mode == "160000" {
		return false
	} // Keep the submodule limitation visible.
	if actual.mode&unix.S_IFMT != unix.S_IFREG {
		return false
	}
	return (mode == "100755") == (actual.mode&0111 != 0)
}

func stamp(st unix.Stat_t) workStamp {
	return workStamp{uint64(st.Dev), st.Ino, uint64(st.Mode), uint64(st.Size),
		uint64(st.Mtim.Sec), uint64(st.Mtim.Nsec), uint64(st.Ctim.Sec), uint64(st.Ctim.Nsec)}
}

func WorktreeRootID(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", ErrWorktreeState
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}

func validReviewPath(path string) bool {
	if !utf8.ValidString(path) || path == "" || filepath.IsAbs(path) || strings.ContainsRune(path, 0) || strings.Contains(path, "\\") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func parseTreeEntries(raw []byte) (map[string]treeEntry, error) {
	entries := make(map[string]treeEntry)
	for _, field := range bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0}) {
		if len(field) == 0 {
			continue
		}
		prefix, path, ok := bytes.Cut(field, []byte{'\t'})
		parts := bytes.Fields(prefix)
		if !ok || len(parts) != 3 || !validReviewPath(string(path)) || !gitObjectPattern.Match(parts[2]) {
			return nil, ErrWorktreeState
		}
		mode := string(parts[0])
		if mode != "100644" && mode != "100755" && mode != "120000" && mode != "160000" {
			return nil, ErrWorktreeState
		}
		entries[string(path)] = treeEntry{mode, string(parts[2])}
	}
	return entries, nil
}

func parseIndexEntries(raw []byte, debug map[string]indexStamp) (map[string]treeEntry, error) {
	entries := make(map[string]treeEntry)
	for _, field := range bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0}) {
		if len(field) == 0 {
			continue
		}
		prefix, path, ok := bytes.Cut(field, []byte{'\t'})
		parts := bytes.Fields(prefix)
		if !ok || len(parts) != 3 || !validReviewPath(string(path)) {
			return nil, ErrWorktreeState
		}
		if string(parts[2]) != "0" {
			return nil, fmt.Errorf("%w: resolve index conflicts first", ErrWorktreeState)
		}
		mode, object := string(parts[0]), string(parts[1])
		metadata, ok := debug[string(path)]
		if !ok {
			return nil, ErrWorktreeState
		}
		if mode == "040000" || mode != "100644" && mode != "100755" && mode != "120000" && mode != "160000" {
			return nil, fmt.Errorf("%w: sparse or special index entries", ErrWorktreeState)
		}
		if !gitObjectPattern.Match(parts[1]) && !allZeroObject(object) {
			return nil, ErrWorktreeState
		}
		if allZeroObject(object) || metadata.flags&0x20000000 != 0 {
			// Intent-to-add has no staged bytes; its working bytes appear unstaged.
			continue
		}
		entries[string(path)] = treeEntry{mode, object}
	}
	return entries, nil
}

func allZeroObject(value string) bool {
	return (len(value) == 40 || len(value) == 64) && strings.Trim(value, "0") == ""
}

func nulPaths(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[len(raw)-1] != 0 {
		return nil, ErrWorktreeState
	}
	var paths []string
	for _, field := range bytes.Split(raw[:len(raw)-1], []byte{0}) {
		path := string(field)
		if !validReviewPath(path) {
			return nil, errors.New("a repository path cannot be displayed safely in this review")
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func (r ReviewReader) worktreeIndex(ctx context.Context, repo ReviewRepository) ([]byte, []byte, []byte, error) {
	index, err := r.git(ctx, repo.Worktree, maxWorktreeIndexOutput, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, nil, nil, err
	}
	flags, err := r.git(ctx, repo.Worktree, maxWorktreeIndexOutput, "ls-files", "-v", "-z")
	if err != nil {
		return nil, nil, nil, err
	}
	for _, field := range bytes.Split(flags, []byte{0}) {
		if len(field) > 0 && (field[0] == 'S' || field[0] == 's') {
			return nil, nil, nil, fmt.Errorf("%w: skip-worktree index entries", ErrWorktreeState)
		}
	}
	untracked, err := r.git(ctx, repo.Worktree, maxWorktreeIndexOutput, "ls-files", "--others", "--exclude-standard", "-z")
	return index, flags, untracked, err
}

func (r ReviewReader) worktreeDebug(ctx context.Context, repo ReviewRepository) ([]byte, map[string]indexStamp, error) {
	raw, err := r.git(ctx, repo.Worktree, maxWorktreeIndexOutput, "ls-files", "--debug", "-z")
	if err != nil {
		return nil, nil, err
	}
	parsed, err := parseIndexDebug(raw)
	return raw, parsed, err
}

func (r ReviewReader) CaptureWorktree(ctx context.Context, repo ReviewRepository, kind string) (WorktreeCapture, error) {
	if repo.Archived || repo.Worktree == "" || kind != "staged" && kind != "unstaged" {
		return WorktreeCapture{}, ErrWorktreeState
	}
	for attempt := 0; attempt < 3; attempt++ {
		capture, changed, err := r.captureWorktreeOnce(ctx, repo, kind)
		if errors.Is(err, ErrWorktreeChanged) {
			continue
		}
		if err != nil {
			return WorktreeCapture{}, err
		}
		if !changed {
			return capture, nil
		}
	}
	return WorktreeCapture{}, ErrWorktreeChanged
}

func (r ReviewReader) indexDigest(ctx context.Context, repo ReviewRepository) ([32]byte, time.Time, error) {
	var digest [32]byte
	out, err := r.git(ctx, repo.Worktree, 8192, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return digest, time.Time{}, err
	}
	path := strings.TrimSpace(string(out))
	file, err := os.Open(path)
	if err != nil {
		return digest, time.Time{}, err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(file, 64<<20+1)); err != nil {
		return digest, time.Time{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return digest, time.Time{}, err
	}
	if info.Size() > 64<<20 {
		return digest, time.Time{}, ErrReviewLimit
	}
	copy(digest[:], h.Sum(nil))
	return digest, info.ModTime(), nil
}

func (r ReviewReader) captureWorktreeOnce(ctx context.Context, repo ReviewRepository, kind string) (WorktreeCapture, bool, error) {
	capture := WorktreeCapture{Kind: kind, SourceHead: repo.Head, CapturedAt: time.Now().UTC(), Contents: make(map[string]ReviewContent)}
	root, err := unix.Open(repo.Worktree, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return capture, false, ErrWorktreeState
	}
	defer unix.Close(root)
	var rootStat unix.Stat_t
	if err := unix.Fstat(root, &rootStat); err != nil {
		return capture, false, err
	}
	capture.RootID = fmt.Sprintf("%d:%d", rootStat.Dev, rootStat.Ino)
	preHead, err := r.git(ctx, repo.Worktree, 1024, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return capture, false, err
	}
	if strings.TrimSpace(string(preHead)) != repo.Head {
		return capture, true, nil
	}
	preIndexDigest, indexWrite, err := r.indexDigest(ctx, repo)
	if err != nil {
		return capture, false, err
	}
	index, flags, others, err := r.worktreeIndex(ctx, repo)
	if err != nil {
		return capture, false, err
	}
	debugRaw, debug, err := r.worktreeDebug(ctx, repo)
	if err != nil {
		return capture, false, err
	}
	entries, err := parseIndexEntries(index, debug)
	if err != nil {
		return capture, false, err
	}
	paths, err := nulPaths(others)
	if err != nil {
		return capture, false, err
	}
	for path, metadata := range debug {
		if metadata.flags&0x20000000 != 0 {
			paths = append(paths, path)
		}
	}
	before := entries
	if kind == "staged" {
		tree, err := r.git(ctx, repo.Directory, maxWorktreeIndexOutput, "ls-tree", "-r", "-z", repo.Head, "--")
		if err != nil {
			return capture, false, err
		}
		before, err = parseTreeEntries(tree)
		if err != nil {
			return capture, false, err
		}
	}
	changedPaths := make(map[string]struct{})
	working := make(map[string]workEntry)
	var retainedWorkingBytes int64
	stamps := make(map[string]workStamp)
	if kind == "staged" {
		for path, entry := range before {
			if entry != entries[path] {
				changedPaths[path] = struct{}{}
			}
		}
		for path, entry := range entries {
			if err := ctx.Err(); err != nil {
				return capture, false, err
			}
			if entry != before[path] {
				changedPaths[path] = struct{}{}
			}
		}
	} else {
		for path, entry := range entries {
			if err := ctx.Err(); err != nil {
				return capture, false, err
			}
			if entry.mode == "160000" {
				if len(changedPaths)+len(capture.UnverifiedSubmodules) >= maxReviewFiles {
					return capture, false, ErrReviewLimit
				}
				capture.UnverifiedSubmodules = append(capture.UnverifiedSubmodules, WorktreeSubmodule{Path: path, Mode: entry.mode, Object: entry.object})
				continue
			}
			beforeStamp, err := statWorkingPath(root, path)
			if err != nil {
				return capture, false, err
			}
			stamps[path] = beforeStamp
			if indexMatchesWorking(debug[path], beforeStamp, entry.mode, indexWrite) {
				continue
			}
			item, err := readWorkingPath(ctx, root, path, entry.mode, len(entry.object))
			if err != nil {
				return capture, false, err
			}
			if item.stamp != beforeStamp {
				return capture, true, nil
			}
			if item.treeEntry != entry || item.warning != "" {
				if err := admitWorkingEntry(working, changedPaths, len(capture.UnverifiedSubmodules), &retainedWorkingBytes, path, item); err != nil {
					return capture, false, err
				}
			}
		}
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return capture, false, err
			}
			if _, tracked := entries[path]; tracked {
				continue
			}
			if _, seen := changedPaths[path]; seen {
				continue
			}
			if len(changedPaths)+len(capture.UnverifiedSubmodules) >= maxReviewFiles {
				return capture, false, ErrReviewLimit
			}
			beforeStamp, err := statWorkingPath(root, path)
			if err != nil {
				return capture, false, err
			}
			item, err := readWorkingPath(ctx, root, path, "", len(repo.Head))
			if err != nil {
				return capture, false, err
			}
			if item.mode == "" || item.stamp != beforeStamp {
				return capture, true, nil
			}
			stamps[path] = beforeStamp
			if err := admitWorkingEntry(working, changedPaths, len(capture.UnverifiedSubmodules), &retainedWorkingBytes, path, item); err != nil {
				return capture, false, err
			}
		}
	}
	if len(changedPaths)+len(capture.UnverifiedSubmodules) > maxReviewFiles {
		return capture, false, ErrReviewLimit
	}
	sort.Slice(capture.UnverifiedSubmodules, func(i, j int) bool {
		return capture.UnverifiedSubmodules[i].Path < capture.UnverifiedSubmodules[j].Path
	})
	ordered := make([]string, 0, len(changedPaths))
	for path := range changedPaths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	for _, path := range ordered {
		if err := ctx.Err(); err != nil {
			return capture, false, err
		}
		old := before[path]
		var next treeEntry
		if kind == "staged" {
			next = entries[path]
		} else {
			next = working[path].treeEntry
		}
		file := ReviewFile{Path: path, OldMode: zeroMode(old.mode), NewMode: zeroMode(next.mode), OldObject: zeroObject(old.object, repo.Head), NewObject: zeroObject(next.object, repo.Head)}
		file.ID = ReviewID(strconv.Itoa(len(capture.Files)) + "\x00" + path)
		switch {
		case old.mode == "":
			file.Status = "A"
		case next.mode == "":
			file.Status = "D"
		case old.mode != next.mode && (old.mode == "120000" || next.mode == "120000"):
			file.Status = "T"
		default:
			file.Status = "M"
		}
		var content ReviewContent
		if old.mode == "" {
			content.Before = ReviewBlob{Kind: "absent"}
		} else {
			content.Before, err = r.blob(ctx, repo.Directory, old.object, old.mode)
			if err != nil {
				return capture, false, err
			}
		}
		if next.mode == "" {
			content.After = ReviewBlob{Kind: "absent"}
		} else if kind == "staged" {
			content.After, err = r.blob(ctx, repo.Directory, next.object, next.mode)
			if err != nil {
				return capture, false, err
			}
		} else {
			content.After = working[path].blob
		}
		if kind == "unstaged" && working[path].warning != "" {
			file.Warning = working[path].warning
		}
		capture.RawBytes += int64(len(content.Before.Text) + len(content.After.Text))
		if capture.RawBytes > MaxWorktreeSnapshotBytes {
			return capture, false, ErrReviewLimit
		}
		if content.Before.Limited || content.After.Limited {
			file.Limited = true
		}
		if !content.Before.Binary && !content.After.Binary && !content.Before.Limited && !content.After.Limited && content.Before.Kind != "submodule" && content.After.Kind != "submodule" {
			content.Diff, file.Additions, file.Deletions, err = worktreeTextDiff(ctx, content.Before, content.After)
			if err != nil {
				return capture, false, err
			}
		}
		capture.Files = append(capture.Files, file)
		capture.Contents[file.ID] = content
	}
	if r.captureHook != nil {
		r.captureHook()
	}
	postHead, err := r.git(ctx, repo.Worktree, 1024, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return capture, false, err
	}
	postIndexDigest, postIndexWrite, err := r.indexDigest(ctx, repo)
	if err != nil {
		return capture, false, err
	}
	postIndex, postFlags, postOthers, err := r.worktreeIndex(ctx, repo)
	if err != nil {
		return capture, false, err
	}
	postDebugRaw, _, err := r.worktreeDebug(ctx, repo)
	if err != nil {
		return capture, false, err
	}
	if !bytes.Equal(preHead, postHead) || preIndexDigest != postIndexDigest || !indexWrite.Equal(postIndexWrite) || !bytes.Equal(index, postIndex) || !bytes.Equal(flags, postFlags) || !bytes.Equal(others, postOthers) || !bytes.Equal(debugRaw, postDebugRaw) {
		return capture, true, nil
	}
	for path, beforeStamp := range stamps {
		if err := ctx.Err(); err != nil {
			return capture, false, err
		}
		afterStamp, err := statWorkingPath(root, path)
		if err != nil || afterStamp != beforeStamp {
			return capture, true, nil
		}
	}
	currentID, err := WorktreeRootID(repo.Worktree)
	if err != nil || currentID != capture.RootID {
		return capture, true, nil
	}
	return capture, false, nil
}

func zeroMode(mode string) string {
	if mode == "" {
		return "000000"
	}
	return mode
}
func zeroObject(object, reference string) string {
	if object == "" {
		return strings.Repeat("0", len(reference))
	}
	return object
}

func openParent(root int, path string) (int, string, error) {
	parts := strings.Split(path, "/")
	fd, err := unix.Dup(root)
	if err != nil {
		return -1, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if openErr != nil {
			if errors.Is(openErr, unix.ENOENT) {
				return -1, "", unix.ENOENT
			}
			return -1, "", ErrWorktreeState
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func statWorkingPath(root int, path string) (workStamp, error) {
	parent, name, err := openParent(root, path)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return workStamp{}, nil
		}
		return workStamp{}, err
	}
	defer unix.Close(parent)
	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return workStamp{}, nil
		}
		return workStamp{}, ErrWorktreeState
	}
	return stamp(st), nil
}

func readWorkingPath(ctx context.Context, root int, path, expectedMode string, objectLength int) (workEntry, error) {
	parent, name, err := openParent(root, path)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return workEntry{}, nil
		}
		return workEntry{}, err
	}
	defer unix.Close(parent)
	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return workEntry{}, nil
		}
		return workEntry{}, ErrWorktreeState
	}
	item := workEntry{stamp: stamp(st)}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		if st.Mode&0111 != 0 {
			item.mode = "100755"
		} else {
			item.mode = "100644"
		}
		if st.Size > MaxReviewBlobBytes {
			item.blob = ReviewBlob{Kind: "file", Bytes: st.Size, Limited: true}
			item.warning = "This working file exceeds the 512 KiB preview limit, so its content was not compared."
			break
		}
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return item, ErrWorktreeChanged
		}
		file := os.NewFile(uintptr(fd), path)
		defer file.Close()
		var opened unix.Stat_t
		if err := unix.Fstat(fd, &opened); err != nil || stamp(opened) != item.stamp {
			return item, ErrWorktreeChanged
		}
		item.object, item.blob, err = hashWorkingFile(ctx, file, st.Size, objectLength)
		if err != nil {
			return item, err
		}
	case unix.S_IFLNK:
		item.mode = "120000"
		if st.Size > MaxReviewBlobBytes || st.Size < 0 {
			item.blob = ReviewBlob{Kind: "symlink", Bytes: st.Size, Limited: true}
			item.warning = "This working link exceeds the 512 KiB preview limit, so its content was not compared."
			break
		}
		buffer := make([]byte, max(int(st.Size)+1, 4096))
		count, err := unix.Readlinkat(parent, name, buffer)
		if err != nil || count == len(buffer) {
			return item, ErrWorktreeChanged
		}
		data := buffer[:count]
		item.object = gitBlobHash(data, objectLength)
		item.blob = previewText(data, ReviewBlob{Kind: "symlink", Bytes: int64(count)})
	case unix.S_IFDIR:
		return item, ErrWorktreeState
	default:
		return item, errors.New("special working files cannot be reviewed")
	}
	after, err := statWorkingPath(root, path)
	if err != nil || after != item.stamp {
		return item, ErrWorktreeChanged
	}
	return item, nil
}

func gitBlobHash(data []byte, objectLength int) string {
	var h hash.Hash = sha1.New()
	if objectLength == 64 {
		h = sha256.New()
	}
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func hashWorkingFile(ctx context.Context, file *os.File, size int64, objectLength int) (string, ReviewBlob, error) {
	var h hash.Hash = sha1.New()
	if objectLength == 64 {
		h = sha256.New()
	}
	fmt.Fprintf(h, "blob %d\x00", size)
	var preview bytes.Buffer
	buffer := make([]byte, 32*1024)
	var read int64
	for {
		if err := ctx.Err(); err != nil {
			return "", ReviewBlob{}, err
		}
		count, err := file.Read(buffer)
		if count > 0 {
			read += int64(count)
			h.Write(buffer[:count])
			if size <= MaxReviewBlobBytes {
				preview.Write(buffer[:count])
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", ReviewBlob{}, err
		}
	}
	if read != size {
		return "", ReviewBlob{}, ErrWorktreeChanged
	}
	blob := ReviewBlob{Kind: "file", Bytes: size}
	if size > MaxReviewBlobBytes {
		blob.Limited = true
	} else {
		blob = previewText(preview.Bytes(), blob)
	}
	return hex.EncodeToString(h.Sum(nil)), blob, nil
}

func worktreeTextDiff(ctx context.Context, before, after ReviewBlob) (*ReviewDiff, *int64, *int64, error) {
	diff := &ReviewDiff{Changes: []ReviewChange{}}
	oldLines, newLines := reviewLineCount(before.Text), reviewLineCount(after.Text)
	if before.Kind == "absent" || after.Kind == "absent" {
		if oldLines+newLines > 0 {
			diff.Changes = append(diff.Changes, ReviewChange{OldLines: oldLines, NewLines: newLines})
		}
	} else if before.Text != after.Text {
		directory, err := os.MkdirTemp("", "portal-review-diff-")
		if err != nil {
			return nil, nil, nil, err
		}
		defer os.RemoveAll(directory)
		oldPath, newPath := filepath.Join(directory, "old"), filepath.Join(directory, "new")
		if err := os.WriteFile(oldPath, []byte(before.Text), 0600); err != nil {
			return nil, nil, nil, err
		}
		if err := os.WriteFile(newPath, []byte(after.Text), 0600); err != nil {
			return nil, nil, nil, err
		}
		args := []string{"--no-pager", "-c", "diff.external=", "diff", "--no-index", "--text", "--no-color", "--no-ext-diff", "--no-textconv", "--diff-algorithm=myers", "--indent-heuristic", "--unified=0", "--inter-hunk-context=0", "--", oldPath, newPath}
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = directory
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1"}
		out, err := command.Output()
		var exit *exec.ExitError
		if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
			return nil, nil, nil, err
		}
		if len(out) > maxReviewOutput {
			return nil, nil, nil, ErrReviewLimit
		}
		for _, line := range bytes.Split(out, []byte{'\n'}) {
			if !bytes.HasPrefix(line, []byte("@@ ")) {
				continue
			}
			parts := reviewHunk.FindSubmatch(line)
			if parts == nil {
				return nil, nil, nil, ErrWorktreeState
			}
			values := [4]int{}
			for i := range values {
				if (i == 1 || i == 3) && len(parts[i+1]) == 0 {
					values[i] = 1
					continue
				}
				value, err := strconv.Atoi(string(parts[i+1]))
				if err != nil || value > MaxReviewLines {
					return nil, nil, nil, ErrWorktreeState
				}
				values[i] = value
			}
			if values[1] > 0 {
				values[0]--
			}
			if values[3] > 0 {
				values[2]--
			}
			diff.Changes = append(diff.Changes, ReviewChange{values[0], values[1], values[2], values[3]})
		}
	}
	oldEnd, newEnd := 0, 0
	var added, deleted int64
	for _, change := range diff.Changes {
		if change.OldStart < oldEnd || change.NewStart < newEnd || change.OldStart-oldEnd != change.NewStart-newEnd ||
			change.OldStart+change.OldLines > oldLines || change.NewStart+change.NewLines > newLines {
			return nil, nil, nil, ErrWorktreeState
		}
		oldEnd, newEnd = change.OldStart+change.OldLines, change.NewStart+change.NewLines
		added += int64(change.NewLines)
		deleted += int64(change.OldLines)
	}
	if oldLines-oldEnd != newLines-newEnd {
		return nil, nil, nil, ErrWorktreeState
	}
	return diff, &added, &deleted, nil
}
