package graph

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/analysis"
)

const (
	ReachabilityStateReachable           = "reachable"
	ReachabilityStateProbablyUnreachable = "probably_unreachable"
	ReachabilityStateUnknown             = "unknown"
)

var frameworkRootAnnotations = map[string]string{
	"SpringBootApplication": "Spring application",
	"Configuration":         "Spring configuration",
	"Component":             "Spring component",
	"Service":               "Spring service",
	"Repository":            "Spring repository",
	"Controller":            "Spring controller",
	"RestController":        "Spring REST controller",
	"Bean":                  "Spring bean factory",
	"RequestMapping":        "Spring request handler",
	"GetMapping":            "Spring GET handler",
	"PostMapping":           "Spring POST handler",
	"PutMapping":            "Spring PUT handler",
	"PatchMapping":          "Spring PATCH handler",
	"DeleteMapping":         "Spring DELETE handler",
	"Scheduled":             "Spring scheduled method",
	"EventListener":         "Spring event listener",
	"KafkaListener":         "Kafka listener",
	"JmsListener":           "JMS listener",
	"RabbitListener":        "RabbitMQ listener",
}

// ReachabilityCompleteness states which evidence was available before any
// unreachable classification is emitted. Runtime completeness remains false
// because static syntax cannot prove reflection, profiles, generated code, or
// external framework registration.
type ReachabilityCompleteness struct {
	StructuralArtifactsComplete bool     `json:"structural_artifacts_complete"`
	DocumentsComplete           bool     `json:"documents_complete"`
	StaticAnalysisComplete      bool     `json:"static_analysis_complete"`
	RuntimeComplete             bool     `json:"runtime_complete"`
	TruncatedDocuments          int      `json:"truncated_documents"`
	IncompleteDocuments         int      `json:"incomplete_documents"`
	AmbiguousRelations          int      `json:"ambiguous_relations"`
	UnresolvedRelations         int      `json:"unresolved_relations"`
	DynamicBoundaries           []string `json:"dynamic_boundaries"`
}

// ReachabilitySummary gives stable counts for UI and automation consumers.
type ReachabilitySummary struct {
	Reachable           int `json:"reachable"`
	ProbablyUnreachable int `json:"probably_unreachable"`
	Unknown             int `json:"unknown"`
	Roots               int `json:"roots"`
	Edges               int `json:"edges"`
}

// ReachabilitySymbol is one declaration and its conservative classification.
type ReachabilitySymbol struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Language    string   `json:"language"`
	State       string   `json:"state"`
	Reason      string   `json:"reason"`
	RootKind    string   `json:"root_kind,omitempty"`
	Annotations []string `json:"annotations,omitempty"`
	Modifiers   []string `json:"modifiers,omitempty"`
	Witness     []string `json:"witness,omitempty"`
	Evidence    Evidence `json:"evidence"`
}

// ReachabilityEdge is a syntax-backed relation between declarations.
type ReachabilityEdge struct {
	Source     string   `json:"source"`
	Target     string   `json:"target"`
	Kind       string   `json:"kind"`
	Confidence string   `json:"confidence"`
	Evidence   Evidence `json:"evidence"`
}

// ReachabilityReport is derived only from persisted revision-pinned structure.
type ReachabilityReport struct {
	Version      int                      `json:"version"`
	ID           string                   `json:"id"`
	GeneratedAt  time.Time                `json:"generated_at"`
	Scope        Scope                    `json:"scope"`
	Completeness ReachabilityCompleteness `json:"completeness"`
	Summary      ReachabilitySummary      `json:"summary"`
	Symbols      []ReachabilitySymbol     `json:"symbols"`
	Edges        []ReachabilityEdge       `json:"edges"`
}

type reachabilityDeclaration struct {
	symbol   ReachabilitySymbol
	document StructuralDocument
	source   analysis.Symbol
}

// Reachability reads the already-persisted structural artifact and never
// starts repository analysis in the caller's request.
func (s *Service) Reachability(ctx context.Context, repositoryID int64) (ReachabilityReport, error) {
	index, err := s.ReadStructure(ctx, repositoryID)
	if err != nil {
		return ReachabilityReport{}, err
	}
	return buildReachability(index, s.currentBaseURL()), nil
}

func buildReachability(index StructuralIndex, baseURL string) ReachabilityReport {
	report := ReachabilityReport{
		Version:     index.Version,
		ID:          index.ID,
		GeneratedAt: index.GeneratedAt.UTC(),
		Scope:       index.Scope,
		Completeness: ReachabilityCompleteness{
			StructuralArtifactsComplete: index.Scope.Complete,
			RuntimeComplete:             false,
			DynamicBoundaries: []string{
				"reflection and string-based dispatch",
				"framework profiles, qualifiers, and conditional beans",
				"generated code and external runtime registration",
				"function values, callbacks, and configuration-driven entry points",
			},
		},
		Symbols: []ReachabilitySymbol{},
		Edges:   []ReachabilityEdge{},
	}
	declarations := make([]reachabilityDeclaration, 0)
	byName := make(map[string][]int)
	for _, document := range index.Structure {
		if document.Truncated {
			report.Completeness.TruncatedDocuments++
		}
		if !document.ParseComplete {
			report.Completeness.IncompleteDocuments++
		}
		for _, symbol := range document.Symbols {
			if !isReachabilityDeclaration(symbol.Kind) {
				continue
			}
			id := fmt.Sprintf(
				"symbol:%d:%s:%s:%s:%d",
				document.RepositoryID,
				document.Path,
				symbol.Kind,
				symbol.Name,
				symbol.Range.StartLine,
			)
			rootKind := reachabilityRoot(document.Language, symbol)
			declaration := reachabilityDeclaration{
				document: document,
				source:   symbol,
				symbol: ReachabilitySymbol{
					ID:          id,
					Name:        symbol.Name,
					Kind:        symbol.Kind,
					Language:    document.Language,
					State:       ReachabilityStateUnknown,
					Reason:      "runtime reachability is not proven by the available static evidence",
					RootKind:    rootKind,
					Annotations: append([]string(nil), symbol.Annotations...),
					Modifiers:   append([]string(nil), symbol.Modifiers...),
					Evidence:    reachabilityEvidence(baseURL, document, symbol.Range.StartLine, symbol.Name),
				},
			}
			declarations = append(declarations, declaration)
			index := len(declarations) - 1
			name := normalizedRelationTarget(symbol.Name)
			if name != "" {
				byName[name] = append(byName[name], index)
			}
		}
	}

	adjacency := make(map[int][]int)
	edgeSeen := make(map[string]struct{})
	for _, document := range index.Structure {
		for _, relation := range document.Relations {
			if relation.Kind == "import" {
				continue
			}
			sourceIndex := containingDeclaration(declarations, document, relation.Range)
			if sourceIndex < 0 {
				report.Completeness.UnresolvedRelations++
				continue
			}
			targets := byName[normalizedRelationTarget(relation.Target)]
			if len(targets) == 0 {
				report.Completeness.UnresolvedRelations++
				continue
			}
			if len(targets) > 1 {
				report.Completeness.AmbiguousRelations++
			}
			confidence := "syntax"
			if len(targets) > 1 {
				confidence = "ambiguous"
			}
			for _, targetIndex := range targets {
				from, to := sourceIndex, targetIndex
				if relation.Kind == "implements" || relation.Kind == "extends" {
					from, to = targetIndex, sourceIndex
				}
				if from == to {
					continue
				}
				key := declarations[from].symbol.ID + "\x00" + declarations[to].symbol.ID + "\x00" + relation.Kind
				if _, exists := edgeSeen[key]; exists {
					continue
				}
				edgeSeen[key] = struct{}{}
				adjacency[from] = append(adjacency[from], to)
				report.Edges = append(report.Edges, ReachabilityEdge{
					Source:     declarations[from].symbol.ID,
					Target:     declarations[to].symbol.ID,
					Kind:       relation.Kind,
					Confidence: confidence,
					Evidence: reachabilityEvidence(
						baseURL,
						document,
						relation.Range.StartLine,
						relation.Target,
					),
				})
			}
		}
	}

	report.Completeness.DocumentsComplete =
		!index.StructureTruncated &&
			report.Completeness.TruncatedDocuments == 0 &&
			report.Completeness.IncompleteDocuments == 0
	report.Completeness.StaticAnalysisComplete =
		report.Completeness.StructuralArtifactsComplete &&
			report.Completeness.DocumentsComplete

	queue := make([]int, 0)
	parents := make(map[int]int)
	for index := range declarations {
		if declarations[index].symbol.RootKind == "" {
			continue
		}
		declarations[index].symbol.State = ReachabilityStateReachable
		declarations[index].symbol.Reason = "recognized executable or framework entry point"
		queue = append(queue, index)
		parents[index] = -1
		report.Summary.Roots++
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, target := range adjacency[current] {
			if declarations[target].symbol.State == ReachabilityStateReachable {
				continue
			}
			declarations[target].symbol.State = ReachabilityStateReachable
			declarations[target].symbol.Reason = "reachable from a recognized root through syntax-backed relations"
			parents[target] = current
			queue = append(queue, target)
		}
	}

	for index := range declarations {
		symbol := &declarations[index].symbol
		switch symbol.State {
		case ReachabilityStateReachable:
			symbol.Witness = reachabilityWitness(declarations, parents, index)
			report.Summary.Reachable++
		default:
			if report.Completeness.StaticAnalysisComplete && isPrivateDeclaration(declarations[index]) {
				symbol.State = ReachabilityStateProbablyUnreachable
				symbol.Reason = "private declaration has no path from a recognized static root; dynamic boundaries still prevent a dead-code verdict"
				report.Summary.ProbablyUnreachable++
			} else {
				report.Summary.Unknown++
			}
		}
		report.Symbols = append(report.Symbols, *symbol)
	}
	report.Summary.Edges = len(report.Edges)
	sort.Slice(report.Symbols, func(i, j int) bool {
		if report.Symbols[i].State != report.Symbols[j].State {
			return report.Symbols[i].State < report.Symbols[j].State
		}
		if report.Symbols[i].Evidence.Path != report.Symbols[j].Evidence.Path {
			return report.Symbols[i].Evidence.Path < report.Symbols[j].Evidence.Path
		}
		return report.Symbols[i].Evidence.Line < report.Symbols[j].Evidence.Line
	})
	sort.Slice(report.Edges, func(i, j int) bool {
		if report.Edges[i].Source != report.Edges[j].Source {
			return report.Edges[i].Source < report.Edges[j].Source
		}
		if report.Edges[i].Target != report.Edges[j].Target {
			return report.Edges[i].Target < report.Edges[j].Target
		}
		return report.Edges[i].Kind < report.Edges[j].Kind
	})
	return report
}

func isReachabilityDeclaration(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "class", "interface", "object", "enum", "function", "method", "constructor":
		return true
	default:
		return false
	}
}

func reachabilityRoot(language string, symbol analysis.Symbol) string {
	if symbol.Name == "main" && (language == "go" || language == "java" || language == "kotlin") {
		return "executable main"
	}
	if language == "go" && symbol.Name == "init" {
		return "Go package initializer"
	}
	for _, annotation := range symbol.Annotations {
		if root, found := frameworkRootAnnotations[annotation]; found {
			return root
		}
	}
	return ""
}

func containingDeclaration(
	declarations []reachabilityDeclaration,
	document StructuralDocument,
	relation analysis.Range,
) int {
	best := -1
	bestSpan := int(^uint(0) >> 1)
	for index, declaration := range declarations {
		if declaration.document.RepositoryID != document.RepositoryID ||
			declaration.document.Path != document.Path {
			continue
		}
		candidate := declaration.source.Range
		if relation.StartByte < candidate.StartByte || relation.EndByte > candidate.EndByte {
			continue
		}
		span := candidate.EndByte - candidate.StartByte
		if span < bestSpan {
			best = index
			bestSpan = span
		}
	}
	return best
}

func normalizedRelationTarget(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if generic := strings.IndexByte(value, '<'); generic >= 0 {
		value = value[:generic]
	}
	value = strings.Trim(value, "[]?*& ")
	if separator := strings.LastIndexAny(value, ".:/\\"); separator >= 0 {
		value = value[separator+1:]
	}
	if fields := strings.Fields(value); len(fields) > 0 {
		value = fields[len(fields)-1]
	}
	return strings.TrimSpace(value)
}

func isPrivateDeclaration(declaration reachabilityDeclaration) bool {
	if slices.Contains(declaration.source.Modifiers, "private") ||
		slices.Contains(declaration.source.Modifiers, "internal") {
		return true
	}
	if declaration.document.Language == "go" && declaration.symbol.Kind == "function" {
		first := declaration.symbol.Name[0]
		return first < 'A' || first > 'Z'
	}
	return false
}

func reachabilityWitness(
	declarations []reachabilityDeclaration,
	parents map[int]int,
	index int,
) []string {
	reversed := make([]string, 0)
	for current := index; current >= 0; current = parents[current] {
		reversed = append(reversed, declarations[current].symbol.ID)
		parent, found := parents[current]
		if !found || parent < 0 {
			break
		}
	}
	slices.Reverse(reversed)
	return reversed
}

func reachabilityEvidence(
	baseURL string,
	document StructuralDocument,
	line int,
	label string,
) Evidence {
	line = max(1, line)
	sourceURL := ""
	if document.Path != "" {
		sourceURL = fmt.Sprintf(
			"%s/source/%d?rev=%s&path=%s&focus=%d-%d#L%d",
			strings.TrimRight(baseURL, "/"),
			document.RepositoryID,
			url.QueryEscape(document.Revision),
			url.QueryEscape(document.Path),
			line,
			line,
			line,
		)
	}
	return Evidence{
		RepositoryID: document.RepositoryID,
		Repository:   document.Repository,
		Revision:     document.Revision,
		Path:         document.Path,
		Line:         line,
		Label:        label,
		URL:          sourceURL,
	}
}
