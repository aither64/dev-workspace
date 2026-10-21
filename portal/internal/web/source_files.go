package web

import (
	"context"
	"errors"
	"io"
	"mime"
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
// a transcript never grants access: the file API checks Git eligibility and
// confinement, plus session identity and repository registration.
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
	query := url.Values{}
	target := url.URL{}
	if repository.ValidWorkspaceSourcePath(relative) {
		target.Path = "/workspace-files"
		query.Set("path", relative)
	} else {
		if len(parts) < 3 || !session.ValidSlug(parts[1]) {
			return "", false
		}
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
		target.Path = "/files/" + parts[1]
	}
	target.RawQuery = query.Encode()
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
					link.SetAttributeString("target", []byte("_blank"))
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

func workspaceSourceTarget(rawQuery string) (string, bool) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil || len(query) != 1 || len(query["path"]) != 1 {
		return "", false
	}
	path := query.Get("path")
	return path, repository.ValidWorkspaceSourcePath(path)
}

func (s *Server) workspaceSourcePage(w http.ResponseWriter, r *http.Request) {
	data := pageData{SourceFileAPI: "/api/workspace/file"}
	if _, ok := workspaceSourceTarget(r.URL.RawQuery); !ok {
		data.Error = "This workspace file link is invalid."
		s.renderStatus(w, http.StatusNotFound, "source-file", data)
		return
	}
	s.render(w, "source-file", data)
}

func (s *Server) workspaceSourceFile(w http.ResponseWriter, r *http.Request) {
	path, ok := workspaceSourceTarget(r.URL.RawQuery)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "This workspace file link is invalid."})
		return
	}
	download := strings.HasSuffix(r.URL.Path, "/download")
	result, err := s.reviews().reader.WorkspaceSourceContent(r.Context(), path, download)
	if s.sourceDownloadError(w, err) {
		return
	}
	if err != nil {
		s.config.Logger.Printf("workspace source file %s: %v", path, err)
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "This file is unavailable. Check that it exists in the workspace and is tracked or not ignored by Git."})
		return
	}
	s.serveSourceContent(w, result, download)
}

func (s *Server) sourceFile(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	query, ok := sourceTarget(r.URL.RawQuery)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "This file link is invalid."})
		return
	}
	download := strings.HasSuffix(r.URL.Path, "/download")
	var result repository.SourceFile
	if artifact := query.Get("artifact"); artifact != "" {
		var err error
		result, err = s.trackingSourceContent(r.Context(), summary, artifact, download)
		if s.sourceDownloadError(w, err) {
			return
		}
		if err != nil {
			s.config.Logger.Printf("session source file %s/%s: %v", summary.Slug, artifact, err)
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
		result, err = s.reviews().reader.SourceContent(r.Context(), summary.Slug, item, summary.Archived, query.Get("path"), download)
		if s.sourceDownloadError(w, err) {
			return
		}
		if err != nil {
			s.config.Logger.Printf("source file %s/%s: %v", summary.Slug, item.Name, err)
			s.sourceUnavailable(w)
			return
		}
	}
	s.serveSourceContent(w, result, download)
}

// The same confined opener serves the ordinary viewer and all legacy adapters.
func (s *Server) trackingSourceContent(ctx context.Context, summary *session.Summary, path string, download bool) (repository.SourceFile, error) {
	result, err := s.reviews().reader.TrackingSourceContent(ctx, summary, path, download)
	if err != nil || download {
		return result, err
	}
	if strings.EqualFold(filepath.Ext(result.Path), ".md") && !result.Content.Binary && !result.Content.Limited {
		result.Preview = &repository.SourcePreview{Kind: "markdown", HTML: string(s.renderTextMarkdown(result.Content.Text))}
	} else if _, image := artifactImageType(result.Path); image {
		file, info, canonical, openErr := session.OpenTrackingFile(summary, result.Path)
		if openErr != nil {
			return result, openErr
		}
		defer file.Close()
		if info.Size() <= repository.MaxSourceDownloadBytes {
			if _, matches := sniffArtifactImage(file, canonical); matches {
				result.Preview = &repository.SourcePreview{Kind: "image", URL: artifactImageURL(summary.Slug, canonical)}
			}
		}
	}
	return result, ctx.Err()
}

func artifactImageURL(slug, path string) string {
	return "/artifact-previews/" + url.PathEscape(slug) + "/" + strings.ReplaceAll(url.PathEscape(path), "%2F", "/")
}

func artifactImageType(path string) (string, bool) {
	types := map[string]string{".gif": "image/gif", ".jpeg": "image/jpeg", ".jpg": "image/jpeg", ".png": "image/png", ".webp": "image/webp"}
	value, ok := types[strings.ToLower(filepath.Ext(path))]
	return value, ok
}

func sniffArtifactImage(file io.Reader, path string) (string, bool) {
	expected, allowed := artifactImageType(path)
	if !allowed {
		return "", false
	}
	var head [512]byte
	count, err := file.Read(head[:])
	if err != nil && err != io.EOF {
		return "", false
	}
	return expected, count > 0 && http.DetectContentType(head[:count]) == expected
}

func (s *Server) trackingFiles(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	files, err := s.reviews().reader.TrackingFiles(r.Context(), summary)
	if err != nil {
		message := "Session files could not be listed. Refresh to try again."
		if errors.Is(err, repository.ErrReviewLimit) {
			message = "The file list is too large to display (limit: 5,000 files or 4 MiB)."
		}
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": message})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) sourceUnavailable(w http.ResponseWriter) {
	s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "This file is unavailable. Check that it exists in the session or registered repository."})
}

// Reject malformed legacy paths before ServeMux can clean or redirect them.
func (s *Server) invalidSourceRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, s.config.Workspace+"/") && filepath.Clean(r.URL.Path) != r.URL.Path
}

func (s *Server) sourceDownloadError(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, repository.ErrSourceDownloadLimit) {
		return false
	}
	s.writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "This file exceeds the 10 MiB download limit."})
	return true
}

func (s *Server) serveSourceContent(w http.ResponseWriter, file repository.SourceFile, download bool) {
	if !download {
		s.writeJSON(w, http.StatusOK, file)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(file.Path)}))
	w.Header().Set("Content-Length", strconv.Itoa(len(file.Content.Text)))
	_, _ = io.WriteString(w, file.Content.Text)
}
