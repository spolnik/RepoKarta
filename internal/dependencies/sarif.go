package dependencies

import (
	"encoding/json"
	"fmt"
	"slices"
)

type advisorySARIFLog struct {
	Version string             `json:"version"`
	Schema  string             `json:"$schema"`
	Runs    []advisorySARIFRun `json:"runs"`
}

type advisorySARIFRun struct {
	Tool       advisorySARIFTool     `json:"tool"`
	Results    []advisorySARIFResult `json:"results"`
	Properties map[string]any        `json:"properties"`
}

type advisorySARIFTool struct {
	Driver advisorySARIFDriver `json:"driver"`
}

type advisorySARIFDriver struct {
	Name            string              `json:"name"`
	InformationURI  string              `json:"informationUri"`
	SemanticVersion string              `json:"semanticVersion"`
	Rules           []advisorySARIFRule `json:"rules"`
}

type advisorySARIFRule struct {
	ID               string               `json:"id"`
	Name             string               `json:"name"`
	ShortDescription advisorySARIFMessage `json:"shortDescription"`
	HelpURI          string               `json:"helpUri"`
	Properties       map[string]any       `json:"properties"`
}

type advisorySARIFResult struct {
	RuleID              string                  `json:"ruleId"`
	Level               string                  `json:"level"`
	Message             advisorySARIFMessage    `json:"message"`
	Locations           []advisorySARIFLocation `json:"locations"`
	PartialFingerprints map[string]string       `json:"partialFingerprints"`
	Properties          map[string]any          `json:"properties"`
}

type advisorySARIFMessage struct {
	Text string `json:"text"`
}

type advisorySARIFLocation struct {
	PhysicalLocation advisorySARIFPhysicalLocation `json:"physicalLocation"`
}

type advisorySARIFPhysicalLocation struct {
	ArtifactLocation advisorySARIFArtifactLocation `json:"artifactLocation"`
	Region           advisorySARIFRegion           `json:"region"`
}

type advisorySARIFArtifactLocation struct {
	URI string `json:"uri"`
}

type advisorySARIFRegion struct {
	StartLine int `json:"startLine"`
	EndLine   int `json:"endLine,omitempty"`
}

// FindingsSARIF exports exactly the already-computed advisory findings. The
// report deliberately remains advisory evidence; importing it into Insights
// does not turn RepoKarta into a CI gate.
func FindingsSARIF(response FindingResponse, applicationVersion string) ([]byte, error) {
	rulesByID := make(map[string]advisorySARIFRule)
	results := make([]advisorySARIFResult, 0, len(response.Findings))
	for _, finding := range response.Findings {
		if _, ok := rulesByID[finding.AdvisoryID]; !ok {
			rulesByID[finding.AdvisoryID] = advisorySARIFRule{
				ID: finding.AdvisoryID, Name: finding.AdvisoryID,
				ShortDescription: advisorySARIFMessage{Text: firstNonEmpty(finding.Summary, finding.AdvisoryID)},
				HelpURI:          finding.AdvisoryEvidence.AdvisoryURL,
				Properties: map[string]any{
					"aliases":            finding.Aliases,
					"severity":           finding.Severity,
					"cvss":               finding.CVSS,
					"affected_range":     finding.AffectedRange,
					"fixed_version":      finding.FixedVersion,
					"source":             finding.AdvisoryEvidence.Source,
					"snapshot_version":   finding.AdvisoryEvidence.SnapshotVersion,
					"snapshot_timestamp": finding.AdvisoryEvidence.SnapshotTimestamp,
				},
			}
		}
		line := finding.ManifestEvidence.Line
		if line <= 0 {
			line = 1
		}
		results = append(results, advisorySARIFResult{
			RuleID: finding.AdvisoryID,
			Level:  sarifLevel(finding.Severity),
			Message: advisorySARIFMessage{Text: fmt.Sprintf(
				"%s %s is affected by %s (%s scope, %s match)",
				finding.Package, finding.Version, finding.AdvisoryID,
				finding.Usage, finding.MatchBasis,
			)},
			Locations: []advisorySARIFLocation{{
				PhysicalLocation: advisorySARIFPhysicalLocation{
					ArtifactLocation: advisorySARIFArtifactLocation{URI: finding.ManifestPath},
					Region:           advisorySARIFRegion{StartLine: line, EndLine: line},
				},
			}},
			PartialFingerprints: map[string]string{"repoKartaDependencyFinding": finding.ID},
			Properties: map[string]any{
				"repository_id":           finding.RepositoryID,
				"repository":              finding.Repository,
				"revision":                finding.Revision,
				"manifest_kind":           finding.ManifestKind,
				"manifest_path":           finding.ManifestPath,
				"manifest_evidence_url":   finding.ManifestEvidence.URL,
				"ecosystem":               finding.Ecosystem,
				"package":                 finding.Package,
				"version":                 finding.Version,
				"match_basis":             finding.MatchBasis,
				"match_confidence":        finding.MatchConfidence,
				"resolution":              finding.Resolution,
				"resolution_source":       finding.ResolutionSource,
				"usage":                   finding.Usage,
				"relationship":            finding.Relationship,
				"declared_scope":          finding.DeclaredScope,
				"fixed_version":           finding.FixedVersion,
				"latest_stable":           finding.LatestStable,
				"advisory_evidence_url":   finding.AdvisoryEvidence.AdvisoryURL,
				"snapshot_version":        finding.AdvisoryEvidence.SnapshotVersion,
				"snapshot_timestamp":      finding.AdvisoryEvidence.SnapshotTimestamp,
				"repoKarta_advisory_only": true,
				"repoKarta_ci_gate":       false,
			},
		})
	}
	ruleIDs := make([]string, 0, len(rulesByID))
	for id := range rulesByID {
		ruleIDs = append(ruleIDs, id)
	}
	slices.Sort(ruleIDs)
	rules := make([]advisorySARIFRule, 0, len(ruleIDs))
	for _, id := range ruleIDs {
		rules = append(rules, rulesByID[id])
	}
	log := advisorySARIFLog{
		Version: "2.1.0",
		Schema:  "https://json.schemastore.org/sarif-2.1.0.json",
		Runs: []advisorySARIFRun{{
			Tool: advisorySARIFTool{Driver: advisorySARIFDriver{
				Name:            "RepoKarta dependency advisories",
				InformationURI:  "https://osv.dev",
				SemanticVersion: applicationVersion,
				Rules:           rules,
			}},
			Results: results,
			Properties: map[string]any{
				"check_state":                   response.CheckState,
				"advisory_only":                 true,
				"ci_gate":                       false,
				"snapshot":                      response.Snapshot,
				"checked_declaration_count":     response.CheckedDeclarationCount,
				"skipped_no_version_count":      response.SkippedNoVersionCount,
				"skipped_invalid_version_count": response.SkippedInvalidVersionCount,
				"not_in_snapshot_count":         response.NotInSnapshotCount,
				"uncovered_ecosystems":          response.UncoveredEcosystems,
			},
		}},
	}
	content, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode dependency findings SARIF: %w", err)
	}
	return append(content, '\n'), nil
}

func sarifLevel(severity string) string {
	switch severity {
	case "critical", "high":
		return "error"
	case "medium":
		return "warning"
	case "low":
		return "note"
	default:
		return "none"
	}
}
