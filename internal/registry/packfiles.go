package registry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// deniedDirectories are never packed, at any depth: VCS data, dependencies,
// tests and local fixtures.
var deniedDirectories = map[string]bool{
	".git":           true,
	"node_modules":   true,
	".beam-packages": true,
	"fixtures":       true,
	"test":           true,
	"tests":          true,
	"__tests__":      true,
}

// deniedFileRules are file names never packed, at any depth. Secrets and
// credentials cannot be re-included by "files" or .beamignore: an action
// artifact is downloadable by everyone who may run it.
var deniedFileRules = mustCompileRules(append(append([]string{
	".env",
	".env.*",
	".npmrc",
	"*.local",
	"*.pem",
}, sshKeyNames()...),
	"*.test.*",
	"*.spec.*",
	".DS_Store",
	".beamignore",
	"*.map",
))

// sshKeyNames lists SSH keys by their default names only, so code such as
// id_utils.mjs is still packed.
func sshKeyNames() []string {
	var names []string
	for _, key := range []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"} {
		names = append(names, key, key+".pub", key+"-cert", key+"-cert.pub")
	}
	return names
}

// requiredFiles are always packed when present: the Registry and runtimes
// read them from the archive.
var requiredFiles = []string{"beam-action.json", "beam-project.json", "package.json"}

type packRule struct {
	negate        bool
	directoryOnly bool
	anchored      bool
	pattern       *regexp.Regexp
}

// packageFiles lists the files to pack, relative to root with "/" separators,
// sorted.
//
// Selection: every regular file, or only those matched by "files" in the
// project's package.json (npm semantics: patterns are relative to the project
// root and a directory includes its contents); minus .beamignore (gitignore
// syntax); minus the built-in deny-list. The manifest, project settings,
// package.json and the entrypoint are always included.
func packageFiles(root, entrypoint string) ([]string, error) {
	includeRules, err := readIncludeRules(root)
	if err != nil {
		return nil, err
	}
	ignoreRules, err := readIgnoreRules(root)
	if err != nil {
		return nil, err
	}
	selected := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		slash := filepath.ToSlash(relative)
		if entry.IsDir() {
			// An excluded directory cannot be re-included, as in gitignore.
			if deniedDirectories[entry.Name()] || lastMatch(ignoreRules, slash, true) == matchExcluded {
				return filepath.SkipDir
			}
			return nil
		}
		// Symbolic links are never followed or packed.
		if !entry.Type().IsRegular() || deniedReason(slash) != "" || lastMatch(ignoreRules, slash, false) == matchExcluded {
			return nil
		}
		if includeRules == nil || included(includeRules, slash) {
			selected[slash] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, required := range append(append([]string{}, requiredFiles...), entrypoint) {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(required))); err != nil {
			continue
		}
		if denial := deniedReason(required); denial != "" {
			return nil, fmt.Errorf("%s is required in the package but is excluded because %s", required, denial)
		}
		selected[required] = true
	}
	result := make([]string, 0, len(selected))
	for file := range selected {
		result = append(result, file)
	}
	sort.Strings(result)
	return result, nil
}

// deniedReason explains why the built-in deny-list refuses file, or returns
// "" when it may be packed.
func deniedReason(file string) string {
	segments := strings.Split(file, "/")
	for _, segment := range segments[:len(segments)-1] {
		if deniedDirectories[segment] {
			return fmt.Sprintf("the %q directory is never packed", segment+"/")
		}
	}
	if lastMatch(deniedFileRules, file, false) == matchExcluded {
		return fmt.Sprintf("%q matches a file name that is never packed", segments[len(segments)-1])
	}
	return ""
}

func readIncludeRules(root string) ([]packRule, error) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read package.json: %w", err)
	}
	var project struct {
		Files json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(data, &project); err != nil {
		return nil, fmt.Errorf("parse package.json: %w", err)
	}
	if project.Files == nil {
		return nil, nil
	}
	var files []string
	if err := json.Unmarshal(project.Files, &files); err != nil || files == nil {
		return nil, fmt.Errorf("package.json files must be an array of strings")
	}
	rules := make([]packRule, 0, len(files))
	for _, entry := range files {
		if rule, ok := compilePackRule(entry, true); ok {
			rules = append(rules, rule)
		}
	}
	return rules, nil
}

func readIgnoreRules(root string) ([]packRule, error) {
	data, err := os.ReadFile(filepath.Join(root, ".beamignore"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read .beamignore: %w", err)
	}
	var rules []packRule
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		if rule, ok := compilePackRule(scanner.Text(), false); ok {
			rules = append(rules, rule)
		}
	}
	return rules, scanner.Err()
}

// included reports whether the last "files" rule matching file, or one of its
// ancestor directories, is positive.
func included(rules []packRule, file string) bool {
	segments := strings.Split(file, "/")
	state := false
	for _, rule := range rules {
		for length := 1; length <= len(segments); length++ {
			if rule.matches(strings.Join(segments[:length], "/"), length < len(segments)) {
				state = !rule.negate
				break
			}
		}
	}
	return state
}

type matchState int

const (
	matchNone matchState = iota
	matchExcluded
	matchReincluded
)

func lastMatch(rules []packRule, candidate string, isDirectory bool) matchState {
	state := matchNone
	for _, rule := range rules {
		if rule.matches(candidate, isDirectory) {
			if rule.negate {
				state = matchReincluded
			} else {
				state = matchExcluded
			}
		}
	}
	return state
}

func (rule packRule) matches(candidate string, isDirectory bool) bool {
	if rule.directoryOnly && !isDirectory {
		return false
	}
	subject := candidate
	if !rule.anchored {
		subject = candidate[strings.LastIndex(candidate, "/")+1:]
	}
	return rule.pattern.MatchString(subject)
}

// compilePackRule compiles one gitignore-style line: "#" comments, "!"
// negation, a trailing "/" for directories only, and a leading or inner "/"
// anchoring the pattern to the project root (otherwise it matches a name at
// any depth). "*" and "?" stay within one path segment; "**" spans segments.
func compilePackRule(line string, anchoredByDefault bool) (packRule, bool) {
	pattern := strings.TrimRight(line, " \t\r\n\f\v")
	if pattern == "" || strings.HasPrefix(pattern, "#") {
		return packRule{}, false
	}
	rule := packRule{}
	if strings.HasPrefix(pattern, "!") {
		rule.negate = true
		pattern = pattern[1:]
	} else if strings.HasPrefix(pattern, `\`) {
		pattern = pattern[1:]
	}
	if strings.HasSuffix(pattern, "/") {
		rule.directoryOnly = !anchoredByDefault
		pattern = strings.TrimRight(pattern, "/")
	}
	if anchoredByDefault {
		pattern = strings.TrimPrefix(pattern, "./")
	}
	rule.anchored = anchoredByDefault || strings.Contains(pattern, "/")
	pattern = strings.TrimLeft(pattern, "/")
	if pattern == "" {
		return packRule{}, false
	}
	rule.pattern = regexp.MustCompile("^" + globSource(pattern) + "$")
	return rule, true
}

func globSource(pattern string) string {
	var source strings.Builder
	for index := 0; index < len(pattern); index++ {
		character := pattern[index]
		switch {
		case character == '*' && index+1 < len(pattern) && pattern[index+1] == '*':
			if index+2 < len(pattern) && pattern[index+2] == '/' {
				source.WriteString("(?:.*/)?")
				index += 2
			} else {
				source.WriteString(".*")
				index++
			}
		case character == '*':
			source.WriteString("[^/]*")
		case character == '?':
			source.WriteString("[^/]")
		default:
			source.WriteString(regexp.QuoteMeta(pattern[index : index+1]))
		}
	}
	return source.String()
}

func mustCompileRules(patterns []string) []packRule {
	rules := make([]packRule, 0, len(patterns))
	for _, pattern := range patterns {
		rule, _ := compilePackRule(pattern, false)
		rules = append(rules, rule)
	}
	return rules
}
