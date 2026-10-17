package web

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/processgroup"
)

// The user manager owns the archive process independently of portal lifetime.
// Retain the exited unit until the portal has a durable terminal receipt.
func (s *Server) supervisedArchives() bool {
	return s.config.HostProfile != "" && s.config.PackageRoot != "" && s.config.WorkspaceName != ""
}

func archiveUnit(operation lifecycleOperation) string {
	return "workspace-archive-" + operation.ReceiptID + "-" + strconv.Itoa(operation.Attempt) + ".service"
}

type archiveWorkerStatus struct {
	State, Error, FinishedAt string
}

func (s *Server) archiveWorker(ctx context.Context, operation lifecycleOperation) (*archiveWorkerStatus, error) {
	if !s.supervisedArchives() || operation.Kind != "archive" || operation.ReceiptID == "" {
		return nil, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := processgroup.CombinedOutput(readCtx, exec.Command("systemctl", "--user", "--timestamp=unix", "show", archiveUnit(operation),
		"--property=LoadState,ActiveState,SubState,Job,ExecMainCode,ExecMainStatus,ExecMainExitTimestamp"))
	if err != nil {
		return nil, fmt.Errorf("read archive worker: %w", err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if values["LoadState"] == "not-found" || values["LoadState"] == "" {
		return nil, nil
	}
	// A transient unit can be loaded but still waiting for its start job.
	// Without the job check, receipt reconciliation can fail it before ExecStart.
	if values["ActiveState"] == "inactive" && values["SubState"] == "dead" && values["Job"] != "" && values["Job"] != "0" {
		return &archiveWorkerStatus{State: "running"}, nil
	}
	if values["ActiveState"] == "activating" || values["SubState"] == "running" || values["SubState"] == "start" {
		return &archiveWorkerStatus{State: "running"}, nil
	}
	if values["SubState"] == "exited" && values["ExecMainStatus"] == "0" {
		return &archiveWorkerStatus{State: "complete", FinishedAt: archiveFinishTime(values["ExecMainExitTimestamp"])}, nil
	}
	logs, _ := processgroup.CombinedOutput(readCtx, exec.Command("journalctl", "--user", "--unit="+archiveUnit(operation), "--no-pager", "--output=cat", "--lines=24"))
	cause := fmt.Sprintf("archive worker ended (state %s/%s, code %s, status %s)", values["ActiveState"], values["SubState"], values["ExecMainCode"], values["ExecMainStatus"])
	return &archiveWorkerStatus{State: "failed", Error: boundedLifecycleError(cause + "\n" + string(logs)), FinishedAt: archiveFinishTime(values["ExecMainExitTimestamp"])}, nil
}

func archiveFinishTime(value string) string {
	seconds, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(value), "@"), 64)
	if err != nil || seconds <= 0 {
		return ""
	}
	return time.Unix(int64(seconds), int64((seconds-float64(int64(seconds)))*1e9)).UTC().Format(time.RFC3339Nano)
}

func (s *Server) runSupervisedArchive(ctx context.Context, slug string, args []string) error {
	s.operationMu.Lock()
	operation, ok := s.operations[slug]
	s.operationMu.Unlock()
	if !ok || operation.Kind != "archive" || operation.ReceiptID == "" {
		return errors.New("archive has no durable operation receipt")
	}
	commandArgs := []string{"--user", "--quiet", "--unit=" + archiveUnit(operation),
		"--property=Type=exec", "--property=RemainAfterExit=yes", "--property=TimeoutStartSec=infinity",
		"--property=KillMode=control-group", "--property=WorkingDirectory=" + s.config.Workspace,
		"--setenv=DEV_WORKSPACE_NAME=" + s.config.WorkspaceName,
		"--setenv=DEV_WORKSPACES_PROFILE=" + s.config.HostProfile}
	if s.config.UserStateRoot != "" {
		commandArgs = append(commandArgs, "--setenv=DEV_WORKSPACES_STATE="+s.config.UserStateRoot)
	}
	commandArgs = append(commandArgs, "--", filepath.Join(s.config.PackageRoot, "bin", "dev-session"))
	commandArgs = append(commandArgs, args...)
	startCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	output, err := processgroup.CombinedOutput(startCtx, exec.Command("systemd-run", commandArgs...))
	cancel()
	if err != nil {
		return commandFailure("start archive worker", "", string(output), err)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		status, err := s.archiveWorker(ctx, operation)
		if err != nil {
			return err // The independent unit retains execution ownership.
		}
		if status != nil && status.State == "complete" {
			return nil
		}
		if status != nil && status.State == "failed" {
			return errors.New(status.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err() // Never cancel the worker on portal shutdown.
		case <-ticker.C:
		}
	}
}
