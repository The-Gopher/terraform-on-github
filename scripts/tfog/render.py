"""Turn plan.json into the text a reviewer reads on the PR.

Mirrors internal/plan/summary.go. Two rules from there are reproduced exactly, because both are
about the number people actually read:

  * A replace is an action *pair* — ["delete","create"], or ["create","delete"] under
    create_before_destroy — not its own action string. Counting it as one create plus one delete
    understates destruction in the headline.
  * `addresses` is the default detail level, and it is a security default rather than a
    terseness one. A plan file embeds a snapshot of prior state, so every secret in state is in
    the plan, and Terraform's `sensitive` marking covers what the schema declares — not a token
    somebody pasted into an unmarked metadata attribute.
"""

from __future__ import annotations

import json

# internal/plan/summary.go caps at 200 for the same reason: a GitHub comment body is limited
# (65536 bytes) and oversize payloads are rejected rather than truncated, so bound the list
# before formatting and say how many were dropped.
MAX_RENDERED_ADDRESSES = 200
MAX_FULL_DIFF_CHARS = 30000

SYMBOL = {
    "create": "+",
    "update": "~",
    "delete": "-",
    "replace": "-/+",
    "read": "<=",
}


def counts(plan_json: bytes | dict) -> dict:
    doc = plan_json if isinstance(plan_json, dict) else json.loads(plan_json)
    tally = {"create": 0, "update": 0, "delete": 0, "replace": 0, "read": 0}
    for rc in doc.get("resource_changes") or []:
        kind = classify(rc)
        if kind:
            tally[kind] += 1
    return tally


def classify(resource_change: dict) -> str | None:
    actions = list((resource_change.get("change") or {}).get("actions") or [])
    if not actions or actions == ["no-op"]:
        return None
    if sorted(actions) == ["create", "delete"]:
        return "replace"
    if actions == ["create"]:
        return "create"
    if actions == ["update"]:
        return "update"
    if actions == ["delete"]:
        return "delete"
    if actions == ["read"]:
        return "read"
    return None


def has_changes(tally: dict) -> bool:
    return tally["create"] + tally["update"] + tally["delete"] + tally["replace"] > 0


def headline(tally: dict) -> str:
    parts = [
        f"{tally['create']} to add",
        f"{tally['update']} to change",
        f"{tally['delete']} to destroy",
    ]
    if tally["replace"]:
        parts.append(f"{tally['replace']} to replace")
    return ", ".join(parts)


def address_lines(plan_json: bytes | dict, limit: int = MAX_RENDERED_ADDRESSES) -> tuple[list[str], int]:
    doc = plan_json if isinstance(plan_json, dict) else json.loads(plan_json)
    lines = []
    for rc in doc.get("resource_changes") or []:
        kind = classify(rc)
        if not kind or kind == "read":
            continue
        lines.append(f"{SYMBOL[kind]:>3} {rc.get('address', '?')}")
    lines.sort(key=lambda s: s.split(None, 1)[-1])
    omitted = max(0, len(lines) - limit)
    return lines[:limit], omitted


# ---------------------------------------------------------------------------
# Comment bodies
# ---------------------------------------------------------------------------


def plan_body(
    *,
    workspace,
    meta: dict,
    plan_json: bytes,
    plan_text: str,
    plan_path,
    tally: dict,
    marker: str,
) -> str:
    changed = has_changes(tally)
    lines, omitted = address_lines(plan_json)

    out = [marker, f"### `terraform plan` — **{workspace.name}**", ""]
    verdict = headline(tally) if changed else "**No changes.** Infrastructure matches the configuration."
    out.append(f"`{workspace.dir}` · {verdict}")
    out.append("")

    if lines:
        out.append("```diff")
        out.extend(lines)
        if omitted:
            out.append(f"… and {omitted} more resource change(s) not shown")
        out.append("```")
        out.append("")

    if workspace.summary_detail == "full" and changed:
        diff = plan_text
        note = ""
        if len(diff) > MAX_FULL_DIFF_CHARS:
            note = f"\n… truncated; {len(diff) - MAX_FULL_DIFF_CHARS} more characters in `plan.txt`"
            diff = diff[:MAX_FULL_DIFF_CHARS]
        out.append("<details><summary>Full diff</summary>")
        out.append("")
        out.append("```terraform")
        out.append(diff + note)
        out.append("```")
        out.append("")
        out.append("</details>")
        out.append("")

    out.append(
        f"plan `{meta['base_sha'][:8]}…{meta['head_sha'][:8]}` · terraform {meta['terraform_version']}"
        f" · {meta['duration_seconds']}s · planned by @{meta['planned_by']}"
    )
    out.append("")
    out.append(
        f"<sub>tree `{meta['planned_tree_sha'][:12]}` · plan `{meta['plan_sha256'][:12]}` · config from "
        f"`{meta['config_ref']}` @ `{meta['config_ref_sha'][:8]}` · artifacts `{plan_path}`</sub>")
    out.append("")
    if workspace.summary_detail != "full" and changed:
        out.append(
            "<sub>Only addresses and actions are shown: a plan file embeds a snapshot of prior "
            "state. The full diff is in `plan.txt` in the store above.</sub>"
        )
        out.append("")
    if not workspace.apply.enabled:
        out.append("<sub>This workspace is **plan-only** — merging will not apply it.</sub>")
        out.append("")
    out.append(_meta_block(meta))
    return "\n".join(out)


def apply_body(*, workspace, meta: dict, tally: dict, ok: bool, detail: str, log_path, marker: str) -> str:
    out = [marker, f"### `terraform apply` — **{workspace.name}**", ""]
    if ok:
        out.append(f"Applied the reviewed plan. {headline(tally)}.")
    else:
        out.append("**Apply failed.**")
    out.append("")
    if detail:
        out.append("```")
        out.append(detail.strip())
        out.append("```")
        out.append("")
    out.append(
        f"plan `{meta['base_sha'][:8]}…{meta['head_sha'][:8]}` · merge commit "
        f"`{(meta.get('merge_commit_sha') or '')[:8]}` · tree `{meta['planned_tree_sha'][:12]}` verified"
    )
    out.append("")
    out.append(f"<sub>terraform {meta['terraform_version']} · log `{log_path}`</sub>")
    if not ok:
        out.append("")
        out.append(
            "<sub>An apply that fails part-way has already changed some infrastructure. Read the "
            "log before re-running anything — the next plan shows what remains.</sub>"
        )
    return "\n".join(out)


def blocked_body(*, marker: str, title: str, detail: str) -> str:
    return "\n".join([marker, f"### {title}", "", detail, ""])


def _meta_block(meta: dict) -> str:
    """meta.json in an HTML comment: invisible in the timeline, readable by tfog-apply.

    This is the portable half of the key. tfog-apply locates the plan in the local store on its
    own, and cross-checks these values against it — so a forged comment can only make an apply
    *refuse*, never make it apply something else. That asymmetry is the point, and it is why
    the comment can be treated as an index without being treated as an authority.
    """
    return "<!-- tfog-meta\n" + json.dumps(meta, indent=2, sort_keys=True) + "\n-->"


def parse_meta_block(body: str) -> dict | None:
    start = body.find("<!-- tfog-meta")
    if start < 0:
        return None
    end = body.find("-->", start)
    if end < 0:
        return None
    try:
        return json.loads(body[start + len("<!-- tfog-meta") : end])
    except json.JSONDecodeError:
        return None
