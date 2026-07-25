package catalog

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// maximumIgnoreFileBytes bounds how much of one .gitignore RepoKarta reads.
// Repository content is untrusted input, so a pathological ignore file must not
// consume unbounded memory during discovery.
const maximumIgnoreFileBytes = 256 << 10

// ignorePattern is one compiled .gitignore rule.
type ignorePattern struct {
	matcher *regexp.Regexp
	// negate marks a `!rule` line, which re-includes a previously ignored path.
	negate bool
	// directoryOnly marks a `rule/` line, which matches directories only.
	directoryOnly bool
}

// ignoreScope holds the rules of one .gitignore file together with the
// directory they apply to. Rules only affect paths beneath that directory.
type ignoreScope struct {
	base     string
	patterns []ignorePattern
}

// loadIgnoreScope reads the .gitignore in directory, if any. It returns false
// when the directory has no readable ignore file or the file declares no usable
// rule.
func loadIgnoreScope(directory string) (ignoreScope, bool) {
	file, err := os.Open(filepath.Join(directory, ".gitignore"))
	if err != nil {
		return ignoreScope{}, false
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.IsDir() || info.Size() > maximumIgnoreFileBytes {
		return ignoreScope{}, false
	}

	scope := ignoreScope{base: directory}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maximumIgnoreFileBytes)
	for scanner.Scan() {
		if pattern, ok := compileIgnorePattern(scanner.Text()); ok {
			scope.patterns = append(scope.patterns, pattern)
		}
	}
	if scanner.Err() != nil || len(scope.patterns) == 0 {
		return ignoreScope{}, false
	}
	return scope, true
}

// compileIgnorePattern translates one .gitignore line into a matcher over a
// slash-separated path relative to the file's directory. It covers the syntax
// that appears in practice: comments, negation, directory-only rules, anchored
// rules, `*`, `?`, `**`, and character classes.
func compileIgnorePattern(line string) (ignorePattern, bool) {
	line = strings.TrimRight(line, " \t")
	if line == "" || strings.HasPrefix(line, "#") {
		return ignorePattern{}, false
	}

	pattern := ignorePattern{}
	if strings.HasPrefix(line, "!") {
		pattern.negate = true
		line = line[1:]
	}
	// An escaped leading `#` or `!` is a literal character.
	if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		pattern.directoryOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if line == "" {
		return ignorePattern{}, false
	}

	// A rule containing a slash anywhere but the end is anchored to the
	// directory holding the .gitignore. Every other rule matches a path
	// component at any depth.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return ignorePattern{}, false
	}

	expression := "^"
	if !anchored {
		expression += "(?:.*/)?"
	}
	expression += globToRegexp(line) + "$"
	matcher, err := regexp.Compile(expression)
	if err != nil {
		return ignorePattern{}, false
	}
	pattern.matcher = matcher
	return pattern, true
}

// globToRegexp converts .gitignore glob syntax to a regular expression over a
// slash-separated relative path.
func globToRegexp(pattern string) string {
	var output strings.Builder
	for index := 0; index < len(pattern); index++ {
		character := pattern[index]
		switch character {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				index++
				// `**/` spans any number of directories, including none.
				if index+1 < len(pattern) && pattern[index+1] == '/' {
					index++
					output.WriteString("(?:.*/)?")
					continue
				}
				output.WriteString(".*")
				continue
			}
			output.WriteString("[^/]*")
		case '?':
			output.WriteString("[^/]")
		case '[':
			if closing := strings.IndexByte(pattern[index:], ']'); closing > 0 {
				class := pattern[index : index+closing+1]
				if strings.HasPrefix(class, "[!") {
					class = "[^" + class[2:]
				}
				output.WriteString(class)
				index += closing
				continue
			}
			output.WriteString(regexp.QuoteMeta("["))
		case '\\':
			if index+1 < len(pattern) {
				index++
				output.WriteString(regexp.QuoteMeta(string(pattern[index])))
				continue
			}
			output.WriteString(regexp.QuoteMeta("\\"))
		default:
			output.WriteString(regexp.QuoteMeta(string(character)))
		}
	}
	return output.String()
}

// ignoredDirectory reports whether the enclosing .gitignore files exclude a
// directory. Scopes are evaluated outermost first and, within one file, the
// last matching rule wins, which is Git's precedence.
func ignoredDirectory(scopes []ignoreScope, path string) bool {
	ignored := false
	for _, scope := range scopes {
		relative, err := filepath.Rel(scope.base, path)
		if err != nil {
			continue
		}
		relative = filepath.ToSlash(relative)
		if relative == "." || strings.HasPrefix(relative, "../") {
			continue
		}
		for _, pattern := range scope.patterns {
			// Every candidate here is a directory, so directory-only rules
			// need no extra handling.
			if pattern.matcher.MatchString(relative) {
				ignored = !pattern.negate
			}
		}
	}
	return ignored
}

// activeIgnoreScopes drops the scopes that no longer enclose path.
func activeIgnoreScopes(scopes []ignoreScope, path string) []ignoreScope {
	kept := scopes[:0]
	for _, scope := range scopes {
		relative, err := filepath.Rel(scope.base, path)
		if err != nil {
			continue
		}
		relative = filepath.ToSlash(relative)
		if relative == ".." || strings.HasPrefix(relative, "../") {
			continue
		}
		kept = append(kept, scope)
	}
	return kept
}
