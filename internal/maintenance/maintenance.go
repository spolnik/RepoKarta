// Package maintenance inventories and safely cleans RepoKarta-owned runtime
// storage. It never traverses or mutates configured source repositories.
package maintenance

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/catalog"
)

const (
	maximumInventoryItems = 500
	maximumCleanupTargets = 100
)

var (
	repositoryArtifactPattern = regexp.MustCompile(`^(?:repository|repo)-(\d+)(?:[._-]|$)`)
	sensitiveValuePattern     = regexp.MustCompile(`(?i)\b(password|passwd|token|secret|api[_-]?key|authorization)(\s*[:=]\s*)([^\s,;]+)`)
	bearerPattern             = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`)
)

// Store is the metadata required to label owned artifacts and find orphaned
// conversation attachments.
type Store interface {
	ListRepositories(context.Context) ([]catalog.Repository, error)
	ConversationImagePaths(context.Context) (map[string]struct{}, error)
}

// Config identifies only RepoKarta-owned storage and non-secret runtime
// metadata. RepositoryRoot is used solely for redaction and boundary checks.
type Config struct {
	DataDirectory   string
	RepositoryRoot  string
	Version         string
	Address         string
	DatabaseVersion int
	MapVersion      int
	WikiVersion     int
	Now             func() time.Time
}

// Service owns storage inspection, cleanup-plan integrity, and diagnostics.
type Service struct {
	config Config
	store  Store
	key    []byte
	mu     sync.Mutex
}

// Category summarizes one non-overlapping top-level storage class.
type Category struct {
	ID               string `json:"id"`
	Label            string `json:"label"`
	SizeBytes        int64  `json:"size_bytes"`
	FileCount        int    `json:"file_count"`
	ReclaimableBytes int64  `json:"reclaimable_bytes"`
}

// Item is one bounded storage entry. RelativePath is always rooted in the
// RepoKarta data directory and is never accepted back as a deletion target.
type Item struct {
	ID           string    `json:"id"`
	Category     string    `json:"category"`
	Label        string    `json:"label"`
	RelativePath string    `json:"relative_path"`
	SizeBytes    int64     `json:"size_bytes"`
	ModifiedAt   time.Time `json:"modified_at"`
	RepositoryID int64     `json:"repository_id,omitempty"`
	Repository   string    `json:"repository,omitempty"`
	Revision     string    `json:"revision,omitempty"`
	State        string    `json:"state"`
	Cleanable    bool      `json:"cleanable"`
	Reason       string    `json:"reason"`
	absolutePath string
}

// Inventory is one point-in-time, bounded view of RepoKarta-owned storage.
type Inventory struct {
	GeneratedAt      time.Time  `json:"generated_at"`
	DataDirectory    string     `json:"data_directory"`
	TotalBytes       int64      `json:"total_bytes"`
	ReclaimableBytes int64      `json:"reclaimable_bytes"`
	Categories       []Category `json:"categories"`
	Items            []Item     `json:"items"`
	ItemsTruncated   bool       `json:"items_truncated"`
}

// CleanupPlan is a dry-run result. Token binds the target IDs to their current
// size and modification time so execution cannot silently act on changed data.
type CleanupPlan struct {
	GeneratedAt time.Time `json:"generated_at"`
	Token       string    `json:"token"`
	Items       []Item    `json:"items"`
	TotalBytes  int64     `json:"total_bytes"`
}

// CleanupResult reports completed work, including bounded partial failure.
type CleanupResult struct {
	RemovedItems int   `json:"removed_items"`
	RemovedBytes int64 `json:"removed_bytes"`
}

// DiagnosticContext contains sanitized runtime facts supplied by the host.
type DiagnosticContext struct {
	AuthMode         string
	PublicURL        string
	AllowOpen        bool
	ProviderStatuses []agent.Status
}

// New creates a maintenance service after resolving and validating the data
// boundary. Source and data directories must not contain one another.
func New(config Config, store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("maintenance store is required")
	}
	dataDirectory, err := filepath.Abs(strings.TrimSpace(config.DataDirectory))
	if err != nil || dataDirectory == "" {
		return nil, errors.New("resolve maintenance data directory")
	}
	repositoryRoot, err := filepath.Abs(strings.TrimSpace(config.RepositoryRoot))
	if err != nil || repositoryRoot == "" {
		return nil, errors.New("resolve repository root")
	}
	dataDirectory = filepath.Clean(dataDirectory)
	repositoryRoot = filepath.Clean(repositoryRoot)
	if pathContains(dataDirectory, repositoryRoot) || pathContains(repositoryRoot, dataDirectory) {
		return nil, errors.New("RepoKarta data directory and repository root must not contain one another")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("create cleanup plan key: %w", err)
	}
	config.DataDirectory = dataDirectory
	config.RepositoryRoot = repositoryRoot
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Service{config: config, store: store, key: key}, nil
}

// Inventory scans only known RepoKarta-owned top-level paths.
func (s *Service) Inventory(ctx context.Context) (Inventory, error) {
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("load repository metadata: %w", err)
	}
	repositoryNames := make(map[int64]string, len(repositories))
	for _, repository := range repositories {
		repositoryNames[repository.ID] = repository.Name
	}
	liveImages, err := s.store.ConversationImagePaths(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("load conversation attachments: %w", err)
	}

	inventory := Inventory{
		GeneratedAt:   s.config.Now().UTC(),
		DataDirectory: s.config.DataDirectory,
		Categories: []Category{
			{ID: "database", Label: "Database"},
			{ID: "indexes", Label: "Search indexes"},
			{ID: "maps", Label: "Repository maps"},
			{ID: "docs", Label: "Deep Wiki"},
			{ID: "advisories", Label: "Dependency advisory snapshots"},
			{ID: "conversations", Label: "Conversation attachments"},
			{ID: "logs", Label: "Logs"},
			{ID: "security", Label: "Security identity"},
			{ID: "other", Label: "Other owned data"},
		},
	}
	categoryIndex := make(map[string]int, len(inventory.Categories))
	for index, category := range inventory.Categories {
		categoryIndex[category.ID] = index
	}

	topLevel, err := os.ReadDir(s.config.DataDirectory)
	if err != nil {
		return Inventory{}, fmt.Errorf("read data directory: %w", err)
	}
	for _, entry := range topLevel {
		if err := ctx.Err(); err != nil {
			return Inventory{}, err
		}
		absolute := filepath.Join(s.config.DataDirectory, entry.Name())
		category := categoryForTopLevel(entry.Name())
		if err := s.scanPath(ctx, &inventory, categoryIndex, category, absolute, repositoryNames, liveImages); err != nil {
			return Inventory{}, err
		}
	}
	s.classifyMapRetention(&inventory)
	for _, item := range inventory.Items {
		if !item.Cleanable {
			continue
		}
		inventory.ReclaimableBytes += item.SizeBytes
		index := categoryIndex[item.Category]
		inventory.Categories[index].ReclaimableBytes += item.SizeBytes
	}
	slices.SortFunc(inventory.Items, func(left, right Item) int {
		if left.Cleanable != right.Cleanable {
			if left.Cleanable {
				return -1
			}
			return 1
		}
		if left.Category != right.Category {
			return strings.Compare(left.Category, right.Category)
		}
		return strings.Compare(left.RelativePath, right.RelativePath)
	})
	if len(inventory.Items) > maximumInventoryItems {
		inventory.Items = inventory.Items[:maximumInventoryItems]
		inventory.ItemsTruncated = true
	}
	return inventory, nil
}

func (s *Service) scanPath(
	ctx context.Context,
	inventory *Inventory,
	categoryIndex map[string]int,
	category, absolute string,
	repositoryNames map[int64]string,
	liveImages map[string]struct{},
) error {
	rootInfo, err := os.Lstat(absolute)
	if err != nil {
		return fmt.Errorf("inspect owned storage %s: %w", filepath.Base(absolute), err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("owned storage path %s is a symbolic link", filepath.Base(absolute))
	}
	return filepath.WalkDir(absolute, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if current != absolute && entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("owned storage contains symbolic link %s", mustRelative(s.config.DataDirectory, current))
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative := mustRelative(s.config.DataDirectory, current)
		index := categoryIndex[category]
		inventory.Categories[index].SizeBytes += info.Size()
		inventory.Categories[index].FileCount++
		inventory.TotalBytes += info.Size()

		item := Item{
			ID:           itemID(category, relative),
			Category:     category,
			Label:        filepath.Base(current),
			RelativePath: filepath.ToSlash(relative),
			SizeBytes:    info.Size(),
			ModifiedAt:   info.ModTime().UTC(),
			State:        "protected",
			Reason:       protectedReason(category),
			absolutePath: current,
		}
		item.RepositoryID = repositoryIDFromPath(relative)
		item.Repository = repositoryNames[item.RepositoryID]
		if item.RepositoryID > 0 && item.Repository == "" {
			item.State = "orphaned"
			item.Cleanable = true
			item.Reason = "No durable repository references this derived artifact."
		} else if temporaryArtifact(entry.Name()) {
			item.State = "interrupted"
			item.Cleanable = true
			item.Reason = "Interrupted temporary output; no published artifact references it."
		} else if category == "logs" {
			item.State = "diagnostic"
			item.Cleanable = true
			item.Reason = "RepoKarta-owned log file; safe to remove after review."
		} else if category == "conversations" {
			if _, exists := liveImages[entry.Name()]; exists {
				item.State = "referenced"
				item.Reason = "Referenced by a durable conversation."
			} else {
				item.State = "orphaned"
				item.Cleanable = true
				item.Reason = "No durable conversation references this attachment."
			}
		} else if category == "maps" && strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			item.State = "cached"
			item.Reason = "Latest usable snapshot in this map scope is preserved."
		}
		inventory.Items = append(inventory.Items, item)
		return nil
	})
}

func (s *Service) classifyMapRetention(inventory *Inventory) {
	latest := make(map[string]int)
	for index, item := range inventory.Items {
		if item.Category != "maps" || item.State != "cached" {
			continue
		}
		group := mapGroup(filepath.Base(item.RelativePath))
		current, exists := latest[group]
		if !exists || inventory.Items[current].ModifiedAt.Before(item.ModifiedAt) {
			latest[group] = index
		}
	}
	for index := range inventory.Items {
		item := &inventory.Items[index]
		if item.Category != "maps" || item.State != "cached" {
			continue
		}
		if latest[mapGroup(filepath.Base(item.RelativePath))] == index {
			continue
		}
		item.State = "stale"
		item.Cleanable = true
		item.Reason = "Superseded map snapshot; the newest usable snapshot is preserved."
	}
}

// Plan validates exact inventory IDs and returns a signed dry run.
func (s *Service) Plan(ctx context.Context, targetIDs []string) (CleanupPlan, error) {
	targetIDs = uniqueTargets(targetIDs)
	if len(targetIDs) == 0 {
		return CleanupPlan{}, errors.New("select at least one cleanable item")
	}
	if len(targetIDs) > maximumCleanupTargets {
		return CleanupPlan{}, fmt.Errorf("cleanup is limited to %d exact targets", maximumCleanupTargets)
	}
	inventory, err := s.Inventory(ctx)
	if err != nil {
		return CleanupPlan{}, err
	}
	byID := make(map[string]Item, len(inventory.Items))
	for _, item := range inventory.Items {
		byID[item.ID] = item
	}
	plan := CleanupPlan{GeneratedAt: s.config.Now().UTC()}
	for _, id := range targetIDs {
		item, exists := byID[id]
		if !exists {
			return CleanupPlan{}, fmt.Errorf("cleanup target %q is unavailable; refresh storage inventory", id)
		}
		if !item.Cleanable {
			return CleanupPlan{}, fmt.Errorf("cleanup target %q is protected: %s", id, item.Reason)
		}
		plan.Items = append(plan.Items, item)
		plan.TotalBytes += item.SizeBytes
	}
	slices.SortFunc(plan.Items, func(left, right Item) int {
		return strings.Compare(left.ID, right.ID)
	})
	plan.Token = s.planToken(plan.Items)
	return plan, nil
}

// Execute re-plans immediately and removes only unchanged, exact regular files.
func (s *Service) Execute(ctx context.Context, targetIDs []string, token string) (CleanupResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, err := s.Plan(ctx, targetIDs)
	if err != nil {
		return CleanupResult{}, err
	}
	if !hmac.Equal([]byte(plan.Token), []byte(strings.TrimSpace(token))) {
		return CleanupResult{}, errors.New("cleanup plan changed; preview the targets again")
	}
	result := CleanupResult{}
	for _, item := range plan.Items {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := s.removeExactFile(item); err != nil {
			return result, fmt.Errorf("cleanup stopped after %d item(s): %w", result.RemovedItems, err)
		}
		result.RemovedItems++
		result.RemovedBytes += item.SizeBytes
	}
	return result, nil
}

func (s *Service) removeExactFile(item Item) error {
	if !pathContains(s.config.DataDirectory, item.absolutePath) ||
		pathContains(s.config.RepositoryRoot, item.absolutePath) {
		return errors.New("cleanup target is outside the RepoKarta data boundary")
	}
	info, err := os.Lstat(item.absolutePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("cleanup target is no longer a regular file")
	}
	if info.Size() != item.SizeBytes || !info.ModTime().UTC().Equal(item.ModifiedAt) {
		return errors.New("cleanup target changed after preview")
	}
	if err := os.Remove(item.absolutePath); err != nil {
		return err
	}
	return nil
}

// Diagnostics creates a bounded ZIP containing JSON metadata only. It never
// includes database pages, logs, prompts, attachments, repository paths, source
// contents, credentials, tokens, or generated Wiki text.
func (s *Service) Diagnostics(ctx context.Context, diagnostic DiagnosticContext) ([]byte, string, error) {
	inventory, err := s.Inventory(ctx)
	if err != nil {
		return nil, "", err
	}
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return nil, "", err
	}
	type repositoryStatus struct {
		ID            int64  `json:"id"`
		Name          string `json:"name"`
		ScanState     string `json:"scan_state"`
		IndexState    string `json:"index_state"`
		IndexedCommit string `json:"indexed_commit,omitempty"`
		ScanError     string `json:"scan_error,omitempty"`
		IndexError    string `json:"index_error,omitempty"`
	}
	repositoryStatuses := make([]repositoryStatus, 0, len(repositories))
	for _, repository := range repositories {
		repositoryStatuses = append(repositoryStatuses, repositoryStatus{
			ID:            repository.ID,
			Name:          repository.Name,
			ScanState:     repository.ScanState,
			IndexState:    repository.IndexState,
			IndexedCommit: repository.IndexedCommit,
			ScanError:     s.redact(repository.ScanError),
			IndexError:    s.redact(repository.IndexError),
		})
	}
	type providerStatus struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Available     bool   `json:"available"`
		Authenticated bool   `json:"authenticated"`
		Detail        string `json:"detail,omitempty"`
	}
	providers := make([]providerStatus, 0, len(diagnostic.ProviderStatuses))
	for _, status := range diagnostic.ProviderStatuses {
		providers = append(providers, providerStatus{
			ID: status.ID, Name: status.Name, Available: status.Available,
			Authenticated: status.Authenticated, Detail: s.redact(status.Detail),
		})
	}
	payload := map[string]any{
		"generated_at": inventory.GeneratedAt,
		"application": map[string]any{
			"version":        s.config.Version,
			"go":             runtime.Version(),
			"os":             runtime.GOOS,
			"architecture":   runtime.GOARCH,
			"listen_address": s.config.Address,
		},
		"formats": map[string]int{
			"database": s.config.DatabaseVersion,
			"maps":     s.config.MapVersion,
			"wiki":     s.config.WikiVersion,
		},
		"security": map[string]any{
			"mode":                      diagnostic.AuthMode,
			"public_url_configured":     strings.TrimSpace(diagnostic.PublicURL) != "",
			"open_mode_startup_allowed": diagnostic.AllowOpen,
		},
		"storage": map[string]any{
			"total_bytes":       inventory.TotalBytes,
			"reclaimable_bytes": inventory.ReclaimableBytes,
			"categories":        inventory.Categories,
			"items_truncated":   inventory.ItemsTruncated,
		},
		"repositories": repositoryStatuses,
		"providers":    providers,
	}
	manifest := map[string]any{
		"format":       1,
		"generated_at": inventory.GeneratedAt,
		"included": []string{
			"application version and platform",
			"non-secret authentication mode flags",
			"artifact format versions",
			"storage category totals",
			"repository names, revisions, readiness, and redacted failures",
			"provider readiness and redacted status detail",
		},
		"omitted": []string{
			"repository source and absolute repository paths",
			"database pages and conversation prompts",
			"logs and generated Wiki content",
			"attachments and images",
			"credentials, tokens, cookies, private keys, and environment variables",
		},
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	if err := writeJSONFile(writer, "manifest.json", manifest); err != nil {
		return nil, "", err
	}
	if err := writeJSONFile(writer, "diagnostics.json", payload); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close diagnostics archive: %w", err)
	}
	name := "repokarta-diagnostics-" + inventory.GeneratedAt.Format("20060102T150405Z") + ".zip"
	return archive.Bytes(), name, nil
}

func writeJSONFile(writer *zip.Writer, name string, value any) error {
	entry, err := writer.Create(name)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if _, err := entry.Write(encoded); err != nil {
		return err
	}
	return nil
}

func (s *Service) redact(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, s.config.DataDirectory, "<data-dir>")
	value = strings.ReplaceAll(value, s.config.RepositoryRoot, "<repository-root>")
	value = bearerPattern.ReplaceAllString(value, "Bearer <redacted>")
	value = sensitiveValuePattern.ReplaceAllString(value, "$1$2<redacted>")
	words := strings.Fields(value)
	for index, word := range words {
		parsed, err := url.Parse(strings.Trim(word, `"'(),`))
		if err == nil && parsed.User != nil {
			parsed.User = url.User("<redacted>")
			words[index] = parsed.String()
		}
	}
	return strings.Join(words, " ")
}

func (s *Service) planToken(items []Item) string {
	mac := hmac.New(sha256.New, s.key)
	for _, item := range items {
		fmt.Fprintf(mac, "%s\x00%d\x00%d\x00", item.ID, item.SizeBytes, item.ModifiedAt.UnixNano())
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func categoryForTopLevel(name string) string {
	switch strings.ToLower(name) {
	case "repokarta.db", "repokarta.db-wal", "repokarta.db-shm":
		return "database"
	case "indexes", "scip", "scip-java":
		return "indexes"
	case "maps":
		return "maps"
	case "docs":
		return "docs"
	case "advisories":
		return "advisories"
	case "conversations":
		return "conversations"
	case "logs":
		return "logs"
	case "security":
		return "security"
	default:
		return "other"
	}
}

func protectedReason(category string) string {
	switch category {
	case "database":
		return "Live database state is never removed by runtime cleanup."
	case "indexes":
		return "Active search indexes are preserved; interrupted temporary files remain individually cleanable."
	case "maps":
		return "Latest usable snapshot in each map scope is preserved."
	case "docs":
		return "Current Deep Wiki content is preserved; delete or regenerate it through its owning workflow."
	case "advisories":
		return "The current reproducible OSV snapshot is preserved; replace it through the dependency advisory refresh."
	case "conversations":
		return "Referenced conversation attachment is preserved."
	case "security":
		return "Authentication private keys and certificates are never removable through the UI."
	default:
		return "Unrecognized owned data is visible but protected by default."
	}
}

func temporaryArtifact(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".tmp") ||
		strings.Contains(lower, ".tmp.") ||
		strings.HasPrefix(lower, "tmp-")
}

func mapGroup(name string) string {
	match := repositoryArtifactPattern.FindStringSubmatch(name)
	if len(match) == 2 {
		return "repository-" + match[1]
	}
	if strings.HasPrefix(name, "all-") {
		return "all"
	}
	return name
}

func repositoryIDFromPath(relative string) int64 {
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		match := repositoryArtifactPattern.FindStringSubmatch(part)
		if len(match) != 2 {
			continue
		}
		id, _ := strconv.ParseInt(match[1], 10, 64)
		return id
	}
	return 0
}

func itemID(category, relative string) string {
	sum := sha256.Sum256([]byte(category + "\x00" + filepath.ToSlash(relative)))
	return category + "-" + hex.EncodeToString(sum[:8])
}

func uniqueTargets(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	output := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		output = append(output, value)
	}
	return output
}

func mustRelative(root, target string) string {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return filepath.Base(target)
	}
	return relative
}

func pathContains(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
