package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

// This descriptor is borrowed from the synchronous Ruby invocation. The parent
// owns its lock until the child exits; this process must never flock or close it.
type communicationAdmissionOptions struct {
	packageRoot, profile, lockPath, descriptor, identity string
}

type communicationProfileIdentity struct {
	device, inode                            uint64
	ctimeSec, ctimeNSec, mtimeSec, mtimeNSec int64
	target                                   string
}

type communicationAdmission struct {
	mu         sync.Mutex
	active     bool
	options    communicationAdmissionOptions
	fd         int
	profile    communicationProfileIdentity
	executable string
}

func decodeCommunicationProfileIdentity(data string) (communicationProfileIdentity, error) {
	var value communicationProfileIdentity
	var fields []json.RawMessage
	if len(data) > 16*1024 || session.DecodePiCreationJSON([]byte(data), &fields, 16*1024) != nil || len(fields) != 7 {
		return value, errors.New("invalid communication profile identity")
	}
	for i, out := range []any{&value.device, &value.inode, &value.ctimeSec, &value.ctimeNSec, &value.mtimeSec, &value.mtimeNSec, &value.target} {
		if json.Unmarshal(fields[i], out) != nil {
			return communicationProfileIdentity{}, errors.New("invalid communication profile identity")
		}
	}
	if value.inode == 0 || value.ctimeNSec < 0 || value.ctimeNSec >= 1e9 || value.mtimeNSec < 0 || value.mtimeNSec >= 1e9 || value.target == "" {
		return communicationProfileIdentity{}, errors.New("invalid communication profile identity")
	}
	return value, nil
}

func readCommunicationProfileIdentity(path string) (communicationProfileIdentity, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return communicationProfileIdentity{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return communicationProfileIdentity{}, errors.New("communication host profile is not a symlink")
	}
	target, err := os.Readlink(path)
	return communicationProfileIdentity{uint64(st.Dev), st.Ino, st.Ctim.Sec, st.Ctim.Nsec, st.Mtim.Sec, st.Mtim.Nsec, target}, err
}

func newCommunicationAdmission(options communicationAdmissionOptions, executable string) (*communicationAdmission, error) {
	values := []string{options.packageRoot, options.profile, options.lockPath, options.descriptor, options.identity}
	present := 0
	for _, value := range values {
		if value != "" {
			present++
		}
	}
	if present == 0 {
		return nil, nil // Direct legacy schema-1 commands retain their old contract.
	}
	if present != len(values) || !canonicalAbsolutePath(options.packageRoot) || !canonicalAbsolutePath(options.profile) || !canonicalAbsolutePath(options.lockPath) {
		return nil, errors.New("communication admission requires the complete parent invocation")
	}
	fd, err := strconv.Atoi(options.descriptor)
	if err != nil || fd < 3 || strconv.Itoa(fd) != options.descriptor {
		return nil, errors.New("invalid communication transition descriptor")
	}
	identity, err := decodeCommunicationProfileIdentity(options.identity)
	if err != nil {
		return nil, err
	}
	a := &communicationAdmission{active: true, options: options, fd: fd, profile: identity, executable: executable}
	if err := a.recheck(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *communicationAdmission) recheck() error {
	if !a.active {
		return errors.New("communication parent invocation has ended")
	}
	var path, descriptor unix.Stat_t
	if err := unix.Lstat(a.options.lockPath, &path); err != nil {
		return err
	}
	if err := unix.Fstat(a.fd, &descriptor); err != nil {
		return err
	}
	if path.Mode&unix.S_IFMT != unix.S_IFREG || descriptor.Mode&unix.S_IFMT != unix.S_IFREG || path.Dev != descriptor.Dev || path.Ino != descriptor.Ino || descriptor.Uid != uint32(os.Geteuid()) || descriptor.Mode&0777 != 0600 {
		return errors.New("communication transition descriptor does not match its owned lock")
	}
	identity, err := readCommunicationProfileIdentity(a.options.profile)
	if err != nil || identity != a.profile {
		return errors.New("communication workspace profile changed")
	}
	root, err := filepath.EvalSymlinks(a.options.packageRoot)
	if err != nil || root != a.options.packageRoot {
		return errors.New("communication package root is not resolved")
	}
	selected, err := filepath.EvalSymlinks(a.options.profile)
	if err != nil || selected != root {
		return errors.New("communication profile does not select its package")
	}
	executable, err := filepath.EvalSymlinks(a.executable)
	if err != nil || executable != filepath.Join(root, "bin/workspace-portal") {
		return errors.New("communication helper does not belong to its package")
	}
	file, err := os.Open(filepath.Join(root, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return err
	}
	var contract map[string]json.RawMessage
	if err := session.DecodePiCreationJSON(data, &contract, 1<<20); err != nil {
		return err
	}
	_, err = session.DecodePiTeamRecordsContract(contract["piTeamRecords"])
	return err
}

func (a *communicationAdmission) withWrite(ctx context.Context, run func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if run == nil {
		return errors.New("communication writer callback is missing")
	}
	if err := a.recheck(); err != nil {
		return err
	}
	return run()
}

func (a *communicationAdmission) expire() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active = false
}
