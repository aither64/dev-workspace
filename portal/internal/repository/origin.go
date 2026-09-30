package repository

import "context"

// Origin is derived from a registered repository, never from a browser URL.
// The provider owns validation and all links leaving the portal.
type Origin struct {
	Provider   string `json:"provider"`
	Repository string `json:"repository"`
	Label      string `json:"label"`
	URL        string `json:"url"`
}

type OriginLinks struct {
	RepositoryURL string `json:"repositoryUrl,omitempty"`
	BranchURL     string `json:"branchUrl,omitempty"`
	CompareURL    string `json:"compareUrl,omitempty"`
	WorkflowsURL  string `json:"workflowsUrl,omitempty"`
}

type OriginRepository struct {
	DefaultBranch string
	HeadSHA       string
}

// OriginProvider is an internal transport and link boundary. Production only
// constructs the GitHub implementation; injection permits deterministic tests.
type OriginProvider interface {
	Resolve(repository string) (*Origin, error)
	Links(origin *Origin, branch, defaultBranch, base, head string, archived bool) OriginLinks
	CommitURL(origin *Origin, sha string) string
	Repository(context.Context, *Origin, string) (OriginRepository, error)
	CompareHeads(context.Context, *Origin, string, string) (string, error)
	Workflows(context.Context, *Origin, string, string) ([]Run, error)
}

func (r Runner) originProvider() OriginProvider {
	if r.Provider != nil {
		return r.Provider
	}
	return githubOriginProvider{command: r.command, executable: r.gh()}
}

func (r ReviewReader) originProvider() OriginProvider {
	if r.Provider != nil {
		return r.Provider
	}
	return githubOriginProvider{}
}
