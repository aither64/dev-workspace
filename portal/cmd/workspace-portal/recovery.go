package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

// This private bridge lets the canonical Ruby helper share the portal's store.
func recoveryCommand(args []string) error {
	flags := flag.NewFlagSet("recovery", flag.ContinueOnError)
	workspace := flags.String("workspace", "", "registered workspace root")
	root := flags.String("user-state-root", "", "private user state root")
	slug := flags.String("slug", "", "retained session slug")
	socket := flags.String("socket", "", "registered Codex socket")
	thread := flags.String("thread-id", "", "exact retained root")
	operation := flags.String("operation", "status", "status, running, restore or stop")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *thread == "" {
		return errors.New("exact recovery identity is required")
	}
	store := session.RecoveryStore{Root: *root, Workspace: *workspace}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if *operation == "archive" || *operation == "delete" {
		return store.Retire(ctx, *slug, *thread, *socket, *operation == "delete")
	}
	if *operation != "status" {
		if *operation != "running" && *operation != "restore" && *operation != "stop" {
			return errors.New("unknown recovery operation")
		}
		return store.Set(ctx, *slug, *thread, *socket, *operation != "stop", *operation == "running")
	}
	record, err := store.Load(*slug)
	if errors.Is(err, os.ErrNotExist) {
		return json.NewEncoder(os.Stdout).Encode(map[string]bool{"held": true})
	}
	if err != nil {
		return err
	}
	if record.ThreadID != *thread || record.SocketPath != *socket {
		return errors.New("retained recovery identity changed")
	}
	epoch, err := session.SocketIdentity(*socket)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]bool{"held": !record.Active(*thread, epoch)})
}
