package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// RequireAdmission is a passive execution fence. It never creates a record or
// resumes a native thread. Existing source tools and acknowledgement control use
// their own operation-bound path rather than weakening ordinary admission.
func RequireAdmission(root, profile, workspace string) error {
	if root == "" || profile == "" || workspace == "" {
		return nil
	}
	store := Store{Root: root, Profile: profile}
	if _, e := store.Startup(); e != nil {
		return e
	}
	o, e := store.Active()
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if o.Terminal() {
		return nil
	}
	if o.Schema == Schema {
		return errors.New("workspace maintenance is awaiting completion; new work is held")
	}
	for _, s := range o.Services {
		if s.Workspace == workspace {
			return errors.New("workspace maintenance is awaiting completion; new work is held")
		}
	}
	return nil
}

// SuppressActive records Stop intent before the caller waits for outer package
// admission. No native request or package-generation lock belongs in this path.
func SuppressActive(ctx context.Context, root, profile, workspace, thread string) error {
	if root == "" || profile == "" || workspace == "" {
		return nil
	}
	store := Store{Root: root, Profile: profile}
	o, e := store.Active()
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if o.Terminal() {
		return nil
	}
	for _, p := range o.Participants {
		if p.Workspace == workspace && p.Thread == thread {
			if o.Schema != Schema {
				identity := Identity{Schema: o.Schema, OperationID: o.OperationID, Profile: o.Profile, SourcePackage: o.SourcePackage}
				data, err := store.ownerControl(ctx, identity, "suppress-resume", workspace, thread)
				if err != nil {
					return err
				}
				var result struct {
					Schema int    `json:"schema"`
					Status string `json:"status"`
				}
				if DecodeEnvelope(data, &result) != nil || result.Schema != TransportSchema || result.Status != "suppressed" {
					return errors.New("retained owner Stop refused")
				}
				return nil
			}
			_, e = (Controller{Store: store}).SuppressResume(ctx, Request{OperationID: o.OperationID, ThreadID: thread})
			return e
		}
	}
	return nil
}

// Historical status belongs to its immutable SourcePackage. Project only common
// identity and phase/membership; never decode old Finished or deadline semantics.
func (s Store) loadStatus(id string) (Operation, error) {
	identity, err := s.Identity(id)
	if err != nil {
		return Operation{}, err
	}
	if identity.Schema == Schema {
		return s.Load(id)
	}
	data, err := s.ownerControl(context.Background(), identity, "status", "", "")
	if err != nil {
		return Operation{}, err
	}
	var result struct {
		Schema    int    `json:"schema"`
		Status    string `json:"status"`
		Operation *struct {
			Schema        int    `json:"schema"`
			OperationID   string `json:"operationId"`
			Profile       string `json:"profile"`
			SourcePackage string `json:"sourcePackage"`
			Phase         string `json:"phase"`
			Services      []struct {
				Workspace string `json:"workspace"`
			} `json:"services"`
			Participants []struct {
				Workspace string `json:"workspace"`
				Thread    string `json:"thread"`
			} `json:"participants"`
		} `json:"operation"`
	}
	if err = DecodeEnvelope(data, &result); err != nil {
		return Operation{}, err
	}
	o := result.Operation
	if result.Schema != TransportSchema || o == nil || o.Schema != identity.Schema || o.OperationID != identity.OperationID || o.Profile != identity.Profile || o.SourcePackage != identity.SourcePackage || o.Phase == "" || result.Status != o.Phase || o.Services == nil || o.Participants == nil {
		return Operation{}, errors.New("retained owner status identity changed")
	}
	projected := Operation{Schema: o.Schema, OperationID: o.OperationID, Profile: o.Profile, SourcePackage: o.SourcePackage, Phase: o.Phase, Services: []Service{}, Participants: []Participant{}}
	services := map[string]bool{}
	for _, service := range o.Services {
		if !filepath.IsAbs(service.Workspace) || services[service.Workspace] {
			return Operation{}, errors.New("invalid retained owner services")
		}
		services[service.Workspace] = true
		projected.Services = append(projected.Services, Service{Workspace: service.Workspace})
	}
	participants := map[string]bool{}
	for _, p := range o.Participants {
		key := p.Workspace + ":" + p.Thread
		if !services[p.Workspace] || p.Thread == "" || participants[key] {
			return Operation{}, errors.New("invalid retained owner participants")
		}
		participants[key] = true
		projected.Participants = append(projected.Participants, Participant{Workspace: p.Workspace, Thread: p.Thread})
	}
	return projected, nil
}

type ownerOutput struct{ buffer bytes.Buffer }

func (b *ownerOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > MaxBytes {
		return 0, errors.New("retained owner output exceeds bound")
	}
	return b.buffer.Write(p)
}
func (s Store) ownerControl(ctx context.Context, identity Identity, action, workspace, thread string) ([]byte, error) {
	owner, err := filepath.EvalSymlinks(identity.SourcePackage)
	if err != nil || owner != identity.SourcePackage {
		return nil, errors.New("retained maintenance owner unavailable")
	}
	executable := filepath.Join(owner, "bin/workspace-portal")
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, errors.New("retained maintenance owner unavailable")
	}
	if workspace == "" {
		workspace = s.Root
	}
	request, _ := json.Marshal(Request{Schema: TransportSchema, Action: action, OperationID: identity.OperationID, ThreadID: thread})
	command := exec.CommandContext(ctx, executable, "maintenance-control", "--user-state-root", s.Root, "--host-profile", s.Profile, "--workspace", workspace)
	command.Stdin = bytes.NewReader(request)
	var output ownerOutput
	command.Stdout = &output
	if err = command.Run(); err != nil {
		return nil, err
	}
	return output.buffer.Bytes(), nil
}
