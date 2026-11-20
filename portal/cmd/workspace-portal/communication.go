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
	portalSocket := flags.String("portal-socket", "", "serving portal private socket")
	portalOrigin := flags.String("portal-origin", "", "registered portal origin")
	expected := flags.String("expected-root-thread-id", "", "bound native caller")
	expire := flags.Bool("expire", false, "lifecycle owner expires grants under its session lock")
	var admissionOptions communicationAdmissionOptions
	flags.StringVar(&admissionOptions.packageRoot, "package-root", "", "parent invocation package")
	flags.StringVar(&admissionOptions.profile, "host-profile", "", "parent invocation profile")
	flags.StringVar(&admissionOptions.lockPath, "transition-lock", "", "parent transition lock")
	flags.StringVar(&admissionOptions.descriptor, "transition-lock-fd", "", "borrowed parent transition descriptor")
	flags.StringVar(&admissionOptions.identity, "expected-profile-identity", "", "parent profile identity")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !canonicalAbsolutePath(*workspace) || !canonicalAbsolutePath(*state) {
		return errors.New("communication requires workspace and private state")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	admission, err := newCommunicationAdmission(admissionOptions, executable)
	if err != nil {
		return err
	}
	if admission != nil {
		defer admission.expire()
	}
	native := communication.Native{Workspace: *workspace, StateRoot: *state, AuthorityDir: *authority, Socket: *socket}
	service, err := native.Service()
	if err != nil {
		return err
	}
	if admission != nil {
		service.Store.Schema2Write = admission.withWrite
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
	caller, err := communication.EndpointFor(*workspace, *state, *slug, request.Action == "history" || request.Action == "list")
	if err != nil {
		return err
	}
	if *expected != "" && (caller.Codex == nil || caller.Codex.Thread != *expected) {
		return errors.New("calling lead identity changed")
	}
	service.ResolveEndpoint = func(slug string) (communication.EndpointRecord, error) {
		return communication.EndpointFor(*workspace, *state, slug, false)
	}
	if request.Action != "list" && request.Action != "history" {
		peer, err := service.ResolveEndpoint(request.Peer)
		if err != nil {
			return err
		}
		forward := caller.Pi != nil || peer.Pi != nil
		if request.MessageID != "" || request.ReplyTo != "" {
			previous, err := service.ReadEndpoint(ctx, caller)
			if err != nil {
				return err
			}
			for _, message := range previous.Messages {
				if (message.ID == request.MessageID || message.ID == request.ReplyTo) && (message.From.Pi != nil || message.To.Pi != nil) {
					forward = true
				}
			}
		}
		if forward {
			if admission == nil {
				return errors.New("Pi communication requires the complete parent invocation")
			}
			if err := admission.recheck(); err != nil {
				return err
			}
			return forwardCommunication(ctx, *workspace, *state, *authority, *portalSocket, *portalOrigin, caller, request, admission, os.Stdout)
		}
	}
	view, err := service.RunEndpoint(ctx, caller, request)
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
