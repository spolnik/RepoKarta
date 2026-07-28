package dependencies

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"deps.dev/util/semver"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/telemetry"
)

const (
	PublicOSVAPI             = "https://api.osv.dev"
	AdvisorySnapshotSchema   = 1
	DefaultFindingLimit      = 100
	MaximumFindingLimit      = 500
	defaultAdvisoryTTL       = 24 * time.Hour
	advisoryBatchSize        = 100
	maximumAdvisoryPackages  = 20_000
	maximumAdvisories        = 50_000
	maximumAdvisoryBody      = 4 << 20
	maximumAdvisoryBatchBody = 32 << 20
	maximumSnapshotBody      = 512 << 20
)

var (
	exactVersionPrefix        = regexp.MustCompile(`^(?:==|=)\s*`)
	normalizedPyPIPunctuation = regexp.MustCompile(`[-_.]+`)
)

// AdvisoryOptions bounds and filters one deterministic findings page.
type AdvisoryOptions struct {
	Query     string
	Ecosystem string
	Severity  string
	Usage     string
	Package   string
	Offset    int
	Limit     int
}

// AdvisorySnapshot is the immutable, locally persisted OSV evidence used for
// matching. It contains only advisories relevant to packages in the recorded
// fleet query, rather than an unbounded copy of every OSV ecosystem dump.
type AdvisorySnapshot struct {
	SchemaVersion int               `json:"schema_version"`
	Source        string            `json:"source"`
	SourceURL     string            `json:"source_url"`
	RetrievedAt   time.Time         `json:"retrieved_at"`
	Version       string            `json:"version"`
	QueryDigest   string            `json:"query_digest"`
	Partial       bool              `json:"partial,omitempty"`
	FailedCount   int               `json:"failed_count,omitempty"`
	Packages      []AdvisoryPackage `json:"packages"`
	Advisories    []OSVAdvisory     `json:"advisories"`
}

// AdvisoryPackage records one exact package identity included in a snapshot.
type AdvisoryPackage struct {
	Ecosystem    string `json:"ecosystem"`
	OSVEcosystem string `json:"osv_ecosystem"`
	Package      string `json:"package"`
}

// OSVAdvisory retains the OSV fields needed for reproducible matching,
// severity, remediation hints, and source attribution. Details are persisted
// but deliberately omitted from compact findings and MCP results.
type OSVAdvisory struct {
	SchemaVersion    string         `json:"schema_version,omitempty"`
	ID               string         `json:"id"`
	Modified         time.Time      `json:"modified,omitempty"`
	Published        time.Time      `json:"published,omitempty"`
	Withdrawn        time.Time      `json:"withdrawn,omitempty"`
	Aliases          []string       `json:"aliases,omitempty"`
	Related          []string       `json:"related,omitempty"`
	Summary          string         `json:"summary,omitempty"`
	Details          string         `json:"details,omitempty"`
	Severity         []OSVSeverity  `json:"severity,omitempty"`
	Affected         []OSVAffected  `json:"affected,omitempty"`
	References       []OSVReference `json:"references,omitempty"`
	DatabaseSpecific map[string]any `json:"database_specific,omitempty"`
}

type OSVSeverity struct {
	Type   string `json:"type"`
	Score  string `json:"score"`
	Source string `json:"source,omitempty"`
}

type OSVReference struct {
	Type string `json:"type,omitempty"`
	URL  string `json:"url,omitempty"`
}

type OSVAffected struct {
	Package           OSVPackage     `json:"package"`
	Severity          []OSVSeverity  `json:"severity,omitempty"`
	Ranges            []OSVRange     `json:"ranges,omitempty"`
	Versions          []string       `json:"versions,omitempty"`
	EcosystemSpecific map[string]any `json:"ecosystem_specific,omitempty"`
	DatabaseSpecific  map[string]any `json:"database_specific,omitempty"`
}

type OSVPackage struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	PURL      string `json:"purl,omitempty"`
}

type OSVRange struct {
	Type   string     `json:"type"`
	Repo   string     `json:"repo,omitempty"`
	Events []OSVEvent `json:"events"`
}

type OSVEvent struct {
	Introduced   string `json:"introduced,omitempty"`
	Fixed        string `json:"fixed,omitempty"`
	LastAffected string `json:"last_affected,omitempty"`
	Limit        string `json:"limit,omitempty"`
}

// Finding is one advisory joined to one commit-pinned inventory declaration.
// Reachability is reserved as an optional extension point; the current
// deterministic pipeline never claims import or call reachability.
type Finding struct {
	ID                      string                 `json:"id"`
	AdvisoryID              string                 `json:"advisory_id"`
	Aliases                 []string               `json:"aliases,omitempty"`
	Summary                 string                 `json:"summary,omitempty"`
	Severity                string                 `json:"severity"`
	CVSS                    []OSVSeverity          `json:"cvss,omitempty"`
	AffectedRange           string                 `json:"affected_range,omitempty"`
	FixedVersion            string                 `json:"fixed_version,omitempty"`
	LatestStable            string                 `json:"latest_stable,omitempty"`
	Ecosystem               string                 `json:"ecosystem"`
	Package                 string                 `json:"package"`
	Version                 string                 `json:"version"`
	MatchBasis              string                 `json:"match_basis"`
	MatchConfidence         string                 `json:"match_confidence"`
	RepositoryID            int64                  `json:"repository_id"`
	Repository              string                 `json:"repository"`
	Revision                string                 `json:"revision"`
	ManifestKind            string                 `json:"manifest_kind"`
	ManifestPath            string                 `json:"manifest_path"`
	Resolution              string                 `json:"resolution"`
	ResolutionSource        string                 `json:"resolution_source,omitempty"`
	Usage                   string                 `json:"usage"`
	Relationship            string                 `json:"relationship"`
	DeclaredScope           string                 `json:"declared_scope,omitempty"`
	ManifestEvidence        graph.Evidence         `json:"manifest_evidence"`
	ManifestOccurrenceCount int                    `json:"manifest_occurrence_count"`
	Occurrences             []FindingOccurrence    `json:"occurrences"`
	AdvisoryEvidence        AdvisoryEvidence       `json:"advisory_evidence"`
	Reachability            *FindingReachability   `json:"reachability,omitempty"`
	AdditionalMetadata      map[string]interface{} `json:"metadata,omitempty"`
}

// FindingOccurrence is one commit-pinned manifest declaration represented by
// a de-duplicated advisory/repository/package/version finding.
type FindingOccurrence struct {
	ManifestKind     string         `json:"manifest_kind"`
	ManifestPath     string         `json:"manifest_path"`
	Resolution       string         `json:"resolution"`
	ResolutionSource string         `json:"resolution_source,omitempty"`
	MatchBasis       string         `json:"match_basis"`
	MatchConfidence  string         `json:"match_confidence"`
	Usage            string         `json:"usage"`
	Relationship     string         `json:"relationship"`
	DeclaredScope    string         `json:"declared_scope,omitempty"`
	Evidence         graph.Evidence `json:"evidence"`
}

type FindingReachability struct {
	State     string   `json:"state"`
	FileCount int      `json:"file_count,omitempty"`
	Files     []string `json:"files,omitempty"`
}

type AdvisoryEvidence struct {
	Source            string    `json:"source"`
	AdvisoryURL       string    `json:"advisory_url"`
	SnapshotVersion   string    `json:"snapshot_version"`
	SnapshotTimestamp time.Time `json:"snapshot_timestamp"`
}

type AdvisorySnapshotStatus struct {
	State           string    `json:"state"`
	Message         string    `json:"message,omitempty"`
	Source          string    `json:"source"`
	SourceURL       string    `json:"source_url"`
	Version         string    `json:"version,omitempty"`
	RetrievedAt     time.Time `json:"retrieved_at,omitempty"`
	AgeSeconds      int64     `json:"age_seconds,omitempty"`
	Stale           bool      `json:"stale"`
	PackageCount    int       `json:"package_count"`
	AdvisoryCount   int       `json:"advisory_count"`
	QueryDigest     string    `json:"query_digest,omitempty"`
	CurrentDigest   string    `json:"current_query_digest,omitempty"`
	CoverageDiffers bool      `json:"coverage_differs"`
}

type AdvisoryGap struct {
	Ecosystem string `json:"ecosystem"`
	Count     int    `json:"count"`
	Reason    string `json:"reason"`
}

type FindingFacets struct {
	Severities   map[string]int `json:"severities"`
	Usages       map[string]int `json:"usages"`
	Repositories map[string]int `json:"repositories"`
	Packages     map[string]int `json:"packages"`
}

type FindingHeadline struct {
	Severity        string `json:"severity"`
	Usage           string `json:"usage"`
	FindingCount    int    `json:"finding_count"`
	RepositoryCount int    `json:"repository_count"`
}

type FindingGroup struct {
	Severity     string `json:"severity"`
	Usage        string `json:"usage"`
	RepositoryID int64  `json:"repository_id"`
	Repository   string `json:"repository"`
	Package      string `json:"package"`
	FindingCount int    `json:"finding_count"`
}

// DependencyCheckGap identifies one declaration that could not participate in
// matching. The response bounds examples while preserving complete counters.
type DependencyCheckGap struct {
	RepositoryID int64  `json:"repository_id"`
	Repository   string `json:"repository"`
	Revision     string `json:"revision"`
	Ecosystem    string `json:"ecosystem"`
	Package      string `json:"package"`
	ManifestPath string `json:"manifest_path"`
	Reason       string `json:"reason"`
	EvidenceURL  string `json:"evidence_url,omitempty"`
}

// FindingResponse distinguishes a checked zero from unavailable or partial
// coverage and keeps every honesty count visible to API, UI, and MCP adapters.
type FindingResponse struct {
	CheckState                   string                 `json:"check_state"`
	CheckMessage                 string                 `json:"check_message,omitempty"`
	AdvisoryOnly                 bool                   `json:"advisory_only"`
	Snapshot                     AdvisorySnapshotStatus `json:"snapshot"`
	DeclarationCount             int                    `json:"declaration_count"`
	CheckedDeclarationCount      int                    `json:"checked_declaration_count"`
	SkippedNoVersionCount        int                    `json:"skipped_no_version_count"`
	SkippedInvalidVersionCount   int                    `json:"skipped_invalid_version_count"`
	NotInSnapshotCount           int                    `json:"not_in_snapshot_count"`
	UncoveredEcosystems          []AdvisoryGap          `json:"uncovered_ecosystems,omitempty"`
	SkippedDeclarations          []DependencyCheckGap   `json:"skipped_declarations,omitempty"`
	TotalFindingCount            int                    `json:"total_finding_count"`
	TotalManifestOccurrenceCount int                    `json:"total_manifest_occurrence_count"`
	FindingCount                 int                    `json:"finding_count"`
	ReturnedCount                int                    `json:"returned_count"`
	Findings                     []Finding              `json:"findings"`
	Facets                       FindingFacets          `json:"facets"`
	Headlines                    []FindingHeadline      `json:"headlines"`
	Groups                       []FindingGroup         `json:"groups"`
	Offset                       int                    `json:"offset"`
	Limit                        int                    `json:"limit"`
	HasMore                      bool                   `json:"has_more"`
	Truncated                    bool                   `json:"truncated"`
}

// AdvisoryRefreshProgress is a race-free, bounded background refresh snapshot.
type AdvisoryRefreshProgress struct {
	State             string `json:"state"`
	Stage             string `json:"stage,omitempty"`
	Total             int    `json:"total"`
	Completed         int    `json:"completed"`
	Failed            int    `json:"failed"`
	Skipped           int    `json:"skipped"`
	PackageTotal      int    `json:"package_total"`
	PackageCompleted  int    `json:"package_completed"`
	AdvisoryTotal     int    `json:"advisory_total"`
	AdvisoryCompleted int    `json:"advisory_completed"`
	StartedAt         string `json:"started_at,omitempty"`
	FinishedAt        string `json:"finished_at,omitempty"`
	SnapshotVersion   string `json:"snapshot_version,omitempty"`
	SnapshotTimestamp string `json:"snapshot_timestamp,omitempty"`
	Error             string `json:"error,omitempty"`
}

func (s *Service) UseAdvisoryDirectory(directory string) error {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return errors.New("advisory directory is required")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create advisory directory: %w", err)
	}
	s.advisoryDirectory = directory
	return nil
}

// Findings reads only the commit-pinned map, registry cache, and persisted OSV
// snapshot. It never performs network I/O.
func (s *Service) Findings(
	ctx context.Context,
	inventory graph.Snapshot,
	options AdvisoryOptions,
) (FindingResponse, error) {
	snapshot, err := s.readAdvisorySnapshot()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return FindingResponse{}, err
	}
	observations, observationErr := s.store.ListDependencyObservations(ctx)
	if observationErr != nil {
		return FindingResponse{}, fmt.Errorf("list dependency observations: %w", observationErr)
	}
	return buildFindings(inventory, snapshot, observations, options, s.now()), nil
}

func buildFindings(
	inventory graph.Snapshot,
	snapshot *AdvisorySnapshot,
	observations []Observation,
	options AdvisoryOptions,
	now time.Time,
) FindingResponse {
	options = normalizeAdvisoryOptions(options)
	declarations := normalizedDeclarations(inventory)
	response := FindingResponse{
		CheckState:       "unavailable",
		CheckMessage:     "No local OSV advisory snapshot is available. Refresh advisories before interpreting zero findings.",
		AdvisoryOnly:     true,
		DeclarationCount: len(declarations),
		Findings:         []Finding{},
		Facets: FindingFacets{
			Severities: map[string]int{}, Usages: map[string]int{},
			Repositories: map[string]int{}, Packages: map[string]int{},
		},
		Offset:    options.Offset,
		Limit:     options.Limit,
		Truncated: inventory.Truncated || inventory.StructureTruncated,
		Snapshot: AdvisorySnapshotStatus{
			State: "unavailable", Source: "OSV.dev", SourceURL: PublicOSVAPI,
		},
	}
	if snapshot == nil {
		return response
	}
	currentPackages := advisoryPackages(declarations)
	currentDigest := advisoryQueryDigest(currentPackages)
	age := now.Sub(snapshot.RetrievedAt)
	if age < 0 {
		age = 0
	}
	response.Snapshot = AdvisorySnapshotStatus{
		State: "ready", Source: snapshot.Source, SourceURL: snapshot.SourceURL,
		Version: snapshot.Version, RetrievedAt: snapshot.RetrievedAt,
		AgeSeconds: int64(age.Seconds()), Stale: age > defaultAdvisoryTTL,
		PackageCount: len(snapshot.Packages), AdvisoryCount: len(snapshot.Advisories),
		QueryDigest: snapshot.QueryDigest, CurrentDigest: currentDigest,
		CoverageDiffers: snapshot.QueryDigest != currentDigest,
	}
	response.CheckState = "ready"
	response.CheckMessage = ""
	if response.Snapshot.Stale {
		response.CheckState = "stale"
		response.Snapshot.State = "stale"
		response.CheckMessage = "Findings were checked against a stale local OSV snapshot."
	}
	if snapshot.Partial {
		response.CheckState = "partial"
		response.Snapshot.State = "partial"
		response.CheckMessage = fmt.Sprintf(
			"OSV snapshot is partial; %d advisory fetches failed", snapshot.FailedCount,
		)
	}

	covered := make(map[string]struct{}, len(snapshot.Packages))
	for _, pkg := range snapshot.Packages {
		covered[advisoryPackageKey(pkg.Ecosystem, pkg.Package)] = struct{}{}
	}
	advisoriesByPackage := make(map[string][]OSVAdvisory)
	advisoryIDsByPackage := make(map[string]map[string]struct{})
	for _, advisory := range snapshot.Advisories {
		if !advisory.Withdrawn.IsZero() {
			continue
		}
		for _, affected := range advisory.Affected {
			ecosystem, ok := localEcosystem(affected.Package.Ecosystem)
			if !ok {
				continue
			}
			key := advisoryPackageKey(ecosystem, affected.Package.Name)
			if advisoryIDsByPackage[key] == nil {
				advisoryIDsByPackage[key] = make(map[string]struct{})
			}
			if _, exists := advisoryIDsByPackage[key][advisory.ID]; exists {
				continue
			}
			advisoryIDsByPackage[key][advisory.ID] = struct{}{}
			advisoriesByPackage[key] = append(advisoriesByPackage[key], advisory)
		}
	}
	latest := make(map[string]string, len(observations))
	for _, observation := range observations {
		key := advisoryPackageKey(observation.Ecosystem, observation.Package)
		if observation.Status == "ok" {
			latest[key] = observation.LatestStable
		}
	}

	uncovered := make(map[string]int)
	allFindings := make([]Finding, 0)
	for _, declaration := range declarations {
		if _, ok := osvEcosystem(declaration.Ecosystem); !ok {
			uncovered[declaration.Ecosystem]++
			appendDependencyGap(&response, declaration, "uncovered_ecosystem")
			continue
		}
		version, basis, confidence, ok := matchVersion(declaration)
		if !ok {
			response.SkippedNoVersionCount++
			appendDependencyGap(&response, declaration, "no_resolvable_version")
			continue
		}
		system, ok := versionSystem(declaration.Ecosystem)
		if !ok {
			uncovered[declaration.Ecosystem]++
			continue
		}
		if _, err := system.Parse(version); err != nil {
			response.SkippedInvalidVersionCount++
			appendDependencyGap(&response, declaration, "invalid_version")
			continue
		}
		key := advisoryPackageKey(declaration.Ecosystem, declaration.Package)
		if _, ok := covered[key]; !ok {
			response.NotInSnapshotCount++
			appendDependencyGap(&response, declaration, "not_in_snapshot")
			continue
		}
		response.CheckedDeclarationCount++
		for _, advisory := range advisoriesByPackage[key] {
			affected, rangeLabel, fixed, matched := matchingAffected(advisory, declaration, version)
			if !matched {
				continue
			}
			severity, cvss := advisorySeverity(advisory, affected)
			finding := Finding{
				AdvisoryID: advisory.ID, Aliases: sortedUnique(advisory.Aliases),
				Summary: strings.TrimSpace(advisory.Summary), Severity: severity, CVSS: cvss,
				AffectedRange: rangeLabel, FixedVersion: fixed, LatestStable: latest[key],
				Ecosystem: declaration.Ecosystem, Package: declaration.Package, Version: version,
				MatchBasis: basis, MatchConfidence: confidence,
				RepositoryID: declaration.RepositoryID, Repository: declaration.Repository,
				Revision: declaration.Revision, ManifestKind: declaration.ManifestKind,
				ManifestPath: declaration.ManifestPath, Resolution: declaration.Resolution,
				ResolutionSource: declaration.ResolutionSource, Usage: declaration.Usage,
				Relationship: declaration.Relationship, DeclaredScope: declaration.DeclaredScope,
				ManifestEvidence: declaration.Evidence,
				AdvisoryEvidence: AdvisoryEvidence{
					Source:          snapshot.Source,
					AdvisoryURL:     strings.TrimRight(snapshot.SourceURL, "/") + "/v1/vulns/" + url.PathEscape(advisory.ID),
					SnapshotVersion: snapshot.Version, SnapshotTimestamp: snapshot.RetrievedAt,
				},
			}
			finding.ID = findingID(finding)
			allFindings = append(allFindings, finding)
		}
	}
	for ecosystem, count := range uncovered {
		response.UncoveredEcosystems = append(response.UncoveredEcosystems, AdvisoryGap{
			Ecosystem: ecosystem, Count: count,
			Reason: "ecosystem is present in the inventory but is not covered by the OSV join",
		})
	}
	slices.SortFunc(response.UncoveredEcosystems, func(left, right AdvisoryGap) int {
		return strings.Compare(left.Ecosystem, right.Ecosystem)
	})
	if response.NotInSnapshotCount > 0 || response.SkippedNoVersionCount > 0 ||
		response.SkippedInvalidVersionCount > 0 || len(response.UncoveredEcosystems) > 0 {
		if response.CheckState == "ready" {
			response.CheckState = "partial"
			response.Snapshot.State = "partial"
		}
		response.CheckMessage = "Some declarations could not be checked; inspect the explicit coverage gaps."
	}

	response.TotalManifestOccurrenceCount = len(allFindings)
	allFindings = deduplicateFindings(allFindings)
	sortFindings(allFindings)
	response.TotalFindingCount = len(allFindings)
	response.Facets = findingFacets(allFindings)
	response.Headlines = findingHeadlines(allFindings)
	response.Groups = findingGroups(allFindings)
	filtered := filterFindings(allFindings, options)
	response.FindingCount = len(filtered)
	offset := min(options.Offset, len(filtered))
	end := min(offset+options.Limit, len(filtered))
	response.Offset = offset
	response.Findings = append([]Finding(nil), filtered[offset:end]...)
	response.ReturnedCount = len(response.Findings)
	response.HasMore = end < len(filtered)
	return response
}

func appendDependencyGap(
	response *FindingResponse,
	declaration Declaration,
	reason string,
) {
	if len(response.SkippedDeclarations) >= 100 {
		return
	}
	response.SkippedDeclarations = append(response.SkippedDeclarations, DependencyCheckGap{
		RepositoryID: declaration.RepositoryID, Repository: declaration.Repository,
		Revision: declaration.Revision, Ecosystem: declaration.Ecosystem,
		Package: declaration.Package, ManifestPath: declaration.ManifestPath,
		Reason: reason, EvidenceURL: declaration.Evidence.URL,
	})
}

func normalizeAdvisoryOptions(options AdvisoryOptions) AdvisoryOptions {
	options.Query = strings.TrimSpace(options.Query)
	options.Ecosystem = strings.ToLower(strings.TrimSpace(options.Ecosystem))
	options.Severity = strings.ToLower(strings.TrimSpace(options.Severity))
	options.Usage = strings.ToLower(strings.TrimSpace(options.Usage))
	options.Package = strings.TrimSpace(options.Package)
	if options.Offset < 0 {
		options.Offset = 0
	}
	if options.Limit <= 0 {
		options.Limit = DefaultFindingLimit
	}
	options.Limit = min(options.Limit, MaximumFindingLimit)
	return options
}

func matchVersion(declaration Declaration) (string, string, string, bool) {
	if resolved := strings.TrimSpace(declaration.Resolved); resolved != "" {
		return resolved, "resolved", "high", true
	}
	declared := exactVersionPrefix.ReplaceAllString(strings.TrimSpace(declaration.Declared), "")
	if declared == "" || strings.ContainsAny(declared, "<>^~*|,[]() \t\r\n") {
		return "", "", "", false
	}
	return declared, "declared", "lower", true
}

func matchingAffected(
	advisory OSVAdvisory,
	declaration Declaration,
	version string,
) (OSVAffected, string, string, bool) {
	for _, affected := range advisory.Affected {
		local, ok := localEcosystem(affected.Package.Ecosystem)
		if !ok || !strings.EqualFold(local, declaration.Ecosystem) ||
			normalizePackage(local, affected.Package.Name) != normalizePackage(local, declaration.Package) {
			continue
		}
		matched, label, fixed := affectedVersion(local, version, affected)
		if matched {
			return affected, label, fixed, true
		}
	}
	return OSVAffected{}, "", "", false
}

func affectedVersion(ecosystem, version string, affected OSVAffected) (bool, string, string) {
	system, ok := versionSystem(ecosystem)
	if !ok {
		return false, "", ""
	}
	for _, listed := range affected.Versions {
		if system.Compare(version, listed) == 0 {
			return true, "version " + listed, nearestFixed(system, version, affected.Ranges)
		}
	}
	for _, osvRange := range affected.Ranges {
		if strings.EqualFold(osvRange.Type, "GIT") {
			continue
		}
		rangeSystem := system
		rangeVersion := version
		if strings.EqualFold(osvRange.Type, "SEMVER") {
			rangeSystem = semver.DefaultSystem
			rangeVersion = strings.TrimPrefix(version, "v")
		}
		events := sortedOSVEvents(rangeSystem, osvRange.Events)
		if matched, fixed := eventsContain(rangeSystem, rangeVersion, events); matched {
			return true, formatAffectedRange(osvRange), fixed
		}
	}
	return false, "", ""
}

func sortedOSVEvents(system semver.System, events []OSVEvent) []OSVEvent {
	output := append([]OSVEvent(nil), events...)
	eventVersion := func(event OSVEvent) string {
		return firstNonEmpty(event.Introduced, event.Fixed, event.LastAffected, event.Limit)
	}
	slices.SortStableFunc(output, func(left, right OSVEvent) int {
		leftVersion, rightVersion := eventVersion(left), eventVersion(right)
		if leftVersion == rightVersion {
			if left.Introduced != "" && right.Introduced == "" {
				return -1
			}
			if right.Introduced != "" && left.Introduced == "" {
				return 1
			}
			return 0
		}
		if leftVersion == "0" || rightVersion == "*" {
			return -1
		}
		if rightVersion == "0" || leftVersion == "*" {
			return 1
		}
		if compared := system.Compare(leftVersion, rightVersion); compared != 0 {
			return compared
		}
		return 0
	})
	return output
}

func eventsContain(system semver.System, version string, events []OSVEvent) (bool, string) {
	active := false
	for _, event := range events {
		switch {
		case event.Introduced != "":
			active = event.Introduced == "0" || system.Compare(version, event.Introduced) >= 0
		case event.Fixed != "":
			if active && system.Compare(version, event.Fixed) < 0 {
				return true, event.Fixed
			}
			active = false
		case event.LastAffected != "":
			if active && system.Compare(version, event.LastAffected) <= 0 {
				return true, ""
			}
			active = false
		case event.Limit != "":
			if active && (event.Limit == "*" || system.Compare(version, event.Limit) < 0) {
				return true, ""
			}
			active = false
		}
	}
	return active, ""
}

func nearestFixed(system semver.System, version string, ranges []OSVRange) string {
	candidates := make([]string, 0)
	for _, osvRange := range ranges {
		for _, event := range osvRange.Events {
			if event.Fixed != "" && system.Compare(event.Fixed, version) > 0 {
				candidates = append(candidates, event.Fixed)
			}
		}
	}
	slices.SortFunc(candidates, func(left, right string) int { return system.Compare(left, right) })
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

func formatAffectedRange(osvRange OSVRange) string {
	parts := make([]string, 0, len(osvRange.Events))
	for _, event := range osvRange.Events {
		switch {
		case event.Introduced != "":
			parts = append(parts, "introduced "+event.Introduced)
		case event.Fixed != "":
			parts = append(parts, "fixed "+event.Fixed+" (exclusive)")
		case event.LastAffected != "":
			parts = append(parts, "last affected "+event.LastAffected+" (inclusive)")
		case event.Limit != "":
			parts = append(parts, "limit "+event.Limit+" (exclusive)")
		}
	}
	return strings.ToUpper(osvRange.Type) + ": " + strings.Join(parts, "; ")
}

func advisorySeverity(advisory OSVAdvisory, affected OSVAffected) (string, []OSVSeverity) {
	cvss := append([]OSVSeverity(nil), affected.Severity...)
	if len(cvss) == 0 {
		cvss = append(cvss, advisory.Severity...)
	}
	for _, metadata := range []map[string]any{
		affected.DatabaseSpecific, affected.EcosystemSpecific, advisory.DatabaseSpecific,
	} {
		if label := severityFromMetadata(metadata); label != "" {
			return label, cvss
		}
	}
	best := 0.0
	for _, score := range cvss {
		for _, label := range []string{score.Type, score.Score} {
			switch strings.ToLower(strings.TrimSpace(label)) {
			case "critical", "high", "medium", "moderate", "low":
				if strings.EqualFold(label, "moderate") {
					return "medium", cvss
				}
				return strings.ToLower(strings.TrimSpace(label)), cvss
			}
		}
		if severity, ok := cvss4VectorSeverity(score); ok {
			return severity, cvss
		}
		value, ok := numericCVSS(score.Score)
		if ok && value > best {
			best = value
		}
	}
	switch {
	case best >= 9:
		return "critical", cvss
	case best >= 7:
		return "high", cvss
	case best >= 4:
		return "medium", cvss
	case best > 0:
		return "low", cvss
	default:
		return "unknown", cvss
	}
}

func cvss4VectorSeverity(score OSVSeverity) (string, bool) {
	vector := strings.TrimSpace(score.Score)
	if !strings.HasPrefix(vector, "CVSS:4.0/") &&
		!strings.Contains(strings.ToUpper(score.Type), "CVSS_V4") {
		return "", false
	}
	metrics := map[string]string{}
	for _, component := range strings.Split(vector, "/")[1:] {
		key, value, ok := strings.Cut(component, ":")
		if ok {
			metrics[key] = value
		}
	}
	impact := 0
	for _, key := range []string{"VC", "VI", "VA", "SC", "SI", "SA"} {
		switch metrics[key] {
		case "H":
			impact += 2
		case "L":
			impact++
		}
	}
	exploitable := metrics["AV"] == "N" && metrics["AC"] == "L" &&
		metrics["AT"] == "N" && metrics["PR"] == "N" && metrics["UI"] == "N"
	switch {
	case exploitable && impact >= 6:
		return "critical", true
	case impact >= 4:
		return "high", true
	case impact >= 2:
		return "medium", true
	case impact > 0:
		return "low", true
	default:
		return "unknown", true
	}
}

func severityFromMetadata(metadata map[string]any) string {
	for key, value := range metadata {
		if !strings.Contains(strings.ToLower(key), "severity") {
			continue
		}
		label := strings.ToLower(strings.TrimSpace(fmt.Sprint(value)))
		switch label {
		case "critical", "high", "medium", "moderate", "low":
			if label == "moderate" {
				return "medium"
			}
			return label
		}
	}
	return ""
}

func numericCVSS(vector string) (float64, bool) {
	if value, err := strconv.ParseFloat(strings.TrimSpace(vector), 64); err == nil {
		return value, true
	}
	if !strings.HasPrefix(vector, "CVSS:3.") {
		return 0, false
	}
	metrics := map[string]string{}
	for _, component := range strings.Split(vector, "/")[1:] {
		key, value, ok := strings.Cut(component, ":")
		if ok {
			metrics[key] = value
		}
	}
	av := map[string]float64{"N": .85, "A": .62, "L": .55, "P": .2}[metrics["AV"]]
	ac := map[string]float64{"L": .77, "H": .44}[metrics["AC"]]
	ui := map[string]float64{"N": .85, "R": .62}[metrics["UI"]]
	scopeChanged := metrics["S"] == "C"
	prValues := map[string]float64{"N": .85, "L": .62, "H": .27}
	if scopeChanged {
		prValues = map[string]float64{"N": .85, "L": .68, "H": .5}
	}
	pr := prValues[metrics["PR"]]
	impactValue := func(value string) float64 {
		return map[string]float64{"H": .56, "L": .22, "N": 0}[value]
	}
	c, i, a := impactValue(metrics["C"]), impactValue(metrics["I"]), impactValue(metrics["A"])
	if av == 0 || ac == 0 || ui == 0 || pr == 0 {
		return 0, false
	}
	iss := 1 - ((1 - c) * (1 - i) * (1 - a))
	impact := 6.42 * iss
	if scopeChanged {
		impact = 7.52*(iss-.029) - 3.25*math.Pow(iss-.02, 15)
	}
	if impact <= 0 {
		return 0, true
	}
	exploitability := 8.22 * av * ac * pr * ui
	base := math.Min(impact+exploitability, 10)
	if scopeChanged {
		base = math.Min(1.08*(impact+exploitability), 10)
	}
	return math.Ceil(base*10) / 10, true
}

func deduplicateFindings(findings []Finding) []Finding {
	grouped := make(map[string]Finding)
	for _, finding := range findings {
		key := strings.Join([]string{
			finding.AdvisoryID,
			strconv.FormatInt(finding.RepositoryID, 10),
			strings.ToLower(finding.Package),
			finding.Version,
		}, "\x00")
		occurrence := findingOccurrence(finding)
		existing, ok := grouped[key]
		if !ok {
			finding.Occurrences = []FindingOccurrence{occurrence}
			finding.ManifestOccurrenceCount = 1
			finding.ID = findingID(finding)
			grouped[key] = finding
			continue
		}
		duplicate := false
		for _, current := range existing.Occurrences {
			if current.ManifestPath == occurrence.ManifestPath &&
				current.Evidence.Line == occurrence.Evidence.Line &&
				current.Usage == occurrence.Usage &&
				current.Relationship == occurrence.Relationship {
				duplicate = true
				break
			}
		}
		if !duplicate {
			existing.Occurrences = append(existing.Occurrences, occurrence)
		}
		if findingOccurrenceLess(occurrence, findingOccurrence(existing)) {
			existing.ManifestKind = finding.ManifestKind
			existing.ManifestPath = finding.ManifestPath
			existing.Resolution = finding.Resolution
			existing.ResolutionSource = finding.ResolutionSource
			existing.MatchBasis = finding.MatchBasis
			existing.MatchConfidence = finding.MatchConfidence
			existing.Usage = finding.Usage
			existing.Relationship = finding.Relationship
			existing.DeclaredScope = finding.DeclaredScope
			existing.ManifestEvidence = finding.ManifestEvidence
		}
		existing.ManifestOccurrenceCount = len(existing.Occurrences)
		grouped[key] = existing
	}
	output := make([]Finding, 0, len(grouped))
	for _, finding := range grouped {
		slices.SortFunc(finding.Occurrences, func(left, right FindingOccurrence) int {
			if findingOccurrenceLess(left, right) {
				return -1
			}
			if findingOccurrenceLess(right, left) {
				return 1
			}
			return 0
		})
		finding.ManifestOccurrenceCount = len(finding.Occurrences)
		output = append(output, finding)
	}
	return output
}

func findingOccurrence(finding Finding) FindingOccurrence {
	return FindingOccurrence{
		ManifestKind: finding.ManifestKind, ManifestPath: finding.ManifestPath,
		Resolution: finding.Resolution, ResolutionSource: finding.ResolutionSource,
		MatchBasis: finding.MatchBasis, MatchConfidence: finding.MatchConfidence,
		Usage: finding.Usage, Relationship: finding.Relationship,
		DeclaredScope: finding.DeclaredScope, Evidence: finding.ManifestEvidence,
	}
}

func findingOccurrenceLess(left, right FindingOccurrence) bool {
	for _, comparison := range []int{
		usageRank(left.Usage) - usageRank(right.Usage),
		strings.Compare(left.ManifestPath, right.ManifestPath),
		left.Evidence.Line - right.Evidence.Line,
		strings.Compare(left.Relationship, right.Relationship),
	} {
		if comparison != 0 {
			return comparison < 0
		}
	}
	return false
}

func findingID(finding Finding) string {
	value := strings.Join([]string{
		finding.AdvisoryID, strconv.FormatInt(finding.RepositoryID, 10),
		finding.Package, finding.Version,
	}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:16])
}

func sortFindings(findings []Finding) {
	slices.SortFunc(findings, func(left, right Finding) int {
		if difference := severityRank(left.Severity) - severityRank(right.Severity); difference != 0 {
			return difference
		}
		for _, comparison := range []int{
			usageRank(left.Usage) - usageRank(right.Usage),
			strings.Compare(strings.ToLower(left.Repository), strings.ToLower(right.Repository)),
			strings.Compare(strings.ToLower(left.Package), strings.ToLower(right.Package)),
			strings.Compare(left.AdvisoryID, right.AdvisoryID),
			strings.Compare(left.ManifestPath, right.ManifestPath),
			strings.Compare(left.ID, right.ID),
		} {
			if comparison != 0 {
				return comparison
			}
		}
		return 0
	})
}

func severityRank(value string) int {
	switch value {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	default:
		return 4
	}
}

func usageRank(value string) int {
	switch value {
	case "production":
		return 0
	case "implementation":
		return 1
	case "build":
		return 2
	case "development":
		return 3
	case "test":
		return 4
	default:
		return 5
	}
}

func filterFindings(findings []Finding, options AdvisoryOptions) []Finding {
	filtered := make([]Finding, 0, len(findings))
	query := strings.ToLower(options.Query)
	for _, finding := range findings {
		if options.Ecosystem != "" && !strings.EqualFold(finding.Ecosystem, options.Ecosystem) {
			continue
		}
		if options.Severity != "" && !strings.EqualFold(finding.Severity, options.Severity) {
			continue
		}
		if options.Usage != "" {
			matched := false
			for _, occurrence := range finding.Occurrences {
				if strings.EqualFold(occurrence.Usage, options.Usage) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if options.Package != "" && !strings.Contains(
			strings.ToLower(finding.Package), strings.ToLower(options.Package),
		) {
			continue
		}
		occurrenceText := make([]string, 0, len(finding.Occurrences))
		for _, occurrence := range finding.Occurrences {
			occurrenceText = append(occurrenceText, strings.Join([]string{
				occurrence.ManifestPath, occurrence.Usage, occurrence.Relationship,
				occurrence.DeclaredScope,
			}, " "))
		}
		if query != "" && !strings.Contains(strings.ToLower(strings.Join([]string{
			finding.AdvisoryID, strings.Join(finding.Aliases, " "), finding.Summary,
			finding.Package, finding.Repository, finding.ManifestPath, finding.Version,
			strings.Join(occurrenceText, " "),
		}, "\n")), query) {
			continue
		}
		filtered = append(filtered, finding)
	}
	return filtered
}

func findingFacets(findings []Finding) FindingFacets {
	facets := FindingFacets{
		Severities: map[string]int{}, Usages: map[string]int{},
		Repositories: map[string]int{}, Packages: map[string]int{},
	}
	for _, finding := range findings {
		facets.Severities[finding.Severity]++
		facets.Usages[finding.Usage]++
		facets.Repositories[finding.Repository]++
		facets.Packages[finding.Package]++
	}
	return facets
}

func findingHeadlines(findings []Finding) []FindingHeadline {
	type key struct{ severity, usage string }
	counts := map[key]int{}
	repositories := map[key]map[int64]struct{}{}
	for _, finding := range findings {
		current := key{finding.Severity, finding.Usage}
		counts[current]++
		if repositories[current] == nil {
			repositories[current] = map[int64]struct{}{}
		}
		repositories[current][finding.RepositoryID] = struct{}{}
	}
	output := make([]FindingHeadline, 0, len(counts))
	for current, count := range counts {
		output = append(output, FindingHeadline{
			Severity: current.severity, Usage: current.usage,
			FindingCount: count, RepositoryCount: len(repositories[current]),
		})
	}
	slices.SortFunc(output, func(left, right FindingHeadline) int {
		if difference := severityRank(left.Severity) - severityRank(right.Severity); difference != 0 {
			return difference
		}
		return usageRank(left.Usage) - usageRank(right.Usage)
	})
	return output
}

func findingGroups(findings []Finding) []FindingGroup {
	type key struct {
		severity, usage, repository, pkg string
		repositoryID                     int64
	}
	counts := make(map[key]int)
	for _, finding := range findings {
		counts[key{
			severity: finding.Severity, usage: finding.Usage,
			repositoryID: finding.RepositoryID, repository: finding.Repository,
			pkg: finding.Package,
		}]++
	}
	output := make([]FindingGroup, 0, len(counts))
	for current, count := range counts {
		output = append(output, FindingGroup{
			Severity: current.severity, Usage: current.usage,
			RepositoryID: current.repositoryID, Repository: current.repository,
			Package: current.pkg, FindingCount: count,
		})
	}
	slices.SortFunc(output, func(left, right FindingGroup) int {
		for _, comparison := range []int{
			severityRank(left.Severity) - severityRank(right.Severity),
			usageRank(left.Usage) - usageRank(right.Usage),
			strings.Compare(strings.ToLower(left.Repository), strings.ToLower(right.Repository)),
			strings.Compare(strings.ToLower(left.Package), strings.ToLower(right.Package)),
		} {
			if comparison != 0 {
				return comparison
			}
		}
		return 0
	})
	return output
}

func sortedUnique(values []string) []string {
	output := append([]string(nil), values...)
	slices.Sort(output)
	return slices.Compact(output)
}

func advisoryPackages(declarations []Declaration) []AdvisoryPackage {
	byKey := make(map[string]AdvisoryPackage)
	for _, declaration := range declarations {
		osvName, ok := osvEcosystem(declaration.Ecosystem)
		if !ok || strings.TrimSpace(declaration.Package) == "" {
			continue
		}
		pkg := AdvisoryPackage{
			Ecosystem:    strings.ToLower(declaration.Ecosystem),
			OSVEcosystem: osvName, Package: declaration.Package,
		}
		byKey[advisoryPackageKey(pkg.Ecosystem, pkg.Package)] = pkg
	}
	output := make([]AdvisoryPackage, 0, len(byKey))
	for _, pkg := range byKey {
		output = append(output, pkg)
	}
	slices.SortFunc(output, func(left, right AdvisoryPackage) int {
		if comparison := strings.Compare(left.Ecosystem, right.Ecosystem); comparison != 0 {
			return comparison
		}
		return strings.Compare(
			normalizePackage(left.Ecosystem, left.Package),
			normalizePackage(right.Ecosystem, right.Package),
		)
	})
	return output
}

func advisoryQueryDigest(packages []AdvisoryPackage) string {
	hasher := sha256.New()
	for _, pkg := range packages {
		_, _ = io.WriteString(hasher, advisoryPackageKey(pkg.Ecosystem, pkg.Package)+"\n")
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}

func advisoryPackageKey(ecosystem, name string) string {
	ecosystem = strings.ToLower(strings.TrimSpace(ecosystem))
	return ecosystem + "\x00" + normalizePackage(ecosystem, name)
}

func normalizePackage(ecosystem, name string) string {
	name = strings.TrimSpace(name)
	switch strings.ToLower(ecosystem) {
	case "npm", "pypi", "cargo", "nuget":
		name = strings.ToLower(name)
	}
	if strings.EqualFold(ecosystem, "pypi") {
		name = normalizedPyPIPunctuation.ReplaceAllString(name, "-")
	}
	return name
}

func osvEcosystem(ecosystem string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(ecosystem)) {
	case "maven":
		return "Maven", true
	case "npm":
		return "npm", true
	case "pypi":
		return "PyPI", true
	case "go":
		return "Go", true
	case "cargo":
		return "crates.io", true
	case "nuget":
		return "NuGet", true
	default:
		return "", false
	}
}

func localEcosystem(ecosystem string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(ecosystem)) {
	case "maven":
		return "maven", true
	case "npm":
		return "npm", true
	case "pypi":
		return "pypi", true
	case "go":
		return "go", true
	case "crates.io":
		return "cargo", true
	case "nuget":
		return "nuget", true
	default:
		return "", false
	}
}

func versionSystem(ecosystem string) (semver.System, bool) {
	switch strings.ToLower(strings.TrimSpace(ecosystem)) {
	case "maven":
		return semver.Maven, true
	case "npm":
		return semver.NPM, true
	case "pypi":
		return semver.PyPI, true
	case "go":
		return semver.Go, true
	case "cargo":
		return semver.Cargo, true
	case "nuget":
		return semver.NuGet, true
	default:
		return semver.DefaultSystem, false
	}
}

// StartAdvisoryRefresh launches a paced OSV refresh and leaves the previous
// complete snapshot readable if any network or validation step fails.
func (s *Service) StartAdvisoryRefresh(
	inventory graph.Snapshot,
	force bool,
) (AdvisoryRefreshProgress, error) {
	s.advisoryStartMu.Lock()
	defer s.advisoryStartMu.Unlock()
	if s.advisoryDirectory == "" {
		return AdvisoryRefreshProgress{}, errors.New("advisory storage is unavailable")
	}
	s.advisoryMu.RLock()
	if s.advisoryProgress.State == "running" {
		progress := s.advisoryProgress
		s.advisoryMu.RUnlock()
		return progress, nil
	}
	s.advisoryMu.RUnlock()

	packages := advisoryPackages(normalizedDeclarations(inventory))
	if len(packages) > maximumAdvisoryPackages {
		return AdvisoryRefreshProgress{}, fmt.Errorf(
			"advisory refresh covers %d unique packages; the maximum is %d",
			len(packages), maximumAdvisoryPackages,
		)
	}
	digest := advisoryQueryDigest(packages)
	if !force {
		if prior, err := s.readAdvisorySnapshot(); err == nil &&
			prior.QueryDigest == digest && s.now().Sub(prior.RetrievedAt) < defaultAdvisoryTTL {
			progress := AdvisoryRefreshProgress{
				State: "complete", Stage: "cached", Total: len(packages),
				Completed: len(packages), Skipped: len(packages),
				PackageTotal: len(packages), PackageCompleted: len(packages),
				SnapshotVersion:   prior.Version,
				SnapshotTimestamp: prior.RetrievedAt.UTC().Format(time.RFC3339),
			}
			s.setAdvisoryProgress(progress)
			return progress, nil
		}
	}
	started := s.now().UTC()
	progress := AdvisoryRefreshProgress{
		State: "running", Stage: "querying_packages", Total: len(packages),
		PackageTotal: len(packages), StartedAt: started.Format(time.RFC3339),
	}
	s.setAdvisoryProgress(progress)
	go s.runAdvisoryRefresh(packages, digest, started)
	return progress, nil
}

func (s *Service) AdvisoryProgress() AdvisoryRefreshProgress {
	s.advisoryMu.RLock()
	defer s.advisoryMu.RUnlock()
	return s.advisoryProgress
}

// StartAdvisoryScheduler refreshes stale fleet-relevant advisory data daily.
// The initial delay keeps startup and dependency artifact preparation off the
// request path.
func (s *Service) StartAdvisoryScheduler(
	ctx context.Context,
	snapshot func(context.Context) (graph.Snapshot, error),
) {
	if snapshot == nil || s.advisoryDirectory == "" {
		return
	}
	go func() {
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		run := func() {
			inventory, err := snapshot(ctx)
			if err == nil {
				_, _ = s.StartAdvisoryRefresh(inventory, false)
			}
		}
		run()
		// Recheck hourly so a cold start whose dependency artifacts are still
		// building does not wait a full day. StartAdvisoryRefresh enforces the
		// 24-hour snapshot TTL, so successful refreshes still occur daily.
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}

func (s *Service) runAdvisoryRefresh(
	packages []AdvisoryPackage,
	digest string,
	started time.Time,
) {
	ctx, finish := telemetry.StartOperation(s.ctx, telemetry.OperationAdvisoryRefresh, telemetry.Labels{
		Trigger: "background",
	})
	var completionErr error
	defer func() {
		if completionErr == nil {
			completionErr = ctx.Err()
		}
		finish(completionErr)
	}()
	advisoryIDs, err := s.queryAdvisoryIDs(ctx, packages)
	if err != nil {
		completionErr = err
		s.finishAdvisoryRefresh(started, err)
		return
	}
	s.updateAdvisoryProgress(func(progress *AdvisoryRefreshProgress) {
		progress.Stage = "fetching_advisories"
		progress.AdvisoryTotal = len(advisoryIDs)
		progress.Total = progress.PackageTotal + len(advisoryIDs)
		progress.Completed = progress.PackageCompleted
	})
	advisories, failed, fetchErr := s.fetchAdvisories(ctx, advisoryIDs)
	retrievedAt := s.now().UTC()
	snapshot := AdvisorySnapshot{
		SchemaVersion: AdvisorySnapshotSchema, Source: "OSV.dev",
		SourceURL: PublicOSVAPI, RetrievedAt: retrievedAt, QueryDigest: digest,
		Partial: failed > 0, FailedCount: failed,
		Packages: packages, Advisories: advisories,
	}
	snapshot.Version = snapshotVersion(snapshot)
	if err := s.writeAdvisorySnapshot(snapshot); err != nil {
		completionErr = err
		s.finishAdvisoryRefresh(started, err)
		return
	}
	s.updateAdvisoryProgress(func(progress *AdvisoryRefreshProgress) {
		progress.State = "complete"
		progress.Stage = "complete"
		if snapshot.Partial {
			progress.State = "partial"
			progress.Stage = "partial"
			progress.Failed = failed
			if fetchErr != nil {
				progress.Error = fetchErr.Error()
			}
			progress.Skipped = max(
				0, progress.AdvisoryTotal-progress.AdvisoryCompleted-failed,
			)
			progress.Completed = progress.PackageCompleted + progress.AdvisoryCompleted
		} else {
			progress.Completed = progress.Total
		}
		progress.FinishedAt = s.now().UTC().Format(time.RFC3339)
		progress.SnapshotVersion = snapshot.Version
		progress.SnapshotTimestamp = snapshot.RetrievedAt.Format(time.RFC3339)
	})
	completionErr = fetchErr
}

type osvBatchQuery struct {
	Package   OSVPackage `json:"package"`
	PageToken string     `json:"page_token,omitempty"`
	LocalKey  string     `json:"-"`
}

type osvBatchResponse struct {
	Results []struct {
		Vulns []struct {
			ID       string    `json:"id"`
			Modified time.Time `json:"modified"`
		} `json:"vulns"`
		NextPageToken string `json:"next_page_token"`
	} `json:"results"`
}

func (s *Service) queryAdvisoryIDs(ctx context.Context, packages []AdvisoryPackage) ([]string, error) {
	pending := make([]osvBatchQuery, 0, len(packages))
	for _, pkg := range packages {
		pending = append(pending, osvBatchQuery{
			Package:  OSVPackage{Ecosystem: pkg.OSVEcosystem, Name: pkg.Package},
			LocalKey: advisoryPackageKey(pkg.Ecosystem, pkg.Package),
		})
	}
	ids := make(map[string]struct{})
	completed := make(map[string]struct{})
	for len(pending) > 0 {
		count := min(advisoryBatchSize, len(pending))
		batch := append([]osvBatchQuery(nil), pending[:count]...)
		pending = pending[count:]
		response, err := s.queryAdvisoryBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		if len(response.Results) != len(batch) {
			return nil, fmt.Errorf(
				"OSV batch returned %d results for %d queries",
				len(response.Results), len(batch),
			)
		}
		for index, result := range response.Results {
			for _, vulnerability := range result.Vulns {
				if vulnerability.ID != "" {
					ids[vulnerability.ID] = struct{}{}
				}
			}
			if len(ids) > maximumAdvisories {
				return nil, fmt.Errorf(
					"OSV returned more than the bounded maximum of %d advisories",
					maximumAdvisories,
				)
			}
			if result.NextPageToken != "" {
				next := batch[index]
				next.PageToken = result.NextPageToken
				pending = append(pending, next)
			} else if _, ok := completed[batch[index].LocalKey]; !ok {
				completed[batch[index].LocalKey] = struct{}{}
				s.updateAdvisoryProgress(func(progress *AdvisoryRefreshProgress) {
					progress.PackageCompleted++
					progress.Completed = progress.PackageCompleted
				})
			}
		}
		if len(pending) > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	output := make([]string, 0, len(ids))
	for id := range ids {
		output = append(output, id)
	}
	slices.Sort(output)
	return output, nil
}

func (s *Service) queryAdvisoryBatch(ctx context.Context, queries []osvBatchQuery) (osvBatchResponse, error) {
	body, err := json.Marshal(struct {
		Queries []osvBatchQuery `json:"queries"`
	}{Queries: queries})
	if err != nil {
		return osvBatchResponse{}, err
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		strings.TrimRight(s.osvBaseURL(), "/")+"/v1/querybatch",
		strings.NewReader(string(body)),
	)
	if err != nil {
		return osvBatchResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "RepoKarta dependency advisories")
	response, err := s.client.Do(request)
	if err != nil {
		return osvBatchResponse{}, fmt.Errorf("query OSV batch: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return osvBatchResponse{}, fmt.Errorf("query OSV batch: HTTP %d", response.StatusCode)
	}
	var output osvBatchResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maximumAdvisoryBatchBody)).Decode(&output); err != nil {
		return osvBatchResponse{}, fmt.Errorf("decode OSV batch: %w", err)
	}
	return output, nil
}

func (s *Service) fetchAdvisories(ctx context.Context, ids []string) ([]OSVAdvisory, int, error) {
	if len(ids) == 0 {
		return []OSVAdvisory{}, 0, nil
	}
	type result struct {
		advisory OSVAdvisory
		err      error
	}
	jobs := make(chan string)
	results := make(chan result, len(ids))
	fatalSignal := make(chan struct{})
	var fatalOnce sync.Once
	workerCount := min(4, len(ids))
	var workers sync.WaitGroup
	pace := time.NewTicker(100 * time.Millisecond)
	defer pace.Stop()
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range jobs {
				select {
				case <-ctx.Done():
					fatalOnce.Do(func() { close(fatalSignal) })
					results <- result{err: ctx.Err()}
					return
				case <-pace.C:
				}
				advisory, err := s.fetchAdvisory(ctx, id)
				if isFatalAdvisoryError(err) {
					fatalOnce.Do(func() { close(fatalSignal) })
					results <- result{advisory: advisory, err: err}
					return
				}
				results <- result{advisory: advisory, err: err}
			}
		}()
	}
	go func() {
	dispatch:
		for _, id := range ids {
			select {
			case jobs <- id:
			case <-fatalSignal:
				break
			}
			if isClosed(fatalSignal) {
				break dispatch
			}
		}
		close(jobs)
		workers.Wait()
		close(results)
	}()
	output := make([]OSVAdvisory, 0, len(ids))
	var firstError error
	failed := 0
	for fetched := range results {
		if fetched.err != nil {
			failed++
			if firstError == nil {
				firstError = fetched.err
			}
			continue
		}
		output = append(output, fetched.advisory)
		s.updateAdvisoryProgress(func(progress *AdvisoryRefreshProgress) {
			progress.AdvisoryCompleted++
			progress.Completed = progress.PackageCompleted + progress.AdvisoryCompleted
		})
	}
	slices.SortFunc(output, func(left, right OSVAdvisory) int {
		return strings.Compare(left.ID, right.ID)
	})
	if firstError != nil {
		return output, failed, firstError
	}
	return output, 0, nil
}

func isClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

type fatalAdvisoryError struct{ error }

func isFatalAdvisoryError(err error) bool {
	var fatal fatalAdvisoryError
	return errors.As(err, &fatal) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

func (s *Service) fetchAdvisory(ctx context.Context, id string) (OSVAdvisory, error) {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		strings.TrimRight(s.osvBaseURL(), "/")+"/v1/vulns/"+url.PathEscape(id),
		nil,
	)
	if err != nil {
		return OSVAdvisory{}, err
	}
	request.Header.Set("User-Agent", "RepoKarta dependency advisories")
	response, err := s.client.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return OSVAdvisory{}, fatalAdvisoryError{err}
		}
		return OSVAdvisory{}, fmt.Errorf("fetch OSV advisory %s: %w", id, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusUnauthorized ||
			response.StatusCode == http.StatusForbidden ||
			response.StatusCode == http.StatusTooManyRequests {
			return OSVAdvisory{}, fatalAdvisoryError{fmt.Errorf(
				"fetch OSV advisory %s: HTTP %d", id, response.StatusCode,
			)}
		}
		return OSVAdvisory{}, fmt.Errorf("fetch OSV advisory %s: HTTP %d", id, response.StatusCode)
	}
	var advisory OSVAdvisory
	if err := json.NewDecoder(io.LimitReader(response.Body, maximumAdvisoryBody)).Decode(&advisory); err != nil {
		return OSVAdvisory{}, fmt.Errorf("decode OSV advisory %s: %w", id, err)
	}
	if advisory.ID != id {
		return OSVAdvisory{}, fatalAdvisoryError{fmt.Errorf(
			"OSV advisory identity mismatch: requested %s, received %s", id, advisory.ID,
		)}
	}
	return advisory, nil
}

func (s *Service) osvBaseURL() string {
	if strings.TrimSpace(s.advisoryBaseURL) != "" {
		return s.advisoryBaseURL
	}
	return PublicOSVAPI
}

func snapshotVersion(snapshot AdvisorySnapshot) string {
	content, _ := json.Marshal(struct {
		Packages   []AdvisoryPackage `json:"packages"`
		Advisories []OSVAdvisory     `json:"advisories"`
		Partial    bool              `json:"partial,omitempty"`
		Failed     int               `json:"failed,omitempty"`
	}{
		Packages: snapshot.Packages, Advisories: snapshot.Advisories,
		Partial: snapshot.Partial, Failed: snapshot.FailedCount,
	})
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (s *Service) advisorySnapshotPath() string {
	return filepath.Join(s.advisoryDirectory, "osv-snapshot.json")
}

func (s *Service) readAdvisorySnapshot() (*AdvisorySnapshot, error) {
	if s.advisoryDirectory == "" {
		return nil, os.ErrNotExist
	}
	s.advisoryFileMu.RLock()
	defer s.advisoryFileMu.RUnlock()
	file, err := os.Open(s.advisorySnapshotPath())
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect OSV advisory snapshot: %w", err)
	}
	if info.Size() > maximumSnapshotBody {
		return nil, fmt.Errorf(
			"OSV advisory snapshot is %d bytes; the maximum is %d",
			info.Size(), maximumSnapshotBody,
		)
	}
	var snapshot AdvisorySnapshot
	if err := json.NewDecoder(io.LimitReader(file, maximumSnapshotBody+1)).Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("decode OSV advisory snapshot: %w", err)
	}
	if snapshot.SchemaVersion != AdvisorySnapshotSchema {
		return nil, fmt.Errorf(
			"OSV advisory snapshot schema %d is unsupported; expected %d",
			snapshot.SchemaVersion, AdvisorySnapshotSchema,
		)
	}
	if snapshot.Version == "" || snapshot.Version != snapshotVersion(snapshot) {
		return nil, errors.New("OSV advisory snapshot content hash does not match its recorded version")
	}
	return &snapshot, nil
}

func (s *Service) writeAdvisorySnapshot(snapshot AdvisorySnapshot) error {
	content, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode OSV advisory snapshot: %w", err)
	}
	content = append(content, '\n')
	s.advisoryFileMu.Lock()
	defer s.advisoryFileMu.Unlock()
	temporary, err := os.CreateTemp(s.advisoryDirectory, "osv-snapshot-*.tmp")
	if err != nil {
		return fmt.Errorf("create OSV advisory snapshot: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return fmt.Errorf("write OSV advisory snapshot: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync OSV advisory snapshot: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close OSV advisory snapshot: %w", err)
	}
	target := s.advisorySnapshotPath()
	if err := os.Rename(temporaryName, target); err == nil {
		return nil
	}
	// Windows does not replace an existing file through os.Rename. Readers are
	// serialized by advisoryFileMu while the verified replacement is published.
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace OSV advisory snapshot: %w", err)
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return fmt.Errorf("publish OSV advisory snapshot: %w", err)
	}
	return nil
}

func (s *Service) setAdvisoryProgress(progress AdvisoryRefreshProgress) {
	s.advisoryMu.Lock()
	s.advisoryProgress = progress
	s.advisoryMu.Unlock()
}

func (s *Service) updateAdvisoryProgress(update func(*AdvisoryRefreshProgress)) {
	s.advisoryMu.Lock()
	update(&s.advisoryProgress)
	s.advisoryMu.Unlock()
}

func (s *Service) finishAdvisoryRefresh(started time.Time, err error) {
	s.updateAdvisoryProgress(func(progress *AdvisoryRefreshProgress) {
		progress.State = "error"
		progress.Stage = "error"
		progress.Failed++
		progress.Error = err.Error()
		progress.FinishedAt = s.now().UTC().Format(time.RFC3339)
		if progress.StartedAt == "" {
			progress.StartedAt = started.UTC().Format(time.RFC3339)
		}
	})
}
