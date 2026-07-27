package querylang

import (
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const MaximumCompletions = 20

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type CompletionOptions struct {
	Repositories []Option
	Revisions    []Option
	Owners       []Option
}

// Completion replaces one UTF-16 range, matching browser selection offsets.
type Completion struct {
	Label        string `json:"label"`
	Detail       string `json:"detail"`
	InsertText   string `json:"insert_text"`
	ReplaceStart int    `json:"replace_start"`
	ReplaceEnd   int    `json:"replace_end"`
}

type CompletionList struct {
	Completions []Completion `json:"completions"`
}

type fieldDefinition struct {
	name   string
	detail string
	values []Option
}

var completionFields = []fieldDefinition{
	{name: "repository", detail: "Repository name or stable ID"},
	{name: "revision", detail: "Exact indexed commit"},
	{name: "language", detail: "Indexed source language", values: options(
		"Bash", "Go", "Groovy", "Java", "JavaScript", "Kotlin", "Python", "SQL", "TSX", "TypeScript",
	)},
	{name: "path", detail: "Repository-relative path contains"},
	{name: "file", detail: "Filename contains"},
	{name: "symbol_kind", detail: "Programming element kind", values: options(
		"class", "constant", "field", "function", "interface", "method", "module", "package", "type", "variable",
	)},
	{name: "result_type", detail: "Explicit result category", values: options(
		"code_insight", "commit", "content", "dependency", "diff", "file_path", "implementation",
		"reference", "repository", "route", "symbol_definition", "wiki_page",
	)},
	{name: "owner", detail: "CODEOWNERS identity or explicit state", values: options(
		"owned", "unavailable", "unowned", "unresolved",
	)},
	{name: "author", detail: "Git author name or email contains"},
	{name: "message", detail: "Git subject or body contains"},
	{name: "added", detail: "Added diff line contains"},
	{name: "removed", detail: "Removed diff line contains"},
	{name: "after", detail: "Authored on or after YYYY-MM-DD"},
	{name: "before", detail: "Authored on or before YYYY-MM-DD"},
	{name: "branch", detail: "Reachable local or remote branch"},
	{name: "from", detail: "Reachable range start revision"},
	{name: "to", detail: "Reachable range end revision"},
}

// Complete returns deterministic field/value completions at the browser caret.
// Cursor and replacement offsets use UTF-16 code units.
func Complete(raw string, cursor int, dynamic CompletionOptions) CompletionList {
	byteCursor := byteOffsetForUTF16(raw, cursor)
	start, end := activeTokenBounds(raw, byteCursor)
	current := raw[start:byteCursor]
	negative := strings.HasPrefix(current, "-")
	lookup := strings.TrimPrefix(strings.TrimPrefix(current, "-"), "+")
	key, prefix, hasValue := strings.Cut(lookup, ":")
	startUTF16 := utf16Length(raw[:start])
	endUTF16 := utf16Length(raw[:end])

	var completions []Completion
	if !hasValue {
		for _, field := range completionFields {
			insert := field.name + ":"
			if negative {
				insert = "-" + insert
			}
			if !strings.HasPrefix(insert, strings.ToLower(current)) {
				continue
			}
			completions = append(completions, Completion{
				Label:        insert,
				Detail:       field.detail,
				InsertText:   insert,
				ReplaceStart: startUTF16,
				ReplaceEnd:   endUTF16,
			})
		}
		return CompletionList{Completions: limitCompletions(completions)}
	}

	field, known := fieldAliases[strings.ToLower(key)]
	if !known {
		return CompletionList{Completions: []Completion{}}
	}
	canonical := canonicalFieldName(field)
	values := completionValues(field, dynamic)
	for _, option := range values {
		if !strings.HasPrefix(strings.ToLower(option.Value), strings.ToLower(prefix)) {
			continue
		}
		insert := canonical + ":" + quoteCompletionValue(option.Value)
		if negative {
			insert = "-" + insert
		}
		completions = append(completions, Completion{
			Label:        insert,
			Detail:       option.Label,
			InsertText:   insert,
			ReplaceStart: startUTF16,
			ReplaceEnd:   endUTF16,
		})
	}
	sort.SliceStable(completions, func(i, j int) bool {
		return completions[i].Label < completions[j].Label
	})
	return CompletionList{Completions: limitCompletions(completions)}
}

func completionValues(field string, dynamic CompletionOptions) []Option {
	switch field {
	case FieldRepository:
		return dynamic.Repositories
	case FieldRevision:
		return dynamic.Revisions
	case FieldOwner:
		output := append([]Option(nil), dynamic.Owners...)
		for _, definition := range completionFields {
			if definition.name == "owner" {
				return append(output, definition.values...)
			}
		}
		return output
	default:
		for _, definition := range completionFields {
			if canonicalFieldName(field) == definition.name {
				return definition.values
			}
		}
		return nil
	}
}

func canonicalFieldName(field string) string {
	switch field {
	case FieldRepository:
		return "repository"
	case FieldRevision:
		return "revision"
	case FieldLanguage:
		return "language"
	case FieldPath:
		return "path"
	case FieldFile:
		return "file"
	case FieldSymbolKind:
		return "symbol_kind"
	case FieldResultType:
		return "result_type"
	case FieldOwner:
		return "owner"
	case FieldAuthor:
		return "author"
	case FieldMessage:
		return "message"
	case FieldAdded:
		return "added"
	case FieldRemoved:
		return "removed"
	case FieldAfter:
		return "after"
	case FieldBefore:
		return "before"
	case FieldBranch:
		return "branch"
	case FieldFrom:
		return "from"
	case FieldTo:
		return "to"
	default:
		return field
	}
}

func activeTokenBounds(raw string, cursor int) (int, int) {
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(raw) {
		cursor = len(raw)
	}
	start := 0
	quoted := false
	for offset := 0; offset < cursor; {
		r, size := utf8.DecodeRuneInString(raw[offset:])
		if r == '"' {
			quoted = !quoted
		}
		if !quoted && isSpace(r) {
			start = offset + size
		}
		offset += size
	}
	end := len(raw)
	for offset, inQuote := cursor, quoted; offset < len(raw); {
		r, size := utf8.DecodeRuneInString(raw[offset:])
		if r == '"' {
			inQuote = !inQuote
		}
		if !inQuote && isSpace(r) {
			end = offset
			break
		}
		offset += size
	}
	return start, end
}

func byteOffsetForUTF16(value string, units int) int {
	if units <= 0 {
		return 0
	}
	used := 0
	for offset, r := range value {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if used+width > units {
			return offset
		}
		used += width
		if used == units {
			return offset + utf8.RuneLen(r)
		}
	}
	return len(value)
}

func utf16Length(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func quoteCompletionValue(value string) string {
	if !strings.ContainsAny(value, " \t\r\n\"") {
		return value
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

func limitCompletions(completions []Completion) []Completion {
	if len(completions) > MaximumCompletions {
		return completions[:MaximumCompletions]
	}
	if completions == nil {
		return []Completion{}
	}
	return completions
}

func options(values ...string) []Option {
	output := make([]Option, 0, len(values))
	for _, value := range values {
		output = append(output, Option{Value: value, Label: value})
	}
	return output
}
