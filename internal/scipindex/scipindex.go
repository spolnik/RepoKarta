// Package scipindex imports compiler-produced SCIP indexes into compact,
// commit-bound artifacts that RepoKarta can query without loading protobufs in
// an interactive request.
package scipindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

const (
	artifactVersion        = 2
	maximumIndexBytes      = 256 << 20
	maximumArtifactBytes   = 512 << 20
	maximumDocuments       = 200_000
	maximumOccurrences     = 2_000_000
	maximumSymbolLength    = 16 << 10
	maximumDisplayNameLen  = 4 << 10
	maximumCachedArtifacts = 16
)

// Store owns RepoKarta's derived SCIP artifacts.
type Store struct {
	directory string
	cacheMu   sync.Mutex
	cache     map[string]cachedArtifact
	loads     map[string]*artifactLoad
}

type cachedArtifact struct {
	artifact Artifact
	found    bool
	lastUsed time.Time
}

type artifactLoad struct {
	done     chan struct{}
	artifact Artifact
	found    bool
	err      error
}

// Artifact is a compact projection of one compiler-produced SCIP index.
type Artifact struct {
	Version      int        `json:"version"`
	RepositoryID int64      `json:"repository_id"`
	Revision     string     `json:"revision"`
	ImportedAt   time.Time  `json:"imported_at"`
	Indexer      Tool       `json:"indexer"`
	SourceRoot   string     `json:"source_root,omitempty"`
	Symbols      []Symbol   `json:"symbols,omitempty"`
	Documents    []Document `json:"documents"`
}

// Tool identifies the language indexer that produced the source artifact.
type Tool struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// Symbol keeps the stable SCIP identity and the indexer's display name.
type Symbol struct {
	ID              string         `json:"id"`
	DisplayName     string         `json:"display_name,omitempty"`
	Kind            string         `json:"kind,omitempty"`
	Signature       string         `json:"signature,omitempty"`
	Documentation   []string       `json:"documentation,omitempty"`
	EnclosingSymbol string         `json:"enclosing_symbol,omitempty"`
	Relationships   []Relationship `json:"relationships,omitempty"`
	External        bool           `json:"external,omitempty"`
}

// Relationship preserves compiler-produced navigation links.
type Relationship struct {
	Symbol         string `json:"symbol"`
	Reference      bool   `json:"reference,omitempty"`
	Implementation bool   `json:"implementation,omitempty"`
	TypeDefinition bool   `json:"type_definition,omitempty"`
	Definition     bool   `json:"definition,omitempty"`
}

// Document contains only source locations required by precise navigation.
type Document struct {
	Path        string       `json:"path"`
	Language    string       `json:"language,omitempty"`
	Occurrences []Occurrence `json:"occurrences,omitempty"`
}

// Occurrence is one compiler-resolved use or definition of a SCIP symbol.
type Occurrence struct {
	Symbol      string `json:"symbol"`
	SymbolRoles int32  `json:"symbol_roles,omitempty"`
	StartLine   int32  `json:"start_line"`
	StartColumn int32  `json:"start_column"`
	EndLine     int32  `json:"end_line"`
	EndColumn   int32  `json:"end_column"`
}

// ImportSummary reports the exact immutable artifact that was published.
type ImportSummary struct {
	RepositoryID int64  `json:"repository_id"`
	Revision     string `json:"revision"`
	SourceRoot   string `json:"source_root,omitempty"`
	Indexer      Tool   `json:"indexer"`
	Documents    int    `json:"documents"`
	Symbols      int    `json:"symbols"`
	Occurrences  int    `json:"occurrences"`
}

// Resolution is the result of resolving a user query against one complete set
// of repository artifacts.
type Resolution struct {
	State           string      `json:"state"`
	Symbol          string      `json:"symbol,omitempty"`
	References      []Reference `json:"references,omitempty"`
	Definitions     []Reference `json:"definitions,omitempty"`
	Implementations []Reference `json:"implementations,omitempty"`
	Candidates      []string    `json:"candidates,omitempty"`
	Hover           *Hover      `json:"hover,omitempty"`
	Stitched        bool        `json:"cross_repository_stitched"`
}

// Reference is one non-definition occurrence of an exact semantic symbol.
type Reference struct {
	RepositoryID int64  `json:"repository_id"`
	Revision     string `json:"revision"`
	Path         string `json:"path"`
	Language     string `json:"language,omitempty"`
	Symbol       string `json:"symbol"`
	Kind         string `json:"kind"`
	Line         int    `json:"line"`
}

// Hover is compiler-produced symbol documentation. It is absent rather than
// synthesized when the indexer did not provide it.
type Hover struct {
	Symbol        string   `json:"symbol"`
	DisplayName   string   `json:"display_name,omitempty"`
	Kind          string   `json:"kind,omitempty"`
	Signature     string   `json:"signature,omitempty"`
	Documentation []string `json:"documentation,omitempty"`
}

// IsSymbol reports whether value is a bounded, valid full SCIP identity.
func IsSymbol(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximumSymbolLength {
		return false
	}
	_, err := scip.ParseSymbol(value)
	return err == nil
}

// New creates a SCIP artifact store below RepoKarta's data directory.
func New(directory string) (*Store, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, errors.New("SCIP artifact directory is required")
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create SCIP artifact directory: %w", err)
	}
	return &Store{
		directory: directory,
		cache:     make(map[string]cachedArtifact),
		loads:     make(map[string]*artifactLoad),
	}, nil
}

// Import validates and atomically publishes a standard protobuf index.scip.
func (s *Store) Import(
	ctx context.Context,
	repositoryID int64,
	revision string,
	sourceRoot string,
	reader io.Reader,
) (ImportSummary, error) {
	if repositoryID <= 0 {
		return ImportSummary{}, errors.New("repository ID is required")
	}
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return ImportSummary{}, errors.New("indexed revision is required")
	}
	sourceRoot, err := canonicalSourceRoot(sourceRoot)
	if err != nil {
		return ImportSummary{}, err
	}
	content, err := io.ReadAll(io.LimitReader(reader, maximumIndexBytes+1))
	if err != nil {
		return ImportSummary{}, fmt.Errorf("read SCIP index: %w", err)
	}
	if len(content) > maximumIndexBytes {
		return ImportSummary{}, fmt.Errorf("SCIP index exceeds %d bytes", maximumIndexBytes)
	}
	if err := ctx.Err(); err != nil {
		return ImportSummary{}, err
	}

	var input scip.Index
	if err := (proto.UnmarshalOptions{RecursionLimit: 100}).Unmarshal(content, &input); err != nil {
		return ImportSummary{}, fmt.Errorf("decode SCIP index: %w", err)
	}
	artifact, summary, err := project(repositoryID, revision, sourceRoot, &input)
	if err != nil {
		return ImportSummary{}, err
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		return ImportSummary{}, fmt.Errorf("encode SCIP artifact: %w", err)
	}
	fileName := filepath.Base(s.artifactPath(repositoryID, revision))
	temporary, err := os.CreateTemp(s.directory, fileName+".*.tmp")
	if err != nil {
		return ImportSummary{}, fmt.Errorf("create SCIP artifact: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return ImportSummary{}, fmt.Errorf("write SCIP artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return ImportSummary{}, fmt.Errorf("close SCIP artifact: %w", err)
	}
	artifactPath := s.artifactPath(repositoryID, revision)
	if err := os.Rename(temporaryName, artifactPath); err != nil {
		if _, statErr := os.Stat(artifactPath); statErr != nil {
			return ImportSummary{}, fmt.Errorf("publish SCIP artifact: %w", err)
		}
		// Windows does not replace an existing destination with os.Rename.
		// Imports are documented as offline operations, so remove only this
		// exact derived artifact before publishing its replacement.
		if removeErr := os.Remove(artifactPath); removeErr != nil {
			return ImportSummary{}, fmt.Errorf("replace SCIP artifact: %w", removeErr)
		}
		if renameErr := os.Rename(temporaryName, artifactPath); renameErr != nil {
			return ImportSummary{}, fmt.Errorf("publish SCIP artifact: %w", renameErr)
		}
	}
	s.cacheMu.Lock()
	delete(s.cache, artifactCacheKey(repositoryID, revision))
	s.cacheMu.Unlock()
	return summary, nil
}

// Read loads an already-imported artifact for exactly one repository revision.
func (s *Store) Read(
	ctx context.Context,
	repositoryID int64,
	revision string,
) (Artifact, bool, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, false, err
	}
	key := artifactCacheKey(repositoryID, revision)
	s.cacheMu.Lock()
	if cached, ok := s.cache[key]; ok {
		cached.lastUsed = time.Now()
		s.cache[key] = cached
		s.cacheMu.Unlock()
		return cached.artifact, cached.found, nil
	}
	if loading, ok := s.loads[key]; ok {
		s.cacheMu.Unlock()
		select {
		case <-ctx.Done():
			return Artifact{}, false, ctx.Err()
		case <-loading.done:
			return loading.artifact, loading.found, loading.err
		}
	}
	loading := &artifactLoad{done: make(chan struct{})}
	s.loads[key] = loading
	s.cacheMu.Unlock()

	artifact, found, err := s.readArtifact(ctx, repositoryID, revision)
	s.cacheMu.Lock()
	loading.artifact, loading.found, loading.err = artifact, found, err
	if err == nil {
		s.cache[key] = cachedArtifact{
			artifact: artifact, found: found, lastUsed: time.Now(),
		}
		s.evictArtifactsLocked()
	}
	delete(s.loads, key)
	close(loading.done)
	s.cacheMu.Unlock()
	return artifact, found, err
}

func (s *Store) readArtifact(
	ctx context.Context,
	repositoryID int64,
	revision string,
) (Artifact, bool, error) {
	file, err := os.Open(s.artifactPath(repositoryID, revision))
	if errors.Is(err, os.ErrNotExist) {
		return Artifact{}, false, nil
	}
	if err != nil {
		return Artifact{}, false, fmt.Errorf("read SCIP artifact: %w", err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maximumArtifactBytes+1))
	if err != nil {
		return Artifact{}, false, fmt.Errorf("read SCIP artifact: %w", err)
	}
	if len(content) > maximumArtifactBytes {
		return Artifact{}, false, fmt.Errorf("SCIP artifact exceeds %d bytes", maximumArtifactBytes)
	}
	var artifact Artifact
	if err := json.Unmarshal(content, &artifact); err != nil {
		return Artifact{}, false, fmt.Errorf("decode SCIP artifact: %w", err)
	}
	if artifact.Version != artifactVersion ||
		artifact.RepositoryID != repositoryID ||
		artifact.Revision != revision {
		return Artifact{}, false, errors.New("SCIP artifact identity does not match its requested repository revision")
	}
	return artifact, true, nil
}

func (s *Store) evictArtifactsLocked() {
	for len(s.cache) > maximumCachedArtifacts {
		var oldestKey string
		var oldest time.Time
		for key, cached := range s.cache {
			if oldestKey == "" || cached.lastUsed.Before(oldest) {
				oldestKey, oldest = key, cached.lastUsed
			}
		}
		delete(s.cache, oldestKey)
	}
}

func artifactCacheKey(repositoryID int64, revision string) string {
	return strconv.FormatInt(repositoryID, 10) + "\x00" + revision
}

// ResolveReferences selects an exact SCIP identity. A source-level name is
// accepted only when it denotes one symbol across the complete supplied scope.
func ResolveReferences(artifacts []Artifact, query string) Resolution {
	query = strings.TrimSpace(query)
	if query == "" {
		return Resolution{State: "not-found"}
	}
	exact := false
	if IsSymbol(query) {
		exact = true
	}
	candidates := make(map[string]struct{})
	for _, artifact := range artifacts {
		for _, symbol := range artifact.Symbols {
			if (exact && symbol.ID == query) ||
				(!exact && symbol.DisplayName == query) {
				candidates[symbol.ID] = struct{}{}
			}
		}
	}
	if exact && len(candidates) == 0 {
		for _, artifact := range artifacts {
			for _, document := range artifact.Documents {
				for _, occurrence := range document.Occurrences {
					if occurrence.Symbol == query {
						candidates[query] = struct{}{}
					}
				}
			}
		}
	}
	orderedCandidates := make([]string, 0, len(candidates))
	for candidate := range candidates {
		orderedCandidates = append(orderedCandidates, candidate)
	}
	sort.Strings(orderedCandidates)
	if len(orderedCandidates) == 0 {
		return Resolution{State: "not-found"}
	}
	if len(orderedCandidates) > 1 {
		return Resolution{State: "ambiguous", Candidates: orderedCandidates}
	}

	resolved := orderedCandidates[0]
	metadata := make(map[string]Symbol)
	for _, artifact := range artifacts {
		for _, symbol := range artifact.Symbols {
			metadata[symbol.ID] = symbol
		}
	}
	referenceSymbols := map[string]struct{}{resolved: {}}
	implementationSymbols := make(map[string]struct{})
	definitionSymbols := map[string]struct{}{resolved: {}}
	for _, relationship := range metadata[resolved].Relationships {
		if relationship.Reference {
			referenceSymbols[relationship.Symbol] = struct{}{}
		}
		if relationship.Implementation {
			implementationSymbols[resolved] = struct{}{}
		}
		if relationship.Definition || relationship.TypeDefinition {
			definitionSymbols[relationship.Symbol] = struct{}{}
		}
	}
	for _, symbol := range metadata {
		for _, relationship := range symbol.Relationships {
			if relationship.Symbol != resolved {
				continue
			}
			if relationship.Reference {
				referenceSymbols[symbol.ID] = struct{}{}
			}
			if relationship.Implementation {
				implementationSymbols[symbol.ID] = struct{}{}
			}
			if relationship.Definition || relationship.TypeDefinition {
				definitionSymbols[symbol.ID] = struct{}{}
			}
		}
	}
	references := make([]Reference, 0)
	definitions := make([]Reference, 0)
	implementations := make([]Reference, 0)
	for _, artifact := range artifacts {
		for _, document := range artifact.Documents {
			for _, occurrence := range document.Occurrences {
				location := Reference{
					RepositoryID: artifact.RepositoryID,
					Revision:     artifact.Revision,
					Path:         document.Path,
					Language:     document.Language,
					Symbol:       occurrence.Symbol,
					Kind:         occurrenceKind(occurrence.SymbolRoles),
					Line:         int(occurrence.StartLine) + 1,
				}
				if occurrence.SymbolRoles&int32(scip.SymbolRole_Definition) != 0 {
					if _, ok := definitionSymbols[occurrence.Symbol]; ok {
						location.Kind = "definition"
						definitions = append(definitions, location)
					}
					if _, ok := implementationSymbols[occurrence.Symbol]; ok {
						location.Kind = "implementation"
						implementations = append(implementations, location)
					}
					continue
				}
				if _, ok := referenceSymbols[occurrence.Symbol]; ok {
					references = append(references, location)
				}
			}
		}
	}
	sortReferences := func(values []Reference) {
		sort.Slice(values, func(left, right int) bool {
			if values[left].RepositoryID != values[right].RepositoryID {
				return values[left].RepositoryID < values[right].RepositoryID
			}
			if values[left].Path != values[right].Path {
				return values[left].Path < values[right].Path
			}
			return values[left].Line < values[right].Line
		})
	}
	sortReferences(references)
	sortReferences(definitions)
	sortReferences(implementations)
	var hover *Hover
	if symbol, ok := metadata[resolved]; ok {
		hover = &Hover{
			Symbol:        symbol.ID,
			DisplayName:   symbol.DisplayName,
			Kind:          symbol.Kind,
			Signature:     symbol.Signature,
			Documentation: append([]string(nil), symbol.Documentation...),
		}
	}
	state := "unique-name"
	if exact {
		state = "exact"
	}
	return Resolution{
		State:           state,
		Symbol:          resolved,
		References:      references,
		Definitions:     definitions,
		Implementations: implementations,
		Candidates:      orderedCandidates,
		Hover:           hover,
		Stitched:        len(artifacts) > 1,
	}
}

func project(
	repositoryID int64,
	revision string,
	sourceRoot string,
	input *scip.Index,
) (Artifact, ImportSummary, error) {
	if input.Metadata == nil {
		return Artifact{}, ImportSummary{}, errors.New("SCIP index metadata is required")
	}
	if len(input.Documents) > maximumDocuments {
		return Artifact{}, ImportSummary{}, fmt.Errorf(
			"SCIP index contains %d documents; maximum is %d",
			len(input.Documents),
			maximumDocuments,
		)
	}
	metadataBySymbol := make(map[string]Symbol)
	addSymbol := func(info *scip.SymbolInformation, external bool) error {
		if info == nil || strings.TrimSpace(info.Symbol) == "" {
			return nil
		}
		if len(info.Symbol) > maximumSymbolLength {
			return errors.New("SCIP symbol exceeds the supported length")
		}
		if _, err := scip.ParseSymbol(info.Symbol); err != nil {
			return fmt.Errorf("invalid SCIP symbol %q: %w", info.Symbol, err)
		}
		displayName := strings.TrimSpace(info.DisplayName)
		if displayName == "" {
			displayName = symbolDisplayName(info.Symbol)
		}
		if len(displayName) > maximumDisplayNameLen {
			return errors.New("SCIP display name exceeds the supported length")
		}
		relationships := make([]Relationship, 0, len(info.Relationships))
		for _, relationship := range info.Relationships {
			if relationship == nil || strings.TrimSpace(relationship.Symbol) == "" {
				continue
			}
			if len(relationship.Symbol) > maximumSymbolLength {
				return errors.New("SCIP relationship symbol exceeds the supported length")
			}
			if _, err := scip.ParseSymbol(relationship.Symbol); err != nil {
				return fmt.Errorf("invalid SCIP relationship symbol %q: %w", relationship.Symbol, err)
			}
			relationships = append(relationships, Relationship{
				Symbol:         relationship.Symbol,
				Reference:      relationship.IsReference,
				Implementation: relationship.IsImplementation,
				TypeDefinition: relationship.IsTypeDefinition,
				Definition:     relationship.IsDefinition,
			})
		}
		signature := ""
		if info.SignatureDocumentation != nil {
			signature = strings.TrimSpace(info.SignatureDocumentation.Text)
		}
		metadataBySymbol[info.Symbol] = Symbol{
			ID:              info.Symbol,
			DisplayName:     displayName,
			Kind:            info.Kind.String(),
			Signature:       signature,
			Documentation:   append([]string(nil), info.Documentation...),
			EnclosingSymbol: strings.TrimSpace(info.EnclosingSymbol),
			Relationships:   relationships,
			External:        external,
		}
		return nil
	}
	for _, info := range input.ExternalSymbols {
		if err := addSymbol(info, true); err != nil {
			return Artifact{}, ImportSummary{}, err
		}
	}

	documents := make([]Document, 0, len(input.Documents))
	occurrenceCount := 0
	seenPaths := make(map[string]struct{}, len(input.Documents))
	for _, inputDocument := range input.Documents {
		if inputDocument == nil {
			continue
		}
		documentPath, err := canonicalDocumentPath(inputDocument.RelativePath)
		if err != nil {
			return Artifact{}, ImportSummary{}, err
		}
		if sourceRoot != "." {
			documentPath = path.Join(sourceRoot, documentPath)
		}
		if _, exists := seenPaths[documentPath]; exists {
			return Artifact{}, ImportSummary{}, fmt.Errorf("duplicate SCIP document path %q", documentPath)
		}
		seenPaths[documentPath] = struct{}{}
		for _, info := range inputDocument.Symbols {
			if err := addSymbol(info, false); err != nil {
				return Artifact{}, ImportSummary{}, err
			}
		}
		document := Document{
			Path:        documentPath,
			Language:    strings.TrimSpace(inputDocument.Language),
			Occurrences: make([]Occurrence, 0, len(inputDocument.Occurrences)),
		}
		for _, inputOccurrence := range inputDocument.Occurrences {
			if inputOccurrence == nil || strings.TrimSpace(inputOccurrence.Symbol) == "" {
				continue
			}
			occurrenceCount++
			if occurrenceCount > maximumOccurrences {
				return Artifact{}, ImportSummary{}, fmt.Errorf(
					"SCIP index contains more than %d symbol occurrences",
					maximumOccurrences,
				)
			}
			if len(inputOccurrence.Symbol) > maximumSymbolLength {
				return Artifact{}, ImportSummary{}, errors.New("SCIP symbol exceeds the supported length")
			}
			if _, err := scip.ParseSymbol(inputOccurrence.Symbol); err != nil {
				return Artifact{}, ImportSummary{}, fmt.Errorf(
					"invalid SCIP occurrence symbol %q: %w",
					inputOccurrence.Symbol,
					err,
				)
			}
			sourceRange, ok := inputOccurrence.SourceRange()
			if !ok {
				return Artifact{}, ImportSummary{}, fmt.Errorf(
					"SCIP occurrence for %q in %q has an invalid range",
					inputOccurrence.Symbol,
					documentPath,
				)
			}
			document.Occurrences = append(document.Occurrences, Occurrence{
				Symbol:      inputOccurrence.Symbol,
				SymbolRoles: inputOccurrence.SymbolRoles,
				StartLine:   sourceRange.Start.Line,
				StartColumn: sourceRange.Start.Character,
				EndLine:     sourceRange.End.Line,
				EndColumn:   sourceRange.End.Character,
			})
			if _, exists := metadataBySymbol[inputOccurrence.Symbol]; !exists {
				metadataBySymbol[inputOccurrence.Symbol] = Symbol{
					ID:          inputOccurrence.Symbol,
					DisplayName: symbolDisplayName(inputOccurrence.Symbol),
				}
			}
		}
		sort.Slice(document.Occurrences, func(left, right int) bool {
			if document.Occurrences[left].StartLine != document.Occurrences[right].StartLine {
				return document.Occurrences[left].StartLine < document.Occurrences[right].StartLine
			}
			if document.Occurrences[left].StartColumn != document.Occurrences[right].StartColumn {
				return document.Occurrences[left].StartColumn < document.Occurrences[right].StartColumn
			}
			return document.Occurrences[left].Symbol < document.Occurrences[right].Symbol
		})
		documents = append(documents, document)
	}
	sort.Slice(documents, func(left, right int) bool {
		return documents[left].Path < documents[right].Path
	})
	symbols := make([]Symbol, 0, len(metadataBySymbol))
	for _, symbol := range metadataBySymbol {
		symbols = append(symbols, symbol)
	}
	sort.Slice(symbols, func(left, right int) bool {
		return symbols[left].ID < symbols[right].ID
	})
	tool := Tool{}
	if input.Metadata.ToolInfo != nil {
		tool.Name = strings.TrimSpace(input.Metadata.ToolInfo.Name)
		tool.Version = strings.TrimSpace(input.Metadata.ToolInfo.Version)
	}
	artifact := Artifact{
		Version:      artifactVersion,
		RepositoryID: repositoryID,
		Revision:     revision,
		ImportedAt:   time.Now().UTC(),
		Indexer:      tool,
		SourceRoot:   sourceRoot,
		Symbols:      symbols,
		Documents:    documents,
	}
	return artifact, ImportSummary{
		RepositoryID: repositoryID,
		Revision:     revision,
		SourceRoot:   sourceRoot,
		Indexer:      tool,
		Documents:    len(documents),
		Symbols:      len(symbols),
		Occurrences:  occurrenceCount,
	}, nil
}

func canonicalSourceRoot(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return ".", nil
	}
	if value == "." {
		return value, nil
	}
	if strings.Contains(value, "\\") ||
		strings.HasPrefix(value, "/") ||
		path.Clean(value) != value ||
		strings.HasPrefix(value, "../") {
		return "", fmt.Errorf("SCIP source root %q is not canonical and repository-relative", value)
	}
	return value, nil
}

func canonicalDocumentPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" ||
		strings.Contains(value, "\\") ||
		strings.HasPrefix(value, "/") ||
		path.Clean(value) != value ||
		value == "." ||
		strings.HasPrefix(value, "../") {
		return "", fmt.Errorf("SCIP document path %q is not canonical and repository-relative", value)
	}
	return value, nil
}

func symbolDisplayName(value string) string {
	parsed, err := scip.ParseSymbol(value)
	if err != nil || len(parsed.Descriptors) == 0 {
		return ""
	}
	for index := len(parsed.Descriptors) - 1; index >= 0; index-- {
		descriptor := parsed.Descriptors[index]
		if descriptor == nil ||
			descriptor.Suffix == scip.Descriptor_Parameter ||
			descriptor.Suffix == scip.Descriptor_TypeParameter {
			continue
		}
		return descriptor.Name
	}
	return ""
}

func occurrenceKind(roles int32) string {
	if roles&int32(scip.SymbolRole_Import) != 0 {
		return "import"
	}
	if roles&int32(scip.SymbolRole_WriteAccess) != 0 {
		return "write"
	}
	if roles&int32(scip.SymbolRole_ReadAccess) != 0 {
		return "read"
	}
	return "reference"
}

func (s *Store) artifactPath(repositoryID int64, revision string) string {
	digest := sha256.Sum256([]byte(revision))
	return filepath.Join(
		s.directory,
		fmt.Sprintf(
			"repository-%d-%s.json",
			repositoryID,
			hex.EncodeToString(digest[:12]),
		),
	)
}
