// Package uploads owns private workspace inputs. Its catalog is independent of
// session manifests and Codex receipts so older package generations can ignore it.
package uploads

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"golang.org/x/sys/unix"
)

const maxCatalogBytes = 16 << 20
const maxRecords = 10000
const draftLifetime = 7 * 24 * time.Hour

var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func DefaultLimits() conversation.UploadLimits {
	return conversation.UploadLimits{FileBytes: 1 << 30, PromptBytes: 2 << 30, SessionBytes: 10 << 30, WorkspaceBytes: 100 << 30, ChunkBytes: 4 << 20, Files: 10}
}

type Store struct {
	Directory    string
	Workspace    string
	UploadLimits conversation.UploadLimits
	MinFreeBytes uint64
	Now          func() time.Time
}

type Scope struct {
	ID      string    `json:"id"`
	Slug    string    `json:"slug,omitempty"`
	Thread  string    `json:"thread,omitempty"`
	Epoch   string    `json:"epoch,omitempty"`
	Draft   bool      `json:"draft"`
	Deleted bool      `json:"deleted,omitempty"`
	Created time.Time `json:"created"`
}

type fileRecord struct {
	ID        string    `json:"id"`
	ClientID  string    `json:"clientId"`
	Scope     string    `json:"scope"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Offset    int64     `json:"offset"`
	Checksums []string  `json:"checksums,omitempty"`
	State     string    `json:"state"`
	Updated   time.Time `json:"updated"`
}

type submission struct {
	Scope   string   `json:"scope"`
	Kind    string   `json:"kind"`
	Attempt string   `json:"attempt"`
	Text    string   `json:"text"`
	Wire    string   `json:"wire"`
	Files   []string `json:"files"`
	State   string   `json:"state"`
	QueueID string   `json:"queueId,omitempty"`
	ItemID  string   `json:"itemId,omitempty"`
}

type catalog struct {
	Schema      int                   `json:"schema"`
	Workspace   string                `json:"workspace"`
	Scopes      map[string]Scope      `json:"scopes"`
	Files       map[string]fileRecord `json:"files"`
	Submissions map[string]submission `json:"submissions"`
}

func New(directory, workspace string) (*Store, error) {
	directory = filepath.Clean(directory)
	if !filepath.IsAbs(directory) || !filepath.IsAbs(workspace) || directory == workspace || strings.HasPrefix(directory, workspace+string(filepath.Separator)) {
		return nil, errors.New("upload storage must be an absolute private path outside the workspace")
	}
	return &Store{Directory: directory, Workspace: workspace, UploadLimits: DefaultLimits(), MinFreeBytes: 1 << 30, Now: time.Now}, nil
}

func problem(status int, message string) error {
	return &conversation.UploadError{Status: status, Message: message}
}
func newID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func (store *Store) transaction(ctx context.Context, fn func(*catalog) (bool, error)) error {
	if err := os.MkdirAll(store.Directory, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(store.Directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("upload directory must be private")
	}
	lock, err := os.OpenFile(filepath.Join(store.Directory, "catalog.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	data := catalog{Schema: 1, Workspace: store.Workspace, Scopes: map[string]Scope{}, Files: map[string]fileRecord{}, Submissions: map[string]submission{}}
	path := filepath.Join(store.Directory, "catalog.json")
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err == nil {
		encoded, readErr := io.ReadAll(io.LimitReader(file, maxCatalogBytes+1))
		file.Close()
		if readErr != nil {
			return readErr
		}
		if len(encoded) > maxCatalogBytes {
			return errors.New("upload catalog is too large")
		}
		decoder := json.NewDecoder(strings.NewReader(string(encoded)))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&data); err != nil {
			return err
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return errors.New("upload catalog has trailing data")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if data.Schema != 1 || data.Workspace != store.Workspace || data.Scopes == nil || data.Files == nil || data.Submissions == nil {
		return errors.New("upload catalog has the wrong identity")
	}
	if len(data.Files) > maxRecords || len(data.Scopes) > maxRecords || len(data.Submissions) > maxRecords {
		return errors.New("upload catalog exceeds its record limit")
	}
	for id, record := range data.Files {
		if !idPattern.MatchString(id) || record.ID != id || record.Size < 0 || record.Offset < 0 || record.Offset > record.Size || len(record.Checksums) > 4096 {
			return errors.New("invalid upload record")
		}
	}
	originalRecords, originalChunks := catalogSize(&data)
	compacted := store.compact(&data)
	changed, err := fn(&data)
	if err != nil || (!changed && !compacted) {
		return err
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	records, chunks := catalogSize(&data)
	// Keep headroom for cancellation, receipt observation and tombstone updates.
	if len(encoded) > maxCatalogBytes || (len(encoded) > maxCatalogBytes-(1<<20) && (records > originalRecords || chunks > originalChunks)) {
		return problem(507, "Upload history is full; delete unused sessions or wait for unused drafts to expire")
	}
	temp, err := os.CreateTemp(store.Directory, ".catalog-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(encoded); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(store.Directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func catalogSize(data *catalog) (records, chunks int) {
	records = len(data.Files) + len(data.Scopes) + len(data.Submissions)
	for _, file := range data.Files {
		chunks += len(file.Checksums)
	}
	return
}

// compact reclaims metadata whose retry and history obligations have ended.
// File expiry itself stays in Collect, after the caller reconciles creation receipts.
func (store *Store) compact(data *catalog) bool {
	changed := false
	for key, sub := range data.Submissions {
		obsolete := data.Scopes[sub.Scope].Deleted
		if sub.State == "prepared" || sub.State == "cancelled" {
			obsolete = true
			for _, id := range sub.Files {
				if file, exists := data.Files[id]; exists && file.State != "deleted" {
					obsolete = false
				}
			}
		}
		if obsolete {
			delete(data.Submissions, key)
			changed = true
		}
	}
	referenced := map[string]bool{}
	occupied := map[string]bool{}
	for _, sub := range data.Submissions {
		occupied[sub.Scope] = true
		for _, id := range sub.Files {
			referenced[id] = true
		}
	}
	for id, file := range data.Files {
		if file.State != "uploading" && len(file.Checksums) != 0 {
			file.Checksums = nil
			data.Files[id] = file
			changed = true
		}
		if file.State == "deleted" && !referenced[id] && (data.Scopes[file.Scope].Deleted || store.Now().Sub(file.Updated) >= draftLifetime) {
			delete(data.Files, id)
			changed = true
		} else {
			occupied[file.Scope] = true
		}
	}
	for id, scope := range data.Scopes {
		if !occupied[id] && (scope.Deleted || store.Now().Sub(scope.Created) >= draftLifetime) {
			delete(data.Scopes, id)
			changed = true
		}
	}
	return changed
}

func (store *Store) NewDraft(ctx context.Context) (Scope, error) {
	var result Scope
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		if len(data.Scopes) >= maxRecords {
			return false, problem(507, "Too many upload drafts")
		}
		id, err := newID()
		if err != nil {
			return false, err
		}
		result = Scope{ID: id, Draft: true, Created: store.Now()}
		data.Scopes[id] = result
		return true, nil
	})
	return result, err
}

func (store *Store) SessionScope(ctx context.Context, slug, thread, epoch string) (Scope, error) {
	var result Scope
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		for _, scope := range data.Scopes {
			if !scope.Deleted && !scope.Draft && scope.Slug == slug && scope.Thread == thread && scope.Epoch == epoch {
				result = scope
				return false, nil
			}
		}
		if len(data.Scopes) >= maxRecords {
			return false, problem(507, "Too many upload scopes")
		}
		id, err := newID()
		if err != nil {
			return false, err
		}
		result = Scope{ID: id, Slug: slug, Thread: thread, Epoch: epoch, Created: store.Now()}
		data.Scopes[id] = result
		return true, nil
	})
	return result, err
}

func (store *Store) Scope(ctx context.Context, id string) (Scope, error) {
	var result Scope
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		var ok bool
		result, ok = data.Scopes[id]
		if !ok || result.Deleted {
			return false, problem(404, "Upload scope is unavailable")
		}
		return false, nil
	})
	return result, err
}

func (store *Store) BindCreation(ctx context.Context, wire, slug, thread, epoch string) error {
	digest := sha256.Sum256([]byte(wire))
	return store.AdoptInitial(ctx, hex.EncodeToString(digest[:]), slug, thread, epoch)
}

// AdoptInitial also recovers initial uploads when session creation was completed
// by the CLI or an older portal after a rollback.
func (store *Store) AdoptInitial(ctx context.Context, digest, slug, thread, epoch string) error {
	return store.transaction(ctx, func(data *catalog) (bool, error) {
		changed := false
		for key, entry := range data.Submissions {
			hash := sha256.Sum256([]byte(entry.Wire))
			if entry.Kind != "initial" || hex.EncodeToString(hash[:]) != digest {
				continue
			}
			// Forks retain the initial submission as a reference. Only the
			// scope that owns its files can adopt the original creation draft.
			ownsFiles := true
			for _, id := range entry.Files {
				if data.Files[id].Scope != entry.Scope {
					ownsFiles = false
					break
				}
			}
			if !ownsFiles {
				continue
			}
			scope := data.Scopes[entry.Scope]
			if scope.Deleted || (!scope.Draft && (scope.Slug != slug || scope.Thread != thread || scope.Epoch != epoch)) {
				return false, problem(409, "Initial attachment ownership changed")
			}
			scope.Draft = false
			scope.Slug = slug
			scope.Thread = thread
			scope.Epoch = epoch
			data.Scopes[scope.ID] = scope
			entry.State = "observed"
			data.Submissions[key] = entry
			changed = true
		}
		return changed, nil
	})
}

func (store *Store) filePath(record fileRecord, partial bool) string {
	extension := filepath.Ext(record.Name)
	if len(extension) > 16 || strings.IndexFunc(extension, func(r rune) bool {
		return !(r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}) >= 0 {
		extension = ""
	}
	name := "file" + extension
	if partial {
		name = "partial"
	}
	return filepath.Join(store.Directory, "files", record.ID, name)
}

func accessible(data *catalog, scopeID, fileID string) bool {
	scope, ok := data.Scopes[scopeID]
	if !ok || scope.Deleted {
		return false
	}
	file, ok := data.Files[fileID]
	if !ok {
		return false
	}
	if file.Scope == scopeID {
		return true
	}
	for _, entry := range data.Submissions {
		if entry.Scope == scopeID && slices.Contains(entry.Files, fileID) {
			return true
		}
	}
	return false
}

func (store *Store) view(data *catalog, record fileRecord, scopeID, baseURL string) conversation.Upload {
	file := codex.Attachment{ID: record.ID, Name: record.Name, Size: record.Size, State: record.State}
	if record.State == "ready" {
		file.DownloadURL = baseURL + "/" + record.ID + "/content"
		file.DeleteURL = baseURL + "/" + record.ID
	}
	refs := map[string]bool{}
	for _, entry := range data.Submissions {
		if slices.Contains(entry.Files, record.ID) && entry.State != "cancelled" {
			scope := data.Scopes[entry.Scope]
			if !scope.Deleted && scope.Slug != "" {
				refs[scope.Slug] = true
			}
		}
	}
	for slug := range refs {
		file.References = append(file.References, slug)
	}
	sort.Strings(file.References)
	return conversation.Upload{Attachment: file, Offset: record.Offset, Checksums: append([]string(nil), record.Checksums...)}
}

type Backend struct {
	Store          *Store
	ScopeID        string
	BaseURL        string
	ReadOnly       bool
	CheckIdle      func(context.Context, []Scope) error
	LockReferences func(context.Context, []Scope) (func(), error)
}

func (backend *Backend) Limits() conversation.UploadLimits { return backend.Store.UploadLimits }
func (backend *Backend) List(ctx context.Context) ([]conversation.Upload, error) {
	result := []conversation.Upload{}
	err := backend.Store.transaction(ctx, func(data *catalog) (bool, error) {
		for id, record := range data.Files {
			if accessible(data, backend.ScopeID, id) {
				view := backend.Store.view(data, record, backend.ScopeID, backend.BaseURL)
				if backend.ReadOnly {
					view.DeleteURL = ""
				}
				result = append(result, view)
			}
		}
		return false, nil
	})
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, err
}
func (backend *Backend) Status(ctx context.Context, id string) (conversation.Upload, error) {
	var result conversation.Upload
	err := backend.Store.transaction(ctx, func(data *catalog) (bool, error) {
		if !accessible(data, backend.ScopeID, id) {
			return false, problem(404, "File is unavailable")
		}
		result = backend.Store.view(data, data.Files[id], backend.ScopeID, backend.BaseURL)
		if backend.ReadOnly {
			result.DeleteURL = ""
		}
		return false, nil
	})
	return result, err
}
func (backend *Backend) Create(ctx context.Context, request conversation.UploadRequest) (conversation.Upload, error) {
	var result conversation.Upload
	store := backend.Store
	if !idPattern.MatchString(request.ClientID) || request.Name == "" || len(request.Name) > 255 || !utf8.ValidString(request.Name) || request.Name == "." || request.Name == ".." || strings.ContainsAny(request.Name, "/\\") || strings.IndexFunc(request.Name, func(r rune) bool { return r < 32 || r == 127 }) >= 0 || request.Size < 0 || request.Size > store.UploadLimits.FileBytes {
		return result, problem(400, "Invalid filename, size or upload identity")
	}
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		scope, ok := data.Scopes[backend.ScopeID]
		if !ok || scope.Deleted {
			return false, problem(404, "Upload scope is unavailable")
		}
		var workspaceBytes, scopeBytes int64
		var count int
		for _, record := range data.Files {
			if record.Scope == backend.ScopeID && record.ClientID == request.ClientID {
				if record.Name != request.Name || record.Size != request.Size {
					return false, problem(409, "Upload identity was reused for a different file")
				}
				result = store.view(data, record, backend.ScopeID, backend.BaseURL)
				return false, nil
			}
			if record.State == "deleted" {
				continue
			}
			workspaceBytes += record.Size
			if record.Scope == backend.ScopeID {
				scopeBytes += record.Size
				count++
			}
		}
		limit := store.UploadLimits.SessionBytes
		if scope.Draft {
			limit = store.UploadLimits.PromptBytes
		}
		if request.Size > limit-scopeBytes || request.Size > store.UploadLimits.WorkspaceBytes-workspaceBytes || count >= 1000 || len(data.Files) >= maxRecords {
			return false, problem(413, "Upload storage limit reached; remove unused files")
		}
		var disk unix.Statfs_t
		if err := unix.Statfs(store.Directory, &disk); err != nil {
			return false, err
		}
		reserved := uint64(0)
		for _, record := range data.Files {
			if record.State == "uploading" {
				reserved += uint64(record.Size - record.Offset)
			}
		}
		free := disk.Bavail * uint64(disk.Bsize)
		needed := store.MinFreeBytes + reserved + uint64(request.Size)
		if free < needed {
			return false, problem(507, "Not enough free disk space for this upload")
		}
		id, err := newID()
		if err != nil {
			return false, err
		}
		record := fileRecord{ID: id, ClientID: request.ClientID, Scope: backend.ScopeID, Name: request.Name, Size: request.Size, State: "uploading", Updated: store.Now()}
		if err = os.MkdirAll(filepath.Dir(store.filePath(record, true)), 0700); err != nil {
			return false, err
		}
		file, err := os.OpenFile(store.filePath(record, true), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return false, err
		}
		if err = file.Sync(); err != nil {
			file.Close()
			return false, err
		}
		file.Close()
		data.Files[id] = record
		result = store.view(data, record, backend.ScopeID, backend.BaseURL)
		return true, nil
	})
	return result, err
}
func (backend *Backend) Append(ctx context.Context, id string, offset int64, checksum string, reader io.Reader) (conversation.Upload, error) {
	var result conversation.Upload
	store := backend.Store
	bytes, err := io.ReadAll(io.LimitReader(reader, store.UploadLimits.ChunkBytes+1))
	if err != nil {
		return result, err
	}
	digest := sha256.Sum256(bytes)
	if len(bytes) == 0 || int64(len(bytes)) > store.UploadLimits.ChunkBytes || hex.EncodeToString(digest[:]) != checksum {
		return result, problem(400, "Upload chunk size or checksum is invalid")
	}
	err = store.transaction(ctx, func(data *catalog) (bool, error) {
		record, ok := data.Files[id]
		if !ok || record.Scope != backend.ScopeID {
			return false, problem(404, "File is unavailable")
		}
		if record.State != "uploading" || record.Offset != offset {
			return false, problem(409, "Upload progress changed; check its current offset")
		}
		expected := min(store.UploadLimits.ChunkBytes, record.Size-offset)
		if int64(len(bytes)) != expected {
			return false, problem(400, "Upload chunk has the wrong length")
		}
		file, err := os.OpenFile(store.filePath(record, true), os.O_WRONLY|unix.O_NOFOLLOW, 0600)
		if err != nil {
			return false, err
		}
		defer file.Close()
		if err = file.Truncate(offset); err != nil {
			return false, err
		}
		if _, err = file.WriteAt(bytes, offset); err != nil {
			return false, err
		}
		if err = file.Sync(); err != nil {
			return false, err
		}
		record.Offset += int64(len(bytes))
		record.Checksums = append(record.Checksums, checksum)
		record.Updated = store.Now()
		data.Files[id] = record
		result = store.view(data, record, backend.ScopeID, backend.BaseURL)
		return true, nil
	})
	return result, err
}
func (backend *Backend) Complete(ctx context.Context, id string) (conversation.Upload, error) {
	var result conversation.Upload
	store := backend.Store
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		record, ok := data.Files[id]
		if !ok || record.Scope != backend.ScopeID {
			return false, problem(404, "File is unavailable")
		}
		if record.State == "deleted" || record.Offset != record.Size {
			return false, problem(409, "Upload is incomplete or removed")
		}
		if record.State == "ready" {
			result = store.view(data, record, backend.ScopeID, backend.BaseURL)
			return false, nil
		}
		final := store.filePath(record, false)
		if err := os.Rename(store.filePath(record, true), final); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
			info, statErr := os.Stat(final)
			if statErr != nil || info.Size() != record.Size {
				return false, err
			}
		}
		if err := os.Chmod(final, 0400); err != nil {
			return false, err
		}
		dir, err := os.Open(filepath.Dir(final))
		if err != nil {
			return false, err
		}
		err = dir.Sync()
		dir.Close()
		if err != nil {
			return false, err
		}
		record.State = "ready"
		record.Checksums = nil
		record.Updated = store.Now()
		data.Files[id] = record
		result = store.view(data, record, backend.ScopeID, backend.BaseURL)
		return true, nil
	})
	return result, err
}
func (backend *Backend) Open(ctx context.Context, id string) (conversation.UploadContent, error) {
	var result conversation.UploadContent
	store := backend.Store
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		record, ok := data.Files[id]
		if !ok || !accessible(data, backend.ScopeID, id) || record.State != "ready" {
			return false, problem(404, "File is unavailable")
		}
		file, err := os.OpenFile(store.filePath(record, false), os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return false, err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() != record.Size {
			file.Close()
			return false, errors.New("uploaded file changed")
		}
		result = conversation.UploadContent{File: file, Name: record.Name, Modified: record.Updated}
		return false, nil
	})
	return result, err
}
func (backend *Backend) Delete(ctx context.Context, id string, confirmed bool) error {
	store := backend.Store

	lockedScopes := map[string]bool{}
	if backend.LockReferences != nil {
		scopes := map[string]Scope{}
		err := store.transaction(ctx, func(data *catalog) (bool, error) {
			if !accessible(data, backend.ScopeID, id) {
				return false, problem(404, "File is unavailable")
			}
			scopes[backend.ScopeID] = data.Scopes[backend.ScopeID]
			for _, sub := range data.Submissions {
				if slices.Contains(sub.Files, id) && sub.State != "cancelled" && sub.State != "prepared" {
					scopes[sub.Scope] = data.Scopes[sub.Scope]
				}
			}
			return false, nil
		})
		if err != nil {
			return err
		}
		refs := []Scope{}
		for scopeID, scope := range scopes {
			lockedScopes[scopeID] = true
			refs = append(refs, scope)
		}
		release, err := backend.LockReferences(ctx, refs)
		if err != nil {
			return err
		}
		defer release()
	}
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		record, ok := data.Files[id]
		if !ok || !accessible(data, backend.ScopeID, id) {
			return false, problem(404, "File is unavailable")
		}
		if record.State == "deleted" {
			return false, nil
		}
		scopes := map[string]Scope{}
		for _, entry := range data.Submissions {
			if !slices.Contains(entry.Files, id) || (entry.State == "cancelled" || entry.State == "prepared") {
				continue
			}
			if entry.State == "pending" || entry.State == "queued" {
				return false, problem(409, "This file is referenced by a queued or unresolved submission")
			}
			if backend.LockReferences != nil && !lockedScopes[entry.Scope] {
				return false, problem(409, "File references changed; retry deletion")
			}
			scopes[entry.Scope] = data.Scopes[entry.Scope]
		}
		if len(scopes) > 0 {
			if !confirmed {
				return false, problem(409, "Confirm deletion of the submitted file")
			}
			refs := []Scope{}
			for _, scope := range scopes {
				if !scope.Deleted {
					refs = append(refs, scope)
				}
			}
			if backend.CheckIdle == nil {
				return false, problem(409, "Unable to verify that referencing sessions are idle")
			}
			if err := backend.CheckIdle(ctx, refs); err != nil {
				return false, err
			}
		}
		// Persist the tombstone before reclaiming bytes. Repeated deletion and startup
		// collection finish cleanup if unlink is interrupted.
		record.State = "deleted"
		record.Checksums = nil
		record.Updated = store.Now()
		data.Files[id] = record
		return true, nil
	})
	if err != nil {
		return err
	}
	return store.reclaim(ctx)
}

func (backend *Backend) Prepare(ctx context.Context, kind, attempt, text string, ids []string) (string, error) {
	store := backend.Store
	var wire string
	text = strings.TrimSpace(text)
	if len(ids) > store.UploadLimits.Files {
		return "", problem(400, "Too many files for one prompt")
	}
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		key := backend.ScopeID + "/" + kind + "/" + attempt
		if previous, ok := data.Submissions[key]; ok {
			if previous.Text != text || !slices.Equal(previous.Files, ids) {
				return false, problem(409, "Submission identity was reused with different attachments or text")
			}
			if previous.State != "prepared" && previous.State != "cancelled" {
				wire = previous.Wire
				return false, nil
			}
		}
		if len(ids) == 0 {
			wire = text
			return false, nil
		}
		if len(data.Submissions) >= maxRecords {
			return false, problem(507, "Too many stored attachment submissions")
		}
		descriptions := []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			Path string `json:"path"`
		}{}
		var total int64
		seen := map[string]bool{}
		for _, id := range ids {
			record, ok := data.Files[id]
			if !ok || !accessible(data, backend.ScopeID, id) || record.State != "ready" || seen[id] {
				return false, problem(400, "An attachment is unavailable, incomplete or repeated")
			}
			seen[id] = true
			total += record.Size
			if total > store.UploadLimits.PromptBytes {
				return false, problem(413, "Attachments exceed the prompt size limit")
			}
			info, err := os.Lstat(store.filePath(record, false))
			if err != nil || !info.Mode().IsRegular() || info.Size() != record.Size {
				return false, problem(409, "An uploaded file is unavailable")
			}
			descriptions = append(descriptions, struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
				Path string `json:"path"`
			}{record.Name, record.Size, store.filePath(record, false)})
		}
		encoded, _ := json.MarshalIndent(descriptions, "", "  ")
		wire = strings.TrimSpace(text + "\n\nAttached local files (inputs; keep these files outside version control):\n" + string(encoded))
		if len(wire) > session.MaxMessageBytes {
			return false, problem(413, "Message and attachment references exceed the prompt limit")
		}
		data.Submissions[key] = submission{Scope: backend.ScopeID, Kind: kind, Attempt: attempt, Text: text, Wire: wire, Files: append([]string(nil), ids...), State: "pending"}
		if kind == "initial" {
			entry := data.Submissions[key]
			entry.State = "prepared"
			data.Submissions[key] = entry
		}
		for _, id := range ids {
			record := data.Files[id]
			record.Updated = store.Now()
			data.Files[id] = record
		}
		return true, nil
	})
	return wire, err
}

func (backend *Backend) ObserveTranscript(ctx context.Context, transcript *codex.Transcript) error {
	return backend.Store.transaction(ctx, func(data *catalog) (bool, error) {
		changed := false
		for index := range transcript.Entries {
			entry := &transcript.Entries[index]
			if entry.Kind != "userMessage" {
				continue
			}
			for key, sub := range data.Submissions {
				if sub.Scope != backend.ScopeID || sub.Wire != entry.Text || (sub.Kind != "initial" && sub.Attempt != entry.ClientUserMessageID && sub.ItemID != entry.ItemID) {
					continue
				}
				if sub.Kind == "initial" && sub.ItemID != "" && sub.ItemID != entry.ItemID {
					continue
				}
				display := sub.Text
				entry.DisplayText = &display
				for _, id := range sub.Files {
					file := backend.Store.view(data, data.Files[id], backend.ScopeID, backend.BaseURL).Attachment
					if backend.ReadOnly {
						file.DeleteURL = ""
					}
					entry.Attachments = append(entry.Attachments, file)
				}
				if sub.State != "observed" || sub.ItemID != entry.ItemID {
					sub.State = "observed"
					sub.ItemID = entry.ItemID
					data.Submissions[key] = sub
					changed = true
				}
				break
			}
		}
		return changed, nil
	})
}
func (backend *Backend) ObserveQueue(ctx context.Context, queue []codex.QueueEntry) error {
	return backend.Store.transaction(ctx, func(data *catalog) (bool, error) {
		changed := false
		for index := range queue {
			entry := &queue[index]
			for key, sub := range data.Submissions {
				if sub.Scope != backend.ScopeID || sub.Kind != "queue" || sub.Attempt != entry.ClientUserMessageID || sub.Wire != entry.Text {
					continue
				}
				display := sub.Text
				entry.DisplayText = &display
				for _, id := range sub.Files {
					entry.Attachments = append(entry.Attachments, backend.Store.view(data, data.Files[id], backend.ScopeID, backend.BaseURL).Attachment)
				}
				if (sub.State == "pending" || sub.State == "queued") && (sub.State != "queued" || sub.QueueID != entry.ID) {
					sub.State = "queued"
					sub.QueueID = entry.ID
					data.Submissions[key] = sub
					changed = true
				}
				break
			}
		}
		return changed, nil
	})
}
func (backend *Backend) QueueDeleted(ctx context.Context, id string) error {
	return backend.Store.transaction(ctx, func(data *catalog) (bool, error) {
		changed := false
		for key, entry := range data.Submissions {
			if entry.Scope == backend.ScopeID && entry.QueueID == id && entry.State != "observed" {
				entry.State = "cancelled"
				data.Submissions[key] = entry
				changed = true
			}
		}
		return changed, nil
	})
}

// Collect only expires unsubmitted files. Session removal requires explicit
// completed deletion evidence supplied through RemoveSession.
func (store *Store) Collect(ctx context.Context) error {
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		pinned := map[string]bool{}
		for _, entry := range data.Submissions {
			if entry.State != "cancelled" && entry.State != "prepared" {
				for _, id := range entry.Files {
					pinned[id] = true
				}
			}
		}
		changed := false
		for id, record := range data.Files {
			if record.State != "deleted" && !pinned[id] && store.Now().Sub(record.Updated) >= draftLifetime {
				record.State = "deleted"
				record.Checksums = nil
				data.Files[id] = record
				changed = true
			}
		}
		return changed, nil
	})
	if err != nil {
		return err
	}
	return store.reclaim(ctx)
}

// reclaim only unlinks durably obsolete blobs; it never expires another draft.
func (store *Store) reclaim(ctx context.Context) error {
	return store.transaction(ctx, func(data *catalog) (bool, error) {
		filesRoot := filepath.Join(store.Directory, "files")
		entries, err := os.ReadDir(filesRoot)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		for _, entry := range entries {
			if _, known := data.Files[entry.Name()]; !known && idPattern.MatchString(entry.Name()) {
				if err := os.RemoveAll(filepath.Join(filesRoot, entry.Name())); err != nil {
					return false, err
				}
			}
		}
		for _, record := range data.Files {
			if record.State == "deleted" {
				if err := os.RemoveAll(filepath.Dir(store.filePath(record, false))); err != nil {
					return false, err
				}
			}
		}
		return false, nil
	})
}

func (store *Store) RemoveSession(ctx context.Context, slug, thread, epoch string) error {
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		owned := map[string]bool{}
		changed := false
		for id, scope := range data.Scopes {
			if scope.Slug == slug && scope.Epoch == epoch && !scope.Deleted && !scope.Draft && thread == "" {
				return false, errors.New("retired thread identity is required to remove session uploads")
			}
			if scope.Slug == slug && ((scope.Thread == thread || scope.Draft) && scope.Epoch == epoch) && !scope.Deleted {
				scope.Deleted = true
				data.Scopes[id] = scope
				owned[id] = true
				changed = true
			}
		}
		for id, record := range data.Files {
			if owned[record.Scope] {
				record.State = "deleted"
				record.Checksums = nil
				data.Files[id] = record
			}
		}
		for key, sub := range data.Submissions {
			if owned[sub.Scope] {
				delete(data.Submissions, key)
			}
		}
		store.compact(data)
		return changed, nil
	})
	if err != nil {
		return err
	}
	return store.reclaim(ctx)
}

// Fork derives inherited references from the actual fork transcript, including
// submissions whose acknowledgement was lost before the source was observed.
func (store *Store) Fork(ctx context.Context, sourceThread string, target Scope, entries []codex.TranscriptEntry) error {
	return store.transaction(ctx, func(data *catalog) (bool, error) {
		changed := false
		for _, sub := range data.Submissions {
			scope := data.Scopes[sub.Scope]
			if scope.Thread != sourceThread || sub.State == "cancelled" {
				continue
			}
			for _, entry := range entries {
				if entry.Kind != "userMessage" || entry.Text != sub.Wire {
					continue
				}
				sub.Scope = target.ID
				sub.State = "observed"
				sub.ItemID = entry.ItemID
				key := target.ID + "/" + sub.Kind + "/" + sub.Attempt
				if _, exists := data.Submissions[key]; !exists {
					data.Submissions[key] = sub
					changed = true
				}
				break
			}
		}
		return changed, nil
	})
}

func (store *Store) Scopes(ctx context.Context) ([]Scope, error) {
	var result []Scope
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		for _, scope := range data.Scopes {
			result = append(result, scope)
		}
		return false, nil
	})
	return result, err
}

var _ conversation.UploadStore = (*Backend)(nil)
var _ conversation.AttachmentProvider = (*Backend)(nil)

// RetainCreations pins accepted initial prompts before draft expiry. Callers must
// supply all durable creation receipts, including paused and unresolved ones.
func (store *Store) RetainCreations(ctx context.Context, goals map[string]Scope) error {
	return store.transaction(ctx, func(data *catalog) (bool, error) {
		changed := false
		for key, entry := range data.Submissions {
			owner, accepted := goals[entry.Wire]
			if accepted && owner.Deleted && entry.Kind == "initial" && data.Scopes[entry.Scope].Draft {
				scope := data.Scopes[entry.Scope]
				scope.Slug, scope.Epoch = "", ""
				data.Scopes[scope.ID] = scope
				entry.State = "cancelled"
				data.Submissions[key] = entry
				changed = true
				continue
			}
			if entry.Kind == "initial" && entry.State == "prepared" && accepted {
				scope := data.Scopes[entry.Scope]
				if scope.Slug != "" && (scope.Slug != owner.Slug || scope.Epoch != owner.Epoch) {
					return false, problem(409, "Initial files belong to another creation")
				}
				scope.Slug = owner.Slug
				scope.Epoch = owner.Epoch
				data.Scopes[scope.ID] = scope
				entry.State = "pending"
				data.Submissions[key] = entry
				changed = true
			}
		}
		return changed, nil
	})
}

func (store *Store) HasThread(ctx context.Context, thread string) (bool, error) {
	found := false
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		for _, sub := range data.Submissions {
			if data.Scopes[sub.Scope].Thread == thread {
				found = true
				break
			}
		}
		return false, nil
	})
	return found, err
}

func ForWorkspace(workspace, stateRoot string) (*Store, error) {
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(stateRoot) {
		return nil, errors.New("workspace and user state root must be absolute")
	}
	return New(filepath.Join(userstate.WorkspaceDirectory(stateRoot, "portal", workspace), "uploads"), workspace)
}
