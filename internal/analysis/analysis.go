// Package analysis derives a bounded, language-neutral structural index from
// source syntax trees. It does not execute repository code and deliberately
// keeps framework-specific interpretation in higher-level graph extractors.
package analysis

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

const (
	maximumSymbolsPerDocument             = 400
	maximumNavigationRelationsPerDocument = 800
	maximumCallRelationsPerDocument       = 800
	maximumBuildFactsPerDocument          = 200
)

// Range is a stable, one-based source location with a half-open byte span.
type Range struct {
	StartByte   int `json:"start_byte"`
	EndByte     int `json:"end_byte"`
	StartLine   int `json:"start_line"`
	StartColumn int `json:"start_column"`
	EndLine     int `json:"end_line"`
	EndColumn   int `json:"end_column"`
}

// Symbol is one named declaration recognized by the language grammar.
type Symbol struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	NodeType   string `json:"node_type"`
	Confidence string `json:"confidence"`
	Range      Range  `json:"range"`
}

// Relation is one syntax-backed reference. Resolution to a specific symbol is
// intentionally a later stage, so Target is the source-level referenced name.
type Relation struct {
	Kind       string `json:"kind"`
	Target     string `json:"target"`
	Receiver   string `json:"receiver,omitempty"`
	Confidence string `json:"confidence"`
	Range      Range  `json:"range"`
}

// BuildFact is one syntax-backed call from a Gradle Groovy or Kotlin script.
type BuildFact struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Value      string `json:"value"`
	Confidence string `json:"confidence"`
	Range      Range  `json:"range"`
}

// Diagnostic explains why a document is only partially represented.
type Diagnostic struct {
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// Document is the normalized structural result for one source file.
type Document struct {
	Path          string       `json:"path"`
	Language      string       `json:"language"`
	Parser        string       `json:"parser"`
	ParseComplete bool         `json:"parse_complete"`
	Truncated     bool         `json:"truncated"`
	NodeKinds     []string     `json:"node_kinds,omitempty"`
	Symbols       []Symbol     `json:"symbols,omitempty"`
	Relations     []Relation   `json:"relations,omitempty"`
	BuildFacts    []BuildFact  `json:"build_facts,omitempty"`
	Diagnostics   []Diagnostic `json:"diagnostics,omitempty"`
}

// LanguageForPath returns the supported parser language for a repository path.
func LanguageForPath(filePath string) string {
	base := strings.ToLower(path.Base(filePath))
	extension := strings.ToLower(path.Ext(base))
	switch {
	case extension == ".go":
		return "go"
	case extension == ".java":
		return "java"
	case extension == ".kt" || extension == ".kts":
		return "kotlin"
	case extension == ".gradle" || extension == ".groovy" || extension == ".gvy":
		return "groovy"
	case extension == ".tsx":
		return "tsx"
	case extension == ".ts":
		return "typescript"
	case extension == ".js" || extension == ".jsx" || extension == ".mjs" || extension == ".cjs":
		return "javascript"
	case extension == ".py":
		return "python"
	case extension == ".sql":
		return "sql"
	case extension == ".sh" || extension == ".bash":
		return "bash"
	default:
		return ""
	}
}

// Analyze parses one supported file and returns false for unsupported paths.
// Syntax errors are represented as diagnostics and partial trees remain useful.
func Analyze(filePath string, source []byte) (document Document, supported bool) {
	languageName := LanguageForPath(filePath)
	if languageName == "" {
		return Document{}, false
	}
	document = Document{
		Path:          filePath,
		Language:      languageName,
		Parser:        "gotreesitter/tree-sitter",
		ParseComplete: false,
	}
	supported = true
	defer func() {
		if recovered := recover(); recovered != nil {
			document.ParseComplete = false
			document.Diagnostics = append(document.Diagnostics, Diagnostic{
				Severity: "error",
				Message:  fmt.Sprintf("syntax parser panicked: %v", recovered),
			})
		}
	}()
	entry := grammars.DetectLanguageByName(languageName)
	if entry == nil || entry.Language == nil {
		document.Diagnostics = append(document.Diagnostics, Diagnostic{
			Severity: "error",
			Message:  fmt.Sprintf("grammar %q is not embedded in this build", languageName),
		})
		return document, true
	}
	language := entry.Language()
	if language == nil {
		document.Diagnostics = append(document.Diagnostics, Diagnostic{
			Severity: "error",
			Message:  fmt.Sprintf("grammar %q could not be loaded", languageName),
		})
		return document, true
	}
	parser := gotreesitter.NewParser(language)
	var (
		tree *gotreesitter.Tree
		err  error
	)
	if entry.TokenSourceFactory != nil {
		tree, err = parser.ParseWithTokenSource(source, entry.TokenSourceFactory(source, language))
	} else {
		tree, err = parser.Parse(source)
	}
	if err != nil {
		document.Diagnostics = append(document.Diagnostics, Diagnostic{
			Severity: "error",
			Message:  "syntax parser failed: " + err.Error(),
		})
		return document, true
	}
	if tree == nil || tree.RootNode() == nil {
		document.Diagnostics = append(document.Diagnostics, Diagnostic{
			Severity: "error",
			Message:  "syntax parser returned no tree",
		})
		return document, true
	}
	defer tree.Release()

	lines := lineIndex(source)
	document.ParseComplete = !tree.RootNode().HasError() && int(tree.RootNode().EndByte()) >= len(source)
	if !document.ParseComplete {
		document.Diagnostics = append(document.Diagnostics, Diagnostic{
			Severity: "warning",
			Message:  "syntax tree contains recovery nodes; extracted facts may be partial",
		})
	}
	document.NodeKinds = collectNodeKinds(tree)
	document.Symbols = extractSymbols(tree, source, lines)
	var relationsTruncated bool
	document.Relations, relationsTruncated = extractRelations(tree, source, lines)
	if isGradlePath(filePath) {
		document.BuildFacts = extractGradleFacts(tree, source, lines)
	}
	document.Truncated =
		len(document.Symbols) >= maximumSymbolsPerDocument ||
			relationsTruncated ||
			len(document.BuildFacts) >= maximumBuildFactsPerDocument
	return document, true
}

func collectNodeKinds(tree *gotreesitter.Tree) []string {
	if tree == nil || tree.RootNode() == nil || tree.Language() == nil {
		return nil
	}
	language := tree.Language()
	seen := make(map[string]struct{})
	stack := []*gotreesitter.Node{tree.RootNode()}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if node == nil {
			continue
		}
		if node.IsNamed() {
			if kind := strings.TrimSpace(node.Type(language)); kind != "" {
				seen[kind] = struct{}{}
			}
		}
		for index := node.ChildCount() - 1; index >= 0; index-- {
			stack = append(stack, node.Child(index))
		}
	}
	kinds := make([]string, 0, len(seen))
	for kind := range seen {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

func extractSymbols(tree *gotreesitter.Tree, source []byte, lines []int) []Symbol {
	language := tree.Language()
	symbols := make([]Symbol, 0)
	seen := make(map[string]struct{})
	appendSymbol := func(kind, name, nodeType string, start, end uint32) {
		name = strings.TrimSpace(name)
		if name == "" || len(symbols) >= maximumSymbolsPerDocument {
			return
		}
		key := fmt.Sprintf("%s\x00%s\x00%d", kind, name, start)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		symbols = append(symbols, Symbol{
			Kind:       kind,
			Name:       name,
			NodeType:   nodeType,
			Confidence: "syntax",
			Range:      sourceRange(lines, int(start), int(end)),
		})
	}
	for _, span := range gotreesitter.ExtractDefinitionSpans(tree) {
		appendSymbol(span.Kind, span.Name, span.NodeType, span.StartByte, span.EndByte)
	}
	walk(tree.RootNode(), func(node *gotreesitter.Node) bool {
		nodeType := node.Type(language)
		kind := customSymbolKind(tree.Language().Name, nodeType, node.Text(source))
		if kind == "" {
			return true
		}
		nameNode := declarationName(node, language)
		if nameNode == nil {
			return true
		}
		appendSymbol(kind, nameNode.Text(source), nodeType, node.StartByte(), node.EndByte())
		return true
	})
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].Range.StartByte != symbols[j].Range.StartByte {
			return symbols[i].Range.StartByte < symbols[j].Range.StartByte
		}
		return symbols[i].Name < symbols[j].Name
	})
	return symbols
}

func customSymbolKind(languageName, nodeType, text string) string {
	switch languageName {
	case "java":
		if nodeType == "enum_constant" {
			return "enum_member"
		}
	case "kotlin":
		switch nodeType {
		case "class_declaration":
			switch {
			case strings.Contains(textBeforeBrace(text), "interface "):
				return "interface"
			case strings.Contains(textBeforeBrace(text), "enum class "):
				return "enum"
			default:
				return "class"
			}
		case "object_declaration":
			return "object"
		case "function_declaration":
			return "function"
		case "type_alias":
			return "type"
		}
	case "groovy":
		switch nodeType {
		case "class_definition":
			return "class"
		case "function_definition", "function_declaration":
			return "function"
		}
	case "bash":
		if nodeType == "function_definition" {
			return "function"
		}
	case "sql":
		switch nodeType {
		case "create_table_statement":
			return "table"
		case "create_view_statement":
			return "view"
		case "create_function_statement":
			return "function"
		case "create_type_statement":
			return "type"
		case "create_schema_statement":
			return "schema"
		case "create_index_statement":
			return "index"
		}
	case "javascript", "typescript", "tsx":
		switch nodeType {
		case "interface_declaration":
			return "interface"
		case "type_alias_declaration":
			return "type"
		case "enum_declaration":
			return "enum"
		case "variable_declarator":
			if strings.Contains(text, "=>") || strings.Contains(text, "function") {
				return "function"
			}
		}
	}
	return ""
}

func declarationName(node *gotreesitter.Node, language *gotreesitter.Language) *gotreesitter.Node {
	for _, field := range []string{"name", "function", "table", "view", "schema", "index"} {
		if child := node.ChildByFieldName(field, language); child != nil {
			return child
		}
	}
	return firstDescendant(node, language,
		"type_identifier",
		"simple_identifier",
		"identifier",
		"word",
		"object_reference",
	)
}

func extractRelations(tree *gotreesitter.Tree, source []byte, lines []int) ([]Relation, bool) {
	navigation := make([]Relation, 0)
	calls := make([]Relation, 0)
	seen := make(map[string]struct{})
	truncated := false
	appendRelation := func(kind, target, receiver string, start, end uint32) {
		target = compactText(target, 240)
		if target == "" {
			return
		}
		key := fmt.Sprintf("%s\x00%s\x00%d", kind, target, start)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		relation := Relation{
			Kind:       kind,
			Target:     target,
			Receiver:   receiver,
			Confidence: "syntax",
			Range:      sourceRange(lines, int(start), int(end)),
		}
		if kind == "call" {
			if len(calls) >= maximumCallRelationsPerDocument {
				truncated = true
				return
			}
			calls = append(calls, relation)
			return
		}
		if len(navigation) >= maximumNavigationRelationsPerDocument {
			truncated = true
			return
		}
		navigation = append(navigation, relation)
	}
	for _, heritage := range gotreesitter.ExtractHeritage(tree) {
		appendRelation(heritage.Kind, heritage.Parent, heritage.Name, heritage.StartByte, heritage.EndByte)
	}
	language := tree.Language()
	walk(tree.RootNode(), func(node *gotreesitter.Node) bool {
		nodeType := node.Type(language)
		if isImportNode(tree.Language().Name, nodeType) {
			appendRelation(
				"import",
				importTarget(tree.Language().Name, node.Text(source)),
				"",
				node.StartByte(),
				node.EndByte(),
			)
		}
		if target := typeReferenceTarget(tree.Language().Name, nodeType, node.Text(source)); target != "" &&
			!isDeclarationName(node, language) &&
			!hasImportAncestor(node, tree.Language().Name, language) {
			appendRelation("type", target, "", node.StartByte(), node.EndByte())
		}
		if kind, target, receiver := memberReference(
			tree.Language().Name,
			node,
			language,
			source,
		); target != "" && !hasImportAncestor(node, tree.Language().Name, language) {
			appendRelation(kind, target, receiver, node.StartByte(), node.EndByte())
		}
		return true
	})
	for _, call := range gotreesitter.ExtractCalls(tree) {
		appendRelation("call", call.Name, call.Receiver, call.StartByte, call.EndByte)
	}
	walk(tree.RootNode(), func(node *gotreesitter.Node) bool {
		nodeType := node.Type(language)
		if isCustomCallNode(tree.Language().Name, nodeType) {
			if target := callName(node, language, source); target != "" {
				appendRelation("call", target, "", node.StartByte(), node.EndByte())
			}
		}
		return true
	})
	relations := append(navigation, calls...)
	sort.Slice(relations, func(i, j int) bool {
		return relations[i].Range.StartByte < relations[j].Range.StartByte
	})
	return relations, truncated
}

func typeReferenceTarget(languageName, nodeType, text string) string {
	switch languageName {
	case "java", "go", "javascript", "typescript", "tsx":
		if nodeType == "type_identifier" {
			return text
		}
	case "kotlin":
		if nodeType == "user_type" {
			return text
		}
	}
	return ""
}

func importTarget(languageName, text string) string {
	text = strings.TrimSpace(text)
	switch languageName {
	case "java":
		text = strings.TrimSpace(strings.TrimPrefix(text, "import"))
		text = strings.TrimSpace(strings.TrimPrefix(text, "static"))
		return strings.TrimSpace(strings.TrimSuffix(text, ";"))
	default:
		return text
	}
}

// memberReference preserves qualified member and method-reference evidence that
// generic call extraction does not represent. This matters especially for Java
// enums: Status.APPROVED is a field access, not a call or a type node in every
// grammar position, and Status::fromCode is a method reference. Keeping the
// member as Target and the qualifier as Receiver lets reference search match
// either source-level name without pretending to resolve a Java type.
func memberReference(
	languageName string,
	node *gotreesitter.Node,
	language *gotreesitter.Language,
	source []byte,
) (kind, target, receiver string) {
	if languageName != "java" || node == nil {
		return "", "", ""
	}
	switch node.Type(language) {
	case "field_access":
		target = childText(node, language, source, "field", "name")
		receiver = childText(node, language, source, "object", "scope")
		if target == "" {
			receiver, target = splitQualifiedMember(node.Text(source), ".")
		}
		return "member", compactText(target, 120), compactText(receiver, 240)
	case "method_reference":
		target = childText(node, language, source, "method", "name")
		receiver = childText(node, language, source, "object", "type", "scope")
		if target == "" {
			receiver, target = splitQualifiedMember(node.Text(source), "::")
		}
		return "method_reference", compactText(target, 120), compactText(receiver, 240)
	default:
		return "", "", ""
	}
}

func childText(
	node *gotreesitter.Node,
	language *gotreesitter.Language,
	source []byte,
	fields ...string,
) string {
	for _, field := range fields {
		if child := node.ChildByFieldName(field, language); child != nil {
			return strings.TrimSpace(child.Text(source))
		}
	}
	return ""
}

func splitQualifiedMember(text, separator string) (receiver, target string) {
	text = strings.TrimSpace(text)
	index := strings.LastIndex(text, separator)
	if index < 0 {
		return "", ""
	}
	return strings.TrimSpace(text[:index]), strings.TrimSpace(text[index+len(separator):])
}

func isDeclarationName(node *gotreesitter.Node, language *gotreesitter.Language) bool {
	parent := node.Parent()
	if parent == nil {
		return false
	}
	name := parent.ChildByFieldName("name", language)
	return sameNodeRange(node, name)
}

func hasImportAncestor(node *gotreesitter.Node, languageName string, language *gotreesitter.Language) bool {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if isImportNode(languageName, parent.Type(language)) {
			return true
		}
	}
	return false
}

func sameNodeRange(left, right *gotreesitter.Node) bool {
	return left != nil && right != nil &&
		left.StartByte() == right.StartByte() &&
		left.EndByte() == right.EndByte()
}

func isImportNode(languageName, nodeType string) bool {
	switch languageName {
	case "go":
		return nodeType == "import_spec"
	case "java":
		return nodeType == "import_declaration"
	case "kotlin":
		return nodeType == "import_header"
	case "groovy":
		return nodeType == "import"
	case "python":
		return nodeType == "import_statement" || nodeType == "import_from_statement"
	case "javascript", "typescript", "tsx":
		return nodeType == "import_statement"
	default:
		return false
	}
}

func isCustomCallNode(languageName, nodeType string) bool {
	switch languageName {
	case "kotlin":
		return nodeType == "call_expression"
	case "groovy":
		return nodeType == "function_call" || nodeType == "juxt_function_call"
	case "bash":
		return nodeType == "command"
	case "sql":
		return nodeType == "function_call"
	default:
		return false
	}
}

func callName(node *gotreesitter.Node, language *gotreesitter.Language, source []byte) string {
	for _, field := range []string{"function", "name", "command"} {
		if child := node.ChildByFieldName(field, language); child != nil {
			return compactText(child.Text(source), 120)
		}
	}
	if descendant := firstDescendant(node, language,
		"command_name",
		"simple_identifier",
		"identifier",
		"word",
	); descendant != nil {
		return compactText(descendant.Text(source), 120)
	}
	return ""
}

func extractGradleFacts(tree *gotreesitter.Tree, source []byte, lines []int) []BuildFact {
	facts := make([]BuildFact, 0)
	seen := make(map[string]struct{})
	language := tree.Language()
	appendFact := func(kind, name, value string, start, end uint32) {
		key := fmt.Sprintf("%s\x00%s\x00%d", kind, name, start)
		if kind == "" || len(facts) >= maximumBuildFactsPerDocument {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		facts = append(facts, BuildFact{
			Kind:       kind,
			Name:       name,
			Value:      compactText(value, 240),
			Confidence: "syntax",
			Range:      sourceRange(lines, int(start), int(end)),
		})
	}
	walk(tree.RootNode(), func(node *gotreesitter.Node) bool {
		if len(facts) >= maximumBuildFactsPerDocument {
			return false
		}
		if isCustomCallNode(tree.Language().Name, node.Type(language)) {
			name := callName(node, language, source)
			appendFact(
				gradleFactKind(name),
				name,
				node.Text(source),
				node.StartByte(),
				node.EndByte(),
			)
		}
		// Groovy's command-style Gradle DSL, such as `implementation "x"`,
		// is represented as adjacent identifier/literal nodes rather than a
		// function call. The identifier remains an exact syntax-tree node.
		if tree.Language().Name != "groovy" || node.Type(language) != "identifier" {
			return len(facts) < maximumBuildFactsPerDocument
		}
		name := callName(node, language, source)
		if name == "" {
			name = strings.TrimSpace(node.Text(source))
		}
		kind := gradleFactKind(name)
		if kind == "" {
			return true
		}
		end := node.EndByte()
		for sibling := node.NextSibling(); sibling != nil; sibling = sibling.NextSibling() {
			if sibling.IsNamed() {
				end = sibling.EndByte()
				break
			}
		}
		appendFact(kind, name, string(source[node.StartByte():end]), node.StartByte(), end)
		return true
	})
	return facts
}

func gradleFactKind(name string) string {
	switch name {
	case "id", "alias":
		return "plugin"
	case "implementation", "api", "compileOnly", "runtimeOnly", "testImplementation",
		"testRuntimeOnly", "annotationProcessor", "kapt", "ksp", "classpath":
		return "dependency"
	case "include", "includeBuild", "project":
		return "project"
	case "task", "tasks", "register", "named":
		return "task"
	default:
		return ""
	}
}

func isGradlePath(filePath string) bool {
	base := strings.ToLower(path.Base(filePath))
	return strings.HasSuffix(base, ".gradle") || strings.HasSuffix(base, ".gradle.kts")
}

func firstDescendant(node *gotreesitter.Node, language *gotreesitter.Language, types ...string) *gotreesitter.Node {
	wanted := make(map[string]struct{}, len(types))
	for _, nodeType := range types {
		wanted[nodeType] = struct{}{}
	}
	var found *gotreesitter.Node
	walk(node, func(candidate *gotreesitter.Node) bool {
		if candidate != node {
			if _, ok := wanted[candidate.Type(language)]; ok {
				found = candidate
				return false
			}
		}
		return true
	})
	return found
}

func walk(node *gotreesitter.Node, visit func(*gotreesitter.Node) bool) bool {
	if node == nil || !visit(node) {
		return false
	}
	for index := 0; index < node.NamedChildCount(); index++ {
		if !walk(node.NamedChild(index), visit) {
			return false
		}
	}
	return true
}

func lineIndex(source []byte) []int {
	lines := []int{0}
	for index, value := range source {
		if value == '\n' {
			lines = append(lines, index+1)
		}
	}
	return lines
}

func sourceRange(lines []int, start, end int) Range {
	startLine, startColumn := sourcePoint(lines, start)
	endLine, endColumn := sourcePoint(lines, end)
	return Range{
		StartByte:   start,
		EndByte:     end,
		StartLine:   startLine,
		StartColumn: startColumn,
		EndLine:     endLine,
		EndColumn:   endColumn,
	}
}

func sourcePoint(lines []int, offset int) (int, int) {
	line := sort.Search(len(lines), func(index int) bool { return lines[index] > offset })
	if line == 0 {
		return 1, offset + 1
	}
	lineStart := lines[line-1]
	return line, offset - lineStart + 1
}

func compactText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	return value[:limit-1] + "…"
}

func textBeforeBrace(value string) string {
	if before, _, ok := strings.Cut(value, "{"); ok {
		return before
	}
	return value
}
