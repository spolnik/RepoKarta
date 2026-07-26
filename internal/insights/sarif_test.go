package insights

import "testing"

func TestParseSARIFPreservesRuleFingerprintSuppressionAndCodeFlow(t *testing.T) {
	report := []byte(`{
  "version":"2.1.0",
  "runs":[{
    "tool":{"driver":{"name":"Example Scanner","semanticVersion":"2.4.0","rules":[{
      "id":"SEC001","name":"UnsafeCall",
      "shortDescription":{"text":"Unsafe call"},
      "helpUri":"https://scanner.example/rules/SEC001",
      "properties":{"tags":["security"]}
    }]}},
    "results":[{
      "ruleId":"SEC001","level":"error","message":{"text":"Unsafe call found"},
      "locations":[{"physicalLocation":{"artifactLocation":{"uri":"internal/service.go"},"region":{"startLine":12,"endLine":13}}}],
      "partialFingerprints":{"primaryLocationLineHash":"abc123"},
      "suppressions":[{"kind":"inSource","status":"accepted","justification":"reviewed"}],
      "codeFlows":[{"threadFlows":[{"locations":[]}]}]
    }]
  }]
}`)
	observations, metadata, warnings, err := parseSARIF(report)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || len(observations) != 1 {
		t.Fatalf("observations=%#v warnings=%#v", observations, warnings)
	}
	observation := observations[0]
	if observation.Key != "SEC001" || observation.Severity != "error" ||
		observation.Path != "internal/service.go" || observation.StartLine != 12 ||
		observation.Fingerprint != "abc123" || !observation.Suppressed ||
		observation.CodeFlows == nil {
		t.Fatalf("observation = %#v", observation)
	}
	if metadata["tool_0"] != "Example Scanner" {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestParseSARIFRejectsOtherVersions(t *testing.T) {
	_, _, _, err := parseSARIF([]byte(`{"version":"2.0.0","runs":[{}]}`))
	if err == nil {
		t.Fatal("expected unsupported version error")
	}
}
