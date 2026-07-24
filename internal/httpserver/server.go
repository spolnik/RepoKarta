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
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/web"
)

type RepositoryStore interface {
	ListRepositories(context.Context) ([]catalog.Repository, error)
}

type Config struct {
	Address        string
	RepositoryRoot string
	Version        string
}

type Server struct {
	config    Config
	server    *http.Server
	templates *template.Template
	store     RepositoryStore
}

type pageData struct {
	Version        string
	RepositoryRoot string
	Repositories   []catalog.Repository
}

func New(config Config, store RepositoryStore) (*Server, error) {
	templates, err := template.ParseFS(web.Files, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	dist, err := fs.Sub(web.Files, "dist")
	if err != nil {
		return nil, fmt.Errorf("load embedded frontend: %w", err)
	}

	server := &Server{
		config:    config,
		templates: templates,
		store:     store,
	}

	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.FileServer(http.FS(dist)))
	mux.HandleFunc("GET /", server.home)
	mux.HandleFunc("GET /repositories", server.repositoryList)
	mux.HandleFunc("GET /healthz", server.health)

	server.server = &http.Server{
		Addr:              config.Address,
		Handler:           requestLog(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return server, nil
}

func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.server.Addr, err)
	}

	errChannel := make(chan error, 1)
	go func() {
		errChannel <- s.server.Serve(listener)
	}()

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
	repositories, err := s.store.ListRepositories(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}

	data := pageData{
		Version:        s.config.Version,
		RepositoryRoot: s.config.RepositoryRoot,
		Repositories:   repositories,
	}

	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(response, "index", data); err != nil {
		slog.Error("render home page", "error", err)
	}
}

func (s *Server) repositoryList(response http.ResponseWriter, request *http.Request) {
	repositories, err := s.store.ListRepositories(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}

	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(response, "repository-list", pageData{
		Repositories: repositories,
	}); err != nil {
		slog.Error("render repository list", "error", err)
	}
}

func (s *Server) health(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]string{
		"status":  "ok",
		"version": s.config.Version,
	})
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
