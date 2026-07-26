package insights

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const maximumImportedObservations = 50_000

type sarifLog struct {
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool struct {
		Driver sarifDriver `json:"driver"`
	} `json:"tool"`
	Results     []sarifResult     `json:"results"`
	Invocations []sarifInvocation `json:"invocations"`
}

type sarifDriver struct {
	Name            string      `json:"name"`
	Version         string      `json:"version"`
	SemanticVersion string      `json:"semanticVersion"`
	Rules           []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	ShortDescription struct {
		Text string `json:"text"`
	} `json:"shortDescription"`
	HelpURI    string         `json:"helpUri"`
	Properties map[string]any `json:"properties"`
}

type sarifInvocation struct {
	ExecutionSuccessful bool           `json:"executionSuccessful"`
	CommandLine         string         `json:"commandLine"`
	Properties          map[string]any `json:"properties"`
}

type sarifResult struct {
	RuleID              string             `json:"ruleId"`
	RuleIndex           int                `json:"ruleIndex"`
	Level               string             `json:"level"`
	Message             sarifMessage       `json:"message"`
	Locations           []sarifLocation    `json:"locations"`
	PartialFingerprints map[string]string  `json:"partialFingerprints"`
	Fingerprints        map[string]string  `json:"fingerprints"`
	Suppressions        []sarifSuppression `json:"suppressions"`
	CodeFlows           json.RawMessage    `json:"codeFlows"`
	Properties          map[string]any     `json:"properties"`
}

type sarifMessage struct {
	Text     string `json:"text"`
	Markdown string `json:"markdown"`
}

type sarifLocation struct {
	PhysicalLocation struct {
		ArtifactLocation struct {
			URI       string `json:"uri"`
			URIBaseID string `json:"uriBaseId"`
		} `json:"artifactLocation"`
		Region struct {
			StartLine   int `json:"startLine"`
			StartColumn int `json:"startColumn"`
			EndLine     int `json:"endLine"`
			EndColumn   int `json:"endColumn"`
		} `json:"region"`
	} `json:"physicalLocation"`
}

type sarifSuppression struct {
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	Justification string `json:"justification"`
}

func parseSARIF(content []byte) ([]Observation, map[string]string, []string, error) {
	var log sarifLog
	if err := json.Unmarshal(content, &log); err != nil {
		return nil, nil, nil, fmt.Errorf("parse SARIF: %w", err)
	}
	if log.Version != "2.1.0" {
		return nil, nil, nil, fmt.Errorf("SARIF version %q is unsupported; expected 2.1.0", log.Version)
	}
	if len(log.Runs) == 0 {
		return nil, nil, nil, errors.New("SARIF contains no runs")
	}
	var observations []Observation
	var warnings []string
	runMetadata := map[string]string{"sarif_version": log.Version}
	for runIndex, run := range log.Runs {
		if run.Tool.Driver.Name != "" {
			runMetadata[fmt.Sprintf("tool_%d", runIndex)] = run.Tool.Driver.Name
		}
		rules := make(map[string]sarifRule, len(run.Tool.Driver.Rules))
		for _, rule := range run.Tool.Driver.Rules {
			rules[rule.ID] = rule
		}
		for _, result := range run.Results {
			if len(observations) >= maximumImportedObservations {
				warnings = append(warnings, fmt.Sprintf("SARIF results truncated at %d observations", maximumImportedObservations))
				return observations, runMetadata, warnings, nil
			}
			rule := rules[result.RuleID]
			if result.RuleID == "" && result.RuleIndex >= 0 && result.RuleIndex < len(run.Tool.Driver.Rules) {
				rule = run.Tool.Driver.Rules[result.RuleIndex]
				result.RuleID = rule.ID
			}
			message := strings.TrimSpace(result.Message.Text)
			if message == "" {
				message = strings.TrimSpace(result.Message.Markdown)
			}
			location := sarifLocation{}
			if len(result.Locations) > 0 {
				location = result.Locations[0]
			}
			file := strings.TrimPrefix(location.PhysicalLocation.ArtifactLocation.URI, "file://")
			file = filepath.ToSlash(file)
			fingerprint := firstFingerprint(result.PartialFingerprints)
			if fingerprint == "" {
				fingerprint = firstFingerprint(result.Fingerprints)
			}
			suppressed := len(result.Suppressions) > 0
			metadata := map[string]any{
				"sarif_run":         runIndex,
				"rule_name":         rule.Name,
				"rule_description":  rule.ShortDescription.Text,
				"rule_help_uri":     rule.HelpURI,
				"rule_properties":   rule.Properties,
				"result_properties": result.Properties,
				"suppressions":      result.Suppressions,
				"start_column":      location.PhysicalLocation.Region.StartColumn,
				"end_column":        location.PhysicalLocation.Region.EndColumn,
				"uri_base_id":       location.PhysicalLocation.ArtifactLocation.URIBaseID,
				"tool":              run.Tool.Driver.Name,
				"tool_version":      firstNonEmpty(run.Tool.Driver.SemanticVersion, run.Tool.Driver.Version),
			}
			var codeFlows any
			if len(result.CodeFlows) > 0 && string(result.CodeFlows) != "null" {
				_ = json.Unmarshal(result.CodeFlows, &codeFlows)
			}
			observations = append(observations, Observation{
				Kind: KindFinding, Key: firstNonEmpty(result.RuleID, rule.Name, "sarif.finding"),
				Severity: normalizeSeverity(result.Level), Message: message, Path: file,
				StartLine:   location.PhysicalLocation.Region.StartLine,
				EndLine:     location.PhysicalLocation.Region.EndLine,
				Fingerprint: fingerprint, Suppressed: suppressed, State: StateMeasured,
				Confidence: "reported", Metadata: metadata, CodeFlows: codeFlows,
			})
		}
	}
	if len(observations) == 0 {
		warnings = append(warnings, "SARIF run contains no findings")
	}
	return observations, runMetadata, warnings, nil
}

func firstFingerprint(values map[string]string) string {
	var bestKey, bestValue string
	for key, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if bestKey == "" || key < bestKey {
			bestKey, bestValue = key, value
		}
	}
	return bestValue
}

func normalizeSeverity(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "error", "critical", "blocker":
		return "error"
	case "warning", "warn", "major":
		return "warning"
	case "note", "info", "minor":
		return "note"
	case "none", "":
		return "none"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

type semgrepReport struct {
	Version string          `json:"version"`
	Results []semgrepResult `json:"results"`
	Errors  []any           `json:"errors"`
}

type semgrepResult struct {
	CheckID string `json:"check_id"`
	Path    string `json:"path"`
	Start   struct {
		Line int `json:"line"`
		Col  int `json:"col"`
	} `json:"start"`
	End struct {
		Line int `json:"line"`
		Col  int `json:"col"`
	} `json:"end"`
	Extra struct {
		Message     string         `json:"message"`
		Severity    string         `json:"severity"`
		Metadata    map[string]any `json:"metadata"`
		Fingerprint string         `json:"fingerprint"`
		IsIgnored   bool           `json:"is_ignored"`
	} `json:"extra"`
}

func parseSemgrep(content []byte) ([]Observation, map[string]string, []string, error) {
	var report semgrepReport
	if err := json.Unmarshal(content, &report); err != nil {
		return nil, nil, nil, fmt.Errorf("parse Semgrep JSON: %w", err)
	}
	if len(report.Results) > maximumImportedObservations {
		report.Results = report.Results[:maximumImportedObservations]
	}
	output := make([]Observation, 0, len(report.Results))
	for _, result := range report.Results {
		output = append(output, Observation{
			Kind: KindFinding, Key: result.CheckID,
			Severity: normalizeSeverity(result.Extra.Severity),
			Message:  result.Extra.Message, Path: filepath.ToSlash(result.Path),
			StartLine: result.Start.Line, EndLine: result.End.Line,
			Fingerprint: result.Extra.Fingerprint, Suppressed: result.Extra.IsIgnored,
			State: StateMeasured, Confidence: "reported",
			Metadata: map[string]any{
				"format": "semgrep-json", "rule_metadata": result.Extra.Metadata,
				"start_column": result.Start.Col, "end_column": result.End.Col,
			},
		})
	}
	var warnings []string
	if len(report.Errors) > 0 {
		warnings = append(warnings, fmt.Sprintf("Semgrep reported %d scanner errors", len(report.Errors)))
	}
	return output, map[string]string{"semgrep_version": report.Version}, warnings, nil
}
