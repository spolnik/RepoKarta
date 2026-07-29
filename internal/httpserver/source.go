package httpserver

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/search"
	"github.com/spolnik/RepoKarta/internal/source"
	"github.com/spolnik/RepoKarta/internal/sourceintelligence"
)

func (s *Server) project(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := strconv.ParseInt(request.PathValue("repositoryID"), 10, 64)
	if err != nil || repositoryID <= 0 {
		http.Error(response, "Invalid repository", http.StatusBadRequest)
		return
	}
	offset, err := nonNegativeInteger(request.URL.Query().Get("offset"), "offset")
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	repository, err := s.intelligence.RepositoryByID(request.Context(), repositoryID)
	if err != nil {
		http.NotFound(response, request)
		return
	}
	tree, err := s.intelligence.ListTree(request.Context(), codeintel.TreeRequest{
		RepositoryID: repositoryID,
		Revision:     request.URL.Query().Get("rev"),
		Path:         request.URL.Query().Get("path"),
		Offset:       offset,
	})
	if err != nil {
		switch {
		case errors.Is(err, source.ErrUnsafePath), errors.Is(err, source.ErrUnknownRevision):
			http.Error(response, "Invalid project path or revision", http.StatusBadRequest)
		default:
			http.Error(response, "Could not open project directory", http.StatusNotFound)
		}
		return
	}
	base, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load project", http.StatusInternalServerError)
		return
	}
	base.ActivePage = "project"
	previousURL := ""
	if tree.Offset > 0 {
		previousURL = projectDirectoryURL(repositoryID, tree.Revision, tree.Path, max(0, tree.Offset-tree.Limit))
	}
	nextURL := ""
	if tree.NextOffset > 0 {
		nextURL = projectDirectoryURL(repositoryID, tree.Revision, tree.Path, tree.NextOffset)
	}
	firstEntry, lastEntry := 0, 0
	if len(tree.Entries) > 0 {
		firstEntry = tree.Offset + 1
		lastEntry = tree.Offset + len(tree.Entries)
	}
	s.render(response, "project", projectPageData{
		pageData:    base,
		Repository:  repository,
		Revision:    tree.Revision,
		Path:        tree.Path,
		Breadcrumbs: projectBreadcrumbs(repositoryID, repository.Name, tree.Revision, tree.Path, false),
		Entries:     projectEntryViews(repositoryID, tree.Revision, tree.Entries, ""),
		PreviousURL: previousURL,
		NextURL:     nextURL,
		FirstEntry:  firstEntry,
		LastEntry:   lastEntry,
	})
}

func (s *Server) source(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := strconv.ParseInt(request.PathValue("repositoryID"), 10, 64)
	if err != nil || repositoryID <= 0 {
		http.NotFound(response, request)
		return
	}
	repository, err := s.intelligence.RepositoryByID(request.Context(), repositoryID)
	if err != nil {
		http.NotFound(response, request)
		return
	}

	startLine, endLine := parseLineRange(request.URL.Query().Get("lines"))
	focusStart, focusEnd := parseFocusRange(request.URL.Query().Get("focus"))
	if focusStart > 0 && (focusStart < startLine || focusEnd > endLine) {
		startLine, endLine = codeintel.SourceWindow(focusStart, focusEnd)
	}
	file, err := source.OpenFile(
		request.Context(),
		repository,
		request.URL.Query().Get("rev"),
		request.URL.Query().Get("path"),
		startLine,
		endLine,
	)
	if err != nil {
		switch {
		case errors.Is(err, source.ErrUnsafePath), errors.Is(err, source.ErrUnknownRevision):
			http.Error(response, "Invalid source citation", http.StatusBadRequest)
		case errors.Is(err, source.ErrUnsupportedFile):
			http.Error(response, "This file cannot be displayed safely", http.StatusUnsupportedMediaType)
		default:
			slog.Error("open source file", "repository", repository.Name, "error", err)
			http.Error(response, "Could not open source file", http.StatusNotFound)
		}
		return
	}

	previousStart, previousEnd := 0, 0
	if file.StartLine > 1 {
		previousEnd = file.StartLine - 1
		previousStart = max(1, previousEnd-(maximumSourceLines-1))
	}
	nextStart, nextEnd := 0, 0
	if file.EndLine < file.TotalLines {
		nextStart = file.EndLine + 1
		nextEnd = min(file.TotalLines, nextStart+(maximumSourceLines-1))
	}
	if focusStart < file.StartLine || focusStart > file.EndLine {
		focusStart, focusEnd = 0, 0
	} else {
		focusEnd = min(focusEnd, file.EndLine)
	}
	citationStart, citationEnd := file.StartLine, file.EndLine
	if focusStart > 0 {
		citationStart, citationEnd = focusStart, focusEnd
	}
	directory := path.Dir(file.Path)
	if directory == "." {
		directory = ""
	}
	tree, treeErr := s.intelligence.ListTree(request.Context(), codeintel.TreeRequest{
		RepositoryID: repositoryID,
		Revision:     file.Revision,
		Path:         directory,
	})
	var treeEntries []projectEntryView
	if treeErr == nil {
		treeEntries = projectEntryViews(repositoryID, file.Revision, tree.Entries, file.Path)
	}

	data := sourcePageData{
		Version:       s.config.Version,
		ChatEnabled:   s.agents != nil,
		File:          file,
		ProjectURL:    projectDirectoryURL(repositoryID, file.Revision, directory, 0),
		Breadcrumbs:   projectBreadcrumbs(repositoryID, repository.Name, file.Revision, file.Path, true),
		TreeEntries:   treeEntries,
		RemoteURL:     remoteFileURL(repository.OriginURL, file.Revision, file.Path, citationStart, citationEnd),
		Citation:      fmt.Sprintf("%s@%s:%s#L%d-L%d", repository.Name, shortCommit(file.Revision), file.Path, citationStart, citationEnd),
		PreviousStart: previousStart,
		PreviousEnd:   previousEnd,
		NextStart:     nextStart,
		NextEnd:       nextEnd,
		FocusStart:    focusStart,
		FocusEnd:      focusEnd,
		Intelligence: s.sourceIntelligence(
			request.Context(),
			repositoryID,
			file.Revision,
			file.Path,
			file.StartLine,
			file.EndLine,
		),
	}
	s.render(response, "source", data)
}

func (s *Server) sourceIntelligence(
	ctx context.Context,
	repositoryID int64,
	revision, filePath string,
	startLine, endLine int,
) sourceIntelligenceView {
	return sourceintelligence.Build(ctx, s.maps, s.dependencies, sourceintelligence.Request{
		RepositoryID: repositoryID,
		Revision:     revision,
		FilePath:     filePath,
		StartLine:    startLine,
		EndLine:      endLine,
	})
}

func routeMatchesCallerEvidence(routeLabel string, evidence []graph.Evidence) bool {
	return sourceintelligence.RouteMatchesCallerEvidence(routeLabel, evidence)
}

func parseLineRange(value string) (int, int) {
	parts := strings.SplitN(value, "-", 2)
	start, _ := strconv.Atoi(parts[0])
	end := start + 199
	if len(parts) == 2 {
		if parsed, err := strconv.Atoi(parts[1]); err == nil {
			end = parsed
		}
	}
	if start <= 0 {
		start = 1
	}
	if end < start {
		end = start
	}
	if end-start+1 > maximumSourceLines {
		end = start + maximumSourceLines - 1
	}
	return start, end
}

func parseFocusRange(value string) (int, int) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0
	}
	parts := strings.SplitN(value, "-", 2)
	start, err := strconv.Atoi(parts[0])
	if err != nil || start <= 0 {
		return 0, 0
	}
	end := start
	if len(parts) == 2 {
		parsed, err := strconv.Atoi(parts[1])
		if err != nil || parsed < start {
			return 0, 0
		}
		end = parsed
	}
	if end-start+1 > maximumSourceLines {
		end = start + maximumSourceLines - 1
	}
	return start, end
}

func highlightLine(line search.LineMatch) template.HTML {
	if len(line.Fragments) == 0 {
		return template.HTML(template.HTMLEscapeString(line.Text))
	}
	fragments := append([]search.Fragment(nil), line.Fragments...)
	sort.Slice(fragments, func(left, right int) bool {
		return fragments[left].Start < fragments[right].Start
	})

	var output strings.Builder
	position := 0
	for _, fragment := range fragments {
		if fragment.Start < position || fragment.Start < 0 || fragment.End > len(line.Text) || fragment.End < fragment.Start {
			continue
		}
		output.WriteString(template.HTMLEscapeString(line.Text[position:fragment.Start]))
		output.WriteString(`<mark class="search-highlight">`)
		output.WriteString(template.HTMLEscapeString(line.Text[fragment.Start:fragment.End]))
		output.WriteString("</mark>")
		position = fragment.End
	}
	output.WriteString(template.HTMLEscapeString(line.Text[position:]))
	return template.HTML(output.String())
}

func fragmentRanges(line search.LineMatch) string {
	values := make([]string, 0, len(line.Fragments))
	for _, fragment := range line.Fragments {
		if fragment.Start < 0 || fragment.End <= fragment.Start || fragment.End > len(line.Text) {
			continue
		}
		start := len(utf16.Encode([]rune(line.Text[:fragment.Start])))
		end := start + len(utf16.Encode([]rune(line.Text[fragment.Start:fragment.End])))
		values = append(values, strconv.Itoa(start)+":"+strconv.Itoa(end))
	}
	return strings.Join(values, ",")
}

func projectDirectoryURL(repositoryID int64, revision, directory string, offset int) string {
	values := url.Values{}
	if revision != "" {
		values.Set("rev", revision)
	}
	if directory != "" {
		values.Set("path", directory)
	}
	if offset > 0 {
		values.Set("offset", strconv.Itoa(offset))
	}
	target := "/projects/" + strconv.FormatInt(repositoryID, 10)
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	return target
}

func projectSourceURL(repositoryID int64, revision, filePath string) string {
	values := url.Values{
		"rev":   {revision},
		"path":  {filePath},
		"lines": {"1-200"},
	}
	return "/source/" + strconv.FormatInt(repositoryID, 10) + "?" + values.Encode()
}

func projectBreadcrumbs(
	repositoryID int64,
	repositoryName, revision, currentPath string,
	includeFile bool,
) []projectBreadcrumbView {
	breadcrumbs := []projectBreadcrumbView{{
		Label: repositoryName,
		URL:   projectDirectoryURL(repositoryID, revision, "", 0),
	}}
	parts := strings.Split(strings.Trim(currentPath, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		return breadcrumbs
	}
	directoryParts := parts
	if includeFile {
		directoryParts = parts[:len(parts)-1]
	}
	for index, part := range directoryParts {
		directory := strings.Join(parts[:index+1], "/")
		breadcrumbs = append(breadcrumbs, projectBreadcrumbView{
			Label: part,
			URL:   projectDirectoryURL(repositoryID, revision, directory, 0),
		})
	}
	if includeFile {
		breadcrumbs = append(breadcrumbs, projectBreadcrumbView{Label: parts[len(parts)-1]})
	}
	return breadcrumbs
}

func projectEntryViews(
	repositoryID int64,
	revision string,
	entries []codeintel.TreeEntry,
	activePath string,
) []projectEntryView {
	output := make([]projectEntryView, 0, len(entries))
	for _, entry := range entries {
		target := projectSourceURL(repositoryID, revision, entry.Path)
		if entry.Type == "directory" {
			target = projectDirectoryURL(repositoryID, revision, entry.Path, 0)
		}
		output = append(output, projectEntryView{
			Name:   path.Base(entry.Path),
			Path:   entry.Path,
			Type:   entry.Type,
			URL:    target,
			Active: entry.Path == activePath,
		})
	}
	return output
}

func remoteFileURL(origin, revision, filePath string, startLine, endLine int) string {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return ""
	}
	if strings.HasPrefix(origin, "git@") {
		parts := strings.SplitN(strings.TrimPrefix(origin, "git@"), ":", 2)
		if len(parts) == 2 {
			origin = "https://" + parts[0] + "/" + parts[1]
		}
	}
	origin = strings.TrimSuffix(origin, ".git")
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	switch strings.ToLower(parsed.Host) {
	case "github.com", "gitlab.com":
		parsed.Path = path.Join(parsed.Path, "blob", revision, filePath)
		parsed.Fragment = fmt.Sprintf("L%d-L%d", startLine, endLine)
	case "bitbucket.org":
		parsed.Path = path.Join(parsed.Path, "src", revision, filePath)
		parsed.Fragment = fmt.Sprintf("lines-%d:%d", startLine, endLine)
	default:
		return origin
	}
	return parsed.String()
}
