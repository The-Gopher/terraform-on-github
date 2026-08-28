"""Load, validate and apply `.terraform-on-github.yaml`; scope a PR to workspaces.

Mirrors internal/config and internal/scope. The two rules worth restating here, because they
are the ones a local script is most tempted to drop:

  * The config is read from ONE TRUSTED REF, never the PR head and never the PR's base
    (DESIGN.md 3.1). `tfog-plan --trusted-ref` defaults to the repo's default branch.
  * Validation fails closed. A repo whose config does not parse gets no plan, rather than a
    plan against a guessed mapping.
"""

from __future__ import annotations

import fnmatch
import posixpath
import re
from dataclasses import dataclass, field

import yaml

FILENAME = ".terraform-on-github.yaml"
SCHEMA_VERSION = 1

NAME_RE = re.compile(r"^[a-z0-9-]{1,48}$")
SA_RE = re.compile(r"^[a-z][a-z0-9-]{4,28}[a-z0-9]@[a-z0-9-]{6,30}\.iam\.gserviceaccount\.com$")
EXACT_VERSION_RE = re.compile(r"^\d+\.\d+\.\d+$")

# Reserved because the runner sets them itself. A repo that could pass -out or -state could
# redirect the artifact this whole design is built around.
RESERVED_PLAN_ARGS = ("-lock", "-out", "-input", "-state", "-var-file")

SUMMARY_DETAILS = ("addresses", "full")

_DURATION_RE = re.compile(r"^(\d+)\s*(s|m|h)$")


class ConfigError(Exception):
    """Config is unusable. Always fatal — never fall back to a default mapping."""


def parse_duration(value, where: str) -> int:
    """Parse a Go-ish duration ("30s", "20m", "24h") to seconds. Bare numbers are seconds."""
    if value is None:
        raise ConfigError(f"{where}: missing duration")
    if isinstance(value, bool):
        raise ConfigError(f"{where}: expected a duration, got a bool")
    if isinstance(value, (int, float)):
        return int(value)
    m = _DURATION_RE.fullmatch(str(value).strip())
    if not m:
        raise ConfigError(f"{where}: bad duration {value!r}; use 30s, 20m or 24h")
    return int(m.group(1)) * {"s": 1, "m": 60, "h": 3600}[m.group(2)]


@dataclass(frozen=True)
class Backend:
    bucket: str
    prefix: str


@dataclass(frozen=True)
class Impersonate:
    plan: str = ""
    apply: str = ""


@dataclass(frozen=True)
class ApplyPolicy:
    enabled: bool = True
    environment: str = ""
    approval_timeout: int = 24 * 3600
    on_stale: str = "fail"
    require_protected_base: bool = False


@dataclass(frozen=True)
class Workspace:
    name: str
    branch: str
    dir: str
    backend: Backend
    impersonate: Impersonate
    apply: ApplyPolicy
    terraform_version: str
    plan_timeout: int
    apply_timeout: int
    summary_detail: str
    watch: tuple = ()
    var_files: tuple = ()
    plan_args: tuple = ()
    terraform_workspace: str = ""


@dataclass(frozen=True)
class Config:
    version: int
    workspaces: tuple
    module_sources: tuple = ()

    def workspace(self, name: str) -> Workspace:
        for w in self.workspaces:
            if w.name == name:
                return w
        known = ", ".join(w.name for w in self.workspaces) or "(none)"
        raise ConfigError(f"no workspace named {name!r}; config declares: {known}")


# ---------------------------------------------------------------------------
# Loading
# ---------------------------------------------------------------------------


def load(text: str) -> Config:
    """Parse and validate the config file. Raises ConfigError on anything doubtful."""
    try:
        raw = yaml.safe_load(text)
    except yaml.YAMLError as e:
        raise ConfigError(f"{FILENAME}: not valid YAML: {e}") from e
    if not isinstance(raw, dict):
        raise ConfigError(f"{FILENAME}: expected a mapping at the top level")

    version = raw.get("version")
    if version != SCHEMA_VERSION:
        raise ConfigError(f"{FILENAME}: version must be {SCHEMA_VERSION}, got {version!r}")

    defaults = raw.get("defaults") or {}
    if not isinstance(defaults, dict):
        raise ConfigError("defaults: expected a mapping")

    module_sources = tuple(raw.get("module_sources") or ())
    for s in module_sources:
        if not isinstance(s, str):
            raise ConfigError(f"module_sources: expected strings, got {s!r}")

    entries = raw.get("workspaces")
    if not isinstance(entries, list) or not entries:
        raise ConfigError("workspaces: at least one workspace is required")

    workspaces = [_workspace(e, defaults, i) for i, e in enumerate(entries)]

    names = set()
    for w in workspaces:
        if w.name in names:
            raise ConfigError(f"workspaces: duplicate name {w.name!r}")
        names.add(w.name)

    # Two workspaces on one branch with overlapping dirs would both plan the same tree against
    # two different states, and both would look authoritative in the PR.
    for i, a in enumerate(workspaces):
        for b in workspaces[i + 1 :]:
            if a.branch == b.branch and _dirs_overlap(a.dir, b.dir):
                raise ConfigError(
                    f"workspaces: {a.name!r} and {b.name!r} share branch {a.branch!r} "
                    f"with overlapping dirs {a.dir!r} and {b.dir!r}"
                )

    return Config(version=version, workspaces=tuple(workspaces), module_sources=module_sources)


def _workspace(raw, defaults: dict, index: int) -> Workspace:
    where = f"workspaces[{index}]"
    if not isinstance(raw, dict):
        raise ConfigError(f"{where}: expected a mapping")

    name = raw.get("name")
    if not isinstance(name, str) or not NAME_RE.fullmatch(name):
        raise ConfigError(f"{where}.name: must match [a-z0-9-]{{1,48}}, got {name!r}")
    where = f"workspaces[{name}]"

    branch = raw.get("branch")
    if not isinstance(branch, str) or not branch:
        raise ConfigError(f"{where}.branch: required")

    directory = _clean_dir(raw.get("dir"), f"{where}.dir")

    backend = raw.get("backend") or {}
    if not isinstance(backend, dict) or not backend.get("bucket"):
        raise ConfigError(f"{where}.backend: bucket is required (GCS backend only in v1)")
    be = Backend(bucket=str(backend["bucket"]), prefix=str(backend.get("prefix") or ""))

    imp_raw = raw.get("impersonate") or {}
    if not isinstance(imp_raw, dict):
        raise ConfigError(f"{where}.impersonate: expected a mapping")
    imp = Impersonate(plan=str(imp_raw.get("plan") or ""), apply=str(imp_raw.get("apply") or ""))
    for tier, value in (("plan", imp.plan), ("apply", imp.apply)):
        # Empty is permitted locally: an operator may already hold the right identity and run
        # with their own credentials. A non-empty value must be a real service-account email,
        # because it is about to be passed to gcloud as an impersonation target.
        if value and not SA_RE.fullmatch(value):
            raise ConfigError(f"{where}.impersonate.{tier}: not a service-account email: {value!r}")

    terraform_version = _pick(raw, defaults, "terraform_version")
    if not isinstance(terraform_version, str) or not EXACT_VERSION_RE.fullmatch(terraform_version):
        raise ConfigError(
            f"{where}.terraform_version: exact version required (e.g. 1.9.8), got "
            f"{terraform_version!r}"
        )

    summary_detail = _pick(raw, defaults, "summary_detail") or "addresses"
    if summary_detail not in SUMMARY_DETAILS:
        raise ConfigError(
            f"{where}.summary_detail: must be one of {', '.join(SUMMARY_DETAILS)}, "
            f"got {summary_detail!r}"
        )

    plan_args = tuple(_pick(raw, defaults, "plan_args") or ())
    for a in plan_args:
        for reserved in RESERVED_PLAN_ARGS:
            if str(a) == reserved or str(a).startswith(reserved + "="):
                raise ConfigError(f"{where}.plan_args: {reserved} is reserved and set by tfog")

    var_files = tuple(str(v) for v in (_pick(raw, defaults, "var_files") or ()))
    for v in var_files:
        _clean_dir(v, f"{where}.var_files", allow_file=True)

    ap = raw.get("apply") or {}
    if not isinstance(ap, dict):
        raise ConfigError(f"{where}.apply: expected a mapping")
    on_stale = str(ap.get("on_stale") or "fail")
    if on_stale != "fail":
        raise ConfigError(
            f"{where}.apply.on_stale: {on_stale!r} is not accepted; `fail` is the only value. "
            "replan_if_equivalent was cut — see docs/DESIGN.md 6.3."
        )
    policy = ApplyPolicy(
        enabled=bool(ap.get("enabled", True)),
        environment=str(ap.get("environment") or name),
        approval_timeout=parse_duration(ap.get("approval_timeout", "24h"), f"{where}.apply.approval_timeout"),
        on_stale=on_stale,
        require_protected_base=bool(ap.get("require_protected_base", False)),
    )

    return Workspace(
        name=name,
        branch=branch,
        dir=directory,
        backend=be,
        impersonate=imp,
        apply=policy,
        terraform_version=terraform_version,
        plan_timeout=parse_duration(_pick(raw, defaults, "plan_timeout") or "20m", f"{where}.plan_timeout"),
        apply_timeout=parse_duration(_pick(raw, defaults, "apply_timeout") or "45m", f"{where}.apply_timeout"),
        summary_detail=summary_detail,
        watch=tuple(str(g) for g in (raw.get("watch") or ())),
        var_files=var_files,
        plan_args=tuple(str(a) for a in plan_args),
        terraform_workspace=str(raw.get("terraform_workspace") or ""),
    )


def _pick(raw: dict, defaults: dict, key: str):
    v = raw.get(key)
    return defaults.get(key) if v in (None, [], ()) else v


def _clean_dir(value, where: str, allow_file: bool = False) -> str:
    if not isinstance(value, str) or not value:
        raise ConfigError(f"{where}: required")
    if value.startswith("/"):
        raise ConfigError(f"{where}: must be repo-relative, got {value!r}")
    norm = posixpath.normpath(value)
    if norm == ".." or norm.startswith("../"):
        raise ConfigError(f"{where}: escapes the repository root: {value!r}")
    if not allow_file and norm == ".":
        return "."
    return norm


def _dirs_overlap(a: str, b: str) -> bool:
    a, b = a.rstrip("/"), b.rstrip("/")
    return a == b or a.startswith(b + "/") or b.startswith(a + "/")


# ---------------------------------------------------------------------------
# Scoping: base ref + changed paths -> workspaces
# ---------------------------------------------------------------------------


@dataclass
class Scope:
    in_scope: list = field(default_factory=list)
    candidates: list = field(default_factory=list)  # bound to this branch, diff notwithstanding
    config_changed: bool = False


def match_branch(pattern: str, ref: str) -> bool:
    """Exact match, or a trailing `*` segment ("release/*"). Nothing more expressive.

    This predicate selects which authorized mapping applies, so its behaviour has to be
    obvious at a glance. A full glob here is a mistake waiting to be made.
    """
    if pattern == ref:
        return True
    if pattern.endswith("/*"):
        prefix = pattern[:-1]
        return ref.startswith(prefix) and "/" not in ref[len(prefix) :] and len(ref) > len(prefix)
    return False


def _path_glob(pattern: str) -> re.Pattern:
    """Compile a doublestar path glob: `*` stops at `/`, `**` does not."""
    out = ["^"]
    i = 0
    while i < len(pattern):
        if pattern.startswith("**/", i):
            out.append("(?:.*/)?")
            i += 3
        elif pattern.startswith("**", i):
            out.append(".*")
            i += 2
        elif pattern[i] == "*":
            out.append("[^/]*")
            i += 1
        elif pattern[i] == "?":
            out.append("[^/]")
            i += 1
        else:
            out.append(re.escape(pattern[i]))
            i += 1
    out.append("$")
    return re.compile("".join(out))


def touches(ws: Workspace, paths) -> bool:
    """Is any changed path inside ws.dir, or matched by a ws.watch glob?

    Dir matching is prefix-on-a-path-boundary, so "envs/prod" does not match
    "envs/production".
    """
    d = ws.dir.rstrip("/")
    if d not in ("", "."):
        for p in paths:
            if p == d or p.startswith(d + "/"):
                return True
    else:
        return bool(paths)
    globs = [_path_glob(g) for g in ws.watch]
    return any(g.match(p) for p in paths for g in globs)


def scope(cfg: Config, base_ref: str, changed_paths, all_candidates: bool = False) -> Scope:
    """Select the workspaces a PR affects.

    `all_candidates` forces every branch-matched workspace in scope. The caller sets it when
    the changed-file list may be truncated (GitHub's compare endpoint caps at 300 files), so
    the failure mode is over-planning rather than silently skipping a workspace.
    """
    result = Scope(config_changed=FILENAME in set(changed_paths))
    for w in cfg.workspaces:
        if not match_branch(w.branch, base_ref):
            continue
        result.candidates.append(w)
        if all_candidates or touches(w, changed_paths):
            result.in_scope.append(w)
    return result


# ---------------------------------------------------------------------------
# Module sources: the pre-init allowlist check
# ---------------------------------------------------------------------------

# `source` has to be extracted before `terraform init` runs, because init is what fetches
# module code and module fetches can execute it — and Terraform has no native allowlist, so
# there is no setting to turn on instead. That forces a pre-pass over the HCL.
#
# This is a brace-tracking scanner, not an HCL parser, and not a line-oriented grep. The
# line-oriented version got `module "vpc" { source = "./x" }` wrong, which is legal HCL and
# common in fixtures. Getting it wrong in that direction is the dangerous one: a missed module
# block is a module source that never reaches the allowlist.
_MODULE_TAIL = re.compile(r'\bmodule\s+"[^"]*"\s*$')
_SOURCE_ASSIGN = re.compile(r'source\s*=\s*"((?:[^"\\]|\\.)*)"')
_HEREDOC_OPEN = re.compile(r"<<-?\s*([A-Za-z_][A-Za-z0-9_]*)[ \t]*\r?\n")
_WORD = re.compile(r"[A-Za-z0-9_]")


class ModuleSourceDenied(Exception):
    pass


def _end_of_string(text: str, i: int) -> int:
    """Index just past the closing quote of the string literal starting at text[i] == '"'."""
    i += 1
    n = len(text)
    while i < n:
        if text[i] == "\\":
            i += 2
            continue
        if text[i] == '"':
            return i + 1
        i += 1
    return n


def _strip_noise(text: str) -> str:
    """Blank out comments and heredoc bodies, preserving offsets is not required — only that
    braces and quotes inside them stop counting. A heredoc holding an unbalanced `{` would
    otherwise desynchronise the depth counter for the rest of the file."""
    out = []
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        if c == '"':
            j = _end_of_string(text, i)
            out.append(text[i:j])
            i = j
            continue
        if c == "#" or text.startswith("//", i):
            j = text.find("\n", i)
            i = n if j < 0 else j
            continue
        if text.startswith("/*", i):
            j = text.find("*/", i)
            i = n if j < 0 else j + 2
            continue
        m = _HEREDOC_OPEN.match(text, i)
        if m:
            tag = m.group(1)
            j = m.end()
            while j < n:
                eol = text.find("\n", j)
                line = text[j : n if eol < 0 else eol]
                if line.strip() == tag:
                    j = n if eol < 0 else eol
                    break
                j = n if eol < 0 else eol + 1
            out.append("\n")
            i = j
            continue
        out.append(c)
        i += 1
    return "".join(out)


def _sources_in_text(text: str) -> list:
    """Every `source` value assigned at the top level of a `module` block."""
    text = _strip_noise(text)
    found = []
    depth = 0
    module_depths = []  # brace depths at which a module body is currently open
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        if c == '"':
            i = _end_of_string(text, i)
            continue
        if c == "{":
            depth += 1
            if _MODULE_TAIL.search(text[max(0, i - 256) : i]):
                module_depths.append(depth)
            i += 1
            continue
        if c == "}":
            if module_depths and module_depths[-1] == depth:
                module_depths.pop()
            depth -= 1
            i += 1
            continue
        if (
            module_depths
            and depth == module_depths[-1]
            and text.startswith("source", i)
            and not (i and _WORD.match(text[i - 1]))
        ):
            m = _SOURCE_ASSIGN.match(text, i)
            if m:
                found.append(m.group(1))
                i = m.end()
                continue
        i += 1
    return found


def _sources_in_file(path) -> list:
    try:
        return _sources_in_text(path.read_text(encoding="utf-8", errors="replace"))
    except OSError:
        return []


def is_local_source(src: str) -> bool:
    return src.startswith("./") or src.startswith("../")


def collect_module_sources(repo_root, root_module_dir) -> list:
    """Walk the root module and every local module it reaches; return (source, relpath) pairs.

    Local sources are followed rather than trusted blindly: a local module can itself pull a
    remote one, and that remote source is exactly what the allowlist exists to gate.
    """
    from pathlib import Path

    repo_root = Path(repo_root).resolve()
    queue = [(Path(root_module_dir).resolve())]
    seen = set()
    found = []
    while queue:
        d = queue.pop()
        if d in seen or not d.is_dir():
            continue
        seen.add(d)
        for f in sorted(d.glob("*.tf")):
            rel = f.relative_to(repo_root).as_posix() if f.is_relative_to(repo_root) else str(f)
            for src in _sources_in_file(f):
                found.append((src, rel))
                if is_local_source(src):
                    target = (d / src).resolve()
                    if not target.is_relative_to(repo_root):
                        raise ModuleSourceDenied(
                            f"{rel}: local module source {src!r} escapes the repository root"
                        )
                    queue.append(target)
    return found


def check_module_sources(sources, allow) -> None:
    """Enforce the allowlist. Empty allowlist means local paths only, per docs/CONFIG.md.

    Patterns are matched with `*` crossing `/`, because these are URLs, not paths — an
    allowlist entry like `git::ssh://git@github.com/acme/mods//*?ref=v*` is expected to cover
    a nested module path.
    """
    denied = []
    for src, where in sources:
        if is_local_source(src):
            continue
        if any(fnmatch.fnmatchcase(src, p) for p in allow):
            continue
        denied.append(f"{where}: {src}")
    if denied:
        hint = (
            "add a matching glob to `module_sources` in the config on the trusted ref"
            if allow
            else "`module_sources` is empty, which permits local paths only"
        )
        raise ModuleSourceDenied("module source not in allowlist (" + hint + "):\n  " + "\n  ".join(denied))
