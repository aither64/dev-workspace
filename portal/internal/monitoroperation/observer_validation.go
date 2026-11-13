package monitoroperation

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

var observerDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

var directCIRepository = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var directCINumber = regexp.MustCompile(`^[1-9][0-9]*$`)
var directCICommit = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ValidateDirectCISource is the fixed read-only GitHub adapter's grammar. Keep
// generic native/persisted source validation separate so exact legacy replay
// never acquires new admission requirements.
func ValidateDirectCISource(source Source) error {
	parts := strings.Split(source.Repository, "/")
	if source.Kind != "ci" || source.Provider != "github" || !directCIRepository.MatchString(source.Repository) || len(parts) != 2 || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." || !directCINumber.MatchString(source.RunID) || !directCINumber.MatchString(source.WorkflowID) || source.RunAttempt < 1 || !directCICommit.MatchString(source.Commit) {
		return errors.New("GitHub observer source is invalid")
	}
	return nil
}

func utcTime(at time.Time) bool { _, offset := at.Zone(); return !at.IsZero() && offset == 0 }

func validateArchive(a ArchiveAuthority) error {
	if !session.ValidSlug(a.Slug) || a.RootThreadID == "" || !observerDigest.MatchString(a.OperationID) ||
		strings.TrimSpace(a.TargetID) == "" || !bounded(a.TargetID, 4096) ||
		strings.TrimSpace(a.Mode) == "" || !bounded(a.Mode, 256) || !bounded(a.ReceiptID, 256) {
		return errors.New("observer archive authority requires exact accepted identities")
	}
	return nil
}

func validateSnapshot(snapshot CISnapshot) error {
	if !utcTime(snapshot.ObservedAt) || !bounded(snapshot.URL, 4096) || !bounded(snapshot.Conclusion, 256) {
		return errors.New("invalid CI observation snapshot")
	}
	switch snapshot.Status {
	case "queued", "in_progress", "waiting", "requested", "pending":
		if snapshot.Conclusion != "" {
			return errors.New("nonterminal CI snapshot has a conclusion")
		}
	case "completed":
		if snapshot.Conclusion == "" {
			return errors.New("completed CI snapshot lacks conclusion")
		}
	default:
		return errors.New("unknown CI snapshot status")
	}
	return nil
}

func validatePoll(receipt PollReceipt) error {
	if !validID(receipt.ID) || receipt.OwnerEpoch == "" || !bounded(receipt.OwnerEpoch, 4096) ||
		!utcTime(receipt.StartedAt) || !bounded(receipt.Handle, 4096) {
		return errors.New("invalid owned CI poll receipt")
	}
	switch receipt.State {
	case "intent":
		if receipt.Handle != "" || receipt.FinishedAt != nil || receipt.ExitStatus != nil {
			return errors.New("poll intent contains execution evidence")
		}
	case "running":
		if receipt.Handle == "" || receipt.FinishedAt != nil || receipt.ExitStatus != nil {
			return errors.New("running poll lacks exact runner receipt")
		}
	case "joined":
		if receipt.Handle == "" || receipt.FinishedAt == nil || !utcTime(*receipt.FinishedAt) || receipt.FinishedAt.Before(receipt.StartedAt) {
			return errors.New("poll join requires an exact finished runner receipt")
		}
	default:
		return errors.New("invalid CI poll state")
	}
	return nil
}

func validateObserver(record ObserverRecord) error {
	owner := record.Owner
	if record.Schema != ObserverSchema || !validID(record.ID) || !canonicalPath(record.Workspace) ||
		!session.ValidSlug(owner.Slug) || owner.RootThreadID == "" || owner.Incarnation == "" || owner.OriginTurnID == "" ||
		!canonicalPath(owner.Socket) || owner.SocketEpoch == "" || owner.RuntimeSchema < 1 ||
		!observerDigest.MatchString(record.RequestDigest) || record.SourceDigest != SourceDigest(record.Source) ||
		!utcTime(record.CreatedAt) || !utcTime(record.UpdatedAt) || record.UpdatedAt.Before(record.CreatedAt) || record.Revision < 1 || !bounded(record.Detail, 4096) {
		return errors.New("invalid monitor observer identity")
	}
	if record.Source.Kind != "ci" {
		return errors.New("observer source must identify an existing CI run")
	}
	if err := validateStart(StartRequest{ID: record.ID, Brief: "retained CI observation", Source: record.Source}); err != nil {
		return err
	}
	switch record.Driver {
	case "direct-ci":
		if record.Source.Provider != "github" {
			return errors.New("unsupported direct CI provider")
		}
	default:
		return errors.New("invalid monitor observer driver")
	}
	switch record.Phase {
	case "observing":
		if record.Driver != "direct-ci" || record.Archive != nil || record.LocalProof != nil {
			return errors.New("observing record has incompatible proof or archive state")
		}
	case "quiescing":
		if record.Archive == nil {
			return errors.New("quiescing observer lacks archive authority")
		}
	case "detached":
		if record.LocalProof == nil {
			return errors.New("quiescent observer lacks local proof")
		}
		if record.Phase == "detached" && record.Archive == nil {
			return errors.New("detached observer lacks accepted archive binding")
		}
	default:
		return errors.New("invalid observer phase")
	}
	if record.Archive != nil {
		if err := validateArchive(*record.Archive); err != nil {
			return err
		}
		if record.Archive.Slug != owner.Slug || record.Archive.RootThreadID != owner.RootThreadID {
			return errors.New("observer archive owner differs")
		}
	}
	if record.Poll != nil {
		if err := validatePoll(*record.Poll); err != nil {
			return err
		}
	}
	if record.Remote != nil {
		if err := validateSnapshot(*record.Remote); err != nil {
			return err
		}
	}
	if record.LocalProof != nil {
		proof := record.LocalProof
		if !utcTime(proof.At) || validateResolution(ResolutionRequest{OperationID: record.ID, ResolutionID: record.ID, Evidence: proof.Evidence, Detail: proof.Detail}) != nil {
			return errors.New("invalid local observation proof")
		}
		if proof.Kind != "runtime-joined" ||
			record.Poll != nil && record.Poll.State != "joined" {
			return errors.New("runtime local proof lacks joined execution")
		}
	}
	if len(record.Events) > 1 {
		return errors.New("too many direct CI events")
	}
	for _, event := range record.Events {
		if err := validateReport(event.Report); err != nil {
			return err
		}
		if event.Report.Slot != "terminal" || event.ID != derivedID(record.ID, "terminal") || event.CreatedAt.IsZero() ||
			event.Delivery.MessageID != event.ID || event.Delivery.Context != "monitor:"+record.ID+":"+event.ID || !bounded(event.Delivery.Envelope, 40*1024) {
			return errors.New("invalid observer event identity")
		}
		switch event.Delivery.Status {
		case "pending", "held", "submitting", "accepted", "canceled":
		default:
			return errors.New("invalid observer delivery state")
		}
		options, _ := json.Marshal(event.Delivery.Options)
		if len(options) > 64*1024 || !bounded(event.Delivery.Detail, 4096) || event.Delivery.Receipt != nil && event.Delivery.Receipt.ClientUserMessageID != event.ID {
			return errors.New("invalid observer delivery receipt/options")
		}
	}
	return nil
}
