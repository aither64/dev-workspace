package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/processgroup"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

const serviceProjectionMaxBytes = 1024 * 1024

// Only this allowlisted public projection crosses the Ruby owner boundary.
// In particular no unit names, commands, paths, environment or logs are read.
type serviceDiagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type serviceRow struct {
	ID         string             `json:"id"`
	Label      string             `json:"label"`
	Phase      string             `json:"phase"`
	State      string             `json:"state"`
	ObservedAt *string            `json:"observedAt"`
	RetiredAt  *string            `json:"retiredAt"`
	Error      *serviceDiagnostic `json:"error"`
}
type serviceCleanup struct {
	Operation   string `json:"operation"`
	OperationID string `json:"operationId"`
	Phase       string `json:"phase"`
}
type serviceProjection struct {
	Schema     int                `json:"schema"`
	Workspace  string             `json:"workspace"`
	Slug       string             `json:"slug"`
	RegistryID *string            `json:"registryId"`
	Registered bool               `json:"registered"`
	Count      int                `json:"count"`
	CountKnown bool               `json:"countKnown"`
	ObservedAt string             `json:"observedAt"`
	Services   []serviceRow       `json:"services"`
	History    []serviceRow       `json:"history"`
	Cleanup    *serviceCleanup    `json:"cleanup"`
	Notice     *serviceDiagnostic `json:"notice"`
}

var errServicesUnavailable = errors.New("Service status is unavailable.")

func (s *Server) sessionServices(ctx context.Context, summary *session.Summary) (*serviceProjection, error) {
	if s.services == nil {
		return nil, errServicesUnavailable
	}
	key, err := s.coordinationIdentity(summary.Slug)
	if err != nil || summary.ObservationIdentity != "" && key.incarnation != summary.ObservationIdentity {
		return nil, errServicesUnavailable
	}
	validate := func() error {
		current, err := s.coordinationIdentity(summary.Slug)
		if err != nil || current != key {
			return errServicesUnavailable
		}
		return nil
	}
	raw, err := s.services.read(ctx, key, func(ctx context.Context) ([]byte, error) {
		command := exec.Command(s.config.DevSession, "--workspace", s.config.WorkspaceName, "service", "list", summary.Slug, "--as-is", "--json")
		command.Dir = s.config.Workspace
		if s.config.UserStateRoot != "" {
			command.Env = append(os.Environ(), "DEV_WORKSPACES_STATE="+s.config.UserStateRoot)
		}
		out := boundedCoordinationBuffer{limit: serviceProjectionMaxBytes}
		diagnostic := boundedCoordinationBuffer{limit: 4096, truncate: true}
		command.Stdout, command.Stderr = &out, &diagnostic
		if err := processgroup.Run(ctx, command); err != nil {
			return nil, errServicesUnavailable
		}
		if _, err := decodeServiceProjection(out.Bytes(), s.config.Workspace, summary.Slug); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}, validate)
	if err != nil {
		return nil, errServicesUnavailable
	}
	if err := validate(); err != nil {
		return nil, err
	}
	return decodeServiceProjection(raw, s.config.Workspace, summary.Slug)
}

func decodeServiceProjection(raw []byte, workspace, slug string) (*serviceProjection, error) {
	if len(raw) > serviceProjectionMaxBytes || rejectDuplicateCreationJSONKeys(raw) != nil {
		return nil, errServicesUnavailable
	}
	var value serviceProjection
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, errServicesUnavailable
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errServicesUnavailable
	}
	if value.Schema != 1 || value.Workspace != workspace || value.Slug != slug || value.Count != len(value.Services) || value.Services == nil || value.History == nil || len(value.Services)+len(value.History) > 256 || !serviceTime(value.ObservedAt) {
		return nil, errServicesUnavailable
	}
	if value.Registered != (value.RegistryID != nil) || value.RegistryID != nil && !queueClientMessageIDPattern.MatchString(*value.RegistryID) {
		return nil, errServicesUnavailable
	}
	if !value.Registered && (len(value.Services)+len(value.History) != 0 || value.Cleanup != nil) {
		return nil, errServicesUnavailable
	}
	if value.Cleanup != nil && ((value.Cleanup.Operation != "archive" && value.Cleanup.Operation != "delete") || !messageDigestPattern.MatchString(value.Cleanup.OperationID) || !servicePreludePhase(value.Cleanup.Phase) && !session.ValidLifecyclePhase(value.Cleanup.Operation, value.Cleanup.Phase)) {
		return nil, errServicesUnavailable
	}
	validDiagnostic := func(value *serviceDiagnostic) bool {
		return value == nil || value.Code != "" && len(value.Code) <= 128 && value.Message != "" && len(value.Message) <= 4096
	}
	if !validDiagnostic(value.Notice) {
		return nil, errServicesUnavailable
	}
	seen := map[string]bool{}
	for index, rows := range [][]serviceRow{value.Services, value.History} {
		for _, row := range rows {
			if !queueClientMessageIDPattern.MatchString(row.ID) || seen[row.ID] || row.Label == "" || len(row.Label) > 4096 || !validDiagnostic(row.Error) {
				return nil, errServicesUnavailable
			}
			seen[row.ID] = true
			if index == 1 {
				if row.Phase != "retired" || row.RetiredAt == nil {
					return nil, errServicesUnavailable
				}
			} else if row.Phase != "starting" && row.Phase != "running" && row.Phase != "stopping" {
				return nil, errServicesUnavailable
			}
			switch row.State {
			case "starting", "running", "exited", "failed", "stopping", "stopped", "unknown":
			default:
				return nil, errServicesUnavailable
			}
			if row.ObservedAt != nil && !serviceTime(*row.ObservedAt) || row.RetiredAt != nil && !serviceTime(*row.RetiredAt) || index == 0 && row.RetiredAt != nil {
				return nil, errServicesUnavailable
			}
		}
	}
	return &value, nil
}
func serviceTime(value string) bool {
	t, err := time.Parse(time.RFC3339Nano, value)
	_, offset := t.Zone()
	return err == nil && offset == 0
}

func (s *Server) loadServicePage(ctx context.Context, data *pageData) {
	data.Services, _ = s.sessionServices(ctx, data.Session)
	if data.Services != nil {
		data.ServiceCount = countSnapshot{Value: data.Services.Count, Known: data.Services.CountKnown, ObservedAt: data.Services.ObservedAt}
	}
}
