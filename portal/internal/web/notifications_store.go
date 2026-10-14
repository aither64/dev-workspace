package web

import (
	"bytes"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

const maxNotificationStateBytes = 4 * 1024 * 1024

type browserRegistration struct {
	ID           string               `json:"id"`
	Origin       string               `json:"origin"`
	Subscription webpush.Subscription `json:"subscription"`
	ExpiresAt    int64                `json:"expiresAt,omitempty"`
	Disabled     bool                 `json:"disabled,omitempty"`
	Result       string               `json:"result,omitempty"`
}

type notificationPreference struct {
	Identity string `json:"identity"`
	Enabled  bool   `json:"enabled"`
}

type notificationDelivery struct {
	Browser string         `json:"browser"`
	Event   attentionEvent `json:"event"`
	Attempt int            `json:"attempt"`
	NextAt  int64          `json:"nextAt"`
}

type notificationState struct {
	Schema      int                               `json:"schema"`
	Workspace   string                            `json:"workspace"`
	PrivateKey  string                            `json:"privateKey"`
	PublicKey   string                            `json:"publicKey"`
	Browsers    map[string]browserRegistration    `json:"browsers"`
	Preferences map[string]notificationPreference `json:"preferences"`
	Sessions    map[string]attentionBaseline      `json:"sessions"`
	Outbox      map[string]notificationDelivery   `json:"outbox"`
}

type notificationPresence struct {
	Identity, EventID string
	Until             time.Time
}

type notificationStore struct {
	mu       sync.Mutex
	state    notificationState
	path     string
	failed   bool
	presence map[string]notificationPresence
}

func newNotificationStore(parent *lifecycleOperationStore) (*notificationStore, error) {
	if err := parent.ensureDirectory(); err != nil {
		return nil, err
	}
	store := &notificationStore{path: filepath.Join(parent.directory, "browser-notifications.json"), presence: make(map[string]notificationPresence)}
	fd, err := syscall.Open(store.path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		private, public, keyErr := webpush.GenerateVAPIDKeys()
		if keyErr != nil {
			return nil, keyErr
		}
		store.state = notificationState{Schema: 1, Workspace: parent.workspace, PrivateKey: private, PublicKey: public,
			Browsers: make(map[string]browserRegistration), Preferences: make(map[string]notificationPreference), Sessions: make(map[string]attentionBaseline), Outbox: make(map[string]notificationDelivery)}
		if err := store.save(store.state); err != nil {
			return nil, err
		}
		return store, nil
	}
	if err != nil {
		return nil, errors.New("notification state cannot be opened")
	}
	file := os.NewFile(uintptr(fd), store.path)
	defer file.Close()
	info, err := file.Stat()
	var owner *syscall.Stat_t
	if err == nil {
		owner, _ = info.Sys().(*syscall.Stat_t)
	}
	if err != nil || owner == nil || owner.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxNotificationStateBytes {
		return nil, errors.New("notification state is not a private bounded file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxNotificationStateBytes+1))
	if err != nil || len(data) > maxNotificationStateBytes || rejectDuplicateCreationJSONKeys(data) != nil {
		return nil, errors.New("notification state cannot be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&store.state) != nil || store.state.Schema != 1 || store.state.Workspace != parent.workspace ||
		store.state.Browsers == nil || store.state.Preferences == nil || store.state.Sessions == nil || store.state.Outbox == nil ||
		len(store.state.PrivateKey) != 43 || len(store.state.PublicKey) != 87 {
		return nil, errors.New("notification state identity or format is invalid")
	}
	if len(store.state.Browsers) > 128 || len(store.state.Preferences) > 4096 || len(store.state.Sessions) > 4096 || len(store.state.Outbox) > 512 {
		return nil, errors.New("notification state exceeds record limits")
	}
	private, privateErr := base64.RawURLEncoding.DecodeString(store.state.PrivateKey)
	public, publicErr := base64.RawURLEncoding.DecodeString(store.state.PublicKey)
	if privateErr != nil || publicErr != nil || len(private) != 32 || len(public) != 65 {
		return nil, errors.New("notification key format is invalid")
	}
	x, y := elliptic.P256().ScalarBaseMult(private)
	if x == nil || !bytes.Equal(elliptic.Marshal(elliptic.P256(), x, y), public) {
		return nil, errors.New("notification key pair is inconsistent")
	}
	var shape struct {
		Preferences map[string]map[string]json.RawMessage `json:"preferences"`
	}
	if json.Unmarshal(data, &shape) != nil {
		return nil, errors.New("notification preferences are invalid")
	}
	for slug, preference := range store.state.Preferences {
		raw := bytes.TrimSpace(shape.Preferences[slug]["enabled"])
		if !session.ValidSlug(slug) || !messageDigestPattern.MatchString(preference.Identity) || (!bytes.Equal(raw, []byte("true")) && !bytes.Equal(raw, []byte("false"))) {
			return nil, errors.New("notification preference is invalid")
		}
	}
	for id, browser := range store.state.Browsers {
		if id != browser.ID || !messageDigestPattern.MatchString(id) || validatePushSubscription(browser.Subscription, browser.ExpiresAt, time.UnixMilli(0)) != nil {
			return nil, errors.New("notification registration is invalid")
		}
	}
	return store, nil
}

// Copy before writing: failed or uncertain persistence fences all sending. It
// must never turn a saved mute into the default-on preference.
func (store *notificationStore) change(update func(*notificationState) error) error {
	if store.failed {
		return errors.New("notification storage is unavailable")
	}
	data, err := json.Marshal(store.state)
	if err != nil {
		store.failed = true
		return errors.New("notification storage is unavailable")
	}
	var next notificationState
	if json.Unmarshal(data, &next) != nil {
		store.failed = true
		return errors.New("notification storage is unavailable")
	}
	if err := update(&next); err != nil {
		return err
	}
	nextData, err := json.Marshal(next)
	if err != nil {
		store.failed = true
		return errors.New("notification storage is unavailable")
	}
	if bytes.Equal(data, nextData) {
		return nil
	}
	if err := store.save(next); err != nil {
		store.failed = true
		return errors.New("notification storage is unavailable")
	}
	store.state = next
	return nil
}

func (store *notificationStore) save(state notificationState) error {
	if len(state.Browsers) > 128 || len(state.Preferences) > 4096 || len(state.Sessions) > 4096 || len(state.Outbox) > 512 {
		return errors.New("notification record limit reached")
	}
	data, err := json.Marshal(state)
	if err != nil || len(data) > maxNotificationStateBytes {
		return errors.New("notification state exceeds its size limit")
	}
	dir := filepath.Dir(store.path)
	file, err := os.CreateTemp(dir, ".notifications-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), store.path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func notificationEnabled(state *notificationState, slug, identity string) bool {
	p, found := state.Preferences[slug]
	return !found || p.Identity != identity || p.Enabled
}

func notificationDeliveryKey(browser, slug string) string { return browser + ":" + slug }
