package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/progressobserver"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
)

const piTeamOutputLimit = 4 << 20

var piTeamHex = regexp.MustCompile(`^[0-9a-f]{64}$`)
var piTeamID = regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

type piTeamSource struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}
type piTeamLead struct {
	Incarnation string `json:"incarnation"`
	NativeID    string `json:"nativeId,omitempty"`
}
type piTeamMember struct {
	Address        string `json:"address"`
	Incarnation    string `json:"incarnation"`
	NativeID       string `json:"nativeId,omitempty"`
	CreationID     string `json:"creationId,omitempty"`
	InitialInputID string `json:"initialInputId,omitempty"`
}
type piTeamClaim struct {
	Source  piTeamSource                     `json:"source"`
	Session monitoroperation.PiSessionRecord `json:"session"`
	Lead    *piTeamLead                      `json:"lead,omitempty"`
	Members []piTeamMember                   `json:"members"`
}
type piTeamPreflight struct {
	Schema   int           `json:"schema"`
	Required bool          `json:"required"`
	Sessions []piTeamClaim `json:"sessions"`
}
type piTeamScanner struct {
	root           string
	workspaces     []string
	count          int
	claimBytes     int
	claims         []piTeamClaim
	monitors       map[string]monitoroperation.PiOperation
	legacyMonitors map[string]bool
}

func piTeamHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
func piTeamCanonical(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\n\r") && utf8.ValidString(path)
}
func piTeamDirectory(path string) ([]os.DirEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || owner.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("retained Pi namespace must be private and owned")
	}
	return os.ReadDir(path)
}
func piTeamRead(path string, limit int) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || owner.Uid != uint32(os.Geteuid()) || info.Size() <= 0 || info.Size() > int64(limit) {
		return nil, errors.New("retained Pi record must be private, owned and bounded")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(data) > limit || !utf8.Valid(data) {
		return nil, errors.New("invalid retained record size or encoding")
	}
	return data, nil
}
func piTeamHeader(data []byte, limit int) (int, error) {
	var fields map[string]json.RawMessage
	if err := monitoroperation.DecodeMixedPiRecord(data, &fields, limit); err != nil || fields == nil {
		return 0, errors.New("invalid retained record JSON")
	}
	var schema int
	if json.Unmarshal(fields["schema"], &schema) != nil || (schema != 1 && schema != 2) {
		return 0, errors.New("unsupported retained record schema")
	}
	if schema == 1 {
		for _, field := range []string{"backend", "backendContract", "sessionIncarnation", "leadIncarnation", "session"} {
			if _, mixed := fields[field]; mixed {
				return 0, errors.New("mixed legacy/Pi retained record")
			}
		}
	}
	return schema, nil
}
func piTeamKnownLock(kind, name string) bool {
	switch kind {
	case "roster":
		return strings.HasSuffix(name, ".operation.lock") && session.ValidSlug(strings.TrimSuffix(name, ".operation.lock")) || strings.HasSuffix(name, ".lock") && session.ValidSlug(strings.TrimSuffix(name, ".lock"))
	case "monitor":
		return name == ".admission.lock" || strings.HasSuffix(name, ".json.lock") && piTeamID.MatchString(strings.TrimSuffix(name, ".json.lock"))
	case "communication":
		return name == "state.lock"
	case "observer":
		return strings.HasSuffix(name, ".json.lock") && piTeamHex.MatchString(strings.TrimSuffix(name, ".json.lock"))
	}
	return false
}

// Inventory every namespace, including ones that no registered workspace owns.
// Atomic temp files are unresolved artifacts, not permission to omit state.
func (s *piTeamScanner) inventory(ctx context.Context, namespace, kind string, limit int, consume func(string, string, []byte, int) error) error {
	base := filepath.Join(s.root, namespace)
	entries, err := piTeamDirectory(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	owners := map[string]string{}
	for _, workspace := range s.workspaces {
		key := piTeamHash(workspace)
		if kind == "roster" {
			key = filepath.Base(userstate.WorkspaceDirectory(s.root, namespace, workspace))
		}
		owners[key] = workspace
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		directory := filepath.Join(base, entry.Name())
		files, err := piTeamDirectory(directory)
		if err != nil {
			return err
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := filepath.Join(directory, file.Name())
			if piTeamKnownLock(kind, file.Name()) {
				stat, err := os.Lstat(path)
				if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 || stat.Size() != 0 {
					return errors.New("invalid retained store lock")
				}
				owner, ok := stat.Sys().(*syscall.Stat_t)
				if !ok || owner.Uid != uint32(os.Geteuid()) {
					return errors.New("foreign retained store lock")
				}
				continue
			}
			if !strings.HasSuffix(file.Name(), ".json") || owners[entry.Name()] == "" {
				return fmt.Errorf("unknown or orphan retained %s artifact", kind)
			}
			s.count++
			if s.count > 65536 {
				return errors.New("retained Pi record inventory exceeds limit")
			}
			data, err := piTeamRead(path, limit)
			if err != nil {
				return err
			}
			schema, err := piTeamHeader(data, limit)
			if err == nil {
				err = consume(owners[entry.Name()], path, data, schema)
			}
			if err != nil {
				return fmt.Errorf("invalid retained %s record: %w", kind, err)
			}
		}
	}
	return nil
}
func (s *piTeamScanner) claim(kind, id string, scope monitoroperation.PiSessionRecord, lead *piTeamLead, members ...piTeamMember) error {
	registered := false
	for _, workspace := range s.workspaces {
		if workspace == scope.Workspace {
			registered = true
			break
		}
	}
	if !registered || scope.Validate() != nil || len(id) == 0 || len(id) > 256 || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
		return errors.New("invalid retained Pi source identity")
	}
	if members == nil {
		members = []piTeamMember{}
	}
	claim := piTeamClaim{Source: piTeamSource{kind, id}, Session: scope, Lead: lead, Members: members}
	encoded, err := json.Marshal(claim)
	if err != nil {
		return err
	}
	s.claimBytes += len(encoded) + 1
	if s.claimBytes > piTeamOutputLimit-64 {
		return errors.New("retained Pi preflight output exceeds limit")
	}
	s.claims = append(s.claims, claim)
	return nil
}
func (s *piTeamScanner) roster(workspace, path string, data []byte, schema int) error {
	record, err := teamruntime.DecodeRosterRecord(data)
	if err != nil {
		return err
	}
	slug := strings.TrimSuffix(filepath.Base(path), ".json")
	if schema == 1 {
		if record.Legacy == nil || record.Legacy.Workspace != workspace || record.Legacy.Slug != slug {
			return errors.New("legacy roster path differs")
		}
		return nil
	}
	r := record.Pi
	if r == nil || r.Workspace != workspace || r.Slug != slug {
		return errors.New("Pi roster path differs")
	}
	members := []piTeamMember{}
	for _, member := range r.Members {
		members = append(members, piTeamMember{Address: member.Address, Incarnation: member.Incarnation, CreationID: member.CreationID, InitialInputID: member.InitialInputID})
	}
	return s.claim("roster", slug, r.Scope(), &piTeamLead{Incarnation: r.LeadIncarnation}, members...)
}
func (s *piTeamScanner) monitor(workspace, path string, data []byte, schema int) error {
	if schema == 1 {
		if s.legacyMonitors == nil {
			s.legacyMonitors = map[string]bool{}
		}
		if s.legacyMonitors[workspace] {
			return nil
		}
		store, err := monitoroperation.NewStore(s.root, workspace)
		if err != nil {
			return err
		}
		_, err = store.List(context.Background(), "")
		if err == nil {
			s.legacyMonitors[workspace] = true
		}
		return err
	}
	op, err := monitoroperation.DecodePiOperation(data)
	if err != nil {
		return err
	}
	if op.Workspace != workspace || filepath.Base(path) != op.ID+".json" {
		return errors.New("Pi monitor path differs")
	}
	key := workspace + "\x00" + op.ID
	if _, duplicate := s.monitors[key]; duplicate {
		return errors.New("duplicate original monitor operation")
	}
	s.monitors[key] = op
	if err := s.claim("monitor", op.ID, op.Owner.Session, &piTeamLead{Incarnation: op.Owner.Lead.Incarnation, NativeID: op.Owner.Lead.NativeID}); err != nil {
		return err
	}
	for _, event := range op.Events {
		if event.Delivery.PiDelivery != nil {
			c := event.Delivery.PiDelivery.Input.Conversation
			if err := s.claim("monitor", event.ID, c.Session, nil, piTeamMember{Address: c.Member, Incarnation: c.Incarnation, NativeID: c.NativeID}); err != nil {
				return err
			}
		}
	}
	if op.Resolution != nil {
		c := op.Resolution.Lead
		return s.claim("monitor", op.Resolution.ID, c.Session, &piTeamLead{Incarnation: c.Incarnation, NativeID: c.NativeID})
	}
	return nil
}
func (s *piTeamScanner) communication(workspace, path string, data []byte, schema int) error {
	if filepath.Base(path) != "state.json" {
		return errors.New("communication aggregate path differs")
	}
	if schema == 1 {
		return communication.ValidateCommunicationRecord(data, workspace)
	}
	state, err := communication.DecodePiCommunication(data)
	if err != nil {
		return err
	}
	if state.Workspace != workspace {
		return errors.New("communication workspace differs")
	}
	before := len(s.claims)
	endpoint := func(kind, id string, e communication.EndpointRecord) error {
		if e.Pi == nil {
			return nil
		}
		return s.claim(kind, id, e.Pi.Session, &piTeamLead{Incarnation: e.Pi.LeadIncarnation, NativeID: e.Pi.NativeID})
	}
	for _, grant := range state.Grants {
		for _, e := range []communication.EndpointRecord{grant.A, grant.B} {
			if err := endpoint("communication_grant", grant.ID, e); err != nil {
				return err
			}
		}
	}
	for _, message := range state.Messages {
		for _, e := range []communication.EndpointRecord{message.From, message.To} {
			if err := endpoint("communication_message", message.ID, e); err != nil {
				return err
			}
		}
	}
	if len(s.claims) == before {
		return errors.New("orphan schema-2 communication has no original Pi fence owner")
	}
	return nil
}
func (s *piTeamScanner) observer(workspace, path string, data []byte, schema int) error {
	key := strings.TrimSuffix(filepath.Base(path), ".json")
	if schema == 1 {
		var header progressobserver.State
		if err := json.Unmarshal(data, &header); err != nil || header.Root == "" || key != piTeamHash(header.Root) {
			return errors.New("legacy observer path differs")
		}
		_, err := (progressobserver.Store{Directory: filepath.Dir(path)}).Load(header.Root)
		return err
	}
	state, err := progressobserver.DecodePiState(data)
	if err != nil {
		return err
	}
	if state.Session.Workspace != workspace || key != piTeamHash("pi:"+state.Session.Incarnation+":"+state.LeadIncarnation) {
		return errors.New("Pi observer path differs")
	}
	members := []piTeamMember{}
	if state.Pending != nil && state.Pending.Conversation != nil {
		c := state.Pending.Conversation
		members = append(members, piTeamMember{Address: c.Member, Incarnation: c.Incarnation, NativeID: c.NativeID})
	}
	return s.claim("observer", key, state.Session, &piTeamLead{Incarnation: state.LeadIncarnation}, members...)
}

func (s *piTeamScanner) domains(ctx context.Context) error {
	for _, store := range []struct {
		namespace, kind string
		limit           int
		consume         func(string, string, []byte, int) error
	}{
		{"codex-teams", "roster", 256 << 10, s.roster},
		{"monitor-operations", "monitor", 512 << 10, s.monitor},
		{"session-communication", "communication", 16 << 20, s.communication},
		{"progress-observers", "observer", 64 << 10, s.observer},
	} {
		if err := s.inventory(ctx, store.namespace, store.kind, store.limit, store.consume); err != nil {
			return err
		}
	}
	return nil
}

func (s *piTeamScanner) utilities(records []piruntime.UtilityRetention) error {
	matched := map[string]bool{}
	for _, record := range records {
		s.count += 1 + len(record.Receipts)
		if s.count > 65536 {
			return errors.New("retained Pi record inventory exceeds limit")
		}
		utility := record.Utility
		op, ok := s.monitors[utility.Session.Workspace+"\x00"+utility.OperationID]
		if !ok || !op.Launch.CreateAttempted || op.Owner.Session.Ref() != utility.Session || op.Launch.Utility.Incarnation != utility.Incarnation || op.Launch.Utility.NativeID != utility.NativeID || op.Launch.CreationID != record.CreationID || op.Launch.Generation != record.Generation || op.Launch.Settings.Runtime() != record.InitialSettings || record.Settings != record.InitialSettings {
			return errors.New("utility mapping disagrees with original monitor launch")
		}
		matched[utility.Session.Workspace+"\x00"+utility.OperationID] = true
		lead := &piTeamLead{Incarnation: op.Owner.Lead.Incarnation, NativeID: op.Owner.Lead.NativeID}
		if err := s.claim("utility_mapping", utility.OperationID, op.Owner.Session, lead); err != nil {
			return err
		}
		for _, receipt := range record.Receipts {
			input := piruntime.SendInput{Message: op.Launch.Envelope, ActionContext: "monitor-launch:" + op.ID, Retry: false}
			if receipt.Schema != 2 || receipt.OwnerKind != "utility" || op.Launch.Input == nil || receipt.Ref.Utility != utility || receipt.Ref.OperationID != op.Launch.MessageID || receipt.Ref.Action != "send" || receipt.Ref.Digest != op.Launch.Input.Digest || receipt.Generation != op.Launch.Generation || receipt.Settings != record.InitialSettings || receipt.Input != input || receipt.Ref.Digest != piruntime.UtilitySubmissionDigest(receipt.Ref, input, receipt.Settings) {
				return errors.New("utility receipt disagrees with original monitor input")
			}
			if op.Launch.Outcome == "accepted" && receipt.Phase != "Accepted" || op.Launch.Outcome == "not_submitted" && receipt.Phase != "NotSubmitted" {
				return errors.New("utility receipt contradicts terminal monitor outcome")
			}
			if err := s.claim("utility_receipt", receipt.Ref.OperationID, op.Owner.Session, lead); err != nil {
				return err
			}
		}
	}
	for key, op := range s.monitors {
		if (op.Launch.Utility.NativeID != "" || op.Launch.Input != nil) && !matched[key] {
			return errors.New("monitor native launch lost its original utility mapping")
		}
	}
	return nil
}

func (s *piTeamScanner) result() (piTeamPreflight, error) {
	identities := map[string]piTeamMember{}
	incarnations := map[string]string{}
	for _, claim := range s.claims {
		key := claim.Session.Workspace + "\x00" + claim.Session.Slug
		if previous := incarnations[key]; previous != "" && previous != claim.Session.Incarnation {
			return piTeamPreflight{}, errors.New("conflicting retained session incarnations")
		}
		incarnations[key] = claim.Session.Incarnation
		members := append([]piTeamMember{}, claim.Members...)
		if claim.Lead != nil {
			members = append(members, piTeamMember{Address: "lead", Incarnation: claim.Lead.Incarnation, NativeID: claim.Lead.NativeID})
		}
		for _, member := range members {
			memberKey := key + "\x00" + member.Address
			previous := identities[memberKey]
			if previous.Incarnation != "" && previous.Incarnation != member.Incarnation || previous.NativeID != "" && member.NativeID != "" && previous.NativeID != member.NativeID || previous.CreationID != "" && member.CreationID != "" && previous.CreationID != member.CreationID || previous.InitialInputID != "" && member.InitialInputID != "" && previous.InitialInputID != member.InitialInputID {
				return piTeamPreflight{}, errors.New("conflicting retained member claims")
			}
			if member.NativeID == "" {
				member.NativeID = previous.NativeID
			}
			if member.CreationID == "" {
				member.CreationID = previous.CreationID
			}
			if member.InitialInputID == "" {
				member.InitialInputID = previous.InitialInputID
			}
			identities[memberKey] = member
		}
	}
	for i := range s.claims {
		sort.Slice(s.claims[i].Members, func(a, b int) bool { return s.claims[i].Members[a].Address < s.claims[i].Members[b].Address })
	}
	sort.Slice(s.claims, func(i, j int) bool {
		a, b := s.claims[i], s.claims[j]
		ak := []string{a.Session.Workspace, a.Session.Slug, a.Session.Incarnation, a.Source.Kind, a.Source.ID}
		bk := []string{b.Session.Workspace, b.Session.Slug, b.Session.Incarnation, b.Source.Kind, b.Source.ID}
		for n := range ak {
			if ak[n] != bk[n] {
				return ak[n] < bk[n]
			}
		}
		aw, _ := json.Marshal(a)
		bw, _ := json.Marshal(b)
		return string(aw) < string(bw)
	})
	claims := []piTeamClaim{}
	for _, claim := range s.claims {
		if len(claims) == 0 || !reflect.DeepEqual(claims[len(claims)-1], claim) {
			claims = append(claims, claim)
		}
	}
	return piTeamPreflight{Schema: 1, Required: len(claims) != 0, Sessions: claims}, nil
}

type piTeamWorkspaceFlags []string

func (f *piTeamWorkspaceFlags) String() string         { return strings.Join(*f, ",") }
func (f *piTeamWorkspaceFlags) Set(value string) error { *f = append(*f, value); return nil }

func piTeamPreflightCommand(args []string) (resultErr error) {
	defer func() {
		if resultErr != nil && len(resultErr.Error()) > 4000 {
			message := resultErr.Error()[:4000]
			for !utf8.ValidString(message) {
				message = message[:len(message)-1]
			}
			resultErr = errors.New(message)
		}
	}()
	rootFlags := 0
	for _, arg := range args {
		if strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0] == "state-root" {
			rootFlags++
		}
	}
	if rootFlags != 1 {
		return errors.New("pi-team-preflight requires exactly one --state-root")
	}
	flags := flag.NewFlagSet("pi-team-preflight", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("state-root", "", "explicit private state root")
	var workspaces piTeamWorkspaceFlags
	flags.Var(&workspaces, "workspace", "registered canonical workspace (repeatable)")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !piTeamCanonical(*root) || len(workspaces) < 1 || len(workspaces) > 1024 {
		return errors.New("pi-team-preflight requires one canonical --state-root and unique --workspace roots")
	}
	seen := map[string]bool{}
	for _, workspace := range workspaces {
		if !piTeamCanonical(workspace) || seen[workspace] {
			return errors.New("invalid or duplicate preflight workspace")
		}
		seen[workspace] = true
	}
	scanner := &piTeamScanner{root: *root, workspaces: workspaces, claims: []piTeamClaim{}, monitors: map[string]monitoroperation.PiOperation{}}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := scanner.domains(ctx); err != nil {
		return err
	}
	utilities, err := piruntime.InspectRetainedUtilities(ctx, *root, workspaces)
	if err != nil {
		return err
	}
	if err := scanner.utilities(utilities); err != nil {
		return err
	}
	for _, store := range []struct{ kind, name string }{{"pi-questions", "questions.json"}, {"pi-plans", "plans.json"}} {
		if err := scanner.workflowRecords(ctx, store.kind, store.name); err != nil {
			return err
		}
	}
	conversations, err := piruntime.InspectRetainedConversations(ctx, *root, workspaces)
	if err != nil {
		return err
	}
	if err := scanner.workflowConversations(conversations); err != nil {
		return err
	}
	result, err := scanner.result()
	if err != nil {
		return err
	}
	data, err := json.Marshal(result)
	if err != nil || len(data)+1 > piTeamOutputLimit {
		return errors.New("retained Pi preflight output exceeds limit")
	}
	_, err = os.Stdout.Write(append(data, '\n'))
	return err
}
