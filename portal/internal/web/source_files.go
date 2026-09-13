package web

import (
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/util"
)

var sourceLineSuffix = regexp.MustCompile(`:([0-9]+)(?::([0-9]+))?$`)
var sourceLineFragment = regexp.MustCompile(`^L([1-9][0-9]*)$`)

func sourceLine(value string) bool {
	number, err := strconv.ParseUint(value, 10, 64)
	return err == nil && number > 0 && number <= 9007199254740991
}

// sourceLink maps workspace path structure without filesystem reads. Rendering
// a transcript never grants access: the file API checks the current session,
// registration, index membership and confinement when the link is opened.
func (s *Server) sourceLink(input *url.URL) (string, bool) {
	if input.Scheme != "" || input.Host != "" || input.User != nil || input.RawQuery != "" || input.ForceQuery {
		return "", false
	}
	escaped := input.EscapedPath()
	line := ""
	if suffix := sourceLineSuffix.FindStringSubmatchIndex(escaped); suffix != nil {
		line = escaped[suffix[2]:suffix[3]]
		if !sourceLine(line) || (suffix[4] >= 0 && !sourceLine(escaped[suffix[4]:suffix[5]])) {
			return "", false
		}
		escaped = escaped[:suffix[0]]
	}
	if input.Fragment != "" {
		fragment := sourceLineFragment.FindStringSubmatch(input.Fragment)
		if fragment == nil || !sourceLine(fragment[1]) || (line != "" && line != fragment[1]) {
			return "", false
		}
		line = fragment[1]
	}
	path, err := url.PathUnescape(escaped)
	if err != nil {
		return "", false
	}
	relative, ok := strings.CutPrefix(path, s.config.Workspace+"/")
	if !ok {
		return "", false
	}
	parts := strings.SplitN(relative, "/", 4)
	if len(parts) < 3 || !session.ValidSlug(parts[1]) {
		return "", false
	}
	query := url.Values{}
	switch parts[0] {
	case "worktrees":
		if len(parts) != 4 || !session.ValidSlug(parts[2]) || !repository.ValidSourcePath(parts[3]) {
			return "", false
		}
		query.Set("repository", repository.ReviewID(parts[2]))
		query.Set("path", parts[3])
	case "work", "archive":
		artifact, ok := session.NormalizeArtifactPath(strings.Join(parts[2:], "/"))
		if !ok {
			return "", false
		}
		query.Set("artifact", artifact)
	default:
		return "", false
	}
	target := url.URL{Path: "/files/" + parts[1], RawQuery: query.Encode()}
	if line != "" {
		number, _ := strconv.ParseUint(line, 10, 64)
		target.Fragment = "L" + strconv.FormatUint(number, 10)
	}
	return target.String(), true
}

func (s *Server) rewriteSourceLinks(document ast.Node) {
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if link, ok := node.(*ast.Link); ok && entering {
			input, err := url.Parse(string(util.URLEscape(link.Destination, true)))
			if err == nil {
				if target, ok := s.sourceLink(input); ok {
					link.Destination = []byte(target)
				}
			}
		}
		return ast.WalkContinue, nil
	})
}

func sourceTarget(rawQuery string) (url.Values, bool) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, false
	}
	for key, values := range query {
		if len(values) != 1 || (key != "repository" && key != "path" && key != "artifact") {
			return nil, false
		}
	}
	if len(query) == 1 {
		if artifact, ok := session.NormalizeArtifactPath(query.Get("artifact")); ok {
			query.Set("artifact", artifact)
			return query, true
		}
	}
	if len(query) == 2 && reviewToken(query.Get("repository")) && repository.ValidSourcePath(query.Get("path")) {
		return query, true
	}
	return nil, false
}

func (s *Server) sourceRedirect(w http.ResponseWriter, r *http.Request) {
	if target, ok := s.sourceLink(r.URL); ok {
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) sourcePage(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, "/files/")
	data := pageData{}
	summary, err := session.Find(s.config.Workspace, slug)
	if _, ok := sourceTarget(r.URL.RawQuery); err != nil || !ok {
		data.Error = "This file is unavailable. Check that the session and file link still exist."
		s.renderStatus(w, http.StatusNotFound, "source-file", data)
		return
	}
	data.Session = summary
	s.render(w, "source-file", data)
}

func (s *Server) sourceFile(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	query, ok := sourceTarget(r.URL.RawQuery)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "This file link is invalid."})
		return
	}
	var result repository.SourceFile
	if artifact := query.Get("artifact"); artifact != "" {
		// Authorize and inspect the file without reading it. ReadSourcePreview
		// enforces the display limit even if the artifact grows during the read.
		file, info, err := session.OpenArtifact(summary, artifact, math.MaxInt64)
		if err != nil {
			s.sourceUnavailable(w)
			return
		}
		defer file.Close()
		result = repository.SourceFile{Path: artifact, Source: repository.SourceArtifact}
		if summary.Archived {
			result.Source = repository.SourceArchivedArtifact
		}
		result.Content, err = repository.ReadSourcePreview(file, info.Size())
		if err != nil {
			s.sourceUnavailable(w)
			return
		}
	} else {
		item, ok := s.reviewRegistration(r.Context(), summary, query.Get("repository"))
		if !ok {
			s.sourceUnavailable(w)
			return
		}
		var err error
		result, err = s.reviews().reader.Source(r.Context(), summary.Slug, item, summary.Archived, query.Get("path"))
		if err != nil {
			s.config.Logger.Printf("source file %s/%s: %v", summary.Slug, item.Name, err)
			s.sourceUnavailable(w)
			return
		}
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) sourceUnavailable(w http.ResponseWriter) {
	s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "This file is unavailable. It must be a tracked file in a verified repository, or a published session artifact."})
}

// Reject malformed legacy paths before ServeMux can clean or redirect them.
func (s *Server) invalidSourceRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, s.config.Workspace+"/") && filepath.Clean(r.URL.Path) != r.URL.Path
}
