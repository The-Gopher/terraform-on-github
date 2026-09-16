package tf

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Module `source` values have to be extracted before `terraform init` runs, because init is what
// fetches module code and a module fetch can execute it. Terraform has no native allowlist, so
// there is no setting to turn on instead: the check has to be a pre-pass over the HCL.
//
// What follows is a brace-tracking scanner, not an HCL parser and emphatically not a
// line-oriented grep. A line-oriented version misses
//
//	module "vpc" { source = "./x" }
//
// which is legal HCL, and a missed module block is a module source that never reaches the
// allowlist — the dangerous direction to be wrong in. Pulling in hashicorp/hcl/v2 would be more
// correct still; it is the right trade once the services need HCL for anything else, and until
// then a scanner with tests beats a dependency with none.

// ModuleRef is one `source` value and where it was found, for an error a human can act on.
type ModuleRef struct {
	Source string
	File   string // repo-relative
}

// CollectModuleSources walks the root module and every local module it reaches transitively,
// returning every `source` value found.
//
// Local sources are followed rather than trusted: a local module can itself pull a remote one,
// and that remote source is exactly what the allowlist exists to gate. A local source pointing
// outside the repository is an error in itself — it would pull code that is not part of the
// reviewed tree.
func CollectModuleSources(repoRoot, rootModuleDir string) ([]ModuleRef, error) {
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	absRoot, err = filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, err
	}
	start, err := filepath.Abs(rootModuleDir)
	if err != nil {
		return nil, err
	}

	var refs []ModuleRef
	seen := map[string]bool{}
	queue := []string{start}

	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if seen[dir] {
			continue
		}
		seen[dir] = true

		entries, err := os.ReadDir(dir)
		if err != nil {
			// A local module source pointing at a directory that does not exist is Terraform's
			// error to report, not ours; we only care that we cannot find a source to check.
			continue
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".tf") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)

		for _, name := range names {
			full := filepath.Join(dir, name)
			body, err := os.ReadFile(full)
			if err != nil {
				continue
			}
			rel, relErr := filepath.Rel(absRoot, full)
			if relErr != nil {
				rel = full
			}
			for _, src := range scanModuleSources(string(body)) {
				refs = append(refs, ModuleRef{Source: src, File: filepath.ToSlash(rel)})
				if !IsLocalSource(src) {
					continue
				}
				target := filepath.Clean(filepath.Join(dir, src))
				inside, err := withinRoot(absRoot, target)
				if err != nil || !inside {
					return nil, fmt.Errorf(
						"%s: local module source %q resolves outside the repository root; it would pull code that is not part of the reviewed tree",
						filepath.ToSlash(rel), src)
				}
				queue = append(queue, target)
			}
		}
	}
	return refs, nil
}

func withinRoot(root, target string) (bool, error) {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false, err
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// IsLocalSource reports whether a `source` is a local path. Terraform treats exactly "./" and
// "../" prefixes as local; everything else goes to a fetcher.
func IsLocalSource(src string) bool {
	return strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../")
}

// CheckModuleSources is the entry point: collect every source under the root module, then
// enforce the allowlist. Called before Init, because Init is what fetches module code.
func CheckModuleSources(repoRoot, rootModuleDir string, allow []string) error {
	refs, err := CollectModuleSources(repoRoot, rootModuleDir)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrModuleSourceDenied, err)
	}
	return CheckModuleRefs(refs, allow)
}

// CheckModuleRefs enforces the allowlist. An empty allowlist permits local paths only, per
// docs/CONFIG.md — a repo that declares nothing gets the most restrictive reading, not the least.
//
// Patterns are matched with `*` crossing `/`, because these are URLs rather than paths: an entry
// like `git::ssh://git@github.com/acme/mods//*?ref=v*` is meant to cover a nested module path.
// That is the opposite convention from Watch globs in internal/scope, deliberately, because the
// two are matching different kinds of string.
func CheckModuleRefs(refs []ModuleRef, allow []string) error {
	var denied []string
	for _, r := range refs {
		if IsLocalSource(r.Source) {
			continue
		}
		if matchAnyURLGlob(r.Source, allow) {
			continue
		}
		denied = append(denied, fmt.Sprintf("  %s: %s", r.File, r.Source))
	}
	if len(denied) == 0 {
		return nil
	}
	hint := "add a matching glob to `module_sources` in the config on the trusted ref"
	if len(allow) == 0 {
		hint = "`module_sources` is empty, which permits local paths only"
	}
	return fmt.Errorf("%w (%s):\n%s", ErrModuleSourceDenied, hint, strings.Join(denied, "\n"))
}

func matchAnyURLGlob(s string, patterns []string) bool {
	for _, p := range patterns {
		if matchURLGlob(p, s) {
			return true
		}
	}
	return false
}

// matchURLGlob is fnmatch with `*` crossing everything, `?` matching one character.
func matchURLGlob(pat, s string) bool {
	starPat, starStr := -1, 0
	p, i := 0, 0
	for i < len(s) {
		switch {
		case p < len(pat) && pat[p] == '*':
			starPat, starStr = p, i
			p++
		case p < len(pat) && (pat[p] == '?' || pat[p] == s[i]):
			p++
			i++
		case starPat >= 0:
			p = starPat + 1
			starStr++
			i = starStr
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}

// ---------------------------------------------------------------------------
// The scanner
// ---------------------------------------------------------------------------

// scanModuleSources returns every `source` assigned at the top level of a `module` block.
//
// Two passes. stripNoise blanks comments and heredoc bodies so that braces and quotes inside
// them stop counting — a heredoc holding an unbalanced "{" would otherwise desynchronise the
// depth counter for the rest of the file. Then a single walk tracks brace depth, skips string
// literals, and records the depths at which module bodies are open.
func scanModuleSources(src string) []string {
	src = stripNoise(src)

	var found []string
	depth := 0
	var moduleDepths []int // brace depths at which a module body is currently open

	for i := 0; i < len(src); {
		switch c := src[i]; {
		case c == '"':
			i = endOfString(src, i)

		case c == '{':
			depth++
			if endsWithModuleHeader(src[:i]) {
				moduleDepths = append(moduleDepths, depth)
			}
			i++

		case c == '}':
			if n := len(moduleDepths); n > 0 && moduleDepths[n-1] == depth {
				moduleDepths = moduleDepths[:n-1]
			}
			depth--
			i++

		default:
			// Only the immediate top level of a module body counts. A nested block with its own
			// `source` argument — a provisioner, say — is not the module's source.
			if n := len(moduleDepths); n > 0 && depth == moduleDepths[n-1] {
				if value, end, ok := sourceAssignment(src, i); ok {
					found = append(found, value)
					i = end
					continue
				}
			}
			i++
		}
	}
	return found
}

// sourceAssignment matches `source = "…"` starting exactly at i, on an identifier boundary so
// that `source_dir = "…"` does not match.
func sourceAssignment(src string, i int) (value string, end int, ok bool) {
	const kw = "source"
	if i > 0 && isIdentByte(src[i-1]) {
		return "", 0, false
	}
	if !strings.HasPrefix(src[i:], kw) {
		return "", 0, false
	}
	j := i + len(kw)
	if j < len(src) && isIdentByte(src[j]) {
		return "", 0, false
	}
	j = skipSpace(src, j)
	if j >= len(src) || src[j] != '=' {
		return "", 0, false
	}
	j = skipSpace(src, j+1)
	if j >= len(src) || src[j] != '"' {
		// A `source` built from an expression rather than a literal. Not something the allowlist
		// can evaluate; Terraform requires a literal here anyway.
		return "", 0, false
	}
	closing := endOfString(src, j)
	return unquote(src[j:closing]), closing, true
}

// endsWithModuleHeader reports whether the text immediately before a "{" is `module "name"`.
func endsWithModuleHeader(before string) bool {
	i := len(before)
	i = trimSpaceRight(before, i)
	if i == 0 || before[i-1] != '"' {
		return false
	}
	// Walk back over the quoted label.
	j := i - 1
	for j > 0 {
		j--
		if before[j] == '"' && (j == 0 || before[j-1] != '\\') {
			break
		}
	}
	if j <= 0 || before[j] != '"' {
		return false
	}
	k := trimSpaceRight(before, j)
	const kw = "module"
	if k < len(kw) || !strings.HasSuffix(before[:k], kw) {
		return false
	}
	// `submodule "x" {` must not read as a module block.
	if k-len(kw) > 0 && isIdentByte(before[k-len(kw)-1]) {
		return false
	}
	return true
}

func stripNoise(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); {
		switch {
		case src[i] == '"':
			end := endOfString(src, i)
			b.WriteString(src[i:end])
			i = end

		case src[i] == '#', strings.HasPrefix(src[i:], "//"):
			if nl := strings.IndexByte(src[i:], '\n'); nl < 0 {
				i = len(src)
			} else {
				i += nl
			}

		case strings.HasPrefix(src[i:], "/*"):
			if end := strings.Index(src[i+2:], "*/"); end < 0 {
				i = len(src)
			} else {
				i += 2 + end + 2
			}

		default:
			if tag, after, ok := heredocOpen(src, i); ok {
				b.WriteByte('\n')
				i = skipHeredocBody(src, after, tag)
				continue
			}
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

// heredocOpen matches `<<TAG\n` or `<<-TAG\n`, returning the tag and the offset after the newline.
func heredocOpen(src string, i int) (tag string, after int, ok bool) {
	if !strings.HasPrefix(src[i:], "<<") {
		return "", 0, false
	}
	j := i + 2
	if j < len(src) && src[j] == '-' {
		j++
	}
	j = skipSpaceNoNewline(src, j)
	start := j
	for j < len(src) && isIdentByte(src[j]) {
		j++
	}
	if j == start {
		return "", 0, false
	}
	tag = src[start:j]
	j = skipSpaceNoNewline(src, j)
	if j < len(src) && src[j] == '\r' {
		j++
	}
	if j >= len(src) || src[j] != '\n' {
		return "", 0, false
	}
	return tag, j + 1, true
}

func skipHeredocBody(src string, i int, tag string) int {
	for i < len(src) {
		nl := strings.IndexByte(src[i:], '\n')
		lineEnd := len(src)
		if nl >= 0 {
			lineEnd = i + nl
		}
		if strings.TrimSpace(src[i:lineEnd]) == tag {
			return lineEnd
		}
		if nl < 0 {
			return len(src)
		}
		i = lineEnd + 1
	}
	return i
}

// endOfString returns the offset just past the closing quote of the literal starting at src[i].
func endOfString(src string, i int) int {
	i++
	for i < len(src) {
		if src[i] == '\\' {
			i += 2
			continue
		}
		if src[i] == '"' {
			return i + 1
		}
		i++
	}
	return len(src)
}

func unquote(s string) string {
	s = strings.TrimPrefix(s, `"`)
	s = strings.TrimSuffix(s, `"`)
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(s)
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isSpaceByte(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func skipSpace(s string, i int) int {
	for i < len(s) && isSpaceByte(s[i]) {
		i++
	}
	return i
}

func skipSpaceNoNewline(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

func trimSpaceRight(s string, i int) int {
	for i > 0 && isSpaceByte(s[i-1]) {
		i--
	}
	return i
}

// RootModuleDir joins a checkout root and a workspace Dir, refusing to escape the checkout.
func RootModuleDir(checkoutRoot, workspaceDir string) (string, error) {
	clean := path.Clean("/" + filepath.ToSlash(workspaceDir))
	return filepath.Join(checkoutRoot, filepath.FromSlash(clean)), nil
}
