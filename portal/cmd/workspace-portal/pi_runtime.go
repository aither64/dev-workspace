package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func parsePiSupervisor(args []string) (map[string]string, error) {
	if len(args) != 9 || args[0] != "supervise" {
		return nil, piruntime.ErrUnavailable
	}
	values := map[string]string{}
	for i := 1; i < len(args); i += 2 {
		key := args[i]
		if key != "--launch-request" && key != "--instance" && key != "--request" && key != "--digest" {
			return nil, piruntime.ErrScope
		}
		if values[key] != "" || args[i+1] == "" {
			return nil, piruntime.ErrScope
		}
		values[key] = args[i+1]
	}
	if len(values) != 4 {
		return nil, piruntime.ErrScope
	}
	return values, nil
}
func handlePiRuntime(args []string) error {
	values, err := parsePiSupervisor(args)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return piruntime.Supervise(ctx, values["--launch-request"], values["--instance"], values["--request"], values["--digest"])
}
