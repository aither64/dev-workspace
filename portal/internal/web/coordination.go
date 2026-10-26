package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"

	"github.com/aither64/dev-workspace/portal/internal/processgroup"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// The runtime reader is authoritative. GET never reconciles or claims jobs.
func (s *Server) coordinationAPI(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	key, err := s.coordinationIdentity(slug)
	if err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, errCoordinationUnavailable.Error())
		return
	}
	validate := func() error {
		current, err := s.coordinationIdentity(slug)
		if err != nil || current != key {
			return errCoordinationUnavailable
		}
		return nil
	}
	payload, err := s.coordination.read(r.Context(), key, func(ctx context.Context) ([]byte, error) {
		return s.readCoordination(ctx, slug)
	}, validate)
	if err == nil {
		err = validate()
	}
	if err != nil {
		if r.Context().Err() != nil {
			err = errCoordinationUnavailable
		}
		s.writeError(w, r, http.StatusServiceUnavailable, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(payload)
}

func (s *Server) coordinationIdentity(slug string) (coordinationKey, error) {
	profile, err := readHostProfileIdentity(s.config.HostProfile)
	if err != nil || profile != s.hostProfile {
		return coordinationKey{}, errCoordinationUnavailable
	}
	key := coordinationKey{workspace: s.config.Workspace, name: s.config.WorkspaceName, profile: profile, slug: slug}
	if slug != "" {
		summary, err := session.Find(s.config.Workspace, slug)
		if err != nil {
			return coordinationKey{}, err
		}
		key.thread = summary.Codex.ThreadID
		key.incarnation, err = session.ObservationIdentity(s.config.Workspace, slug,
			filepath.Join(s.config.Workspace, summary.Root, slug), key.thread)
		if err != nil {
			return coordinationKey{}, err
		}
	}
	return key, nil
}

func (s *Server) readCoordination(ctx context.Context, slug string) ([]byte, error) {
	var command *exec.Cmd
	if slug == "" {
		command = exec.Command(filepath.Join(s.config.PackageRoot, "bin/workspace-host"), "coordination", "status", "--json", "--portal", "--workspace", s.config.WorkspaceName)
	} else {
		command = exec.Command(s.config.DevSession, "--workspace", s.config.WorkspaceName, "coordination", "status", slug, "--as-is", "--json", "--portal")
	}
	out := boundedCoordinationBuffer{limit: 1024 * 1024}
	diagnostic := boundedCoordinationBuffer{limit: 4096, truncate: true}
	command.Stdout, command.Stderr = &out, &diagnostic
	command.Dir = s.config.Workspace
	if err := processgroup.Run(ctx, command); err != nil {
		return nil, fmt.Errorf("Operations cannot be read: %s", diagnostic.String())
	}
	return out.Bytes(), nil
}

type boundedCoordinationBuffer struct {
	bytes.Buffer
	limit    int
	truncate bool
}

func (b *boundedCoordinationBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		if b.truncate {
			n := len(p)
			b.Buffer.Write(p[:b.limit-b.Len()])
			return n, nil
		}
		return 0, fmt.Errorf("operations response exceeds its limit")
	}
	return b.Buffer.Write(p)
}
