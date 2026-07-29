package httpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/spolnik/RepoKarta/internal/apicontract"
	"github.com/spolnik/RepoKarta/internal/telemetry"
)

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
	writeJSON(response, http.StatusOK, apicontract.HealthResponse{
		Status:  "ok",
		Version: s.config.Version,
	})
}

func (s *Server) apiTelemetryStatus(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.telemetry.Status())
}

// staticAssets serves the embedded frontend with a build-derived validator.
//
// Asset paths are unversioned (/assets/app.js, /assets/app.css) and embed.FS
// reports a zero modification time, so http.ServeContent emitted no ETag, no
// Last-Modified, and no Cache-Control. A browser was therefore free to keep
// serving a previous build's JavaScript and CSS against freshly rendered HTML
// after an upgrade, which presents as a badly broken page rather than as a
// caching problem. Revalidating on every load costs nothing over loopback.
func staticAssets(dist fs.FS) http.Handler {
	tag := buildETag(dist)
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if tag != "" {
			response.Header().Set("ETag", tag)
		}
		response.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(response, request)
	})
}

// buildETag hashes every embedded asset once at startup so all assets from one
// build share a validator. An empty result disables conditional requests rather
// than failing to serve.
func buildETag(dist fs.FS) string {
	digest := sha256.New()
	err := fs.WalkDir(dist, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, readErr := fs.ReadFile(dist, path)
		if readErr != nil {
			return readErr
		}
		fmt.Fprintf(digest, "%s:%d:", path, len(content))
		digest.Write(content)
		return nil
	})
	if err != nil {
		slog.Warn("compute asset etag", "error", err)
		return ""
	}
	return fmt.Sprintf("%q", hex.EncodeToString(digest.Sum(nil))[:32])
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		response.Header().Set(
			"Content-Security-Policy",
			"default-src 'self'; base-uri 'self'; frame-ancestors 'none'; object-src 'none'; "+
				"script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; "+
				"font-src 'self'; connect-src 'self'; form-action 'self'",
		)
		next.ServeHTTP(response, request)
	})
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
		tracker := &statusResponseWriter{ResponseWriter: response}
		next.ServeHTTP(tracker, request)
		status := tracker.status
		if status == 0 {
			status = http.StatusOK
		}
		slog.DebugContext(
			request.Context(),
			"HTTP request",
			"http.request.method", request.Method,
			"http.route", telemetry.RoutePattern(request.Context()),
			"http.response.status_code", status,
			"duration_ms", float64(time.Since(started).Microseconds())/1000,
		)
	})
}
