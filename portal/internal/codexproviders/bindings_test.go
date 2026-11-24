package codexproviders

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type bindingCorpus struct {
	Base  json.RawMessage `json:"base"`
	Cases []struct {
		Name      string   `json:"name"`
		Action    string   `json:"action"`
		Valid     bool     `json:"valid"`
		RawState  string   `json:"rawState"`
		Thread    string   `json:"thread"`
		Targets   []string `json:"targets"`
		Revision  uint64   `json:"revision"`
		Bound     string   `json:"bound"`
		Schema    int      `json:"schema"`
		RoundTrip bool     `json:"roundTrip"`
		Patches   []struct {
			Target string   `json:"target"`
			Path   []string `json:"path"`
			Value  any      `json:"value"`
			Remove bool     `json:"remove"`
		} `json:"patches"`
	} `json:"cases"`
}

func corpusJSON(t *testing.T, data []byte, result any) {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(result); err != nil {
		t.Fatal(err)
	}
}
func corpusBytes(v any) []byte { b, _ := json.Marshal(v); return b }
func corpusPatch(t *testing.T, value any, path []string, replacement any, remove bool) {
	t.Helper()
	if len(path) == 0 {
		t.Fatal("empty corpus path")
	}
	var child any
	switch v := value.(type) {
	case map[string]any:
		if len(path) == 1 {
			if remove {
				delete(v, path[0])
			} else {
				v[path[0]] = replacement
			}
			return
		}
		child = v[path[0]]
	case []any:
		index, err := strconv.Atoi(path[0])
		if err != nil || index >= len(v) {
			t.Fatal("invalid corpus index")
		}
		if len(path) == 1 {
			v[index] = replacement
			return
		}
		child = v[index]
	default:
		t.Fatal("invalid corpus path")
	}
	corpusPatch(t, child, path[1:], replacement, remove)
}
func loadBindingCorpus(t *testing.T) bindingCorpus {
	t.Helper()
	b, err := os.ReadFile("../../../test/fixtures/codex-provider-bindings/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var c bindingCorpus
	corpusJSON(t, b, &c)
	return c
}

func TestBindingSharedCorpus(t *testing.T) {
	corpus := loadBindingCorpus(t)
	for _, test := range corpus.Cases {
		t.Run(test.Name, func(t *testing.T) {
			var document map[string]any
			corpusJSON(t, corpus.Base, &document)
			for _, p := range test.Patches {
				target := any(document)
				if p.Target != "document" {
					target = document[p.Target]
				}
				corpusPatch(t, target, p.Path, p.Value, p.Remove)
			}
			var catalog Catalog
			corpusJSON(t, corpusBytes(document["catalog"]), &catalog)
			data := corpusBytes(document["state"])
			if test.RawState != "" {
				data = []byte(test.RawState)
			}
			state, err := DecodeBindingState(data, "work", "/workspace", "/runtime/codex.sock", catalog)
			before := corpusBytes(state)
			if err == nil {
				switch test.Action {
				case "bounds":
					err = bindingBoundControl(t, state, catalog, document, test.Bound)
				case "decode":
				case "resolve":
					var current CurrentBinding
					corpusJSON(t, corpusBytes(document["current"]), &current)
					var resolved ResolvedBinding
					resolved, err = state.ResolveCurrent(current)
					if err == nil && test.Thread != "" && resolved.ThreadID != test.Thread {
						t.Fatalf("thread=%s", resolved.ThreadID)
					}
				case "receipts":
					var key BindingKey
					corpusJSON(t, corpusBytes(document["key"]), &key)
					var targets []ReceiptTarget
					targets, err = state.ReceiptTargets(key, document["bindingRequired"].(bool), func(id string) ([]byte, error) {
						raw, ok := document["evidence"].(map[string]any)[id]
						if !ok {
							return nil, errors.New("missing sealed operation")
						}
						return []byte(raw.(string)), nil
					})
					if err == nil {
						ids := []string{}
						for _, target := range targets {
							ids = append(ids, target.ThreadID)
						}
						if !reflect.DeepEqual(ids, test.Targets) {
							t.Fatalf("targets=%v want=%v", ids, test.Targets)
						}
					}
				case "commit":
					var commit BindingCommit
					corpusJSON(t, corpusBytes(document["commit"]), &commit)
					if raw, ok := document["commitEvidence"]; ok {
						commit.Evidence = json.RawMessage(raw.(string))
					}
					var candidate State
					candidate, err = state.PrepareBindingCommit(commit, catalog)
					if err == nil {
						for slug, session := range state.Bindings {
							for origin, prior := range session.Conversations {
								affected := false
								for _, u := range commit.Updates {
									affected = affected || (u.Key.Slug == slug && u.Key.OriginalThreadID == origin)
								}
								if !affected && !reflect.DeepEqual(prior, candidate.Bindings[slug].Conversations[origin]) {
									t.Fatal("unrelated historical row changed")
								}
							}
						}
						if test.RoundTrip {
							encoded, encodeErr := json.Marshal(candidate)
							if encodeErr != nil {
								t.Fatal(encodeErr)
							}
							decoded, decodeErr := DecodeBindingState(encoded, "work", "/workspace", "/runtime/codex.sock", catalog)
							if decodeErr != nil {
								t.Fatal(decodeErr)
							}
							var fields map[string]json.RawMessage
							if json.Unmarshal(encoded, &fields) != nil {
								t.Fatal("encoded object invalid")
							}
							if candidate.Schema == 3 {
								if string(fields["bindings"]) != "{}" || decoded.Bindings == nil || len(decoded.Bindings) != 0 || decoded.BindingRevision != 3 {
									t.Fatal("required empty schema3 map lost")
								}
							} else {
								if _, ok := fields["bindings"]; ok {
									t.Fatal("legacy acquired bindings")
								}
								if _, ok := fields["bindingRevision"]; ok {
									t.Fatal("legacy acquired revision")
								}
							}
							t.Logf("roundtrip schema=%d JSON=%s", candidate.Schema, encoded)
						}
						if test.Schema != 0 && candidate.Schema != test.Schema {
							t.Fatalf("schema=%d", candidate.Schema)
						}
						if test.Schema == 1 && !bytes.Equal(before, corpusBytes(candidate)) {
							t.Fatal("workspace no-op changed legacy document")
						}
						if candidate.BindingRevision != test.Revision {
							t.Fatalf("revision=%d", candidate.BindingRevision)
						}
						if !reflect.DeepEqual(candidate.Bindings["beta"], state.Bindings["beta"]) || !reflect.DeepEqual(candidate.Receipts, state.Receipts) {
							t.Fatal("unrelated session or receipts changed")
						}
						if test.Revision == state.BindingRevision && !reflect.DeepEqual(candidate.Bindings, state.Bindings) {
							t.Fatal("selection-only commit changed lineage")
						}
					}
				default:
					t.Fatal("unknown corpus action")
				}
			}
			if !bytes.Equal(before, corpusBytes(state)) {
				t.Fatal("pure operation mutated input")
			}
			if (err == nil) != test.Valid {
				t.Fatalf("valid=%v err=%v", test.Valid, err)
			}
		})
	}
}

func TestBindingProductionAdmissionRemainsOff(t *testing.T) {
	c := loadBindingCorpus(t)
	var doc struct {
		State   State   `json:"state"`
		Catalog Catalog `json:"catalog"`
	}
	corpusJSON(t, c.Base, &doc)
	dir := t.TempDir()
	p, _ := StatePath(dir, "work")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	for _, schema := range []int{1, 2, 3} {
		state := doc.State
		state.Schema = schema
		if schema < 3 {
			state.BindingRevision = 0
			state.Bindings = nil
		}
		if err := os.WriteFile(p, corpusBytes(state), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadState(dir, "work", "/workspace", "/runtime/codex.sock", doc.Catalog)
		if (err == nil) != (schema < 3) {
			t.Fatalf("schema%d err=%v", schema, err)
		}
	}
}

func TestBindingBoundsAndAtomicFailure(t *testing.T) {
	c := loadBindingCorpus(t)
	var doc struct {
		State          State             `json:"state"`
		Catalog        Catalog           `json:"catalog"`
		Key            BindingKey        `json:"key"`
		Current        CurrentBinding    `json:"current"`
		Commit         BindingCommit     `json:"commit"`
		CommitEvidence string            `json:"commitEvidence"`
		Evidence       map[string]string `json:"evidence"`
	}
	corpusJSON(t, c.Base, &doc)
	if _, err := DecodeBindingState(append(corpusBytes(doc.State), bytes.Repeat([]byte(" "), bindingStateLimit)...), "work", "/workspace", "/runtime/codex.sock", doc.Catalog); err == nil {
		t.Fatal("oversized state accepted")
	}
	raw := bytes.Repeat([]byte(" "), bindingEvidenceLimit+1)
	if _, err := doc.State.decodeEvidence(raw, "11111111-1111-4111-8111-111111111111", strings.Repeat("a", 64)); err == nil {
		t.Fatal("oversized evidence accepted")
	}
	doc.State.Receipts = map[string]RequestReceipt{}
	for n := 0; n < 2049; n++ {
		doc.State.Receipts[strconv.Itoa(n)] = RequestReceipt{}
	}
	if err := ValidateBindings(doc.State, doc.Catalog); err == nil {
		t.Fatal("receipt retention exceeded")
	}
	// A complete, correctly sealed operation may still collide with unrelated
	// native identity. Refuse the whole candidate, preserving the caller's state.
	corpusJSON(t, c.Base, &doc)
	var evidence BindingEvidence
	corpusJSON(t, []byte(doc.CommitEvidence), &evidence)
	doc.Commit.Updates[0].ThreadID = "beta-a"
	evidence.Participants[0].NewThreadID = "beta-a"
	doc.Commit.Evidence = corpusBytes(evidence)
	before := corpusBytes(doc.State)
	if _, err := doc.State.PrepareBindingCommit(doc.Commit, doc.Catalog); err == nil {
		t.Fatal("cross-session target collision accepted")
	}
	if !bytes.Equal(before, corpusBytes(doc.State)) {
		t.Fatal("failed candidate partially applied")
	}
	// Changes to a later participant fail before returning any candidate.
	doc.Commit.Updates[1].Key.Incarnation = strings.Repeat("f", 64)
	if _, err := doc.State.PrepareBindingCommit(doc.Commit, doc.Catalog); err == nil {
		t.Fatal("late participant mismatch accepted")
	}
}

func TestBindingEvidenceHashesExactBytes(t *testing.T) {
	c := loadBindingCorpus(t)
	var doc struct {
		State    State             `json:"state"`
		Catalog  Catalog           `json:"catalog"`
		Key      BindingKey        `json:"key"`
		Evidence map[string]string `json:"evidence"`
	}
	corpusJSON(t, c.Base, &doc)
	_, err := doc.State.ReceiptTargets(doc.Key, true, func(id string) ([]byte, error) { return []byte(doc.Evidence[id] + "\n"), nil })
	if err == nil {
		t.Fatal("reformatted evidence accepted without matching seal")
	}
	b := doc.Evidence[doc.State.Bindings["alpha"].Conversations["lead-a"].OperationID]
	h := sha256.Sum256([]byte(b))
	if hex.EncodeToString(h[:]) != doc.State.Bindings["alpha"].Conversations["lead-a"].EvidenceDigest {
		t.Fatal("corpus seal differs")
	}
}

func bindingBoundControl(t *testing.T, state State, catalog Catalog, document map[string]any, bound string) error {
	t.Helper()
	var copied State
	corpusJSON(t, corpusBytes(state), &copied)
	state = copied
	switch bound {
	case "state":
		_, err := DecodeBindingState(append(corpusBytes(state), bytes.Repeat([]byte(" "), bindingStateLimit)...), "work", "/workspace", "/runtime/codex.sock", catalog)
		return err
	case "evidence":
		_, err := state.decodeEvidence(bytes.Repeat([]byte(" "), bindingEvidenceLimit+1), "11111111-1111-4111-8111-111111111111", strings.Repeat("a", 64))
		return err
	case "receipts":
		state.Receipts = map[string]RequestReceipt{}
		for n := 0; n < 2049; n++ {
			state.Receipts[fmt.Sprintf("%08x-aaaa-4aaa-8aaa-%012x", n, n)] = RequestReceipt{Target: "openai", CatalogDigest: strings.Repeat("a", 64)}
		}
		return ValidateBindings(state, catalog)
	case "sessions":
		state.Bindings = map[string]SessionBinding{}
		for n := 0; n < 4097; n++ {
			state.Bindings[fmt.Sprintf("s%d", n)] = SessionBinding{}
		}
		return ValidateBindings(state, catalog)
	case "lineage":
		var key BindingKey
		corpusJSON(t, corpusBytes(document["key"]), &key)
		var lastErr error
		for _, count := range []int{bindingLineageLimit, bindingLineageLimit + 1} {
			evidence := map[string][]byte{}
			oldID, op, digest := key.OriginalThreadID, "", ""
			for n := 1; n <= count; n++ {
				nextID, nextOp := fmt.Sprintf("long-%d", n), fmt.Sprintf("%08x-aaaa-4aaa-8aaa-%012x", n, n)
				e := BindingEvidence{Schema: 1, OperationID: nextOp, Workspace: state.Workspace, Root: state.Root, Socket: state.Socket, PreviousBindingRevision: uint64(n), CommittedBindingRevision: uint64(n + 1), SnapshotDigest: strings.Repeat("a", 64), Participants: []BindingParticipant{{Key: key, OldThreadID: oldID, NewThreadID: nextID, OldProvider: "openai", NewProvider: "openai", PreviousOperationID: op, PreviousEvidenceDigest: digest}}}
				b := corpusBytes(e)
				sum := sha256.Sum256(b)
				evidence[nextOp] = b
				oldID, op, digest = nextID, nextOp, hex.EncodeToString(sum[:])
			}
			state.BindingRevision = uint64(count + 1)
			session := state.Bindings[key.Slug]
			session.Revision = state.BindingRevision
			session.Conversations[key.OriginalThreadID] = ConversationBinding{Member: key.Member, ThreadID: oldID, Provider: "openai", OperationID: op, EvidenceDigest: digest}
			state.Bindings[key.Slug] = session
			targets, err := state.ReceiptTargets(key, true, func(id string) ([]byte, error) { return evidence[id], nil })
			lastErr = err
			if count == bindingLineageLimit && (err != nil || len(targets) != count+1) {
				t.Fatalf("complete retained lineage refused: count=%d err=%v", len(targets), err)
			}
		}
		return lastErr
	}
	t.Fatal("unknown corpus bound")
	return nil
}

func TestBindingProspectivePresenceLeavesLegacyReadersUnchanged(t *testing.T) {
	corpus := loadBindingCorpus(t)
	for _, test := range corpus.Cases {
		if test.Name != "receipt-empty-session" && test.Name != "receipt-empty-session-inherit-false" && test.Name != "receipt-empty-session-inherit-true" && test.Name != "selection-empty-root-with-creation" && test.Name != "selection-empty-creation-with-root" {
			continue
		}
		for _, schema := range []int{1, 2} {
			if schema == 1 && strings.HasPrefix(test.Name, "selection-") {
				continue
			}
			t.Run(fmt.Sprintf("%d/%s", schema, test.Name), func(t *testing.T) {
				var document map[string]any
				corpusJSON(t, corpus.Base, &document)
				for _, p := range test.Patches {
					corpusPatch(t, document[p.Target], p.Path, p.Value, p.Remove)
				}
				state := document["state"].(map[string]any)
				state["schema"] = schema
				delete(state, "bindings")
				delete(state, "bindingRevision")
				if schema == 1 {
					delete(state, "sessions")
				}
				data := corpusBytes(state)
				var catalog Catalog
				corpusJSON(t, corpusBytes(document["catalog"]), &catalog)
				if _, err := DecodeBindingState(data, "work", "/workspace", "/runtime/codex.sock", catalog); err == nil {
					t.Fatal("prospective decoder normalized present-empty identity")
				}
				path, _ := StatePath(t.TempDir(), "work")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				_, err := ReadState(filepath.Dir(filepath.Dir(filepath.Dir(path))), "work", "/workspace", "/runtime/codex.sock", catalog)
				accepted := test.Name != "receipt-empty-session-inherit-true"
				if (err == nil) != accepted {
					t.Fatalf("legacy acceptance changed: err=%v", err)
				}
			})
		}
	}
}

func TestBindingNilMapIsNotSynthesizedByEncoding(t *testing.T) {
	corpus := loadBindingCorpus(t)
	var doc struct {
		State State `json:"state"`
	}
	corpusJSON(t, corpus.Base, &doc)
	doc.State.Bindings = nil
	if _, err := json.Marshal(doc.State); err == nil {
		t.Fatal("nil required binding map encoded")
	}
	for _, schema := range []int{1, 2} {
		doc.State.Schema = schema
		doc.State.BindingRevision = 0
		data, err := json.Marshal(doc.State)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		corpusJSON(t, data, &fields)
		if _, ok := fields["bindings"]; ok {
			t.Fatal("legacy bindings encoded")
		}
		if _, ok := fields["bindingRevision"]; ok {
			t.Fatal("legacy binding revision encoded")
		}
	}
}
