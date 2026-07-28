package graph

import (
	"path"
	"regexp"
	"strings"
)

// OwnershipIndex is the commit-pinned CODEOWNERS projection for one
// repository. Available distinguishes a repository without CODEOWNERS from an
// empty file that intentionally owns nothing.
type OwnershipIndex struct {
	RepositoryID int64           `json:"repository_id"`
	Repository   string          `json:"repository"`
	Revision     string          `json:"revision"`
	Available    bool            `json:"available"`
	Path         string          `json:"path,omitempty"`
	Rules        []OwnershipRule `json:"rules,omitempty"`
	Evidence     Evidence        `json:"evidence,omitempty"`
}

// OwnershipRule preserves source order because the last matching CODEOWNERS
// rule wins.
type OwnershipRule struct {
	Pattern          string   `json:"pattern"`
	Owners           []string `json:"owners,omitempty"`
	UnresolvedOwners []string `json:"unresolved_owners,omitempty"`
	Line             int      `json:"line"`
}

// OwnershipMatch is the explicit owner state for one repository-relative path.
type OwnershipMatch struct {
	State            string   `json:"state"`
	Owners           []string `json:"owners,omitempty"`
	UnresolvedOwners []string `json:"unresolved_owners,omitempty"`
	Pattern          string   `json:"pattern,omitempty"`
	Evidence         Evidence `json:"evidence,omitempty"`
}

func parseCODEOWNERS(
	repository Repository,
	filePath string,
	content []byte,
	evidence func(string, int, string) Evidence,
) OwnershipIndex {
	index := OwnershipIndex{
		RepositoryID: repository.ID,
		Repository:   repository.Name,
		Revision:     repository.Revision,
		Available:    true,
		Path:         filePath,
		Evidence:     evidence(filePath, 1, "CODEOWNERS"),
		Rules:        []OwnershipRule{},
	}
	for lineIndex, raw := range strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		rule := OwnershipRule{Pattern: fields[0], Line: lineIndex + 1}
		for _, owner := range fields[1:] {
			owner = strings.TrimSpace(owner)
			if validCODEOWNER(owner) {
				rule.Owners = append(rule.Owners, owner)
			} else if owner != "" {
				rule.UnresolvedOwners = append(rule.UnresolvedOwners, owner)
			}
		}
		index.Rules = append(index.Rules, rule)
	}
	return index
}

func validCODEOWNER(owner string) bool {
	if strings.HasPrefix(owner, "@") {
		return len(owner) > 1 && !strings.ContainsAny(owner, " \t")
	}
	local, domain, found := strings.Cut(owner, "@")
	return found && local != "" && strings.Contains(domain, ".")
}

// ResolveOwners evaluates one path using CODEOWNERS last-match-wins semantics.
// Invalid or unsupported patterns simply do not match; their source remains in
// the index for diagnosis.
func ResolveOwners(index OwnershipIndex, filePath string) OwnershipMatch {
	if !index.Available {
		return OwnershipMatch{State: "unavailable"}
	}
	filePath = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(filePath), "\\", "/"), "/")
	var matched *OwnershipRule
	for ruleIndex := range index.Rules {
		if codeownersPatternMatches(index.Rules[ruleIndex].Pattern, filePath) {
			matched = &index.Rules[ruleIndex]
		}
	}
	if matched == nil {
		return OwnershipMatch{State: "unowned"}
	}
	evidence := index.Evidence
	evidence.Line = matched.Line
	evidence.Label = "CODEOWNERS " + matched.Pattern
	if len(matched.Owners) == 0 {
		return OwnershipMatch{
			State:            "unresolved_owner",
			UnresolvedOwners: append([]string(nil), matched.UnresolvedOwners...),
			Pattern:          matched.Pattern,
			Evidence:         evidence,
		}
	}
	state := "owned"
	if len(matched.UnresolvedOwners) > 0 {
		state = "unresolved_owner"
	}
	return OwnershipMatch{
		State:            state,
		Owners:           append([]string(nil), matched.Owners...),
		UnresolvedOwners: append([]string(nil), matched.UnresolvedOwners...),
		Pattern:          matched.Pattern,
		Evidence:         evidence,
	}
}

func codeownersPatternMatches(pattern, filePath string) bool {
	pattern = strings.TrimSpace(strings.ReplaceAll(pattern, "\\ ", " "))
	if pattern == "" || strings.HasPrefix(pattern, "!") || strings.ContainsAny(pattern, "[]") {
		return false
	}
	anchored := strings.HasPrefix(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "/")
	directory := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimSuffix(pattern, "/")
	if pattern == "" {
		return false
	}
	var expression strings.Builder
	if anchored || strings.Contains(pattern, "/") {
		expression.WriteString("^")
	} else {
		expression.WriteString(`(?:^|.*/)`)
	}
	for offset := 0; offset < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[offset:], "**"):
			expression.WriteString(".*")
			offset += 2
		case pattern[offset] == '*':
			expression.WriteString(`[^/]*`)
			offset++
		case pattern[offset] == '?':
			expression.WriteString(`[^/]`)
			offset++
		default:
			expression.WriteString(regexp.QuoteMeta(pattern[offset : offset+1]))
			offset++
		}
	}
	if directory {
		expression.WriteString(`(?:/.*)?$`)
	} else {
		expression.WriteString("$")
	}
	compiled, err := regexp.Compile(expression.String())
	return err == nil && compiled.MatchString(filePath)
}

func codeownersPath(files []string) string {
	available := make(map[string]struct{}, len(files))
	for _, filePath := range files {
		available[path.Clean(strings.ReplaceAll(filePath, "\\", "/"))] = struct{}{}
	}
	for _, candidate := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"} {
		if _, ok := available[candidate]; ok {
			return candidate
		}
	}
	return ""
}
