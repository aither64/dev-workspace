package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"time"
)

// The runtime reader is authoritative. GET never reconciles or claims jobs.
func (s *Server) coordinationAPI(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var command *exec.Cmd
	if slug == "" {
		command = exec.CommandContext(ctx, filepath.Join(s.config.PackageRoot, "bin/workspace-host"), "coordination", "status", "--json", "--portal", "--workspace", s.config.WorkspaceName)
	} else {
		command = exec.CommandContext(ctx, s.config.DevSession, "--workspace", s.config.WorkspaceName, "coordination", "status", slug, "--as-is", "--json", "--portal")
	}
	out := boundedCoordinationBuffer{limit: 1024 * 1024}
	diagnostic := boundedCoordinationBuffer{limit: 4096, truncate: true}
	command.Stdout, command.Stderr = &out, &diagnostic
	command.Dir = s.config.Workspace
	if err := command.Run(); err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "Operations cannot be read: "+diagnostic.String())
		return
	}
	if !json.Valid(out.Bytes()) {
		s.writeError(w, r, http.StatusServiceUnavailable, "Operations returned an invalid response.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(out.Bytes())
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
