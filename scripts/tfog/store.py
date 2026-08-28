"""The plan store: same key layout as DESIGN.md 5, on a local filesystem.

    $TFOG_STORE/plans/<owner>/<repo>/<workspace>/<base_sha>/<head_sha>/
        tfplan       the opaque binary — the thing that gets applied
        plan.json    terraform show -json
        plan.txt     terraform show
        meta.json    provenance
        state.json   local run state (planned / applied / apply_failed)
        apply-<n>.log

Keeping the GCS layout means the local store is a drop-in rehearsal for the bucket: the same
(base_sha, head_sha) pair is the key, so "the reviewed plan" and "the applied plan" are one
object rather than two things that ought to agree.

Two properties from the design survive; one does not.

Survives — write-once. Artifacts are created with O_EXCL and mode 0600, so a second plan of
the same pair cannot silently swap the bytes a reviewer read (DESIGN 5.2). `--force` deletes
the whole key first, visibly, rather than overwriting in place.

Survives — tamper evidence. meta.json carries `digest`, a SHA-256 over its own canonical
encoding, and `plan_sha256` over the tfplan bytes. tfog-apply checks both.

Does not survive — the KMS signature (DESIGN 5.3). `digest` detects a careless edit; it stops
nobody who can write to the store, because the store owner computes it. On one machine, under
one operator, the filesystem is the boundary. Do not carry this store to a shared host and
call it signed.
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

SCHEMA = 1

ARTIFACT_PLAN = "tfplan"
ARTIFACT_PLAN_JSON = "plan.json"
ARTIFACT_PLAN_TEXT = "plan.txt"
ARTIFACT_META = "meta.json"
ARTIFACT_STATE = "state.json"

STATE_PLANNED = "planned"
STATE_APPLYING = "applying"
STATE_APPLIED = "applied"
STATE_APPLY_FAILED = "apply_failed"

DEFAULT_ROOT = Path(
    os.environ.get("TFOG_STORE")
    or Path(os.environ.get("XDG_DATA_HOME", Path.home() / ".local" / "share"))
    / "terraform-on-github"
)


class StoreError(Exception):
    pass


@dataclass(frozen=True)
class PlanKey:
    owner: str
    repo: str
    workspace: str
    base_sha: str
    head_sha: str

    def prefix(self) -> str:
        return f"{self.owner}/{self.repo}/{self.workspace}/{self.base_sha}/{self.head_sha}"

    def short(self) -> str:
        return f"{self.base_sha[:8]}…{self.head_sha[:8]}"


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def utcnow() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def canonical(meta: dict) -> bytes:
    """The bytes `digest` covers: meta with `digest` removed, sorted keys, no whitespace.

    Field order has to be stable and encoder-independent or the digest is decorative. Sorted
    keys give that without a hand-maintained field list, at the cost of the explicit ordering
    internal/store/key.go writes by hand for the KMS signature.
    """
    return json.dumps({k: v for k, v in meta.items() if k != "digest"}, sort_keys=True, separators=(",", ":")).encode()


def digest_of(meta: dict) -> str:
    return sha256_bytes(canonical(meta))


class Store:
    def __init__(self, root=None):
        self.root = Path(root or DEFAULT_ROOT).expanduser()

    # -- paths ------------------------------------------------------------

    def key_dir(self, key: PlanKey) -> Path:
        return self.root / "plans" / key.prefix()

    def path(self, key: PlanKey, artifact: str) -> Path:
        return self.key_dir(key) / artifact

    def complete(self, key: PlanKey) -> bool:
        """meta.json is written last, so its presence means the plan is whole."""
        return self.path(key, ARTIFACT_META).is_file()

    # -- writing ----------------------------------------------------------

    def _mkdir(self, key: PlanKey) -> Path:
        d = self.key_dir(key)
        d.mkdir(parents=True, exist_ok=True)
        # 0700 all the way down: a plan file embeds a snapshot of prior state, so every secret
        # in state is in this directory (DESIGN 5.1).
        for p in (self.root, self.root / "plans", d):
            try:
                os.chmod(p, 0o700)
            except OSError:
                pass
        return d

    def create(self, key: PlanKey, artifact: str, data: bytes) -> Path:
        """Write-once create. Raises if the artifact already exists."""
        self._mkdir(key)
        path = self.path(key, artifact)
        try:
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        except FileExistsError as e:
            raise StoreError(f"{path} already exists; the store is write-once (use --force)") from e
        with os.fdopen(fd, "wb") as f:
            f.write(data)
        return path

    def create_from_file(self, key: PlanKey, artifact: str, src) -> Path:
        with open(src, "rb") as f:
            return self.create(key, artifact, f.read())

    def write_meta(self, key: PlanKey, meta: dict) -> dict:
        meta = dict(meta)
        meta["digest"] = digest_of(meta)
        self.create(key, ARTIFACT_META, json.dumps(meta, indent=2, sort_keys=True).encode() + b"\n")
        return meta

    def read_meta(self, key: PlanKey) -> dict:
        path = self.path(key, ARTIFACT_META)
        if not path.is_file():
            raise StoreError(f"no plan stored at {key.prefix()} (looked in {self.key_dir(key)})")
        meta = json.loads(path.read_text())
        recorded = meta.get("digest")
        if not recorded:
            raise StoreError(f"{path}: meta.json has no digest; refusing to use it")
        actual = digest_of(meta)
        if recorded != actual:
            raise StoreError(
                f"{path}: meta.json digest mismatch (recorded {recorded[:12]}, computed "
                f"{actual[:12]}) — the record was edited after it was written"
            )
        return meta

    # `state.json` is the local stand-in for the coordination bucket's run index (DESIGN 5.4).
    # It exists for one question only — has this plan already been applied — so it is a plain
    # rewrite with no compare-and-set. One operator, one process: there is no race to lose.
    def read_state(self, key: PlanKey) -> dict:
        path = self.path(key, ARTIFACT_STATE)
        return json.loads(path.read_text()) if path.is_file() else {}

    def write_state(self, key: PlanKey, state: str, **extra) -> dict:
        self._mkdir(key)
        record = {"state": state, "updated_at": utcnow(), **extra}
        path = self.path(key, ARTIFACT_STATE)
        tmp = path.with_suffix(".tmp")
        tmp.write_text(json.dumps(record, indent=2, sort_keys=True) + "\n")
        os.chmod(tmp, 0o600)
        tmp.replace(path)
        return record

    def apply_log_path(self, key: PlanKey) -> tuple[Path, int]:
        d = self._mkdir(key)
        attempt = 1 + len(list(d.glob("apply-*.log")))
        return d / f"apply-{attempt}.log", attempt

    def purge(self, key: PlanKey) -> None:
        shutil.rmtree(self.key_dir(key), ignore_errors=True)

    # -- finding ----------------------------------------------------------

    def find(self, owner: str, repo: str, workspace: str, head_sha: str | None = None) -> list[PlanKey]:
        """Every complete plan for a workspace, optionally pinned to one head SHA.

        This is how tfog-apply locates the reviewed plan: it knows the merged PR's head SHA,
        and the head SHA is half the key. base_sha is globbed rather than derived, because by
        apply time the base branch tip has moved past what was planned.
        """
        base = self.root / "plans" / owner / repo / workspace
        keys = []
        if not base.is_dir():
            return keys
        for base_dir in sorted(base.iterdir()):
            if not base_dir.is_dir():
                continue
            for head_dir in sorted(base_dir.iterdir()):
                if not head_dir.is_dir():
                    continue
                if head_sha and head_dir.name != head_sha:
                    continue
                key = PlanKey(owner, repo, workspace, base_dir.name, head_dir.name)
                if self.complete(key):
                    keys.append(key)
        return keys


def new_meta(
    key: PlanKey,
    *,
    pr: int,
    planned_tree_sha: str,
    config_ref: str,
    config_ref_sha: str,
    terraform_version: str,
    provider_lock_sha256: str,
    plan_sha256: str,
    has_changes: bool,
    counts: dict,
    duration_seconds: float,
    base_ref: str,
    workspace_dir: str,
    planned_by: str,
) -> dict:
    """Build meta.json. Same fields as internal/store.Meta, minus `signature`.

    `merge_commit_sha` is absent by construction: at plan time the merge does not exist. The
    load-bearing field is `planned_tree_sha` — the tree of what was planned, which every merge
    strategy preserves (DESIGN 6.2).
    """
    return {
        "schema": SCHEMA,
        "owner": key.owner,
        "repo": key.repo,
        "pr": pr,
        "workspace": key.workspace,
        "workspace_dir": workspace_dir,
        "base_ref": base_ref,
        "base_sha": key.base_sha,
        "head_sha": key.head_sha,
        "planned_tree_sha": planned_tree_sha,
        "config_ref": config_ref,
        "config_ref_sha": config_ref_sha,
        "terraform_version": terraform_version,
        "provider_lock_sha256": provider_lock_sha256,
        "plan_sha256": plan_sha256,
        "has_changes": has_changes,
        "resource_change_counts": counts,
        "planned_at": utcnow(),
        "duration_seconds": round(duration_seconds, 1),
        "planned_by": planned_by,
        "tool": "tfog-plan (local)",
    }
