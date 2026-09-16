package scope

import (
	"strings"
)

// matchPathGlob reports whether a repo-relative path matches a Watch glob.
//
// Doublestar semantics, matched directly rather than compiled to a regexp: `*` stops at a `/`,
// `**` crosses them, `?` matches one non-separator character. That distinction is the whole
// reason this exists — `modules/*` must not match `modules/vpc/main.tf`, while `modules/vpc/**`
// must match `modules/vpc/a/b/c.tf`. Go's path.Match gets the first case right and has no `**`
// at all, which is exactly the case config authors reach for.
//
// The implementation is a backtracking matcher over segments. It is not a general glob library
// and does not need to be: the patterns come from a config on the trusted ref, and the only
// question asked of them is "did this diff touch this workspace".
func matchPathGlob(pattern, name string) bool {
	return matchSegments(splitPath(pattern), splitPath(name))
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// A trailing "**" matches every remaining segment, including none.
			if len(pat) == 1 {
				return true
			}
			// Otherwise try to match the rest of the pattern at each remaining position.
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if !matchSegment(pat[0], name[0]) {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// matchSegment matches one path segment, where `*` and `?` cannot cross a separator (there is
// none left to cross inside a single segment).
func matchSegment(pat, name string) bool {
	// star tracks the position to backtrack to when a later literal fails to line up.
	var starPat, starName = -1, 0
	p, n := 0, 0
	for n < len(name) {
		switch {
		case p < len(pat) && pat[p] == '*':
			starPat, starName = p, n
			p++
		case p < len(pat) && (pat[p] == '?' || pat[p] == name[n]):
			p++
			n++
		case starPat >= 0:
			// Back up: let the last `*` consume one more character.
			p = starPat + 1
			starName++
			n = starName
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}
