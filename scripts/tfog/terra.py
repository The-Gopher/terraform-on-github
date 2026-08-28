"""git checkout and Terraform CLI wrappers.

Mirrors internal/tf. The wrapper exists so the flags the design depends on cannot be forgotten
at a call site: `-lock=false` on plan, no `-upgrade` on init, and a saved plan file on apply.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any


class TFError(Exception):
    pass


class StalePlan(TFError):
    """Terraform refused the saved plan because state moved under it.

    Expected rather than exceptional when two PRs merge into one workspace. `on_stale: fail` is
    the only accepted policy (DESIGN 6.3), so this is always terminal — a human re-plans.
    """


class BranchBehindBase(TFError):
    """The base has commits the head lacks, so the head's tree is not the merge result."""


def terraform_bin() -> str:
    return os.environ.get("TFOG_TERRAFORM_BIN") or "terraform"


def require_tools(*names: str) -> None:
    missing = [n for n in names if shutil.which(n) is None]
    if missing:
        raise TFError("not found on PATH: " + ", ".join(missing))


def terraform_version(binary: str | None = None) -> str:
    binary = binary or terraform_bin()
    require_tools(binary)
    out = subprocess.run([binary, "version", "-json"], capture_output=True, text=True)
    if out.returncode != 0:
        raise TFError(f"{binary} version -json failed:\n{out.stderr.strip()}")
    return json.loads(out.stdout)["terraform_version"]


# ---------------------------------------------------------------------------
# Checkout
# ---------------------------------------------------------------------------


def _git(*args: str, cwd=None, check=True) -> subprocess.CompletedProcess:
    p = subprocess.run(["git", *args], cwd=cwd, capture_output=True, text=True)
    if check and p.returncode != 0:
        raise TFError(f"git {' '.join(args)} failed:\n{(p.stderr or p.stdout).strip()}")
    return p


def local_repo_root(start=None) -> Path | None:
    p = _git("rev-parse", "--show-toplevel", cwd=start or os.getcwd(), check=False)
    return Path(p.stdout.strip()) if p.returncode == 0 else None


@dataclass
class Checkout:
    """A detached working tree at one commit, discarded on exit.

    tree_sha is `<commit>^{tree}` — the content identity of the tree Terraform is about to
    read. Recorded at plan time and compared at apply time; that comparison is what makes the
    guarantee survive a squash or rebase merge, where the commit SHA cannot match by
    construction (DESIGN 6.2).
    """

    root: Path
    commit_sha: str
    tree_sha: str
    _worktree_of: Path | None = None
    _tmp: str | None = None

    def cleanup(self) -> None:
        if self._worktree_of:
            _git("worktree", "remove", "--force", str(self.root), cwd=str(self._worktree_of), check=False)
        if self._tmp:
            shutil.rmtree(self._tmp, ignore_errors=True)

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.cleanup()
        return False


def checkout(sha: str, clone_url: str, source_repo: Path | None = None, fetch_refs=()) -> Checkout:
    """Materialise `sha` in a throwaway working tree.

    Prefers `git worktree` off a local clone: it is fast, and it reuses whatever credentials
    the operator's clone already has, so the scripts never handle a token themselves. Falls
    back to a shallow clone into a temp directory when run outside a clone of the repo.

    `fetch_refs` are refs to try when the bare SHA is not fetchable — GitHub serves
    `refs/pull/<n>/head` reliably, individual SHAs only when the server permits it.
    """
    tmp = tempfile.mkdtemp(prefix="tfog-")
    dest = Path(tmp) / "src"

    if source_repo and (source_repo / ".git").exists():
        if _git("cat-file", "-e", f"{sha}^{{commit}}", cwd=str(source_repo), check=False).returncode != 0:
            _fetch(source_repo, sha, fetch_refs)
        _git("worktree", "add", "--detach", "--quiet", str(dest), sha, cwd=str(source_repo))
        tree = _git("rev-parse", f"{sha}^{{tree}}", cwd=str(dest)).stdout.strip()
        return Checkout(root=dest, commit_sha=sha, tree_sha=tree, _worktree_of=source_repo, _tmp=tmp)

    dest.mkdir(parents=True)
    _git("init", "--quiet", cwd=str(dest))
    _git("remote", "add", "origin", clone_url, cwd=str(dest))
    _fetch(dest, sha, fetch_refs)
    _git("checkout", "--quiet", "--detach", "FETCH_HEAD", cwd=str(dest))
    tree = _git("rev-parse", "HEAD^{tree}", cwd=str(dest)).stdout.strip()
    got = _git("rev-parse", "HEAD", cwd=str(dest)).stdout.strip()
    if got != sha:
        # A ref fetch can land on a different commit than asked for if the ref moved between
        # the API read and the fetch. Planning that tree would plan something nobody reviewed.
        shutil.rmtree(tmp, ignore_errors=True)
        raise TFError(f"checkout landed on {got[:12]}, expected {sha[:12]} — the ref moved; retry")
    return Checkout(root=dest, commit_sha=sha, tree_sha=tree, _tmp=tmp)


def _fetch(repo: Path, sha: str, fetch_refs) -> None:
    attempts = [(sha,), *[(r,) for r in fetch_refs]]
    errors = []
    for args in attempts:
        p = _git("fetch", "--quiet", "origin", *args, cwd=str(repo), check=False)
        if p.returncode == 0:
            if _git("cat-file", "-e", f"{sha}^{{commit}}", cwd=str(repo), check=False).returncode == 0:
                return
            errors.append(f"fetched {args[0]} but {sha[:12]} is still missing")
            continue
        errors.append((p.stderr or p.stdout).strip().splitlines()[-1] if (p.stderr or p.stdout).strip() else "failed")
    raise TFError(f"could not fetch {sha[:12]}:\n  " + "\n  ".join(errors))


def divergence(compare: dict) -> None:
    """Enforce DESIGN 4.1 from a compare response. Raises BranchBehindBase, or returns.

    Only "ahead" and "identical" mean the head already contains its base. Anything else and the
    head's tree is not what a merge would produce, so planning it would show a clean diff for a
    change that may conflict with what already landed.
    """
    status = compare.get("status")
    if status in ("ahead", "identical"):
        return
    behind = compare.get("behind_by") or 0
    detail = f"status={status}"
    if behind:
        detail += f", {behind} commit(s) on the base branch are missing from the head"
    raise BranchBehindBase(f"the PR branch is not up to date with its base ({detail})")


# ---------------------------------------------------------------------------
# Terraform
# ---------------------------------------------------------------------------


@dataclass
class Runner:
    """Terraform in one checked-out root module directory."""

    workdir: Path
    binary: str
    impersonate: str = ""
    # Output is teed to stderr and, when set, to this file — the apply log the operator reads
    # when an apply half-succeeds.
    log: Any = None

    def env(self) -> dict:
        e = dict(os.environ)
        e["TF_IN_AUTOMATION"] = "1"
        e["TF_INPUT"] = "0"
        e["CHECKPOINT_DISABLE"] = "1"
        if self.impersonate:
            # Honoured by both the google provider and the gcs backend, so the tier's identity
            # covers state access and API calls without editing any HCL in the target repo.
            # This is the local stand-in for DESIGN 7's per-workspace impersonation: it is the
            # same mechanism, but the *ceiling* is the operator's own tokenCreator grants
            # rather than a service boundary.
            e["GOOGLE_IMPERSONATE_SERVICE_ACCOUNT"] = self.impersonate
        return e

    def run(self, args: list[str], timeout: int, allow: tuple = (0,)) -> tuple[int, str]:
        cmd = [self.binary, *args]
        header = f"\n$ terraform {' '.join(args)}\n"
        self._emit(header)
        try:
            p = subprocess.Popen(
                cmd,
                cwd=str(self.workdir),
                env=self.env(),
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                bufsize=1,
            )
        except FileNotFoundError as e:
            raise TFError(f"{self.binary} not found") from e
        chunks = []
        try:
            assert p.stdout is not None
            for line in p.stdout:
                chunks.append(line)
                self._emit(line)
            p.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            p.kill()
            raise TFError(f"terraform {args[0]} exceeded its {timeout}s timeout") from None
        out = "".join(chunks)
        if p.returncode not in allow:
            raise TFError(f"terraform {args[0]} failed (exit {p.returncode})\n{_tail(out)}")
        return p.returncode, out

    def _emit(self, text: str) -> None:
        sys.stderr.write(text)
        sys.stderr.flush()
        if self.log:
            self.log.write(text)
            self.log.flush()

    def init(self, backend, terraform_workspace: str = "") -> None:
        """`terraform init` with the backend pinned and no `-upgrade`.

        No -upgrade is not a style choice: it would let the plan resolve provider versions
        different from .terraform.lock.hcl, so the reviewed plan and the applied plan could be
        built against different provider code.

        This is also the dangerous step — init fetches modules and can execute their code.
        Check module sources before calling it.
        """
        args = ["init", "-input=false", "-no-color", "-reconfigure"]
        args.append(f"-backend-config=bucket={backend.bucket}")
        if backend.prefix:
            args.append(f"-backend-config=prefix={backend.prefix}")
        self.run(args, timeout=600)
        if terraform_workspace:
            self.run(["workspace", "select", "-or-create", "-no-color", terraform_workspace], timeout=120)

    def plan(self, out_file: Path, var_files=(), extra_args=(), timeout: int = 1200) -> bool:
        """`terraform plan -lock=false -out=<f> -detailed-exitcode`. Returns has_changes.

        -lock=false is deliberate. The GCS backend locks by writing a .tflock object into the
        state bucket, so a locking plan needs write access to the one thing the plan tier must
        never touch. The race it admits — an apply landing mid-plan — is already covered:
        Terraform refuses to apply a saved plan whose state serial has moved (DESIGN 4.2).
        """
        args = ["plan", "-input=false", "-no-color", "-lock=false", "-detailed-exitcode", f"-out={out_file}"]
        for vf in var_files:
            args.append(f"-var-file={vf}")
        args.extend(extra_args)
        code, _ = self.run(args, timeout=timeout, allow=(0, 2))
        return code == 2

    def show_json(self, plan_file: Path, timeout: int = 300) -> bytes:
        p = subprocess.run(
            [self.binary, "show", "-json", str(plan_file)],
            cwd=str(self.workdir),
            env=self.env(),
            capture_output=True,
            timeout=timeout,
        )
        if p.returncode != 0:
            raise TFError(f"terraform show -json failed:\n{p.stderr.decode(errors='replace')}")
        return p.stdout

    def show_text(self, plan_file: Path, timeout: int = 300) -> str:
        p = subprocess.run(
            [self.binary, "show", "-no-color", str(plan_file)],
            cwd=str(self.workdir),
            env=self.env(),
            capture_output=True,
            text=True,
            timeout=timeout,
        )
        if p.returncode != 0:
            raise TFError(f"terraform show failed:\n{p.stderr}")
        return p.stdout

    def apply(self, plan_file: Path, timeout: int = 2700) -> str:
        """`terraform apply <planfile>`.

        A saved plan needs no -auto-approve: Terraform applies it non-interactively and refuses
        it outright if the state lineage or serial changed since the plan was made. That
        refusal is the guarantee the whole design leans on — the applied plan is the reviewed
        plan, or nothing happens.
        """
        try:
            _, out = self.run(["apply", "-input=false", "-no-color", str(plan_file)], timeout=timeout)
            return out
        except TFError as e:
            if _looks_stale(str(e)):
                raise StalePlan(
                    "Terraform refused the saved plan: state has moved since it was created. "
                    "The reviewed plan is no longer applicable — re-plan the merge and review "
                    "again (docs/DESIGN.md 6.3)."
                ) from e
            raise


def _looks_stale(text: str) -> bool:
    low = text.lower()
    return "saved plan is stale" in low or "plan is stale" in low or "saved plan does not match" in low


def _tail(text: str, lines: int = 40) -> str:
    parts = text.strip().splitlines()
    return "\n".join(parts[-lines:])


def provider_lock_digest(workdir: Path) -> str:
    """SHA-256 of .terraform.lock.hcl, or "" when the repo has no lock file.

    Recorded in meta.json so the apply can assert it resolved the same provider versions the
    plan did. An empty value is a real answer, not a failure: it means the repo pins nothing,
    and the apply will only be able to check that it also found nothing.
    """
    from .store import sha256_file

    lock = Path(workdir) / ".terraform.lock.hcl"
    return sha256_file(lock) if lock.is_file() else ""
