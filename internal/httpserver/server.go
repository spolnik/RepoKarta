package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/search"
	"github.com/spolnik/RepoKarta/internal/source"
	"github.com/spolnik/RepoKarta/web"
)

const (
	maximumSourceLines = 500
	eventPollInterval  = time.Second
)

// CatalogueRefresher manually rediscovers and queues repositories.
type CatalogueRefresher interface {
	Refresh(context.Context) error
}

// ConversationService supplies provider status and streamed read-only turns.
type ConversationService interface {
	Statuses(context.Context) []agent.Status
	Send(context.Context, agent.TurnRequest, func(agent.Event) error) error
}

// Config controls the local HTTP server.
type Config struct {
	Address        string
	RepositoryRoot string
	Version        string
	OpenBrowser    bool
	MCPHandler     http.Handler
	Conversations  ConversationService
}

// Server hosts RepoKarta's loopback interface.
type Server struct {
	config       Config
	server       *http.Server
	templates    *template.Template
	intelligence *codeintel.Service
	refresher    CatalogueRefresher
	agents       ConversationService
}

type pageData struct {
	Version        string
	RepositoryRoot string
	Repositories   []catalog.Repository
	ReadyCount     int
	PendingCount   int
	ErrorCount     int
	ActivePage     string
	ChatEnabled    bool
	Search         searchData
}

type searchData struct {
	Query           search.Query
	Performed       bool
	Error           string
	Duration        string
	MatchCount      int
	FileCount       int
	EstimatedFiles  int
	ReturnedFiles   int
	Limit           int
	TotalFilesExact bool
	FilesSkipped    int
	ShardsSkipped   int
	Warnings        []search.Warning
	Truncated       bool
	Matches         []searchMatchView
}

type searchMatchView struct {
	RepositoryID int64
	Repository   string
	Revision     string
	Path         string
	Language     string
	FocusLine    int
	Lines        []search.LineMatch
}

type sourcePageData struct {
	Version       string
	File          source.File
	RemoteURL     string
	Citation      string
	PreviousStart int
	PreviousEnd   int
	NextStart     int
	NextEnd       int
	FocusStart    int
	FocusEnd      int
}

// New builds the local HTTP server and parses embedded templates.
func New(config Config, intelligence *codeintel.Service, refresher CatalogueRefresher) (*Server, error) {
	functions := template.FuncMap{
		"formatTime":     formatTime,
		"highlightLine":  highlightLine,
		"fragmentRanges": fragmentRanges,
		"nextLine":       func(line int) int { return line + 1 },
		"previousLine":   func(line int) int { return max(1, line-1) },
		"sourceWindowStart": func(line int) int {
			start, _ := codeintel.SourceWindow(line, line)
			return start
		},
		"sourceWindowEnd": func(line int) int {
			_, end := codeintel.SourceWindow(line, line)
			return end
		},
		"lineFocused": func(line, start, end int) bool {
			return start > 0 && line >= start && line <= end
		},
		"shortCommit": shortCommit,
		"statusLabel": statusLabel,
	}
	templates, err := template.New("repokarta").Funcs(functions).ParseFS(web.Files, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	dist, err := fs.Sub(web.Files, "dist")
	if err != nil {
		return nil, fmt.Errorf("load embedded frontend: %w", err)
	}

	server := &Server{
		config:       config,
		templates:    templates,
		intelligence: intelligence,
		refresher:    refresher,
		agents:       config.Conversations,
	}

	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.FileServer(http.FS(dist)))
	mux.HandleFunc("GET /{$}", server.home)
	mux.HandleFunc("GET /repositories", server.repositoryList)
	mux.HandleFunc("POST /repositories/refresh", server.refreshRepositories)
	mux.HandleFunc("GET /search", server.search)
	mux.HandleFunc("GET /source/{repositoryID}", server.source)
	mux.HandleFunc("GET /api/search", server.apiSearch)
	mux.HandleFunc("GET /api/repositories", server.apiRepositories)
	mux.HandleFunc("GET /api/file/{repository}", server.apiFile)
	mux.HandleFunc("GET /api/tree/{repository}", server.apiTree)
	mux.HandleFunc("GET /events", server.events)
	mux.HandleFunc("GET /healthz", server.health)
	if config.MCPHandler != nil {
		mux.Handle("/mcp", config.MCPHandler)
	}
	if config.Conversations != nil {
		mux.HandleFunc("GET /chat", server.chatPage)
		mux.HandleFunc("GET /api/providers", server.providerStatuses)
		mux.HandleFunc("POST /api/chat", server.chat)
	}

	server.server = &http.Server{
		Addr:              config.Address,
		Handler:           requestLog(validateLocalRequest(config.Address, mux)),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return server, nil
}

func (s *Server) providerStatuses(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]any{
		"providers": s.agents.Statuses(request.Context()),
	})
}

func (s *Server) chat(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var turn agent.TurnRequest
	if err := json.NewDecoder(request.Body).Decode(&turn); err != nil {
		http.Error(response, "Invalid conversation request", http.StatusBadRequest)
		return
	}
	turn.Message = strings.TrimSpace(turn.Message)
	turn.Provider = strings.TrimSpace(turn.Provider)
	turn.Model = strings.TrimSpace(turn.Model)
	turn.ConversationID = strings.TrimSpace(turn.ConversationID)
	if turn.Message == "" || (turn.ConversationID == "" && turn.Provider == "") {
		http.Error(response, "Provider and message are required", http.StatusBadRequest)
		return
	}

	flusher, ok := response.(http.Flusher)
	if !ok {
		http.Error(response, "Streaming is not supported", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Accel-Buffering", "no")
	encoder := json.NewEncoder(response)
	emit := func(event agent.Event) error {
		if err := encoder.Encode(event); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if err := s.agents.Send(request.Context(), turn, emit); err != nil {
		slog.Warn("conversation turn failed", "provider", turn.Provider, "error", err)
		_ = emit(agent.Event{Type: agent.EventError, ConversationID: turn.ConversationID, Text: err.Error()})
	}
}

func (s *Server) apiSearch(response http.ResponseWriter, request *http.Request) {
	limit, err := apiSearchLimit(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	result, err := s.intelligence.Search(request.Context(), codeintel.SearchRequest{
		Query:      request.URL.Query().Get("q"),
		Repository: request.URL.Query().Get("repo"),
		Language:   request.URL.Query().Get("lang"),
		Path:       request.URL.Query().Get("path"),
		File:       request.URL.Query().Get("file"),
		Mode:       request.URL.Query().Get("mode"),
		Limit:      limit,
	})
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiRepositories(response http.ResponseWriter, request *http.Request) {
	repositories, err := s.intelligence.Repositories(request.Context())
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, repositories)
}

func (s *Server) apiFile(response http.ResponseWriter, request *http.Request) {
	start, end := parseLineRange(request.URL.Query().Get("lines"))
	file, err := s.intelligence.GetFile(request.Context(), codeintel.FileRequest{
		Repository: request.PathValue("repository"),
		Revision:   request.URL.Query().Get("rev"),
		Path:       request.URL.Query().Get("path"),
		StartLine:  start,
		EndLine:    end,
	})
	if err != nil {
		writeCodeIntelligenceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, file)
}

func (s *Server) apiTree(response http.ResponseWriter, request *http.Request) {
	tree, err := s.intelligence.ListTree(request.Context(), codeintel.TreeRequest{
		Repository: request.PathValue("repository"),
		Revision:   request.URL.Query().Get("rev"),
		Path:       request.URL.Query().Get("path"),
	})
	if err != nil {
		writeCodeIntelligenceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, tree)
}

// Run listens until the context is cancelled and then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	s.server.BaseContext = func(net.Listener) context.Context {
		return ctx
	}
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.server.Addr, err)
	}

	errChannel := make(chan error, 1)
	go func() {
		errChannel <- s.server.Serve(listener)
	}()
	if s.config.OpenBrowser {
		go func() {
			localURL := "http://" + listener.Addr().String()
			if err := openBrowser(localURL); err != nil {
				slog.Warn("open browser", "url", localURL, "error", err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		return nil
	case err := <-errChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) home(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	s.render(response, "index", data)
}

func (s *Server) chatPage(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	data.ActivePage = "chat"
	s.render(response, "chat", data)
}

func (s *Server) repositoryList(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	s.render(response, "repository-list", data)
}

func (s *Server) refreshRepositories(response http.ResponseWriter, request *http.Request) {
	if err := s.refresher.Refresh(request.Context()); err != nil {
		slog.Error("refresh repository catalogue", "error", err)
		http.Error(response, "Could not refresh repositories", http.StatusInternalServerError)
		return
	}
	s.repositoryList(response, request)
}

func (s *Server) search(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}

	query := search.Query{
		Text:       strings.TrimSpace(request.URL.Query().Get("q")),
		Repository: strings.TrimSpace(request.URL.Query().Get("repo")),
		Language:   strings.TrimSpace(request.URL.Query().Get("lang")),
		Path:       strings.TrimSpace(request.URL.Query().Get("path")),
		File:       strings.TrimSpace(request.URL.Query().Get("file")),
		Mode:       strings.TrimSpace(request.URL.Query().Get("mode")),
		Limit:      parseSearchLimit(request.URL.Query().Get("limit")),
	}
	data.Search.Query = query
	data.Search.Performed = query.Text != ""
	if data.Search.Performed {
		result, searchError := s.intelligence.Search(request.Context(), codeintel.SearchRequest{
			Query:      query.Text,
			Repository: query.Repository,
			Language:   query.Language,
			Path:       query.Path,
			File:       query.File,
			Mode:       query.Mode,
			Limit:      query.Limit,
		})
		if searchError != nil {
			data.Search.Error = searchError.Error()
		} else {
			data.Search.Duration = formatMilliseconds(result.DurationMS)
			data.Search.MatchCount = result.MatchCount
			data.Search.FileCount = result.MatchingFiles
			data.Search.EstimatedFiles = result.EstimatedTotalFiles
			data.Search.ReturnedFiles = result.ReturnedFiles
			data.Search.Limit = result.Limit
			data.Search.TotalFilesExact = result.TotalFilesExact
			data.Search.FilesSkipped = result.FilesSkipped
			data.Search.ShardsSkipped = result.ShardsSkipped
			data.Search.Warnings = result.Warnings
			data.Search.Truncated = result.Truncated
			data.Search.Matches = resolveSearchViews(result.Matches, data.Repositories)
		}
	}

	if request.Header.Get("HX-Request") == "true" {
		s.render(response, "search-results", data)
		return
	}
	s.render(response, "index", data)
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

	data := sourcePageData{
		Version:       s.config.Version,
		File:          file,
		RemoteURL:     remoteFileURL(repository.OriginURL, file.Revision, file.Path, citationStart, citationEnd),
		Citation:      fmt.Sprintf("%s@%s:%s#L%d-L%d", repository.Name, shortCommit(file.Revision), file.Path, citationStart, citationEnd),
		PreviousStart: previousStart,
		PreviousEnd:   previousEnd,
		NextStart:     nextStart,
		NextEnd:       nextEnd,
		FocusStart:    focusStart,
		FocusEnd:      focusEnd,
	}
	s.render(response, "source", data)
}

func (s *Server) events(response http.ResponseWriter, request *http.Request) {
	flusher, ok := response.(http.Flusher)
	if !ok {
		http.Error(response, "Streaming is not supported", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("X-Accel-Buffering", "no")

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	signature := ""
	for {
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
			repositories, err := s.intelligence.CatalogRepositories(request.Context())
			if err != nil {
				return
			}
			nextSignature := repositorySignature(repositories)
			if nextSignature != signature {
				signature = nextSignature
				fmt.Fprint(response, "event: repositories\ndata: updated\n\n")
				flusher.Flush()
			}
		case <-heartbeat.C:
			fmt.Fprint(response, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) health(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]string{
		"status":  "ok",
		"version": s.config.Version,
	})
}

func (s *Server) pageData(ctx context.Context) (pageData, error) {
	repositories, err := s.intelligence.CatalogRepositories(ctx)
	if err != nil {
		return pageData{}, err
	}
	data := pageData{
		Version:        s.config.Version,
		RepositoryRoot: s.config.RepositoryRoot,
		Repositories:   repositories,
		ActivePage:     "search",
		ChatEnabled:    s.agents != nil,
		Search: searchData{
			Query: search.Query{Limit: codeintel.DefaultSearchLimit},
		},
	}
	for _, repository := range repositories {
		switch repository.IndexState {
		case "ready":
			data.ReadyCount++
		case "error":
			data.ErrorCount++
		default:
			data.PendingCount++
		}
	}
	return data, nil
}

func (s *Server) render(response http.ResponseWriter, name string, data any) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(response, name, data); err != nil {
		slog.Error("render template", "template", name, "error", err)
	}
}

func resolveSearchViews(matches []codeintel.SearchMatch, repositories []catalog.Repository) []searchMatchView {
	views := make([]searchMatchView, 0, len(matches))
	for _, match := range matches {
		view := searchMatchView{
			Repository: match.Repository,
			Revision:   match.Revision,
			Path:       match.Path,
			Language:   match.Language,
			Lines:      make([]search.LineMatch, 0, len(match.Lines)),
		}
		if len(match.Lines) > 0 {
			view.FocusLine = match.Lines[0].Number
		}
		for _, line := range match.Lines {
			view.Lines = append(view.Lines, search.LineMatch{
				Number:    line.Number,
				Text:      line.Text,
				Before:    line.Before,
				After:     line.After,
				Fragments: line.Fragments,
			})
		}
		for _, repository := range repositories {
			if repository.IndexedCommit != match.Revision && repository.HeadCommit != match.Revision {
				continue
			}
			normalizedSearchName := strings.ReplaceAll(match.Repository, "\\", "/")
			if normalizedSearchName == repository.Name ||
				strings.HasSuffix(strings.ToLower(normalizedSearchName), "/"+strings.ToLower(repository.Name)) {
				view.RepositoryID = repository.ID
				view.Repository = repository.Name
				break
			}
		}
		views = append(views, view)
	}
	return views
}

func parseSearchLimit(value string) int {
	limit, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || limit <= 0 {
		return codeintel.DefaultSearchLimit
	}
	return min(limit, codeintel.MaximumSearchLimit)
}

func apiSearchLimit(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return codeintel.DefaultSearchLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 || limit > codeintel.MaximumSearchLimit {
		return 0, fmt.Errorf("limit must be an integer from 1 to %d", codeintel.MaximumSearchLimit)
	}
	return limit, nil
}

func writeCodeIntelligenceError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, source.ErrUnsafePath), errors.Is(err, source.ErrUnknownRevision):
		writeAPIError(response, http.StatusBadRequest, err)
	case errors.Is(err, source.ErrUnsupportedFile):
		writeAPIError(response, http.StatusUnsupportedMediaType, err)
	default:
		writeAPIError(response, http.StatusNotFound, err)
	}
}

func writeAPIError(response http.ResponseWriter, status int, err error) {
	writeJSON(response, status, map[string]any{
		"error": map[string]string{
			"message": err.Error(),
		},
	})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		slog.Warn("encode JSON response", "error", err)
	}
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

func formatDuration(duration time.Duration) string {
	if duration < time.Millisecond {
		return "<1 ms"
	}
	return fmt.Sprintf("%d ms", duration.Milliseconds())
}

func formatMilliseconds(milliseconds float64) string {
	if milliseconds < 1 {
		return "<1 ms"
	}
	return fmt.Sprintf("%.0f ms", milliseconds)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "Not yet"
	}
	return value.Local().Format("Jan 2, 15:04")
}

func shortCommit(commit string) string {
	if len(commit) <= 8 {
		return commit
	}
	return commit[:8]
}

func statusLabel(state string) string {
	switch state {
	case "ready":
		return "Indexed"
	case "indexing":
		return "Indexing"
	case "error":
		return "Needs attention"
	default:
		return "Queued"
	}
}

func repositorySignature(repositories []catalog.Repository) string {
	var builder strings.Builder
	for _, repository := range repositories {
		fmt.Fprintf(&builder, "%d:%s:%s:%s;", repository.ID, repository.HeadCommit, repository.IndexState, repository.IndexError)
	}
	return builder.String()
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

func validateLocalRequest(address string, next http.Handler) http.Handler {
	_, configuredPort, _ := net.SplitHostPort(address)
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		host, port, err := net.SplitHostPort(request.Host)
		if err != nil {
			host = request.Host
			port = ""
		}
		if !isLoopbackHost(host) || (configuredPort != "" && port != "" && port != configuredPort) {
			http.Error(response, "Invalid Host", http.StatusForbidden)
			return
		}
		if origin := request.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || !isLoopbackHost(parsed.Hostname()) ||
				(configuredPort != "" && parsed.Port() != "" && parsed.Port() != configuredPort) {
				http.Error(response, "Invalid Origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(response, request)
	})
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func openBrowser(localURL string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", localURL)
	case "darwin":
		command = exec.Command("open", localURL)
	default:
		command = exec.Command("xdg-open", localURL)
	}
	return command.Start()
}

func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(response, request)
		slog.Debug(
			"HTTP request",
			"method", request.Method,
			"path", request.URL.Path,
			"duration", time.Since(started),
		)
	})
}
