package pihosttools

import (
	"context"
	"encoding/json"
	"regexp"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// PiUtilityCaller is captured by the authenticated callback utility arm. It is
// intentionally not a retained PiCaller or roster conversation identity.
type PiUtilityCaller struct {
	Utility                      monitoroperation.PiUtilityRecord
	Generation, InputOperationID string
}
type UtilityHandlers interface {
	MonitorStatus(context.Context, PiUtilityCaller) (any, error)
	MonitorReport(context.Context, PiUtilityCaller, monitoroperation.ReportRequest) (string, error)
}
type UtilityTools struct {
	caller   PiUtilityCaller
	handlers UtilityHandlers
}

func (tools *UtilityTools) HostTools() []string {
	if tools == nil || tools.handlers == nil {
		return nil
	}
	projection, ok := tools.handlers.(interface{ HostTools() []string })
	if !ok {
		return nil
	}
	names := projection.HostTools()
	if len(names) == 0 {
		return nil
	}
	if len(names) != 1 || names[0] != "monitor_operation" {
		return nil
	}
	return []string{"monitor_operation"}
}

// MonitorDomainHandlers connects only to the existing admitted operation store.
// It installs no transport and cannot grant authority to a missing verifier.
type MonitorDomainHandlers struct {
	Service monitoroperation.PiReportService
	Now     func() time.Time
}

func (h MonitorDomainHandlers) MonitorStatus(ctx context.Context, c PiUtilityCaller) (any, error) {
	return h.Service.Status(ctx, c.Utility, c.InputOperationID, c.Generation)
}
func (h MonitorDomainHandlers) MonitorReport(ctx context.Context, c PiUtilityCaller, report monitoroperation.ReportRequest) (string, error) {
	at := time.Now().UTC()
	if h.Now != nil {
		at = h.Now()
	}
	return h.Service.Report(ctx, c.Utility.Session, c.Generation, c.Utility, c.InputOperationID, report, at)
}

var utilityID = regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var utilityGeneration = regexp.MustCompile(`^[0-9a-f]{64}$`)

func BindUtilityTools(caller PiUtilityCaller, handlers UtilityHandlers) (*UtilityTools, error) {
	if caller.Utility.Validate(true) != nil || !utilityGeneration.MatchString(caller.Generation) || !utilityID.MatchString(caller.InputOperationID) {
		return nil, piruntime.ErrScope
	}
	return &UtilityTools{caller: caller, handlers: handlers}, nil
}

// Call admits only the existing monitor status/report actions. The callback
// bridge must prove this exact input Accepted and native-witnessed first; a
// tool-origin claim is insufficient. Registration remains the bridge owner's.
func (t *UtilityTools) Call(ctx context.Context, name string, data json.RawMessage) (any, error) {
	if name != "monitor_operation" {
		return nil, piruntime.ErrScope
	}
	var args map[string]json.RawMessage
	if err := monitoroperation.DecodeMixedPiRecord(data, &args, 32*1024); err != nil {
		return nil, err
	}
	var action string
	if json.Unmarshal(args["action"], &action) != nil {
		return nil, piruntime.ErrScope
	}
	delete(args, "action")
	if t == nil || t.handlers == nil {
		return nil, piruntime.ErrUnavailable
	}
	if action == "status" {
		if len(args) != 0 {
			return nil, piruntime.ErrScope
		}
		return t.handlers.MonitorStatus(ctx, t.caller)
	}
	if action != "report" {
		return nil, piruntime.ErrScope
	}
	reportJSON, _ := json.Marshal(args)
	var report monitoroperation.ReportRequest
	if err := monitoroperation.DecodeMixedPiRecord(reportJSON, &report, 32*1024); err != nil {
		return nil, err
	}
	return t.handlers.MonitorReport(ctx, t.caller, report)
}
