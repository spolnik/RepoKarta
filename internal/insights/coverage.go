package insights

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

func parseCoverage(format string, content []byte) ([]Observation, []string, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "lcov":
		return parseLCOV(content)
	case "jacoco", "jacoco-xml":
		return parseJaCoCo(content)
	case "cobertura", "cobertura-xml":
		return parseCobertura(content)
	default:
		return nil, nil, fmt.Errorf("unsupported coverage format %q", format)
	}
}

type coverageCounts struct {
	linesFound    float64
	linesHit      float64
	branchesFound float64
	branchesHit   float64
}

func (c coverageCounts) observations(file string, metadata map[string]any) []Observation {
	output := []Observation{
		metric("coverage.lines.covered", c.linesHit, "lines", file, metadata),
		metric("coverage.lines.total", c.linesFound, "lines", file, metadata),
	}
	if c.linesFound > 0 {
		output = append(output, metric("coverage.line", 100*c.linesHit/c.linesFound, "percent", file, metadata))
	} else {
		output = append(output, Observation{
			Kind: KindMetric, Key: "coverage.line", Unit: "percent", Path: file,
			State: StateSkipped, Confidence: "reported",
			Message: "report contains no executable lines for this scope", Metadata: metadata,
		})
	}
	output = append(output,
		metric("coverage.branches.covered", c.branchesHit, "branches", file, metadata),
		metric("coverage.branches.total", c.branchesFound, "branches", file, metadata),
	)
	if c.branchesFound > 0 {
		output = append(output, metric("coverage.branch", 100*c.branchesHit/c.branchesFound, "percent", file, metadata))
	} else {
		output = append(output, Observation{
			Kind: KindMetric, Key: "coverage.branch", Unit: "percent", Path: file,
			State: StateSkipped, Confidence: "reported",
			Message: "report contains no branch measurements for this scope", Metadata: metadata,
		})
	}
	return output
}

func metric(key string, value float64, unit, file string, metadata map[string]any) Observation {
	return Observation{
		Kind: KindMetric, Key: key, Value: number(value), Unit: unit, Path: file,
		State: StateMeasured, Confidence: "reported", Metadata: cloneMetadata(metadata),
	}
}

func number(value float64) *float64 { return &value }

func cloneMetadata(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func parseLCOV(content []byte) ([]Observation, []string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	var output []Observation
	var warnings []string
	var current string
	var counts coverageCounts
	record := 0
	flush := func() {
		if current == "" {
			return
		}
		record++
		output = append(output, counts.observations(current, map[string]any{
			"format": "lcov", "scope": "file", "record": record,
		})...)
		current = ""
		counts = coverageCounts{}
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "SF:"):
			flush()
			current = strings.TrimSpace(strings.TrimPrefix(line, "SF:"))
		case strings.HasPrefix(line, "LF:"):
			counts.linesFound = parseLCOVNumber(line, "LF:", &warnings)
		case strings.HasPrefix(line, "LH:"):
			counts.linesHit = parseLCOVNumber(line, "LH:", &warnings)
		case strings.HasPrefix(line, "BRF:"):
			counts.branchesFound = parseLCOVNumber(line, "BRF:", &warnings)
		case strings.HasPrefix(line, "BRH:"):
			counts.branchesHit = parseLCOVNumber(line, "BRH:", &warnings)
		case line == "end_of_record":
			flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, warnings, fmt.Errorf("read LCOV: %w", err)
	}
	flush()
	if len(output) == 0 {
		return nil, warnings, errors.New("LCOV contains no source file records")
	}
	return appendCoverageAggregate(output), warnings, nil
}

func parseLCOVNumber(line, prefix string, warnings *[]string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 64)
	if err != nil || value < 0 {
		*warnings = append(*warnings, "ignored invalid "+strings.TrimSuffix(prefix, ":")+" counter")
		return 0
	}
	return value
}

type jacocoReport struct {
	XMLName  xml.Name        `xml:"report"`
	Name     string          `xml:"name,attr"`
	Counters []xmlCounter    `xml:"counter"`
	Packages []jacocoPackage `xml:"package"`
}

type xmlCounter struct {
	Type    string  `xml:"type,attr"`
	Missed  float64 `xml:"missed,attr"`
	Covered float64 `xml:"covered,attr"`
}

type jacocoPackage struct {
	Name        string             `xml:"name,attr"`
	SourceFiles []jacocoSourceFile `xml:"sourcefile"`
}

type jacocoSourceFile struct {
	Name     string       `xml:"name,attr"`
	Counters []xmlCounter `xml:"counter"`
}

func parseJaCoCo(content []byte) ([]Observation, []string, error) {
	var report jacocoReport
	if err := decodeXML(content, &report); err != nil {
		return nil, nil, fmt.Errorf("parse JaCoCo XML: %w", err)
	}
	var output []Observation
	for _, pkg := range report.Packages {
		for _, file := range pkg.SourceFiles {
			counts := countersToCoverage(file.Counters)
			filePath := path.Join(strings.Trim(pkg.Name, "/"), file.Name)
			output = append(output, counts.observations(filePath, map[string]any{
				"format": "jacoco", "scope": "file", "report": report.Name,
			})...)
		}
	}
	if len(output) == 0 && len(report.Counters) > 0 {
		output = countersToCoverage(report.Counters).observations("", map[string]any{
			"format": "jacoco", "scope": "report", "report": report.Name,
		})
	}
	if len(output) == 0 {
		return nil, nil, errors.New("JaCoCo report contains no coverage counters")
	}
	return appendCoverageAggregate(output), nil, nil
}

func countersToCoverage(counters []xmlCounter) coverageCounts {
	var output coverageCounts
	for _, counter := range counters {
		switch strings.ToUpper(counter.Type) {
		case "LINE":
			output.linesFound = counter.Missed + counter.Covered
			output.linesHit = counter.Covered
		case "BRANCH":
			output.branchesFound = counter.Missed + counter.Covered
			output.branchesHit = counter.Covered
		}
	}
	return output
}

type coberturaReport struct {
	XMLName         xml.Name           `xml:"coverage"`
	LinesValid      float64            `xml:"lines-valid,attr"`
	LinesCovered    float64            `xml:"lines-covered,attr"`
	BranchesValid   float64            `xml:"branches-valid,attr"`
	BranchesCovered float64            `xml:"branches-covered,attr"`
	Packages        []coberturaPackage `xml:"packages>package"`
}

type coberturaPackage struct {
	Name    string           `xml:"name,attr"`
	Classes []coberturaClass `xml:"classes>class"`
}

type coberturaClass struct {
	Name       string          `xml:"name,attr"`
	Filename   string          `xml:"filename,attr"`
	LineRate   float64         `xml:"line-rate,attr"`
	BranchRate float64         `xml:"branch-rate,attr"`
	Lines      []coberturaLine `xml:"lines>line"`
}

type coberturaLine struct {
	Number            int    `xml:"number,attr"`
	Hits              int    `xml:"hits,attr"`
	Branch            bool   `xml:"branch,attr"`
	ConditionCoverage string `xml:"condition-coverage,attr"`
}

func parseCobertura(content []byte) ([]Observation, []string, error) {
	var report coberturaReport
	if err := decodeXML(content, &report); err != nil {
		return nil, nil, fmt.Errorf("parse Cobertura XML: %w", err)
	}
	var output []Observation
	for _, pkg := range report.Packages {
		for _, class := range pkg.Classes {
			counts := coverageCounts{}
			for _, line := range class.Lines {
				counts.linesFound++
				if line.Hits > 0 {
					counts.linesHit++
				}
				if line.Branch {
					covered, total := parseConditionCoverage(line.ConditionCoverage)
					counts.branchesHit += covered
					counts.branchesFound += total
				}
			}
			if len(class.Lines) == 0 {
				// Some producers provide rates but omit detailed line records.
				if class.LineRate > 0 {
					counts.linesFound = 100
					counts.linesHit = 100 * class.LineRate
				}
				if class.BranchRate > 0 {
					counts.branchesFound = 100
					counts.branchesHit = 100 * class.BranchRate
				}
			}
			output = append(output, counts.observations(class.Filename, map[string]any{
				"format": "cobertura", "scope": "class", "package": pkg.Name,
				"class": class.Name,
			})...)
		}
	}
	if len(output) == 0 && report.LinesValid+report.BranchesValid > 0 {
		output = coverageCounts{
			linesFound: report.LinesValid, linesHit: report.LinesCovered,
			branchesFound: report.BranchesValid, branchesHit: report.BranchesCovered,
		}.observations("", map[string]any{"format": "cobertura", "scope": "report"})
	}
	if len(output) == 0 {
		return nil, nil, errors.New("Cobertura report contains no coverage measurements")
	}
	return appendCoverageAggregate(output), nil, nil
}

func decodeXML(content []byte, output any) error {
	decoder := xml.NewDecoder(io.LimitReader(bytes.NewReader(content), int64(len(content))))
	decoder.Strict = true
	return decoder.Decode(output)
}

func parseConditionCoverage(value string) (float64, float64) {
	start := strings.Index(value, "(")
	end := strings.Index(value, ")")
	if start < 0 || end <= start {
		return 0, 0
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimSpace(value[start+1:end]), ")"), "/")
	if len(parts) != 2 {
		return 0, 0
	}
	covered, _ := strconv.ParseFloat(parts[0], 64)
	total, _ := strconv.ParseFloat(parts[1], 64)
	return covered, total
}

func appendCoverageAggregate(observations []Observation) []Observation {
	var counts coverageCounts
	fileCount := 0
	for _, observation := range observations {
		if observation.Path == "" || observation.Value == nil {
			continue
		}
		switch observation.Key {
		case "coverage.lines.covered":
			counts.linesHit += *observation.Value
			fileCount++
		case "coverage.lines.total":
			counts.linesFound += *observation.Value
		case "coverage.branches.covered":
			counts.branchesHit += *observation.Value
		case "coverage.branches.total":
			counts.branchesFound += *observation.Value
		}
	}
	if fileCount == 0 {
		return observations
	}
	aggregate := counts.observations("", map[string]any{
		"scope": "aggregate", "files": fileCount,
	})
	return append(observations, aggregate...)
}
