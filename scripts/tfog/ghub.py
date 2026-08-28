"""GitHub access via the `gh` CLI.

Using `gh` rather than a signed App JWT is the single biggest simplification in the local
iteration: there is no App, no installation token, no down-scoped permission set. The operator's
own credentials do the work, which means the local scripts have exactly the operator's access —
no more, and no less. docs/LOCAL.md 3 says what that costs.
"""

from __future__ import annotations

import base64
import json
import shutil
import subprocess


class GHError(Exception):
    pass


def require_gh() -> None:
    if shutil.which("gh") is None:
        raise GHError("`gh` not found on PATH; install the GitHub CLI (https://cli.github.com)")
    p = subprocess.run(["gh", "auth", "status"], capture_output=True, text=True)
    if p.returncode != 0:
        raise GHError("`gh auth status` failed; run `gh auth login`\n" + (p.stderr or p.stdout).strip())


def gh(*args: str, stdin: str | None = None) -> str:
    p = subprocess.run(["gh", *args], capture_output=True, text=True, input=stdin)
    if p.returncode != 0:
        raise GHError(f"{_shown(args)}: {_reason(p)}")
    return p.stdout


def _shown(args) -> str:
    """The command as the operator would type it, with our boilerplate headers dropped."""
    keep, skip = [], False
    for a in args:
        if skip:
            skip = False
            continue
        if a == "-H":
            skip = True
            continue
        keep.append(a)
    return "gh " + " ".join(keep)


def _reason(proc) -> str:
    text = (proc.stderr or proc.stdout).strip()
    lines = [ln for ln in text.splitlines() if ln.strip()]
    return lines[-1].removeprefix("gh: ").strip() if lines else f"exit {proc.returncode}"


def gh_json(*args: str, stdin: str | None = None):
    out = gh(*args, stdin=stdin)
    try:
        return json.loads(out)
    except json.JSONDecodeError as e:
        raise GHError(f"gh {' '.join(args)}: response was not JSON: {e}") from e


def api(path: str, method: str | None = None, fields: dict | None = None):
    args = ["api", "-H", "Accept: application/vnd.github+json", path]
    if method:
        args = ["api", "-X", method, "-H", "Accept: application/vnd.github+json", path]
    if fields:
        return gh_json(*args, "--input", "-", stdin=json.dumps(fields))
    return gh_json(*args)


def viewer_login() -> str:
    return gh_json("api", "user")["login"]


def repo_from_cwd() -> tuple[str, str]:
    d = gh_json("repo", "view", "--json", "owner,name")
    return d["owner"]["login"], d["name"]


def default_branch(owner: str, repo: str) -> str:
    return api(f"repos/{owner}/{repo}")["default_branch"]


def clone_url(owner: str, repo: str) -> str:
    return f"https://github.com/{owner}/{repo}.git"


def pull_request(owner: str, repo: str, number: int) -> dict:
    try:
        return api(f"repos/{owner}/{repo}/pulls/{number}")
    except GHError as e:
        if "not found" in str(e).lower():
            raise GHError(f"no pull request #{number} in {owner}/{repo}") from e
        raise


def commit(owner: str, repo: str, ref: str) -> dict:
    """One commit. `.sha` is the commit, `.commit.tree.sha` is its tree — the field DESIGN 6.2
    leans on."""
    return api(f"repos/{owner}/{repo}/commits/{ref}")


def tree_sha(owner: str, repo: str, ref: str) -> str:
    return commit(owner, repo, ref)["commit"]["tree"]["sha"]


# GitHub's compare endpoint returns at most 300 files, paginated 100 at a time.
_COMPARE_FILE_CAP = 300


def compare(owner: str, repo: str, base: str, head: str) -> dict:
    """GET /compare/{base}...{head}, with the file list paginated.

    `status` is the answer to DESIGN 4.1: only "ahead" or "identical" means the head already
    contains its base, and therefore that the head's tree is the tree every merge strategy
    would produce.

    Sets `files_truncated` when the 300-file cap may have hidden a path. Callers must treat
    that as "scope every candidate workspace" — over-planning is recoverable, silently not
    planning a workspace is not.
    """
    first = None
    files: list = []
    page = 1
    while True:
        d = api(f"repos/{owner}/{repo}/compare/{base}...{head}?per_page=100&page={page}")
        if first is None:
            first = d
        batch = d.get("files") or []
        files.extend(batch)
        if len(batch) < 100 or len(files) >= _COMPARE_FILE_CAP:
            break
        page += 1
    first["files"] = files
    first["files_truncated"] = len(files) >= _COMPARE_FILE_CAP
    return first


def file_at_ref(owner: str, repo: str, path: str, ref: str) -> tuple[str, str]:
    """Return (decoded text, commit SHA of `ref`).

    The commit SHA is returned alongside deliberately. It becomes Meta.config_ref_sha: the
    trusted ref moves, and "which version of the mapping authorized this identity" is the
    question you most want answered after an apply that should not have happened
    (DESIGN.md 3.1).
    """
    resolved = commit(owner, repo, ref)["sha"]
    try:
        d = api(f"repos/{owner}/{repo}/contents/{path}?ref={resolved}")
    except GHError as e:
        if "not found" in str(e).lower():
            raise GHError(f"{path} does not exist at {ref} ({resolved[:8]})") from e
        raise
    if d.get("encoding") != "base64":
        raise GHError(f"{path}: unexpected encoding {d.get('encoding')!r}")
    return base64.b64decode(d["content"]).decode("utf-8"), resolved


def branch_requires_reviews(owner: str, repo: str, branch: str):
    """True/False, or None when the token cannot read branch protection.

    Backs `apply.require_protected_base`. None is not treated as False by the caller: an
    unanswerable question about a protection rule is a refusal to apply, not a pass.
    """
    try:
        d = api(f"repos/{owner}/{repo}/branches/{branch}")
    except GHError:
        return None
    if not d.get("protected"):
        return False
    rules = (d.get("protection") or {}).get("required_pull_request_reviews")
    if rules is None:
        return None if d.get("protection") is None else False
    return bool(rules)


# ---------------------------------------------------------------------------
# Comments
#
# The design publishes check runs; a local script cannot (checks:write belongs to an App, and
# a check run needs a head SHA it owns). PR comments are the closest thing an operator's own
# token can post, and they carry the same content. DESIGN 13.2 asked whether comments should
# be on by default; locally they are the only channel.
# ---------------------------------------------------------------------------


def marker(kind: str, workspace: str) -> str:
    return f"<!-- tfog:{kind}:{workspace} -->"


def list_comments(owner: str, repo: str, number: int) -> list:
    return gh_json("api", "--paginate", f"repos/{owner}/{repo}/issues/{number}/comments")


def add_comment(owner: str, repo: str, number: int, body: str) -> dict:
    return api(f"repos/{owner}/{repo}/issues/{number}/comments", method="POST", fields={"body": body})


def upsert_comment(owner: str, repo: str, number: int, mark: str, body: str) -> dict:
    """Edit the comment carrying `mark`, or post a new one.

    Plan comments are sticky: a push supersedes the previous plan, and leaving both in the
    timeline invites a reviewer to read the wrong one. The body always names the head SHA it
    describes, so which push it belongs to stays unambiguous.
    """
    if mark not in body:
        body = mark + "\n" + body
    for c in list_comments(owner, repo, number):
        if mark in (c.get("body") or ""):
            return api(
                f"repos/{owner}/{repo}/issues/comments/{c['id']}", method="PATCH", fields={"body": body}
            )
    return add_comment(owner, repo, number, body)


def find_comment(owner: str, repo: str, number: int, mark: str) -> dict | None:
    for c in list_comments(owner, repo, number):
        if mark in (c.get("body") or ""):
            return c
    return None
