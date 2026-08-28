// Package cli holds what cmd/tfog-plan and cmd/tfog-apply both need: argument plumbing, the
// store location, and the small formatting helpers that keep their output readable.
//
// Progress goes to stderr and results to stdout, so `tfog-plan 412 --no-comment > body.md` does
// something useful.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sampleserve/terraform-on-github/internal/config"
	"github.com/sampleserve/terraform-on-github/internal/ghcli"
)

// Logf writes progress to stderr.
func Logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// TakeLeadingArg pulls a leading positional argument out of argv, so that
//
//	tfog-plan 412 --workspace prod
//
// works. Go's flag package stops parsing at the first non-flag argument, which would leave
// "--workspace prod" as positional junk — and that form is both the natural one and the one the
// docs use. Flags-first still works: the caller falls back to fs.Arg(0).
func TakeLeadingArg(argv []string) (arg string, rest []string) {
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		return argv[0], argv[1:]
	}
	return "", argv
}

// PRNumber parses a pull request number from the two places it may appear.
func PRNumber(leading string, remaining []string) (int, error) {
	candidates := make([]string, 0, 2)
	if leading != "" {
		candidates = append(candidates, leading)
	}
	candidates = append(candidates, remaining...)
	if len(candidates) != 1 {
		return 0, fmt.Errorf("exactly one pull request number is required")
	}
	n, err := strconv.Atoi(candidates[0])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a pull request number", candidates[0])
	}
	return n, nil
}

// StringList is a repeatable string flag.
type StringList []string

func (s *StringList) String() string { return strings.Join(*s, ",") }

func (s *StringList) Set(v string) error {
	if v == "" {
		return fmt.Errorf("empty value")
	}
	*s = append(*s, v)
	return nil
}

// StoreRoot resolves the plan store location: the flag, then $TFOG_STORE, then the XDG data
// directory.
func StoreRoot(flag string) string {
	if flag != "" {
		return expand(flag)
	}
	if env := os.Getenv("TFOG_STORE"); env != "" {
		return expand(env)
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "terraform-on-github")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "terraform-on-github")
	}
	return filepath.Join(home, ".local", "share", "terraform-on-github")
}

func expand(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// ResolveRepo takes owner/repo from the flag, or infers it from the current directory.
func ResolveRepo(ctx context.Context, gh *ghcli.Client, flag string) (ghcli.Repo, error) {
	if flag != "" {
		return ghcli.ParseRepo(flag)
	}
	r, err := gh.RepoFromCwd(ctx)
	if err != nil {
		return ghcli.Repo{}, fmt.Errorf("could not determine the repository; pass --repo owner/repo\n%w", err)
	}
	return r, nil
}

// Short truncates a SHA for display.
func Short(s string) string { return Trim(s, 12) }

// Trim truncates to n runes' worth of bytes, which is enough for hex SHAs.
func Trim(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// RefLabel names a trusted ref for a human, including the empty case.
func RefLabel(ref string) string {
	if ref == "" {
		return "the default branch"
	}
	return ref
}

// IdentityLabel describes which credentials a run will use.
func IdentityLabel(sa string) string {
	if sa == "" {
		return "ambient credentials (no impersonation)"
	}
	return sa
}

// WorkspaceNames joins workspace names for a log line.
func WorkspaceNames(ws []config.Workspace) string {
	names := make([]string, len(ws))
	for i, w := range ws {
		names[i] = w.Name
	}
	return strings.Join(names, ", ")
}

// Preview renders the first n items of a list with an ellipsis.
func Preview(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + " …"
}

// Truncate bounds a string for a comment body.
func Truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… truncated"
}

// FirstLine is the summary line of a multi-line error.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ConfirmByName asks the operator to type a value back, and reports whether they did.
//
// This is the local stand-in for the GitHub Environment approval gate (DESIGN 6.4). A typed name
// rather than y/N, because this is the last gate before real infrastructure changes and the whole
// value of a gate is that it cannot be cleared by reflex.
//
// Refuses rather than proceeds when stdin is not a terminal: a non-interactive apply must say
// --yes explicitly, so a script cannot inherit an approval it never gave.
func ConfirmByName(name string) (bool, error) {
	if !IsTerminal(os.Stdin) {
		return false, fmt.Errorf("stdin is not a terminal and --yes was not given; refusing to apply unconfirmed")
	}
	fmt.Fprintf(os.Stderr, "Type the workspace name (%s) to apply, anything else to skip: ", name)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(line) == name, nil
}

// IsTerminal reports whether f is a character device.
func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
