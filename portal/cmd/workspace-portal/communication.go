package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/communication"
)

func communicationCommand(args []string) error {
	flags := flag.NewFlagSet("communication", flag.ContinueOnError)
	workspace := flags.String("workspace", "", "workspace root")
	state := flags.String("user-state-root", "", "private state root")
	authority := flags.String("authority-dir", "", "runtime authority directory")
	socket := flags.String("socket", "", "Codex socket")
	slug := flags.String("session-slug", "", "source session")
	expected := flags.String("expected-root-thread-id", "", "bound native caller")
	expire := flags.Bool("expire", false, "lifecycle owner expires grants under its session lock")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !canonicalAbsolutePath(*workspace) || !canonicalAbsolutePath(*state) {
		return errors.New("communication requires workspace and private state")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	native := communication.Native{Workspace: *workspace, StateRoot: *state, AuthorityDir: *authority, Socket: *socket}
	service, err := native.Service()
	if err != nil {
		return err
	}
	if *expire {
		return service.Expire(ctx, *slug)
	}
	var request communication.Request
	d := json.NewDecoder(io.LimitReader(os.Stdin, 256*1024))
	d.DisallowUnknownFields()
	if err := d.Decode(&request); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing communication request")
	}
	caller, err := communication.IdentityFor(*workspace, *slug, request.Action == "history" || request.Action == "list")
	if err != nil {
		return err
	}
	if *expected != "" && caller.Thread != *expected {
		return errors.New("calling lead identity changed")
	}
	view, err := service.Run(ctx, caller, request)
	if err != nil {
		return err
	}
	if request.MessageID != "" {
		messages := view.Messages[:0]
		for _, message := range view.Messages {
			if message.ID == request.MessageID {
				messages = append(messages, message)
			}
		}
		view.Messages = messages
	}
	if request.Action == "list" {
		view.Messages = nil
	}
	if len(view.Messages) > 10 {
		view.Messages = view.Messages[len(view.Messages)-10:]
	}
	return json.NewEncoder(os.Stdout).Encode(view)
}
