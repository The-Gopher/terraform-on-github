#!/usr/bin/env python3
"""Tests for the pure logic behind tfog-plan and tfog-apply.

    python3 scripts/tests/test_tfog.py

Stdlib only, no network, no Terraform. Everything here is a decision that has to be right
before either script touches real infrastructure: which workspaces a diff selects, whether a
module source is allowed, whether a destructive change is counted as one, and whether the store
refuses to overwrite a reviewed plan.

The module-source scanner has the most cases because it is the one that failed first: a
line-oriented version missed `module "vpc" { source = "./x" }`, and a missed module block is a
module source that never reaches the allowlist.
"""

import dataclasses
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from tfog import cfg, render, store  # noqa: E402

EXAMPLE = Path(__file__).resolve().parents[2] / ".terraform-on-github.example.yaml"


def workspace(**over) -> cfg.Workspace:
    base = cfg.Workspace(
        name="ws",
        branch="main",
        dir="envs/prod",
        backend=cfg.Backend("b", "p"),
        impersonate=cfg.Impersonate(),
        apply=cfg.ApplyPolicy(),
        terraform_version="1.9.8",
        plan_timeout=1200,
        apply_timeout=2700,
        summary_detail="addresses",
    )
    return dataclasses.replace(base, **over)


class TestLoad(unittest.TestCase):
    def setUp(self):
        self.text = EXAMPLE.read_text()

    def test_example_config_loads(self):
        c = cfg.load(self.text)
        self.assertEqual([w.name for w in c.workspaces], ["prod-networking", "integration", "prod-networking-next"])

    def test_defaults_inherit_and_override(self):
        c = cfg.load(self.text)
        self.assertEqual(c.workspace("prod-networking").summary_detail, "addresses")
        self.assertEqual(c.workspace("integration").summary_detail, "full")
        self.assertEqual(c.workspace("prod-networking").plan_timeout, 20 * 60)
        self.assertEqual(c.workspace("prod-networking").apply_timeout, 45 * 60)

    def test_apply_defaults(self):
        c = cfg.load(self.text)
        self.assertTrue(c.workspace("integration").apply.enabled)
        self.assertFalse(c.workspace("prod-networking-next").apply.enabled)
        # environment defaults to the workspace name
        self.assertEqual(c.workspace("integration").apply.environment, "integration")
        self.assertEqual(c.workspace("integration").apply.approval_timeout, 24 * 3600)

    def _reject(self, text):
        with self.assertRaises(cfg.ConfigError):
            cfg.load(text)

    def test_rejects(self):
        t = self.text
        self._reject(t.replace("version: 1", "version: 2"))
        self._reject(t.replace("dir: envs/prod/networking", "dir: ../../etc", 1))
        self._reject(t.replace("dir: envs/prod/networking", "dir: /etc", 1))
        self._reject(t.replace("name: prod-networking\n", "name: Prod_Networking\n", 1))
        self._reject(t.replace("name: integration", "name: prod-networking"))
        self._reject(t.replace('terraform_version: "1.9.8"', 'terraform_version: "~> 1.9"'))
        self._reject(t.replace("tf-prod-networking-plan@acme-tf.iam.gserviceaccount.com", "nope", 1))
        self._reject(t.replace("plan_timeout: 20m", "plan_timeout: 20 minutes"))
        self._reject(t.replace("      bucket: acme-tfstate-prod\n", "", 1))
        self._reject("version: 1\nworkspaces: []\n")

    def test_rejects_cut_on_stale_policy(self):
        # replan_if_equivalent was cut (DESIGN 6.3); accepting it silently would be worse than
        # rejecting it, because the name promises a behaviour nothing implements.
        with self.assertRaises(cfg.ConfigError) as e:
            cfg.load(self.text.replace("on_stale: fail", "on_stale: replan_if_equivalent"))
        self.assertIn("6.3", str(e.exception))

    def test_rejects_reserved_plan_args(self):
        for arg in ("-out=/tmp/x", "-lock=true", "-state=other.tfstate", "-var-file=x.tfvars", "-input=true"):
            with self.assertRaises(cfg.ConfigError, msg=arg):
                cfg.load(self.text.replace("summary_detail: addresses", f'summary_detail: addresses\n  plan_args: ["{arg}"]', 1))

    def test_rejects_overlapping_dirs_on_one_branch(self):
        t = self.text.replace("dir: envs/integration", "dir: envs/prod/networking/sub").replace(
            "branch: integration", "branch: main"
        )
        self._reject(t)

    def test_impersonate_may_be_empty_locally(self):
        t = self.text.replace("tf-prod-networking-plan@acme-tf.iam.gserviceaccount.com", '""', 1)
        self.assertEqual(cfg.load(t).workspace("prod-networking").impersonate.plan, "")


class TestBranchMatching(unittest.TestCase):
    def test_exact(self):
        self.assertTrue(cfg.match_branch("main", "main"))
        self.assertFalse(cfg.match_branch("main", "mainline"))

    def test_trailing_star_is_one_segment_only(self):
        self.assertTrue(cfg.match_branch("release/*", "release/2026-08"))
        self.assertFalse(cfg.match_branch("release/*", "release/a/b"))
        self.assertFalse(cfg.match_branch("release/*", "release/"))
        self.assertFalse(cfg.match_branch("release/*", "releases/x"))


class TestTouches(unittest.TestCase):
    def test_dir_matches_on_a_path_boundary(self):
        ws = workspace(dir="envs/prod")
        self.assertTrue(cfg.touches(ws, ["envs/prod/main.tf"]))
        self.assertFalse(cfg.touches(ws, ["envs/production/main.tf"]))

    def test_watch_doublestar(self):
        ws = workspace(dir="envs/prod", watch=("modules/vpc/**",))
        self.assertTrue(cfg.touches(ws, ["modules/vpc/a/b/c.tf"]))
        self.assertFalse(cfg.touches(ws, ["modules/dns/main.tf"]))

    def test_watch_single_star_stops_at_slash(self):
        ws = workspace(dir="envs/prod", watch=("modules/*",))
        self.assertTrue(cfg.touches(ws, ["modules/main.tf"]))
        self.assertFalse(cfg.touches(ws, ["modules/vpc/main.tf"]))


class TestScope(unittest.TestCase):
    def setUp(self):
        self.cfg = cfg.load(EXAMPLE.read_text())

    def test_branch_selects_then_paths_filter(self):
        s = cfg.scope(self.cfg, "main", ["envs/prod/networking/main.tf"])
        self.assertEqual([w.name for w in s.in_scope], ["prod-networking"])
        s = cfg.scope(self.cfg, "main", ["README.md"])
        self.assertEqual(s.in_scope, [])
        self.assertEqual([w.name for w in s.candidates], ["prod-networking"])

    def test_truncated_file_list_scopes_every_candidate(self):
        # Over-planning is recoverable; silently skipping a workspace is not.
        s = cfg.scope(self.cfg, "main", ["README.md"], all_candidates=True)
        self.assertEqual([w.name for w in s.in_scope], ["prod-networking"])

    def test_config_change_is_flagged(self):
        self.assertTrue(cfg.scope(self.cfg, "main", [cfg.FILENAME]).config_changed)
        self.assertFalse(cfg.scope(self.cfg, "main", ["a.tf"]).config_changed)


class TestModuleSourceScanner(unittest.TestCase):
    def check(self, text, want):
        self.assertEqual(cfg._sources_in_text(text), want)

    def test_single_line_block(self):
        self.check('module "vpc" { source = "./modules/vpc" }', ["./modules/vpc"])

    def test_multi_line_block(self):
        self.check('module "vpc" {\n  source  = "registry.terraform.io/acme/vpc/google"\n  version = "1.0"\n}\n',
                   ["registry.terraform.io/acme/vpc/google"])

    def test_several_blocks(self):
        self.check('module "a" { source = "./a" }\nmodule "b" {\n source = "./b"\n}\n', ["./a", "./b"])

    def test_nested_block_source_is_not_the_modules(self):
        self.check('module "a" {\n source = "./a"\n provisioner "x" { source = "./nope" }\n}\n', ["./a"])

    def test_non_module_blocks_ignored(self):
        self.check('resource "null_resource" "r" { source = "./nope" }\nmodule "m" { source = "./yes" }', ["./yes"])

    def test_comments(self):
        self.check('module "a" {\n # source = "./x"\n // source = "./y"\n /* source = "./z" */\n source = "./real"\n}',
                   ["./real"])

    def test_heredoc_with_unbalanced_brace(self):
        self.check('module "a" {\n  script = <<-EOT\n    if true { echo "}" }\n  EOT\n  source = "./real"\n}', ["./real"])

    def test_braces_and_escapes_inside_strings(self):
        self.check('module "a" {\n  name = "a{b}c"\n  msg = "say \\"hi\\" {"\n  source = "./real"\n}', ["./real"])

    def test_module_block_quoted_inside_a_string_is_not_a_block(self):
        self.check('locals { s = "module \\"x\\" { source = \\"./evil\\" }" }\nmodule "m" { source = "./yes" }', ["./yes"])

    def test_source_prefixed_identifier_is_not_source(self):
        self.check('module "a" {\n  source_dir = "./nope"\n  source = "./real"\n}', ["./real"])

    def test_no_modules(self):
        self.check('variable "x" {}\noutput "y" { value = 1 }', [])


class TestModuleSourceAllowlist(unittest.TestCase):
    def allowed(self, src, allow):
        try:
            cfg.check_module_sources([(src, "a.tf")], allow)
            return True
        except cfg.ModuleSourceDenied:
            return False

    def test_local_always_allowed(self):
        self.assertTrue(self.allowed("./modules/vpc", []))
        self.assertTrue(self.allowed("../shared", []))

    def test_empty_allowlist_permits_local_only(self):
        self.assertFalse(self.allowed("registry.terraform.io/acme/vpc/google", []))

    def test_glob_crosses_slashes_because_sources_are_urls(self):
        pat = "git::ssh://git@github.com/acme/terraform-modules//*?ref=v*"
        self.assertTrue(self.allowed("git::ssh://git@github.com/acme/terraform-modules//vpc/core?ref=v1.2.0", [pat]))
        self.assertFalse(self.allowed("git::ssh://git@github.com/evil/mods//x?ref=v1", [pat]))

    def test_transitive_through_local_modules(self):
        # A local module can pull a remote one, and that remote source is exactly what the
        # allowlist exists to gate.
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root / "envs/prod").mkdir(parents=True)
            (root / "modules/vpc").mkdir(parents=True)
            (root / "envs/prod/main.tf").write_text('module "vpc" { source = "../../modules/vpc" }')
            (root / "modules/vpc/main.tf").write_text('module "inner" { source = "registry.terraform.io/evil/x/y" }')
            found = cfg.collect_module_sources(root, root / "envs/prod")
            self.assertIn("registry.terraform.io/evil/x/y", [s for s, _ in found])
            with self.assertRaises(cfg.ModuleSourceDenied):
                cfg.check_module_sources(found, [])

    def test_local_source_escaping_the_repo_is_denied(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d) / "repo"
            (root / "envs").mkdir(parents=True)
            (root / "envs/main.tf").write_text('module "x" { source = "../../../etc" }')
            with self.assertRaises(cfg.ModuleSourceDenied):
                cfg.collect_module_sources(root, root / "envs")


class TestRender(unittest.TestCase):
    def plan(self, *action_lists):
        return json.dumps({
            "resource_changes": [
                {"address": f"res.r{i}", "change": {"actions": a}} for i, a in enumerate(action_lists)
            ]
        }).encode()

    def test_replace_is_one_change_not_two(self):
        # The headline number reviewers actually read. Counting a replace as one create plus one
        # delete understates destruction.
        t = render.counts(self.plan(["delete", "create"]))
        self.assertEqual(t["replace"], 1)
        self.assertEqual((t["create"], t["delete"]), (0, 0))

    def test_create_before_destroy_pair_is_also_a_replace(self):
        self.assertEqual(render.counts(self.plan(["create", "delete"]))["replace"], 1)

    def test_no_op_and_read_are_not_changes(self):
        t = render.counts(self.plan(["no-op"], ["read"]))
        self.assertFalse(render.has_changes(t))
        self.assertEqual(t["read"], 1)

    def test_headline_mentions_replace_only_when_present(self):
        self.assertNotIn("replace", render.headline(render.counts(self.plan(["create"]))))
        self.assertIn("1 to replace", render.headline(render.counts(self.plan(["delete", "create"]))))

    def test_address_lines_are_capped_and_report_omissions(self):
        pj = self.plan(*[["create"]] * 250)
        lines, omitted = render.address_lines(pj)
        self.assertEqual(len(lines), render.MAX_RENDERED_ADDRESSES)
        self.assertEqual(omitted, 250 - render.MAX_RENDERED_ADDRESSES)

    def test_meta_block_round_trips(self):
        meta = {"base_sha": "a" * 40, "head_sha": "c" * 40, "digest": "d" * 64}
        body = render._meta_block(meta)
        self.assertEqual(render.parse_meta_block("prose\n" + body + "\nmore"), meta)
        self.assertIsNone(render.parse_meta_block("no block here"))


class TestStore(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.st = store.Store(self.dir)
        self.key = store.PlanKey("acme", "infra", "prod-networking", "a" * 40, "c" * 40)

    def meta(self):
        return store.new_meta(
            self.key, pr=412, planned_tree_sha="9" * 40, config_ref="main", config_ref_sha="b" * 40,
            terraform_version="1.9.8", provider_lock_sha256="d" * 64, plan_sha256="e" * 64,
            has_changes=True, counts={"create": 1, "update": 0, "delete": 0, "replace": 0, "read": 0},
            duration_seconds=42.0, base_ref="main", workspace_dir="envs/prod/networking", planned_by="someone",
        )

    def test_key_layout_matches_the_bucket(self):
        self.assertEqual(self.key.prefix(), f"acme/infra/prod-networking/{'a' * 40}/{'c' * 40}")

    def test_write_once(self):
        self.st.create(self.key, store.ARTIFACT_PLAN, b"PLAN")
        with self.assertRaises(store.StoreError):
            self.st.create(self.key, store.ARTIFACT_PLAN, b"SWAPPED")
        self.assertEqual(self.st.path(self.key, store.ARTIFACT_PLAN).read_bytes(), b"PLAN")

    def test_artifacts_are_not_world_readable(self):
        # A plan file embeds a snapshot of prior state (DESIGN 5.1).
        self.st.create(self.key, store.ARTIFACT_PLAN, b"PLAN")
        self.assertEqual(os.stat(self.st.path(self.key, store.ARTIFACT_PLAN)).st_mode & 0o777, 0o600)
        self.assertEqual(os.stat(self.st.key_dir(self.key)).st_mode & 0o777, 0o700)

    def test_complete_only_after_meta(self):
        self.st.create(self.key, store.ARTIFACT_PLAN, b"PLAN")
        self.assertFalse(self.st.complete(self.key))
        self.st.write_meta(self.key, self.meta())
        self.assertTrue(self.st.complete(self.key))

    def test_meta_digest_detects_an_edit(self):
        self.st.write_meta(self.key, self.meta())
        p = self.st.path(self.key, store.ARTIFACT_META)
        d = json.loads(p.read_text())
        d["planned_tree_sha"] = "0" * 40
        p.write_text(json.dumps(d))
        with self.assertRaises(store.StoreError):
            self.st.read_meta(self.key)

    def test_meta_without_a_digest_is_refused(self):
        self.st._mkdir(self.key)
        self.st.create(self.key, store.ARTIFACT_META, json.dumps({"owner": "acme"}).encode())
        with self.assertRaises(store.StoreError):
            self.st.read_meta(self.key)

    def test_find_by_head_sha_globs_the_base(self):
        # By apply time the base branch tip has moved past what was planned, so base_sha cannot
        # be re-derived — only the head SHA is still known.
        self.st.create(self.key, store.ARTIFACT_PLAN, b"PLAN")
        self.st.write_meta(self.key, self.meta())
        found = self.st.find("acme", "infra", "prod-networking", head_sha="c" * 40)
        self.assertEqual([k.prefix() for k in found], [self.key.prefix()])
        self.assertEqual(self.st.find("acme", "infra", "prod-networking", head_sha="f" * 40), [])

    def test_find_ignores_incomplete_plans(self):
        self.st.create(self.key, store.ARTIFACT_PLAN, b"PLAN")  # no meta.json
        self.assertEqual(self.st.find("acme", "infra", "prod-networking"), [])

    def test_state_transitions(self):
        self.assertEqual(self.st.read_state(self.key), {})
        self.st.write_state(self.key, store.STATE_PLANNED, pr=412)
        self.assertEqual(self.st.read_state(self.key)["state"], "planned")
        self.st.write_state(self.key, store.STATE_APPLIED, attempt=1)
        self.assertEqual(self.st.read_state(self.key)["state"], "applied")

    def test_apply_log_attempts_increment(self):
        p1, a1 = self.st.apply_log_path(self.key)
        p1.write_text("first")
        p2, a2 = self.st.apply_log_path(self.key)
        self.assertEqual((a1, a2), (1, 2))
        self.assertNotEqual(p1, p2)

    def test_purge_removes_the_key(self):
        self.st.create(self.key, store.ARTIFACT_PLAN, b"PLAN")
        self.st.purge(self.key)
        self.assertFalse(self.st.key_dir(self.key).exists())


class TestDivergence(unittest.TestCase):
    def test_only_ahead_or_identical_may_proceed(self):
        from tfog import terra

        for status in ("ahead", "identical"):
            terra.divergence({"status": status})  # must not raise
        for status in ("behind", "diverged"):
            with self.assertRaises(terra.BranchBehindBase):
                terra.divergence({"status": status, "behind_by": 3})


if __name__ == "__main__":
    unittest.main(verbosity=2)
