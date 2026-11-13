package session

import (
	"errors"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"gopkg.in/yaml.v3"
)

type ManifestBackend struct {
	Kind               agentbackend.Kind `yaml:"kind" json:"kind"`
	Contract           int               `yaml:"contract" json:"contract"`
	SessionIncarnation string            `yaml:"session_incarnation" json:"sessionIncarnation"`
}

func (m Manifest) MarshalYAML() (any, error) {
	if m.Schema != 4 {
		type plain Manifest
		return plain(m), nil
	}
	if err := m.Validate(m.Slug); err != nil {
		return nil, err
	}
	type creation struct {
		State           string `yaml:"state"`
		GoalSHA256      string `yaml:"goal_sha256"`
		InitialGoalSent bool   `yaml:"initial_goal_sent"`
	}
	return struct {
		Schema       int              `yaml:"schema"`
		Slug         string           `yaml:"slug"`
		Backend      *ManifestBackend `yaml:"backend"`
		Creation     creation         `yaml:"creation"`
		Repositories []Repository     `yaml:"repositories"`
		Sensitive    bool             `yaml:"sensitive,omitempty"`
		Artifacts    []Artifact       `yaml:"artifacts,omitempty"`
	}{m.Schema, m.Slug, m.Backend, creation{m.Creation.State, m.Creation.GoalSHA256, m.Creation.InitialGoalSent}, m.Repositories, m.Sensitive, m.Artifacts}, nil
}

// A tracking projection is not authority. Callers must compare this identity
// with the durable private binding before routing or publishing completion.
func (m Manifest) ValidateBackendBinding(binding BackendBinding) error {
	if err := m.Validate(binding.Slug); err != nil {
		return err
	}
	if err := binding.Validate(binding.Workspace, binding.Slug); err != nil {
		return err
	}
	if m.Schema != 4 || m.Backend.SessionIncarnation != binding.SessionIncarnation ||
		(m.Creation.State == "ready" && binding.State != "ready") {
		return agentbackend.ErrBindingConflict
	}
	return nil
}

func validatePiManifestNode(root *yaml.Node) error {
	fields, err := strictMapping(root, []string{"schema", "slug", "backend", "creation", "repositories", "sensitive", "artifacts"},
		[]string{"schema", "slug", "backend", "creation", "repositories"})
	if err != nil {
		return err
	}
	backend, err := strictMapping(fields["backend"], []string{"kind", "contract", "session_incarnation"}, []string{"kind", "contract", "session_incarnation"})
	if err != nil {
		return err
	}
	var contract int
	if err := backend["contract"].Decode(&contract); err != nil {
		return err
	}
	if scalarTag(backend["kind"], "!!str", "kind") != nil || backend["kind"].Value != "pi" ||
		scalarTag(backend["contract"], "!!int", "contract") != nil || contract != 1 ||
		scalarTag(backend["session_incarnation"], "!!str", "session_incarnation") != nil || !sha256Pattern.MatchString(backend["session_incarnation"].Value) {
		return errors.New("invalid Pi tracking backend")
	}
	creation, err := strictMapping(fields["creation"], []string{"state", "goal_sha256", "initial_goal_sent"}, []string{"state", "goal_sha256", "initial_goal_sent"})
	if err != nil {
		return err
	}
	if scalarTag(creation["state"], "!!str", "state") != nil ||
		scalarTag(creation["goal_sha256"], "!!str", "goal_sha256") != nil ||
		scalarTag(creation["initial_goal_sent"], "!!bool", "initial_goal_sent") != nil {
		return errors.New("invalid Pi tracking creation")
	}
	return nil
}
