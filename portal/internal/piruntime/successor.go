package piruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func (m *Manager) reserveRuntimeRecord(start StartRequest, instance string) (runtimeRecord, error) {
	if instance == "" {
		instance = newUUID()
	}
	ref := RuntimeRef{Session: start.Session, InstanceID: instance, RequestID: start.RequestID, Generation: randomHex(32)}
	launch := launchFor(start, ref)
	data, err := json.Marshal(launch)
	if err != nil {
		return runtimeRecord{}, err
	}
	data = append(data, '\n')
	_, ref.Digest, err = DecodeLaunch(data)
	if err != nil {
		return runtimeRecord{}, err
	}
	config, err := json.Marshal(m.runtimeConfiguration(launch, ref))
	if err != nil {
		return runtimeRecord{}, err
	}
	config = append(config, '\n')
	sum := sha256.Sum256(config)
	return runtimeRecord{Schema: 1, Start: start, Runtime: ref, RuntimeConfigSHA256: hex.EncodeToString(sum[:]), Phase: "preparing", HostStateRoot: m.config.HostStateRoot, AuthDirectory: m.config.AuthDirectory, AccountBindings: append([]AccountBinding{}, m.config.AccountBindings...)}, nil
}

func (m *Manager) frozenRequestBytes(record runtimeRecord) ([]byte, []byte, error) {
	launch, err := json.Marshal(launchFor(record.Start, record.Runtime))
	if err != nil {
		return nil, nil, err
	}
	launch = append(launch, '\n')
	config, err := json.Marshal(m.runtimeConfiguration(launchFor(record.Start, record.Runtime), record.Runtime))
	if err != nil {
		return nil, nil, err
	}
	config = append(config, '\n')
	sum := sha256.Sum256(config)
	if hex.EncodeToString(sum[:]) != record.RuntimeConfigSHA256 {
		return nil, nil, ErrConflict
	}
	return launch, config, nil
}

func (m *Manager) retainRuntimeRequest(ctx context.Context, record runtimeRecord) error {
	directory := filepath.Join(m.instance(record.Runtime), "requests", record.Runtime.RequestID)
	launch, config, err := m.frozenRequestBytes(record)
	if err != nil {
		return err
	}
	if err := freezeBytes(filepath.Join(directory, "launch.json"), launch); err != nil {
		return err
	}
	if err := freezeBytes(filepath.Join(directory, "runtime-config.json"), config); err != nil {
		return err
	}
	root := filepath.Join(directory, "package-root")
	if target, err := os.Readlink(root); err == nil {
		if target != record.Start.AppStorePath {
			return ErrConflict
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(record.Start.AppStorePath, root); err != nil {
			return err
		}
	} else {
		return err
	}
	folder, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := folder.Sync()
	closeErr := folder.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return err
	}
	return m.config.retainPackage(ctx, root, record.Start.AppStorePath)
}

func (m *Manager) checkPredecessorFiles(previous runtimeRecord, descriptor []byte) error {
	launch, config, err := m.frozenRequestBytes(previous)
	if err != nil {
		return err
	}
	if !bytes.Equal(descriptor, launch) {
		return ErrConflict
	}
	path := filepath.Join(m.instance(previous.Runtime), "requests", previous.Runtime.RequestID, "runtime-config.json")
	actual, err := readPrivateBytes(path, 65536)
	if err != nil || !bytes.Equal(actual, config) {
		return ErrHeld
	}
	// Preserve and verify the legacy instance root; successors never retarget it.
	root := filepath.Join(m.instance(previous.Runtime), "package-root")
	if previous.Start.ReplacesRequestID != "" {
		// Every successor already retained its own exact request before native
		// start. Missing history/root here is inconsistent, never preparation.
		root = filepath.Join(m.instance(previous.Runtime), "requests", previous.Runtime.RequestID, "package-root")
		retained, err := readPrivateBytes(filepath.Join(filepath.Dir(root), "launch.json"), 65536)
		if err != nil || !bytes.Equal(retained, launch) {
			return ErrHeld
		}
	}
	if target, err := os.Readlink(root); err != nil || target != previous.Start.AppStorePath {
		return ErrHeld
	}
	return nil
}

func (m *Manager) prepareSuccessor(ctx context.Context, start StartRequest, records runtimeRecords) (RuntimeStatus, error) {
	unknown := RuntimeStatus{Phase: "unknown", ObservedAt: time.Now().UTC()}
	var current runtimeRecord
	if existing, ok := records.Requests[start.RequestID]; ok {
		if existing.Start != start || records.CurrentRequestID != start.RequestID {
			return unknown, ErrConflict
		}
		current = existing
		if current.Phase != "preparing" {
			return m.Inspect(ctx, current.Runtime)
		}
	} else {
		if start.ReplacesRequestID != records.CurrentRequestID {
			return unknown, ErrConflict
		}
	}
	previous, ok := records.Requests[start.ReplacesRequestID]
	if !ok || previous.Phase != "starting" || start.ReplacesRequestID == start.RequestID {
		return unknown, ErrHeld
	}
	if err := m.config.validatePackage(start.AppStorePath); err != nil {
		return unknown, err
	}
	// This public call MUST precede launch.lock. Every H0 verb takes that lock.
	observed, err := m.config.Host.Inspect(ctx, previous.Runtime)
	if err != nil || !validTerminalReceipt(previous, observed) {
		return unknown, ErrHeld
	}
	if err := m.config.verifyDrain(observed); err != nil {
		return unknown, ErrHeld
	}
	if err := m.assertRuntimeSettled(ctx, records); err != nil {
		return unknown, ErrHeld
	}
	previousBytes, _, err := m.frozenRequestBytes(previous)
	if err != nil {
		return unknown, err
	}
	descriptorPath := filepath.Join(m.instance(previous.Runtime), "launch.json")
	descriptor, err := readPrivateBytes(descriptorPath, 65536)
	if err != nil {
		return unknown, ErrHeld
	}
	if current.Runtime.RequestID == "" {
		if err := m.checkPredecessorFiles(previous, descriptor); err != nil {
			return unknown, err
		}
		current, err = m.reserveRuntimeRecord(start, records.InstanceID)
		if err != nil {
			return unknown, err
		}
		records.Schema = 2
		records.Requests[start.RequestID] = current
		records.CurrentRequestID = start.RequestID
		directory, _ := stateDirectory(m.config.UserStateRoot, start.Session)
		// The new pointer/generation are durable before preparation effects. A
		// failed preparation never rolls back this pointer or mints another ID.
		if err := writePrivate(filepath.Join(directory, "runtime.json"), records); err != nil {
			return unknown, err
		}
	}
	unknown.Runtime = current.Runtime
	launch, _, err := m.frozenRequestBytes(current)
	if err != nil {
		return unknown, err
	}
	if !bytes.Equal(descriptor, previousBytes) && !bytes.Equal(descriptor, launch) {
		return unknown, ErrConflict
	}
	if err := m.checkPredecessorFiles(previous, previousBytes); err != nil {
		return unknown, err
	}
	if err := m.retainRuntimeRequest(ctx, previous); err != nil {
		return unknown, err
	}
	if err := m.retainRuntimeRequest(ctx, current); err != nil {
		return unknown, err
	}
	lock, err := os.OpenFile(filepath.Join(m.instance(current.Runtime), "launch.lock"), os.O_RDWR, 0600)
	if err != nil {
		return unknown, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return unknown, ErrHeld
	}
	// Revalidate only immutable captured H0 evidence and current local proof.
	// Calling H0 Inspect here would conflict with our exclusive publication lock.
	if !validTerminalReceipt(previous, observed) || m.config.verifyDrain(observed) != nil {
		return unknown, ErrHeld
	}
	if err := m.assertRuntimeSettled(ctx, records); err != nil {
		return unknown, ErrHeld
	}
	actual, err := readPrivateBytes(descriptorPath, 65536)
	if err != nil || (!bytes.Equal(actual, previousBytes) && !bytes.Equal(actual, launch)) {
		return unknown, ErrConflict
	}
	if !bytes.Equal(actual, launch) {
		if err := writePrivateBytes(descriptorPath, launch); err != nil {
			return unknown, err
		}
	}
	current.Phase = "starting"
	directory, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	if err := m.saveCurrentRecord(directory, current); err != nil {
		return unknown, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		return unknown, ErrHeld
	}
	// H0 performs its fresh native idle/job/invocation/drain check now. A failed
	// or lost reply leaves exactly this current request/generation held.
	result, err := m.config.Host.Start(ctx, current.Runtime)
	unknown.Observation = result
	if err != nil {
		return unknown, err
	}
	return m.inspectObservation(ctx, current, result)
}
