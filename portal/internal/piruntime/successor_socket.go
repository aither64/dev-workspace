package piruntime

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Supervise has already verified executable, exact launch/config digest and its
// live native boot/invocation/instance cgroup. The successful H0 Start gate is
// the predecessor-drain authority; the new cgroup is no longer empty here.
func (s *supervisor) validateSuccessorStartup(instance string) error {
	if s.launch.ReplacesRequestID == "" || filepath.Base(instance) != s.ref.InstanceID || filepath.Base(filepath.Dir(instance)) != "instances" || s.handshake.BootID == "" || s.handshake.InvocationID == "" || s.handshake.MainPID != os.Getpid() {
		return ErrScope
	}
	m := Manager{config: Config{UserStateRoot: s.config.UserStateRoot, HostStateRoot: filepath.Dir(filepath.Dir(instance)), AuthDirectory: s.config.AuthDirectory, AccountBindings: s.config.AccountBindings}}
	records, err := m.records(s.ref.Session)
	if err != nil {
		return err
	}
	current, ok := records.Requests[s.ref.RequestID]
	previous, havePrevious := records.Requests[s.launch.ReplacesRequestID]
	if records.Schema != 2 || records.CurrentRequestID != s.ref.RequestID || !ok || !havePrevious || current.Runtime != s.ref || current.Start.ReplacesRequestID != s.launch.ReplacesRequestID || current.Phase != "starting" || current.RuntimeConfigSHA256 != s.handshake.RuntimeConfigSHA256 || previous.Phase != "starting" {
		return ErrScope
	}
	return nil
}

type controlSocketIdentity struct {
	Device uint64
	Inode  uint64
	UID    uint32
	Mode   os.FileMode
}

func identifyControlSocket(info os.FileInfo) (controlSocketIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return controlSocketIdentity{}, ErrScope
	}
	return controlSocketIdentity{Device: uint64(stat.Dev), Inode: stat.Ino, UID: stat.Uid, Mode: info.Mode().Type()}, nil
}

func prepareControlSocket(ctx context.Context, directory *os.File, successor bool, probe func(context.Context, string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join("/proc/self/fd", strconv.Itoa(int(directory.Fd())), "control.sock")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !successor {
		return ErrHeld
	}
	before, err := identifyControlSocket(info)
	if err != nil {
		return err
	}
	if probe == nil {
		probe = func(ctx context.Context, path string) error {
			connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
			if err == nil {
				_ = connection.Close()
			}
			return err
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	// No payload and exactly one probe. Success or ambiguity is a live owner,
	// never authority to replace it or to stop another listener.
	if err := probe(probeCtx, path); !errors.Is(err, syscall.ECONNREFUSED) {
		return ErrHeld
	}
	if probeCtx.Err() != nil {
		return ErrHeld
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return ErrHeld
	}
	after, err := identifyControlSocket(current)
	if err != nil || after != before {
		return ErrHeld
	}
	return os.Remove(path)
}
