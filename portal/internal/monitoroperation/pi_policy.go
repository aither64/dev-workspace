package monitoroperation

import (
	"errors"
	"reflect"
	"regexp"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

var piUtilityHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

var ErrPiUtilityUnavailable = errors.New("Pi utility authority and record variant unavailable")

// PiPolicyFromCatalog snapshots the existing installed instruction/policy
// contract only. Its native configuration name is not Pi execution authority.
func PiPolicyFromCatalog(packageRoot string, retained, selected *agentteams.Catalog) (PiUtilityPolicy, error) {
	p, err := PolicyFromCatalog(packageRoot, retained, selected)
	if err != nil {
		return PiUtilityPolicy{}, err
	}
	access := ""
	if selected.FullExecutionAccess() {
		access = "full_access"
	}
	return PiUtilityPolicy{PackageRoot: p.PackageRoot, CatalogDigest: p.CatalogDigest,
		Model: p.Model, Effort: p.Effort, Instructions: p.Instructions,
		SavedAccess: retained.Utilities.VerificationWatcher.Access, ExecutionAccess: access,
		MaxConcurrent: p.MaxConcurrent}, nil
}

// CheckPiUtilityRetry compares the original frozen launch/policy/source. It
// never resolves a new catalog or allocates another operation on retry.
func CheckPiUtilityRetry(original, retry PiUtilityLaunch) error {
	if !reflect.DeepEqual(original, retry) {
		return piruntime.ErrConflict
	}
	return nil
}

// CheckPiUtilityLaunch is a permanent refusal for this independent slice. A
// complete distinct utility mode and existing-record variant are prerequisites;
// a plausible projection cannot enable the missing production path.
func CheckPiUtilityLaunch(launch PiUtilityLaunch) error {
	h := launch.Handle
	if !canonicalPath(h.Session.Workspace) || !session.ValidSlug(h.Session.Slug) ||
		!piUtilityHex.MatchString(h.Session.Incarnation) || !piUtilityHex.MatchString(h.Incarnation) ||
		!piUtilityHex.MatchString(h.Generation) || !validID(h.OperationID) ||
		launch.Request.ID != h.OperationID || validateStart(launch.Request) != nil {
		return piruntime.ErrScope
	}
	return ErrPiUtilityUnavailable
}

func validPiUtilityHandle(h PiUtilityHandle) bool {
	return canonicalPath(h.Session.Workspace) && session.ValidSlug(h.Session.Slug) &&
		piUtilityHex.MatchString(h.Session.Incarnation) && piUtilityHex.MatchString(h.Incarnation) &&
		piUtilityHex.MatchString(h.Generation) && validID(h.OperationID) &&
		bounded(h.NativeID, 4096) && strings.TrimSpace(h.NativeID) != ""
}

// CheckPiUtilityReport is validation of projected evidence, not publication.
// The existing owner must publish/reconcile a versioned event under its locks.
func CheckPiUtilityReport(expected, caller PiUtilityHandle, previous *ReportRequest, report ReportRequest, settled PiUtilitySettlement) error {
	if expected != caller || !validPiUtilityHandle(expected) {
		return piruntime.ErrScope
	}
	if err := validateReport(report); err != nil {
		return err
	}
	if previous != nil && !reflect.DeepEqual(*previous, report) {
		return piruntime.ErrConflict
	}
	if report.Slot == "terminal" && report.Outcome != "incomplete" &&
		(!settled.SourceComplete || !settled.EffectsSettled || !settled.ReportPublished) {
		return piruntime.ErrHeld
	}
	return nil
}
