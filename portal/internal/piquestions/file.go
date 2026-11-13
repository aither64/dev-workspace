package piquestions

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var hexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
var memberRE = regexp.MustCompile(`^(lead|[a-z][a-z0-9]{0,31}[0-9]+)$`)
var slugRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}-[a-z0-9][a-z0-9-]*$`)

func UUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func ValidID(id string) bool { return uuidRE.MatchString(id) }
func Text(s string, max int) bool {
	return utf8.ValidString(s) && len(s) <= max && strings.TrimSpace(s) != "" && !strings.ContainsRune(s, 0)
}
func ValidateRef(r agentbackend.ConversationRef) error {
	if r.Backend != agentbackend.Pi || !filepath.IsAbs(r.Session.Workspace) || filepath.Clean(r.Session.Workspace) != r.Session.Workspace || !slugRE.MatchString(r.Session.Slug) || !hexRE.MatchString(r.Session.Incarnation) || !hexRE.MatchString(r.Incarnation) || !memberRE.MatchString(r.Member) || !Text(r.NativeID, 4096) {
		return ErrInvalid
	}
	return nil
}
func ValidateCaller(c Caller) error {
	if ValidateRef(c.Handle.Conversation) != nil || !Text(c.Handle.Generation, 4096) || !Text(c.Lineage.AnchorEntryID, 4096) || !Text(c.Lineage.SelectionToken, 4096) || !Text(c.NativeToolCallID, 4096) {
		return ErrInvalid
	}
	return nil
}

// StateFile is shared only by the question and Plan stores. It supplies bounded
// private atomic I/O, not schema interpretation, native admission or collection.
type StateFile struct {
	Root, Kind, Name string
	Ref              agentbackend.ConversationRef
}

func (f StateFile) Path() (string, error) {
	if ValidateRef(f.Ref) != nil || !filepath.IsAbs(f.Root) || filepath.Clean(f.Root) != f.Root || (f.Kind != "pi-questions" && f.Kind != "pi-plans") || (f.Name != "questions.json" && f.Name != "plans.json") {
		return "", ErrInvalid
	}
	h := sha256.Sum256([]byte(f.Ref.Session.Workspace))
	return filepath.Join(f.Root, f.Kind, hex.EncodeToString(h[:]), f.Ref.Session.Slug, f.Ref.Session.Incarnation, f.Ref.Member, f.Ref.Incarnation, f.Name), nil
}

// Decode rejects duplicate, null, unknown and trailing JSON, including nested objects.
func Decode(data []byte, out any) error {
	if len(data) > StoreLimit || !utf8.Valid(data) {
		return ErrCapacity
	}
	d := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		if token == nil {
			return ErrInvalid
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return err
					}
					s, ok := key.(string)
					if !ok || seen[s] {
						return ErrInvalid
					}
					seen[s] = true
					if err = value(); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err = value(); err != nil {
						return err
					}
				}
			default:
				return ErrInvalid
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalid
	}
	if err := exactFields(data, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

// Go's JSON decoder otherwise accepts case-insensitive keys and omitted fields.
func exactFields(data []byte, t reflect.Type) error {
	if t.Kind() == reflect.Pointer {
		return exactFields(data, t.Elem())
	}
	if t == reflect.TypeOf(time.Time{}) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if json.Unmarshal(data, &object) != nil {
			return ErrInvalid
		}
		fields := map[string]reflect.Type{}
		optional := map[string]bool{}
		var collect func(reflect.Type)
		collect = func(st reflect.Type) {
			for i := 0; i < st.NumField(); i++ {
				f := st.Field(i)
				if f.Anonymous {
					collect(f.Type)
					continue
				}
				tag := strings.Split(f.Tag.Get("json"), ",")
				name := tag[0]
				if name == "-" {
					continue
				}
				if name == "" {
					name = f.Name
				}
				fields[name] = f.Type
				optional[name] = len(tag) > 1 && tag[1] == "omitempty"
			}
		}
		collect(t)
		for name, raw := range object {
			field, ok := fields[name]
			if !ok {
				return ErrInvalid
			}
			if err := exactFields(raw, field); err != nil {
				return err
			}
		}
		for name := range fields {
			if _, ok := object[name]; !ok && !optional[name] {
				return ErrInvalid
			}
		}
	case reflect.Slice:
		var array []json.RawMessage
		if json.Unmarshal(data, &array) != nil {
			return ErrInvalid
		}
		for _, raw := range array {
			if err := exactFields(raw, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
func ensurePrivateDirs(root, dir string) error {
	if err := private(root, true); err != nil {
		return err
	}
	relative, err := filepath.Rel(root, dir)
	if err != nil || strings.HasPrefix(relative, "..") {
		return ErrInvalid
	}
	parent := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		next := filepath.Join(parent, part)
		err := os.Mkdir(next, 0700)
		if err != nil && !os.IsExist(err) {
			return err
		}
		if err = private(next, true); err != nil {
			return err
		}
		folder, err := os.Open(parent)
		if err != nil {
			return err
		}
		err = folder.Sync()
		folder.Close()
		if err != nil {
			return err
		}
		parent = next
	}
	return nil
}
func private(path string, dir bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0600)
	if dir {
		mode = 0700
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != mode || info.IsDir() != dir || (!dir && !info.Mode().IsRegular()) {
		return fmt.Errorf("private Pi state has invalid permissions")
	}
	return nil
}
func (f StateFile) access(ctx context.Context, out any, mutate func() error) error {
	path, err := f.Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if mutate == nil {
		if _, err = os.Stat(path); err != nil {
			return err
		}
	} else {
		if err = ensurePrivateDirs(f.Root, dir); err != nil {
			return err
		}
	}
	for p := dir; ; p = filepath.Dir(p) {
		if err = private(p, true); err != nil {
			return err
		}
		if p == f.Root {
			break
		}
		if p == filepath.Dir(p) {
			return ErrInvalid
		}
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = private(path+".lock", false); err != nil {
		return err
	}
	op := syscall.LOCK_SH
	if mutate != nil {
		op = syscall.LOCK_EX
	}
	for {
		err = syscall.Flock(int(lock.Fd()), op|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	file, err := os.Open(path)
	if err == nil {
		if err = private(path, false); err != nil {
			file.Close()
			return err
		}
		data, e := io.ReadAll(io.LimitReader(file, StoreLimit+1))
		file.Close()
		if e != nil {
			return e
		}
		if err = Decode(data, out); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) || mutate == nil {
		return err
	}
	if mutate == nil {
		return nil
	}
	if err = mutate(); err != nil {
		return err
	}
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if len(data) > StoreLimit {
		return ErrCapacity
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".write-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	folder, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer folder.Close()
	return folder.Sync()
}
func (f StateFile) Read(ctx context.Context, out any) error { return f.access(ctx, out, nil) }
func (f StateFile) Update(ctx context.Context, out any, fn func() error) error {
	return f.access(ctx, out, fn)
}
