package querylang

import (
	"reflect"
	"testing"
)

func TestParseRetainsSimpleTextAndCanonicalizesFilters(t *testing.T) {
	parsed, err := Parse(`payment retry repo:checkout lang:Go path:"internal api" -file:_test.go`)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Text != "payment retry" {
		t.Fatalf("text = %q", parsed.Text)
	}
	want := []Filter{
		{Field: FieldRepository, Value: "checkout"},
		{Field: FieldLanguage, Value: "Go"},
		{Field: FieldPath, Value: "internal api"},
		{Field: FieldFile, Value: "_test.go", Negative: true},
	}
	if !reflect.DeepEqual(parsed.Filters, want) {
		t.Fatalf("filters = %#v, want %#v", parsed.Filters, want)
	}
}

func TestParseSupportsEveryDocumentedFieldAndAliases(t *testing.T) {
	parsed, err := Parse(
		`repo:catalog rev:abc123 lang:Go path:internal file:.go ` +
			`kind:method type:reference owner:platform -content:generated`,
	)
	if err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 0, len(parsed.Filters))
	for _, filter := range parsed.Filters {
		fields = append(fields, filter.Field)
	}
	want := []string{
		FieldRepository, FieldRevision, FieldLanguage, FieldPath, FieldFile,
		FieldSymbolKind, FieldResultType, FieldOwner, FieldContent,
	}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields = %#v, want %#v", fields, want)
	}
	if !parsed.Filters[len(parsed.Filters)-1].Negative {
		t.Fatal("negative content filter was not preserved")
	}
}

func TestParseKeepsUnknownColonSyntaxAsText(t *testing.T) {
	parsed, err := Parse(`https://example.test sym:Known madeup:name`)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Text != "https://example.test sym:Known madeup:name" || len(parsed.Filters) != 0 {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func TestParseRejectsIncompleteGrammar(t *testing.T) {
	for _, raw := range []string{`repo:`, `path:"unterminated`, `content:"broken\`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := Parse(raw); err == nil {
				t.Fatalf("Parse(%q) succeeded", raw)
			}
		})
	}
}
