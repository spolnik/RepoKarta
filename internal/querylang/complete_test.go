package querylang

import "testing"

func TestCompleteFieldsAndNegativeValues(t *testing.T) {
	fields := Complete("needle rep", len("needle rep"), CompletionOptions{})
	if len(fields.Completions) != 1 ||
		fields.Completions[0].InsertText != "repository:" ||
		fields.Completions[0].ReplaceStart != len("needle ") {
		t.Fatalf("field completions = %#v", fields)
	}

	values := Complete("-type:ref", len("-type:ref"), CompletionOptions{})
	if len(values.Completions) != 1 ||
		values.Completions[0].InsertText != "-result_type:reference" {
		t.Fatalf("value completions = %#v", values)
	}
}

func TestCompleteRepositoryQuotesAndUsesUTF16Offsets(t *testing.T) {
	raw := "🔎 repo:pay"
	result := Complete(raw, utf16Length(raw), CompletionOptions{
		Repositories: []Option{{Value: "payments api", Label: "payments api · 01234567"}},
	})
	if len(result.Completions) != 1 {
		t.Fatalf("completions = %#v", result)
	}
	completion := result.Completions[0]
	if completion.InsertText != `repository:"payments api"` ||
		completion.ReplaceStart != utf16Length("🔎 ") ||
		completion.ReplaceEnd != utf16Length(raw) {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestCompleteRevisionAndOwnerStates(t *testing.T) {
	revisions := Complete("revision:012", len("revision:012"), CompletionOptions{
		Revisions: []Option{{Value: "0123456789", Label: "catalog · 01234567"}},
	})
	if len(revisions.Completions) != 1 ||
		revisions.Completions[0].InsertText != "revision:0123456789" {
		t.Fatalf("revision completions = %#v", revisions)
	}

	owners := Complete("owner:un", len("owner:un"), CompletionOptions{})
	if len(owners.Completions) != 3 {
		t.Fatalf("owner completions = %#v", owners)
	}
}
