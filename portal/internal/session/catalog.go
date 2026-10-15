package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Catalog lives only while a portal owns this workspace. Command-only processes
// retain uncached reads. Every cached file is revalidated before it is reused;
// the one-second list snapshot is for discovery, never execution permission.
type Catalog struct {
	listMu     sync.Mutex
	mu         sync.Mutex
	refs       int
	workspace  string
	files      map[string]catalogEntry
	listed     []Summary
	listErr    error
	listedAt   time.Time
	owners     map[string][]string
	ownerRoot  string
	ownersAt   time.Time
	ownerStamp catalogStamp
}

type catalogStamp struct {
	Dev, Ino     uint64
	Mode, UID    uint32
	Size         int64
	MTime, CTime unix.Timespec
}
type catalogEntry struct {
	stamps  [3]catalogStamp
	summary Summary
}

var catalogs = struct {
	sync.Mutex
	values map[string]*Catalog
}{values: map[string]*Catalog{}}

// StartCatalog shares a catalog with all in-process readers of the exact
// workspace. Release only after those readers have stopped.
func StartCatalog(workspace string) func() {
	catalogs.Lock()
	c := catalogs.values[workspace]
	if c == nil {
		c = &Catalog{workspace: workspace, files: map[string]catalogEntry{}}
		catalogs.values[workspace] = c
	}
	c.refs++
	catalogs.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			catalogs.Lock()
			defer catalogs.Unlock()
			c.refs--
			if c.refs == 0 {
				delete(catalogs.values, workspace)
			}
		})
	}
}

func activeCatalog(workspace string) *Catalog {
	catalogs.Lock()
	defer catalogs.Unlock()
	return catalogs.values[workspace]
}

func cloneSummary(value Summary) Summary {
	value.Repositories = slices.Clone(value.Repositories)
	value.Artifacts = slices.Clone(value.Artifacts)
	if value.Creation.InitialGoalAttempted != nil {
		copy := *value.Creation.InitialGoalAttempted
		value.Creation.InitialGoalAttempted = &copy
	}
	return value
}

func cloneSummaries(values []Summary) []Summary {
	result := make([]Summary, len(values))
	for i, v := range values {
		result[i] = cloneSummary(v)
	}
	return result
}

func summaryStamps(workspace, root, slug string) ([3]catalogStamp, error) {
	var stamps [3]catalogStamp
	for i, name := range []string{ManifestName, "state.md", "plan.md"} {
		f, err := openConfined(workspace, filepath.Join(root, slug, name), unix.O_RDONLY, 0)
		if err != nil {
			return stamps, err
		}
		var stat unix.Stat_t
		err = unix.Fstat(int(f.Fd()), &stat)
		f.Close()
		if err != nil {
			return stamps, err
		}
		stamps[i] = catalogStamp{Dev: uint64(stat.Dev), Ino: stat.Ino, Mode: stat.Mode, UID: stat.Uid, Size: stat.Size, MTime: stat.Mtim, CTime: stat.Ctim}
	}
	return stamps, nil
}

func (c *Catalog) load(root, slug string, archived bool) (*Summary, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := filepath.Join(root, slug)
	for attempt := 0; attempt < 2; attempt++ {
		before, err := summaryStamps(c.workspace, root, slug)
		if err != nil {
			delete(c.files, key)
			return nil, err
		}
		if entry, ok := c.files[key]; ok && entry.stamps == before {
			value := cloneSummary(entry.summary)
			return &value, nil
		}
		summary, err := loadSummaryUncached(c.workspace, root, slug, archived)
		if err != nil {
			delete(c.files, key)
			return nil, err
		}
		after, err := summaryStamps(c.workspace, root, slug)
		if err != nil {
			delete(c.files, key)
			return nil, err
		}
		if before != after {
			continue
		}
		c.files[key] = catalogEntry{stamps: after, summary: cloneSummary(*summary)}
		return summary, nil
	}
	delete(c.files, key)
	return nil, errors.New("session changed while reading its records")
}

// The separate discovery lock avoids blocking direct owner lookups behind an
// inventory refresh. Loads use their own short catalog lock.

func (c *Catalog) list() ([]Summary, error) {
	c.listMu.Lock()
	defer c.listMu.Unlock()
	c.mu.Lock()
	if !c.listedAt.IsZero() && time.Since(c.listedAt) < time.Second {
		values, err := cloneSummaries(c.listed), c.listErr
		c.mu.Unlock()
		return values, err
	}
	c.mu.Unlock()
	values, err := listUncached(c.workspace)
	c.mu.Lock()
	c.listed, c.listErr, c.listedAt = cloneSummaries(values), err, time.Now()
	// Deleted/archived records must not accumulate for the process lifetime.
	keep := make(map[string]bool, len(values))
	for _, v := range values {
		keep[filepath.Join(v.Root, v.Slug)] = true
	}
	for key := range c.files {
		if !keep[key] {
			delete(c.files, key)
		}
	}
	c.mu.Unlock()
	return values, err
}

func (c *Catalog) recoveryOwners(store RecoveryStore, thread string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Recovery writers publish by rename (and retire by unlink), so directory
	// identity/change metadata invalidates ownership immediately, including when
	// a second session starts claiming a previously unique thread.
	for attempt := 0; attempt < 2; attempt++ {
		before, err := recoveryDirectoryStamp(store)
		if err != nil {
			return nil, err
		}
		if c.ownerRoot == store.Root && c.ownerStamp == before && !c.ownersAt.IsZero() && time.Since(c.ownersAt) < time.Second {
			return slices.Clone(c.owners[thread]), nil
		}
		owners, err := store.readOwners()
		if err != nil {
			return nil, err
		}
		after, err := recoveryDirectoryStamp(store)
		if err != nil {
			return nil, err
		}
		if before != after {
			continue
		}
		c.owners, c.ownerRoot, c.ownersAt, c.ownerStamp = owners, store.Root, time.Now(), after
		return slices.Clone(c.owners[thread]), nil
	}
	return nil, errors.New("recovery owners changed while reading")
}

func recoveryDirectoryStamp(store RecoveryStore) (catalogStamp, error) {
	var stat unix.Stat_t
	err := unix.Stat(store.directory(), &stat)
	if errors.Is(err, os.ErrNotExist) {
		return catalogStamp{}, nil
	}
	if err != nil {
		return catalogStamp{}, err
	}
	return catalogStamp{Dev: uint64(stat.Dev), Ino: stat.Ino, Mode: stat.Mode, UID: stat.Uid, Size: stat.Size, MTime: stat.Mtim, CTime: stat.Ctim}, nil
}

func (s RecoveryStore) readOwners() (map[string][]string, error) {
	owners := map[string][]string{}
	entries, err := os.ReadDir(s.directory())
	if errors.Is(err, os.ErrNotExist) {
		return owners, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		slug := strings.TrimSuffix(entry.Name(), ".json")
		if !ValidSlug(slug) {
			continue
		}
		record, err := s.Load(slug)
		if err != nil {
			return nil, fmt.Errorf("read recovery owner %s: %w", slug, err)
		}
		for _, thread := range record.ActiveThreads {
			if !slices.Contains(owners[thread], slug) {
				owners[thread] = append(owners[thread], slug)
			}
		}
	}
	return owners, nil
}
