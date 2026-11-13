package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func communicationAdmissionFixture(t *testing.T) (communicationAdmissionOptions, *os.File) {
	t.Helper()
	root := t.TempDir()
	pkg := filepath.Join(root, "package")
	for _, dir := range []string{"bin", "share/workspace-portal"} {
		if err := os.MkdirAll(filepath.Join(pkg, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	contract := `{"piTeamRecords":{"policy":1,"roster":2,"monitorOperation":2,"communication":2,"progressObserver":2,"utilityReceipt":2}}`
	if err := os.WriteFile(filepath.Join(pkg, "share/workspace-portal/runtime-contract.json"), []byte(contract), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "bin/workspace-portal"), nil, 0700); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "profile")
	if err := os.Symlink(pkg, profile); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(root, "transition.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	identity, err := readCommunicationProfileIdentity(profile)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal([]any{identity.device, identity.inode, identity.ctimeSec, identity.ctimeNSec, identity.mtimeSec, identity.mtimeNSec, identity.target})
	if err != nil {
		t.Fatal(err)
	}
	return communicationAdmissionOptions{pkg, profile, lock.Name(), strconv.Itoa(int(lock.Fd())), string(data)}, lock
}

func TestCommunicationAdmissionRejectsMissingStaleAndExpiredProof(t *testing.T) {
	o, lock := communicationAdmissionFixture(t)
	executable := filepath.Join(o.packageRoot, "bin/workspace-portal")
	a, err := newCommunicationAdmission(o, executable)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	sentinel := errors.New("callback failure")
	if err := a.withWrite(context.Background(), func() error { called++; return sentinel }); !errors.Is(err, sentinel) || called != 1 {
		t.Fatalf("callback: %d %v", called, err)
	}
	probe, err := os.OpenFile(lock.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("parent SH changed: %v", err)
	}
	a.expire()
	if err := a.withWrite(context.Background(), func() error { called++; return nil }); err == nil || called != 1 {
		t.Fatal("expired callback admitted")
	}
	if _, err := lock.Stat(); err != nil {
		t.Fatal("borrowed File closed", err)
	}
	if direct, err := newCommunicationAdmission(communicationAdmissionOptions{}, executable); err != nil || direct != nil {
		t.Fatalf("legacy: %v", err)
	}
	for _, mutate := range []func(*communicationAdmissionOptions){
		func(o *communicationAdmissionOptions) { o.descriptor = "" },
		func(o *communicationAdmissionOptions) { o.descriptor = "-1" },
		func(o *communicationAdmissionOptions) { o.descriptor = "999999" },
		func(o *communicationAdmissionOptions) { o.identity = "[1,2,3,4,5,6,null]" },
		func(o *communicationAdmissionOptions) { o.identity = "[1,2,3.0,4,5,6,\"x\"]" },
		func(o *communicationAdmissionOptions) { o.identity += "[]" },
		func(o *communicationAdmissionOptions) { o.identity = strings.Repeat(" ", 16385) },
		func(o *communicationAdmissionOptions) { o.packageRoot = filepath.Dir(o.packageRoot) },
	} {
		bad := o
		mutate(&bad)
		if _, err := newCommunicationAdmission(bad, executable); err == nil {
			t.Fatalf("accepted invalid tuple: %+v", bad)
		}
	}
	for _, raw := range []string{`{}`, `{"piTeamRecords":null}`, `{"piTeamRecords":{"policy":1}}`, `{"piTeamRecords":{"policy":1,"roster":2,"monitorOperation":2,"communication":2,"progressObserver":2,"utilityReceipt":2,"other":2}}`} {
		if err := os.WriteFile(filepath.Join(o.packageRoot, "share/workspace-portal/runtime-contract.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newCommunicationAdmission(o, executable); err == nil {
			t.Fatalf("accepted map %s", raw)
		}
	}
}

func TestCommunicationAdmissionRealBorrowedDescriptor(t *testing.T) {
	if os.Getenv("P2_COMMUNICATION_ADMISSION_CHILD") == "1" {
		var o communicationAdmissionOptions
		var wire [5]string
		if err := json.Unmarshal([]byte(os.Getenv("P2_COMMUNICATION_ADMISSION_TUPLE")), &wire); err != nil {
			t.Fatal(err)
		}
		o = communicationAdmissionOptions{wire[0], wire[1], wire[2], "3", wire[4]}
		// The Ruby chain supplies its actual forwarded argv, rather than the
		// direct child's test tuple. No application entrypoint is substituted.
		for _, field := range []struct {
			name string
			out  *string
		}{
			{"--package-root", &o.packageRoot}, {"--host-profile", &o.profile},
			{"--transition-lock", &o.lockPath}, {"--transition-lock-fd", &o.descriptor},
			{"--expected-profile-identity", &o.identity},
		} {
			for i, arg := range os.Args {
				if arg == field.name && i+1 < len(os.Args) {
					*field.out = os.Args[i+1]
				}
			}
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		a, err := newCommunicationAdmission(o, executable)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.withWrite(context.Background(), func() error {
			probe, err := os.OpenFile(o.lockPath, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			defer probe.Close()
			// Parent EX must still exclude another SH. Downgrading or unlocking
			// the borrowed description in this child would make this succeed.
			if err := unix.Flock(int(probe.Fd()), unix.LOCK_SH|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
				return errors.New("parent EX was changed")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		a.expire()
		if os.Getenv("P2_COMMUNICATION_ADMISSION_FAIL") == "1" {
			os.Exit(23)
		}
		return
	}
	o, lock := communicationAdmissionFixture(t)
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target := filepath.Join(o.packageRoot, "bin/workspace-portal")
	copy, err := os.OpenFile(target, os.O_WRONLY|os.O_TRUNC, 0700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(copy, source); err != nil {
		t.Fatal(err)
	}
	if err := copy.Close(); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal([5]string{o.packageRoot, o.profile, o.lockPath, "3", o.identity})
	if err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		cmd := exec.Command(target, "-test.run=^TestCommunicationAdmissionRealBorrowedDescriptor$", "-test.v")
		cmd.ExtraFiles = []*os.File{lock}
		cmd.Env = append(os.Environ(), "P2_COMMUNICATION_ADMISSION_CHILD=1", "P2_COMMUNICATION_ADMISSION_TUPLE="+string(wire))
		if fail {
			cmd.Env = append(cmd.Env, "P2_COMMUNICATION_ADMISSION_FAIL=1")
		}
		out, err := cmd.CombinedOutput()
		if !fail && err != nil {
			t.Fatalf("child: %v %s", err, out)
		}
		if fail {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("failure child: %v %s", err, out)
			}
		}
		probe, err := os.OpenFile(lock.Name(), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		err = unix.Flock(int(probe.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		_ = probe.Close()
		if !errors.Is(err, unix.EWOULDBLOCK) {
			t.Fatalf("parent lock lost after child: %v", err)
		}
	}
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script := `
require 'stringio'
require 'shellwords'
load File.join(ARGV.shift, 'libexec/workspace-host')
load File.join(ARGV.shift, 'libexec/dev-session')
require 'digest'
root, package, profile, lock, helper = ARGV
host = DevWorkspaceHost::Host.new(env: {'HOME'=>root, 'DEV_WORKSPACES_STATE'=>root, 'DEV_WORKSPACE_TRANSITION_LOCK_FD'=>'3'}, out: StringIO.new, err: StringIO.new)
borrowed = host.send(:inherited_exclusive_transition_file)
abort 'host did not validate exact inherited EX' unless borrowed && borrowed.fileno == 3
child = File.join(root, 'borrowed-cli.rb')
File.write(child, <<~'CHILD')
  require 'stringio'
  load ARGV.shift
  class BorrowedCommunicationCLI < DevSession::CLI
    private
    def run_command(command)
      raise DevSession::Error, 'wrong fixture command' unless command == 'archive'
      with_lifecycle_transition { runner.expire_communication!('fixture') }
    end
  end
  exit BorrowedCommunicationCLI.new(ARGV).run
CHILD
directory = File.join(root, 'session-communication', Digest::SHA256.hexdigest(root))
FileUtils.mkdir_p(directory, mode: 0700)
File.write(File.join(directory, 'state.json'), '{}')
source = File.join(ENV.fetch('P2_COMMUNICATION_REPO'), 'libexec/dev-session')
argv = [child, source, '--workspace', root, '--authority-dir', File.join(root,'authority'), '--portal-command', [helper, '-test.run=^TestCommunicationAdmissionRealBorrowedDescriptor$', '-test.v'].shelljoin, '--host-profile', profile, '--expected-host-generation', package, '--expected-host-profile-token', DevWorkspaceProfileIdentity.token(profile), '--transition-lock', lock, '--transition-lock-fd', borrowed.fileno.to_s, '--', 'archive', 'fixture']
host.send(:system_env!, ENV.to_h.merge('DEV_WORKSPACES_STATE'=>root), RbConfig.ruby, *argv, pass_fds: [borrowed])
abort 'host closed parent descriptor' if borrowed.closed?
`
	for _, fail := range []bool{false, true} {
		cmd := exec.Command(ruby, "-e", script, repo, repo, filepath.Dir(o.lockPath), o.packageRoot, o.profile, o.lockPath, target)
		cmd.ExtraFiles = []*os.File{lock}
		cmd.Env = append(os.Environ(), "P2_COMMUNICATION_ADMISSION_CHILD=1", "P2_COMMUNICATION_ADMISSION_TUPLE="+string(wire), "P2_COMMUNICATION_REPO="+repo)
		if fail {
			cmd.Env = append(cmd.Env, "P2_COMMUNICATION_ADMISSION_FAIL=1")
		}
		out, err := cmd.CombinedOutput()
		if !fail && err != nil {
			t.Fatalf("host->CLI->Go: %v %s", err, out)
		}
		if fail && err == nil {
			t.Fatal("child failure disappeared through Ruby chain")
		}
		probe, err := os.OpenFile(lock.Name(), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		err = unix.Flock(int(probe.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		_ = probe.Close()
		if !errors.Is(err, unix.EWOULDBLOCK) {
			t.Fatalf("Ruby chain changed parent EX: %v", err)
		}
	}
}

func TestCommunicationAdmissionRechecksActualDescriptorAndProfile(t *testing.T) {
	o, _ := communicationAdmissionFixture(t)
	executable := filepath.Join(o.packageRoot, "bin/workspace-portal")
	a, err := newCommunicationAdmission(o, executable)
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.OpenFile(filepath.Join(filepath.Dir(o.lockPath), "other.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	bad := o
	bad.descriptor = strconv.Itoa(int(other.Fd()))
	if _, err := newCommunicationAdmission(bad, executable); err == nil {
		t.Fatal("different owned inode admitted")
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := newCommunicationAdmission(bad, executable); err == nil {
		t.Fatal("closed descriptor admitted")
	}
	if _, err := newCommunicationAdmission(o, filepath.Join(o.packageRoot, "bin/other")); err == nil {
		t.Fatal("unrelated executable admitted")
	}
	if err := os.Remove(o.profile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(o.packageRoot, o.profile); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := a.withWrite(context.Background(), func() error { called = true; return nil }); err == nil || called {
		t.Fatal("replaced profile link admitted callback")
	}
}
