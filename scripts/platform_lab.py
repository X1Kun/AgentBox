#!/usr/bin/env python3
"""Command routing for the owned platform; scenarios load only when selected."""
import argparse
import importlib
import json
from pathlib import Path
import platform
import sys
import uuid

from lib.core.paths import ROOT, CLUSTER, CONTEXT, GVISOR_CASES as CASES
from lib.core.errors import ToolError, LabError, CommandError
from lib.core.runner import Runner
from lib.core.state import utc, file_hash, atomic_json
from lib.manifests import load_lock, read_yaml, lock_identity, python_image_source
from lib.platform.lab import Lab, pinned_source

sys.dont_write_bytecode = True

def load(module, name):
    return getattr(importlib.import_module('lib.'+module), name)

def source_inventory(root):
    files = [p for directory in ('scripts','deploy','workloads','experiments/configs','tests/fixtures')
             for p in (root/directory).rglob('*') if p.is_file() and
             (p.suffix in ('.py','.yaml','.sh','.Dockerfile','.toml') or p.name=='Dockerfile')]
    files += [p for directory in ('cmd','internal') for p in (root/directory).rglob('*.go')]
    files += [root/'go.mod', root/'go.sum', root/'scripts/requirements-platform.txt']
    return {str(p.relative_to(root)):file_hash(p) for p in sorted(set(files))}


def arguments(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("preflight", "create", "smoke", "delete", "gvisor-install", "gvisor-test", "network-install", "network-test", "session-build", "session-install", "session-test", "api-test", "api-exec-test", "api-retention-test", "agent-test", "up", "ready", "batch-install", "batch-test", "batch-cleanup", "batch-image-build", "batch-product-test", "batch-product-cleanup", "observability-test", "maintenance-test", "session-boundary-test", "session-matrix-test", "static-baseline", "static-baseline-cleanup", "diagnostics-test", "product-entry-test", "product-images"))
    parser.add_argument("--baseline-config", type=Path, default=ROOT / "experiments/configs/session/baseline.yaml", help="frozen R/B baseline YAML")
    parser.add_argument("--baseline-report", type=Path, help="original static-baseline.json for scoped cleanup recovery")
    parser.add_argument("--baseline-stage", choices=("calibration", "comparison"), default="calibration", help="R calibration first; R/B comparison requires a matching reference")
    parser.add_argument("--reference-report", type=Path, help="R calibration static-baseline.json for comparison gating")
    parser.add_argument("--with-batch", action="store_true", help="include the verified image worker in product-images output")
    parser.add_argument("--matrix-scenarios", help="comma-separated matrix scenarios; isolated diagnostic selection")
    parser.add_argument("--dry-run", action="store_true", help="offline plan; no tools, writes or Docker calls")
    parser.add_argument("--output", type=Path, help="new evidence directory; never overwritten")
    parser.add_argument("--node-image-source", help="mirror repository with the SAME locked node digest (create)")
    parser.add_argument("--smoke-image-source", help="mirror with the SAME task digest (smoke/gvisor-test/network-test/batch-test)")
    parser.add_argument("--network-profile", choices=("kindnet", "calico"), default="kindnet", help="CNI profile at cluster creation only")
    parser.add_argument("--calico-manifest", type=Path, help="pinned upstream VXLAN YAML (network-install)")
    parser.add_argument("--calico-image-prefix", help="digest-preserving registry/repository mirror (network-install)")
    parser.add_argument("--session-controller-source", help="digest-preserving controller image source (session-build)")
    parser.add_argument("--session-python-source", help="digest-preserving Python base source (session-build)")
    parser.add_argument("--session-manifest", type=Path, help="fixed upstream release YAML (session-install)")
    parser.add_argument("--session-profile", choices=("cold", "warm", "fault"), default="cold", help="session-test fixture: cold, fixed warm pool, or scoped lifecycle faults")
    parser.add_argument("--kueue-manifest", type=Path, help="fixed upstream release YAML (batch-install)")
    parser.add_argument("--kueue-image-source", help="digest-preserving Kueue controller mirror (batch-install)")
    parser.add_argument("--batch-report", type=Path, help="original batch-test.json identity record (batch-cleanup)")
    parser.add_argument("--gvisor-case", choices=CASES, help="run only one gvisor-test case; default runs all")
    parser.add_argument("--gvisor-archive", type=Path, help="verified complete gVisor tar.bz2 (gvisor-install only)")
    parser.add_argument("--timeout", type=int, default=180, help="Job/probe observation budget per startup or terminal phase, 30..600 seconds")
    args = parser.parse_args(argv)
    if args.action == "static-baseline-cleanup" and not args.baseline_report:
        parser.error("static-baseline-cleanup requires --baseline-report")
    if args.baseline_report and args.action != "static-baseline-cleanup":
        parser.error("--baseline-report applies only to static-baseline-cleanup")
    if args.action == "static-baseline":
        if args.baseline_stage == "comparison" and not args.reference_report:
            parser.error("comparison requires --reference-report")
        if args.baseline_stage == "calibration" and args.reference_report:
            parser.error("--reference-report applies only to comparison")
    elif args.reference_report:
        parser.error("--reference-report applies only to static-baseline")
    if args.node_image_source and args.action not in ("create", "up"):
        parser.error("--node-image-source applies only to create or up")
    if args.smoke_image_source and args.action not in ("smoke", "gvisor-test", "network-test", "batch-test"):
        parser.error("--smoke-image-source applies only to smoke, gvisor-test, network-test or batch-test")
    if (args.kueue_manifest or args.kueue_image_source) and args.action != "batch-install":
        parser.error("Kueue source options apply only to batch-install")
    if (args.batch_report is not None) != (args.action in ("batch-cleanup", "batch-product-cleanup")):
        parser.error("--batch-report is required for batch-cleanup/batch-product-cleanup and applies only to those actions")
    if args.network_profile != "kindnet" and args.action != "create":
        parser.error("--network-profile applies only to create")
    if (args.calico_manifest or args.calico_image_prefix) and args.action not in ("network-install", "up"):
        parser.error("Calico source options apply only to network-install or up")
    if (args.session_controller_source or args.session_python_source) and args.action != "session-build":
        parser.error("session image sources apply only to session-build")
    if args.session_manifest and args.action not in ("session-install", "up"):
        parser.error("--session-manifest applies only to session-install or up")
    if args.session_profile != "cold" and args.action != "session-test":
        parser.error("--session-profile applies only to session-test")
    if args.gvisor_case and args.action != "gvisor-test":
        parser.error("--gvisor-case applies only to gvisor-test")
    if args.gvisor_archive and args.action not in ("gvisor-install", "up"):
        parser.error("--gvisor-archive applies only to gvisor-install or up")
    if not 30 <= args.timeout <= 600:
        parser.error("--timeout must be 30..600 seconds")
    return args


def main(argv=None):
    args = arguments(argv)
    if args.dry_run:
        if args.action == "static-baseline":
            from lib.experiments.results import plan, digest
            config = read_yaml(args.baseline_config)
            reference = json.loads(args.reference_report.read_text()) if args.reference_report else None
            cells = plan(config, args.baseline_stage, reference)
            print(json.dumps({"action": args.action, "config": config,
                              "config_sha256": digest(config), "cells": cells,
                              "cell_count": len(cells), "dry_run": True}, indent=2))
            return 0
        lock = load_lock(ROOT / "deploy", "versions")
        runtime = load_lock(ROOT / "deploy", "gvisor") if args.action.startswith("gvisor-") else None
        sessions = load_lock(ROOT / "deploy", "session") if args.action.startswith("session-") else None
        batch_lock = load_lock(ROOT / "deploy", "kueue") if args.action.startswith("batch-") else None
        session_sources = ({"controller": load('platform.sandbox', 'checked_source')(sessions["controller_image"], args.session_controller_source),
                            "python": load('platform.sandbox', 'checked_source')(lock["smoke"]["image"], args.session_python_source or python_image_source(ROOT / "deploy"))} if sessions else None)
        print(json.dumps({"action": args.action, "network_profile": "calico" if args.action == "up" else args.network_profile if args.action == "create" else "recorded lab profile", "gvisor": runtime, "gvisor_case": args.gvisor_case, "cluster": CLUSTER, "context": CONTEXT,
                          "kubeconfig": str(ROOT / ".local/platform/kubeconfig"),
                          "node_image": pinned_source(lock["kubernetes"]["node_image"], args.node_image_source),
                          "smoke_image": pinned_source(lock["smoke"]["image"], args.smoke_image_source or python_image_source(ROOT / "deploy")),
                          "sessions": sessions, "session_sources": session_sources, "session_profile": args.session_profile,
                          "bootstrap_stages": ["control-plane", "calico", "nodes", "gvisor", "control-plane-after-runtime", "sessions"] if args.action == "up" else None,
                          "calico_image_prefix": args.calico_image_prefix,
                          "kueue": batch_lock, "kueue_image": load('platform.kueue', 'controller_source')(ROOT / "deploy", args.kueue_image_source) if batch_lock else None,
                          "batch_report": str(args.batch_report) if args.batch_report else None,
                          "mutates_cluster": args.action not in ("preflight", "session-build", "ready"), "dry_run": True}, indent=2))
        return 0
    output = args.output or ROOT / "artifacts" / ("platform-" + args.action + "-" + uuid.uuid4().hex[:12])
    output.mkdir(parents=True, exist_ok=False)
    runner = Runner(output)
    source_root = Path(__file__).resolve().parents[1]
    report = {"schema_version": 1, "action": args.action, "started_at": utc(), "ok": False,
              "implementation_sha256": file_hash(__file__), "python_version": platform.python_version(),
              "kernel": platform.release(),
              "scope": "trusted platform functional probes; not full isolation or performance acceptance",
              "source_sha256": source_inventory(source_root)}

    lab = None
    try:
        lab = Lab(ROOT, runner)
        report["versions_lock"] = lab.lock
        if args.action == "preflight":
            report["result"] = lab.preflight()
            report["ok"] = report["result"]["ok"]
            if not report["ok"]:
                report["reason"] = "preflight_failed"
                report["failed_checks"] = [{"name": c["name"], "reason": c["reason"]}
                                           for c in report["result"]["checks"] if not c["ok"]]
        elif args.action == "up":
            report["result"] = lab.up(args.calico_manifest, args.calico_image_prefix, args.gvisor_archive, args.session_manifest, args.node_image_source)
            report["ok"] = True
        elif args.action == "ready":
            report["result"] = lab.wait_ready(args.timeout)
            report["ok"] = True
        elif args.action == "create":
            report["result"] = lab.create(args.node_image_source, args.network_profile)
            report["ok"] = True
        elif args.action == "smoke":
            report["result"] = load("acceptance.smoke", "SmokeAcceptance")(lab).test(args.smoke_image_source, args.timeout)
            report["ok"] = True
        elif args.action == "gvisor-install":
            runtime = load('platform.gvisor', 'GVisor')(lab)
            archive = args.gvisor_archive or ROOT / ".local/platform-tools" / runtime.lock["archive"]
            report["result"] = runtime.install(archive)
            report["ok"] = True
        elif args.action == "gvisor-test":
            image = lab.python_image(args.smoke_image_source)
            report["result"] = load('acceptance.gvisor', 'GVisorAcceptance')(lab).test(image, args.timeout, [args.gvisor_case] if args.gvisor_case else None)
            report["ok"] = True
        elif args.action == "network-install":
            network = load('platform.calico', 'Network')(lab)
            manifest = args.calico_manifest or ROOT / ".local/platform-tools" / network.lock["manifest"]
            report["result"] = network.install(manifest, args.calico_image_prefix)
            report["ok"] = True
        elif args.action == "network-test":
            image = lab.python_image(args.smoke_image_source)
            report["result"] = load('acceptance.network', 'NetworkAcceptance')(lab).test(image)
            report["ok"] = True
        elif args.action == "session-build":
            report["result"] = load('platform.sandbox', 'Sessions')(lab).build(args.session_controller_source, args.session_python_source)
            report["ok"] = True
        elif args.action == "session-install":
            sessions = load('platform.sandbox', 'Sessions')(lab)
            source = args.session_manifest or ROOT / ".local/platform-tools" / sessions.lock["manifest"]
            report["result"] = sessions.install(source)
            report["ok"] = True
        elif args.action == "session-test":
            report["result"] = load('acceptance.session', 'SessionAcceptance')(lab).test(args.session_profile)
            report["ok"] = True
        elif args.action == "agent-test":
            report["result"] = load('acceptance.agent', 'AgentAcceptance')(lab).test()
            report["ok"] = True
        elif args.action == "batch-install":
            batch = load('platform.kueue', 'Kueue')(lab)
            source = args.kueue_manifest or ROOT / ".local/platform-tools" / batch.lock["manifest"]
            report["result"] = batch.install(source, args.kueue_image_source)
            report["ok"] = True
        elif args.action == "batch-test":
            report["result"] = load('acceptance.batch', 'BatchAcceptance')(lab).test(lab.python_image(args.smoke_image_source))
            report["ok"] = True
        elif args.action == "batch-image-build":
            report["result"] = load('product.worker_image', 'build_worker')(lab)
            report["ok"] = True
        elif args.action == "batch-product-test":
            report["result"] = load('acceptance.batch_product', 'ProductAcceptance')(lab).test()
            report["ok"] = True
        elif args.action == "session-matrix-test":
            matrix = load('experiments.session_boundaries', 'SessionMatrix')(lab)
            matrix.selected_scenarios = args.matrix_scenarios.split(",") if args.matrix_scenarios else None
            report["result"] = matrix.test()
            report["ok"] = True
        elif args.action == "static-baseline":
            report["result"] = load('experiments.static_baseline', 'StaticBaseline')(lab).test(args.baseline_config, args.baseline_stage, args.reference_report)
            report["ok"] = True
        elif args.action == "static-baseline-cleanup":
            report["result"] = load('experiments.static_baseline', 'StaticBaseline')(lab).cleanup(args.baseline_report)
            report["ok"] = True
        elif args.action == "diagnostics-test":
            report["result"] = load('acceptance.diagnostics', 'DiagnosticsAcceptance')(lab).test()
            report["ok"] = True
        elif args.action == "product-images":
            report["result"] = load('product.images', 'prepare_product_images')(lab, args.with_batch)
            report["ok"] = True
        elif args.action == "product-entry-test":
            report["result"] = load('acceptance.product_entry', 'ProductEntryAcceptance')(lab).test()
            report["ok"] = True
        elif args.action == "session-boundary-test":
            report["result"] = load('experiments.session_boundaries', 'SessionBoundaries')(lab).test()
            report["ok"] = True
        elif args.action == "maintenance-test":
            report["result"] = load('acceptance.maintenance', 'MaintenanceAcceptance')(lab).test()
            report["ok"] = True
        elif args.action == "observability-test":
            report["result"] = load('acceptance.observability', 'ObservationAcceptance')(lab).test()
            report["ok"] = True
        elif args.action == "batch-product-cleanup":
            report["result"] = load('acceptance.batch_product', 'cleanup_product')(lab, args.batch_report)
            report["ok"] = True
        elif args.action == "batch-cleanup":
            report["result"] = load('acceptance.batch', 'BatchAcceptance')(lab).cleanup(args.batch_report)
            report["ok"] = True
        elif args.action == "api-retention-test":
            report["result"] = load('acceptance.retention', 'APIRetentionAcceptance')(lab).test()
            report["ok"] = True
        elif args.action == "api-exec-test":
            report["result"] = load('acceptance.api_execution', 'APIExecutionAcceptance')(lab).test()
            report["ok"] = True
        elif args.action == "api-test":
            report["result"] = load('acceptance.api', 'APIAcceptance')(lab).test()
            report["ok"] = True
        else:
            report["result"] = lab.delete()
            report["ok"] = True
    except (ToolError, OSError, ValueError, KeyError, KeyboardInterrupt) as error:
        reason = "interrupted" if isinstance(error, KeyboardInterrupt) else getattr(error, "reason", "invalid_state")
        report.update(reason=reason, detail=str(error))
        if lab and args.action in ("create", "smoke", "gvisor-install", "gvisor-test", "network-install", "network-test", "session-install", "session-test", "api-test", "api-exec-test", "api-retention-test", "agent-test", "up", "ready", "batch-install", "batch-test", "batch-cleanup", "batch-product-test", "batch-product-cleanup") and reason not in {
                "operation_in_progress", "existing_state", "unowned_cluster", "preflight_failed",
                "image_digest_mismatch", "invalid_image", "tool_unavailable", "calico_checksum_mismatch",
                "invalid_image_prefix", "wrong_network_profile", "network_definition_changed",
                "missing_bootstrap_asset", "cluster_not_ready", "baseline_changed", "kueue_checksum_mismatch",
                "unowned_kueue_resource", "kueue_definition_changed", "unowned_batch_report", "invalid_batch_cleanup"}:
            lab.diagnostics()
    finally:
        report.update(finished_at=utc(), commands=runner.commands)
        atomic_json(output / "report.json", report)
        print(json.dumps({"ok": report["ok"], "reason": report.get("reason"),
                          "failed_checks": report.get("failed_checks", []),
                          "phase": report.get("result", {}).get("state", {}).get("phase"),
                          "report": str(output / "report.json")}), flush=True)
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ToolError, OSError, ValueError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
