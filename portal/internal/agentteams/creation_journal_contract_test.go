package agentteams

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the real Ruby producer and Go consumer together. Separate fixtures
// for these two sides allowed valid CLI journals to become unreadable by Go.
func TestRubyCreationJournalContract(t *testing.T) {
	for _, schema := range []int{1, 2, 3} {
		for _, state := range []string{"creating", "ready"} {
			for _, provider := range []string{"", "openai", "pool", "a" + strings.Repeat("_", 62)} {
				t.Run(fmt.Sprintf("schema=%d/%s/provider=%s", schema, state, provider), func(t *testing.T) {
					root := t.TempDir()
					goal := filepath.Join(root, "goal")
					if err := os.WriteFile(goal, []byte("Creation contract"), 0600); err != nil {
						t.Fatal(err)
					}
					team, err := json.Marshal(directCreationSnapshot(true))
					if err != nil {
						t.Fatal(err)
					}
					script := `load ARGV.shift
require 'json'
root, schema, state, provider, team = ARGV
runner = DevSession::Runner.new(workspace: root, tmux_socket: File.join(root, 'tmux.sock'),
  env: ENV.to_h.merge('DEV_WORKSPACES_STATE' => File.join(root, 'state')))
schema = Integer(schema)
journal = runner.send(:prepare_creation_journal, 'contract', File.join(root, 'goal'),
  exclusive: true, run_codex: true, model: 'model-1', effort: 'high',
  provider: provider.empty? ? nil : provider,
  team_binding: schema == 2 ? 'v1.e30.' + 'b' * 64 : nil,
  direct_team: schema == 3 ? JSON.parse(team) : nil)
if state == 'ready'
  journal = runner.send(:ready_creation_journal, journal)
  runner.send(:write_creation_journal, runner.send(:creation_journal_file, 'contract'), journal, create: false)
end
puts JSON.generate(journal)
`
					output, err := exec.Command("ruby", "-e", script, "../../../libexec/dev-session", root,
						fmt.Sprint(schema), state, provider, string(team)).CombinedOutput()
					if err != nil {
						t.Fatalf("Ruby journal production: %v\n%s", err, output)
					}
					journal, present, err := ReadCreationJournal(root, "contract")
					if err != nil || !present || journal.Schema != schema || journal.State != state || !journal.RunCodex {
						t.Fatalf("real Ruby journal rejected: present=%t journal=%#v error=%v\n%s", present, journal, err, output)
					}
					if provider == "" && journal.Provider != nil || provider != "" && (journal.Provider == nil || *journal.Provider != provider) {
						t.Fatalf("provider selection changed: %#v, wanted %q", journal.Provider, provider)
					}
					var raw map[string]json.RawMessage
					if err := json.Unmarshal(output, &raw); err != nil {
						t.Fatal(err)
					}
					for _, invalid := range []string{`null`, `""`, `false`, `7`, `[]`, `{}`, `"OpenAI"`, `"../pool"`, `"pool\n"`, `"` + strings.Repeat("a", 64) + `"`} {
						raw["provider"] = json.RawMessage(invalid)
						data, _ := json.Marshal(raw)
						if _, err := DecodeCreationJournal(data, "contract"); err == nil {
							t.Fatalf("accepted invalid provider %s", invalid)
						}
					}
					delete(raw, "provider")
					for _, change := range []string{"unknown", "missing", "duplicate"} {
						copy := make(map[string]json.RawMessage, len(raw))
						for key, value := range raw {
							copy[key] = value
						}
						if change == "unknown" {
							copy["unexpected"] = json.RawMessage(`true`)
						} else if change == "missing" {
							delete(copy, "run_codex")
						}
						data, _ := json.Marshal(copy)
						if change == "duplicate" {
							data = append([]byte(`{"provider":"openai","provider":"pool",`), data[1:]...)
						}
						if _, err := DecodeCreationJournal(data, "contract"); err == nil {
							t.Fatalf("accepted %s field corruption", change)
						}
					}
				})
			}
		}
	}
}
