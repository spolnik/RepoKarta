// Package querylang defines RepoKarta's stable deterministic search grammar.
package querylang

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	FieldContent    = "content"
	FieldRepository = "repository"
	FieldRevision   = "revision"
	FieldLanguage   = "language"
	FieldPath       = "path"
	FieldFile       = "file"
	FieldSymbolKind = "symbol_kind"
	FieldResultType = "result_type"
	FieldOwner      = "owner"
	FieldPackage    = "package"
	FieldTypeName   = "type_name"
	FieldMethod     = "method"
	FieldMember     = "member"
	FieldFullName   = "full_name"
	FieldMatch      = "match"
	FieldCase       = "case"
	FieldAuthor     = "author"
	FieldMessage    = "message"
	FieldAdded      = "added"
	FieldRemoved    = "removed"
	FieldAfter      = "after"
	FieldBefore     = "before"
	FieldBranch     = "branch"
	FieldFrom       = "from"
	FieldTo         = "to"
)

var fieldAliases = map[string]string{
	"content":     FieldContent,
	"repo":        FieldRepository,
	"repository":  FieldRepository,
	"rev":         FieldRevision,
	"revision":    FieldRevision,
	"lang":        FieldLanguage,
	"language":    FieldLanguage,
	"path":        FieldPath,
	"file":        FieldFile,
	"filename":    FieldFile,
	"kind":        FieldSymbolKind,
	"symbol-kind": FieldSymbolKind,
	"symbol_kind": FieldSymbolKind,
	"type":        FieldResultType,
	"result-type": FieldResultType,
	"result_type": FieldResultType,
	"owner":       FieldOwner,
	"package":     FieldPackage,
	"pkg":         FieldPackage,
	"type-name":   FieldTypeName,
	"type_name":   FieldTypeName,
	"method":      FieldMethod,
	"member":      FieldMember,
	"full-name":   FieldFullName,
	"full_name":   FieldFullName,
	"match":       FieldMatch,
	"case":        FieldCase,
	"author":      FieldAuthor,
	"message":     FieldMessage,
	"added":       FieldAdded,
	"removed":     FieldRemoved,
	"after":       FieldAfter,
	"date-from":   FieldAfter,
	"date_from":   FieldAfter,
	"before":      FieldBefore,
	"date-to":     FieldBefore,
	"date_to":     FieldBefore,
	"branch":      FieldBranch,
	"from":        FieldFrom,
	"to":          FieldTo,
}

// Filter is one canonical positive or negative query constraint.
type Filter struct {
	Field    string `json:"field"`
	Value    string `json:"value"`
	Negative bool   `json:"negative,omitempty"`
}

// Query is the parsed form shared by the HTML, JSON, and MCP surfaces. Text
// retains ordinary free text so the existing simple search remains valid.
type Query struct {
	Raw     string   `json:"raw"`
	Text    string   `json:"text"`
	Filters []Filter `json:"filters,omitempty"`
}

type token struct {
	value string
	start int
	end   int
}

// Parse recognizes only documented field names. Unknown colon-bearing text,
// including URLs and source syntax, remains ordinary search text.
func Parse(raw string) (Query, error) {
	if !utf8.ValidString(raw) {
		return Query{}, errors.New("query must be valid UTF-8")
	}
	tokens, err := tokenize(raw)
	if err != nil {
		return Query{}, err
	}
	parsed := Query{Raw: raw, Filters: []Filter{}}
	free := make([]string, 0, len(tokens))
	for _, item := range tokens {
		value := item.value
		negative := false
		if strings.HasPrefix(value, "-") && len(value) > 1 {
			negative = true
			value = value[1:]
		} else if strings.HasPrefix(value, "+") && len(value) > 1 {
			value = value[1:]
		}
		key, filterValue, found := strings.Cut(value, ":")
		field, known := fieldAliases[strings.ToLower(strings.TrimSpace(key))]
		if found && known {
			filterValue = strings.TrimSpace(filterValue)
			if filterValue == "" {
				return Query{}, fmt.Errorf("%s filter requires a value", field)
			}
			parsed.Filters = append(parsed.Filters, Filter{
				Field:    field,
				Value:    filterValue,
				Negative: negative,
			})
			continue
		}
		if negative {
			if strings.TrimSpace(value) == "" {
				return Query{}, errors.New("negative content filter requires a value")
			}
			parsed.Filters = append(parsed.Filters, Filter{
				Field:    FieldContent,
				Value:    value,
				Negative: true,
			})
			continue
		}
		free = append(free, item.value)
	}
	parsed.Text = strings.TrimSpace(strings.Join(free, " "))
	return parsed, nil
}

func tokenize(raw string) ([]token, error) {
	output := make([]token, 0)
	for offset := 0; offset < len(raw); {
		for offset < len(raw) {
			r, size := utf8.DecodeRuneInString(raw[offset:])
			if !isSpace(r) {
				break
			}
			offset += size
		}
		if offset >= len(raw) {
			break
		}
		start := offset
		var value strings.Builder
		quoted := false
		escaped := false
		for offset < len(raw) {
			r, size := utf8.DecodeRuneInString(raw[offset:])
			if escaped {
				value.WriteRune(r)
				escaped = false
				offset += size
				continue
			}
			if r == '\\' && quoted {
				escaped = true
				offset += size
				continue
			}
			if r == '"' {
				quoted = !quoted
				offset += size
				continue
			}
			if !quoted && isSpace(r) {
				break
			}
			value.WriteRune(r)
			offset += size
		}
		if escaped {
			return nil, errors.New("query ends with an incomplete escape")
		}
		if quoted {
			return nil, errors.New("query contains an unterminated quote")
		}
		if value.Len() > 0 {
			output = append(output, token{value: value.String(), start: start, end: offset})
		}
	}
	return output, nil
}

func isSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
}
