package insights

import (
	"strings"
	"testing"
)

func TestParseLCOVPreservesLineBranchAndAggregateStates(t *testing.T) {
	observations, warnings, err := parseLCOV([]byte(strings.Join([]string{
		"TN:",
		"SF:internal/service.go",
		"LF:10",
		"LH:8",
		"BRF:4",
		"BRH:3",
		"end_of_record",
		"SF:internal/empty.go",
		"LF:0",
		"LH:0",
		"BRF:0",
		"BRH:0",
		"end_of_record",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v", warnings)
	}
	assertObservation(t, observations, "internal/service.go", "coverage.line", StateMeasured, 80)
	assertObservation(t, observations, "internal/service.go", "coverage.branch", StateMeasured, 75)
	assertObservation(t, observations, "internal/empty.go", "coverage.line", StateSkipped, 0)
	assertObservation(t, observations, "", "coverage.line", StateMeasured, 80)
}

func TestParseJaCoCoAndCobertura(t *testing.T) {
	jacoco := []byte(`<report name="service"><package name="internal">
<sourcefile name="service.go"><counter type="LINE" missed="2" covered="8"/>
<counter type="BRANCH" missed="1" covered="3"/></sourcefile></package></report>`)
	observations, _, err := parseJaCoCo(jacoco)
	if err != nil {
		t.Fatal(err)
	}
	assertObservation(t, observations, "internal/service.go", "coverage.line", StateMeasured, 80)

	cobertura := []byte(`<coverage><packages><package name="service"><classes>
<class name="Service" filename="internal/service.go"><lines>
<line number="1" hits="1"/><line number="2" hits="0" branch="true" condition-coverage="50% (1/2)"/>
</lines></class></classes></package></packages></coverage>`)
	observations, _, err = parseCobertura(cobertura)
	if err != nil {
		t.Fatal(err)
	}
	assertObservation(t, observations, "internal/service.go", "coverage.line", StateMeasured, 50)
	assertObservation(t, observations, "internal/service.go", "coverage.branch", StateMeasured, 50)
}

func assertObservation(t *testing.T, observations []Observation, file, key, state string, value float64) {
	t.Helper()
	for _, observation := range observations {
		if observation.Path != file || observation.Key != key || observation.State != state {
			continue
		}
		if state == StateSkipped {
			if observation.Value != nil {
				t.Fatalf("%s %s skipped value = %v", file, key, *observation.Value)
			}
			return
		}
		if observation.Value == nil || *observation.Value != value {
			t.Fatalf("%s %s value = %v, want %v", file, key, observation.Value, value)
		}
		return
	}
	t.Fatalf("missing observation path=%q key=%q state=%q in %#v", file, key, state, observations)
}
