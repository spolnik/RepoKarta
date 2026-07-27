package analysis

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

const (
	MaximumStructuralQueryBytes = 16 << 10
	DefaultStructuralMatchLimit = 1_000
	DefaultStructuralWorkBudget = 250_000
	MaximumCaptureTextBytes     = 500
)

// StructuralQuery is an immutable compiled Tree-sitter query. It is safe to
// reuse across files and concurrent executions.
type StructuralQuery struct {
	languageName string
	language     *gotreesitter.Language
	query        *gotreesitter.Query
}

// QueryOptions bounds execution within one source file.
type QueryOptions struct {
	MatchLimit uint32
	WorkBudget int
}

// QueryCapture is one named capture with a bounded source excerpt.
type QueryCapture struct {
	Name     string `json:"name"`
	NodeType string `json:"node_type"`
	Text     string `json:"text,omitempty"`
	Range    Range  `json:"range"`
}

// QueryMatch contains all captures produced by one successful pattern.
type QueryMatch struct {
	PatternIndex int            `json:"pattern_index"`
	Captures     []QueryCapture `json:"captures"`
}

// QueryResult reports bounded matches and whether the per-file matcher stopped
// before exhausting the syntax tree.
type QueryResult struct {
	Matches   []QueryMatch `json:"matches"`
	Truncated bool         `json:"truncated"`
}

// CompileStructuralQuery validates a Tree-sitter query for one embedded
// language. Java and Go are the supported public contract in this milestone.
func CompileStructuralQuery(languageName, querySource string) (*StructuralQuery, error) {
	languageName = strings.ToLower(strings.TrimSpace(languageName))
	if languageName != "java" && languageName != "go" {
		return nil, errors.New("language must be java or go")
	}
	querySource = strings.TrimSpace(querySource)
	if querySource == "" {
		return nil, errors.New("structural query is required")
	}
	if len(querySource) > MaximumStructuralQueryBytes {
		return nil, fmt.Errorf("structural query exceeds %d bytes", MaximumStructuralQueryBytes)
	}
	entry := grammars.DetectLanguageByName(languageName)
	if entry == nil || entry.Language == nil {
		return nil, fmt.Errorf("grammar %q is not embedded in this build", languageName)
	}
	language := entry.Language()
	if language == nil {
		return nil, fmt.Errorf("grammar %q could not be loaded", languageName)
	}
	compiled, err := gotreesitter.NewQuery(querySource, language)
	if err != nil {
		return nil, fmt.Errorf("compile %s structural query: %w", languageName, err)
	}
	if compiled.CaptureCount() == 0 {
		return nil, errors.New("structural query must include at least one named capture")
	}
	return &StructuralQuery{languageName: languageName, language: language, query: compiled}, nil
}

// RequiredRootKinds returns root node kinds that every successful match must
// begin with. An empty result means the query could not be analyzed safely and
// callers must not prefilter candidates.
func RequiredRootKinds(querySource string) []string {
	var (
		depth    int
		inString bool
		escaped  bool
		comment  bool
		roots    []string
	)
	for index := 0; index < len(querySource); index++ {
		character := querySource[index]
		if comment {
			if character == '\n' {
				comment = false
			}
			continue
		}
		if inString {
			if escaped {
				escaped = false
			} else if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}
			continue
		}
		if character == ';' {
			comment = true
			continue
		}
		if character == '"' {
			inString = true
			continue
		}
		switch character {
		case '(':
			if depth == 0 {
				root, ok := structuralPatternRoot(querySource[index+1:])
				if !ok {
					return nil
				}
				roots = append(roots, root)
			}
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil
			}
		case '[':
			if depth == 0 {
				return nil
			}
		}
	}
	if depth != 0 || inString || len(roots) == 0 {
		return nil
	}
	sort.Strings(roots)
	return compactSortedStrings(roots)
}

func structuralPatternRoot(remainder string) (string, bool) {
	remainder = strings.TrimLeft(remainder, " \t\r\n")
	if strings.HasPrefix(remainder, "(") {
		remainder = strings.TrimLeft(remainder[1:], " \t\r\n")
	}
	if remainder == "" || remainder[0] == '[' || remainder[0] == '"' {
		return "", false
	}
	end := 0
	for end < len(remainder) {
		character := remainder[end]
		if !((character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '_') {
			break
		}
		end++
	}
	if end == 0 {
		return "", false
	}
	root := remainder[:end]
	if root == "_" || root == "MISSING" ||
		(end < len(remainder) && remainder[end] == '/') {
		return "", false
	}
	return root, true
}

func compactSortedStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	output := values[:1]
	for _, value := range values[1:] {
		if value != output[len(output)-1] {
			output = append(output, value)
		}
	}
	return output
}

// Execute parses source without executing repository code and streams bounded
// query matches from the resulting immutable syntax tree.
func (q *StructuralQuery) Execute(source []byte, options QueryOptions) (result QueryResult, err error) {
	if q == nil || q.query == nil || q.language == nil {
		return QueryResult{}, errors.New("structural query is not compiled")
	}
	matchLimit := options.MatchLimit
	if matchLimit == 0 {
		matchLimit = DefaultStructuralMatchLimit
	}
	workBudget := options.WorkBudget
	if workBudget <= 0 {
		workBudget = DefaultStructuralWorkBudget
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			result = QueryResult{}
			err = fmt.Errorf("structural query parser panicked: %v", recovered)
		}
	}()
	parser := gotreesitter.NewParser(q.language)
	entry := grammars.DetectLanguageByName(q.languageName)
	var tree *gotreesitter.Tree
	if entry != nil && entry.TokenSourceFactory != nil {
		tree, err = parser.ParseWithTokenSource(source, entry.TokenSourceFactory(source, q.language))
	} else {
		tree, err = parser.Parse(source)
	}
	if err != nil {
		return QueryResult{}, fmt.Errorf("parse %s source: %w", q.languageName, err)
	}
	if tree == nil || tree.RootNode() == nil {
		return QueryResult{}, errors.New("syntax parser returned no tree")
	}
	defer tree.Release()

	lines := lineIndex(source)
	cursor := q.query.Exec(tree.RootNode(), q.language, source)
	cursor.SetMatchLimit(matchLimit)
	cursor.SetMatchWorkBudget(workBudget)
	result.Matches = make([]QueryMatch, 0)
	for {
		match, ok := cursor.NextMatch()
		if !ok {
			break
		}
		output := QueryMatch{
			PatternIndex: match.PatternIndex,
			Captures:     make([]QueryCapture, 0, len(match.Captures)),
		}
		for _, capture := range match.Captures {
			if capture.Node == nil {
				continue
			}
			output.Captures = append(output.Captures, QueryCapture{
				Name:     capture.Name,
				NodeType: capture.Node.Type(q.language),
				Text:     compactCaptureText(capture.Text(source), MaximumCaptureTextBytes),
				Range: sourceRange(
					lines,
					int(capture.Node.StartByte()),
					int(capture.Node.EndByte()),
				),
			})
		}
		result.Matches = append(result.Matches, output)
	}
	result.Truncated = cursor.DidExceedMatchLimit()
	return result, nil
}

func compactCaptureText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	end := max(0, limit-3)
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end] + "..."
}
