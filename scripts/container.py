#!/usr/bin/env python3
"""One container launcher for runtime, systemd, and performance verification."""

import argparse
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import time

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[1]
RUNTIME = {"runtime", "qos", "mcp", "gateway", "live"}
PATTERNS = {"qos": "^Test(CPU(ControlCgroup|QoSRuntime)|PressureSchedulingRuntime)$",
            "mcp": "^TestMCPRuntime$", "gateway": "^TestGatewayRuntime$",
            "live": "^TestDeepSeekLiveRuntime$"}


def arguments(argv=None, environment=None):
    environment = os.environ if environment is None else environment
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("suite", choices=sorted(RUNTIME | {"systemd", "benchmark", "observe", "static"}))
    parser.add_argument("--platform", choices=("kvm", "systrap"), default="kvm")
    parser.add_argument("--mode", choices=("cold", "warm"), default="cold", help="systemd or single-run runtime mode")
    parser.add_argument("--gateway", action="store_true", help="paired Gateway systemd acceptance")
    parser.add_argument("--pattern", help="selected runtime/qos Go tests")
    parser.add_argument("--create-concurrency", type=int, choices=(1, 2), default=1, help="benchmark shared creation concurrency")
    parser.add_argument("--samples", type=int, default=60, help="benchmark sample count")
    parser.add_argument("--rate", type=int, default=10, help="benchmark offered requests/second")
    parser.add_argument("--comparison", choices=("static", "dynamic", "baseline"), default="static", help="static suite: fixed quotas, low/high/low comparison, or core-only baseline")
    parser.add_argument("--duration", type=int, default=60, help="seconds per static group; per phase for dynamic/baseline")
    parser.add_argument("--repetitions", type=int, default=3, help="static comparison repetitions")
    parser.add_argument("--dry-run", action="store_true", help="print commands without building, Docker access, or API calls")
    args = parser.parse_args(argv)
    if not (10 <= args.samples <= 1000 and 1 <= args.rate <= 100 and 5 <= args.duration <= 300 and 1 <= args.repetitions <= 9):
        parser.error("samples 10..1000, rate 1..100, duration 5..300, repetitions 1..9 required")
    live = environment.get("AGENTBOX_LIVE_DEEPSEEK", "0")
    if live not in ("0", "1") or (live == "1") != (args.suite == "live"):
        parser.error("live API calls require both the live suite and AGENTBOX_LIVE_DEEPSEEK=1")
    if args.pattern and args.suite not in ("runtime", "qos"):
        parser.error("--pattern is restricted to runtime/qos; named suites have fixed selections")
    if args.comparison != "static" and args.suite != "static":
        parser.error("--comparison applies only to static suite")
    if args.gateway and args.suite != "systemd":
        parser.error("--gateway applies only to systemd")
    for key in ("AGENTBOX_TEST_DEPENDENCIES_TMPFS", "AGENTBOX_TEST_IMAGE_ROOTFS"):
        if environment.get(key, "0") not in ("0", "1"):
            parser.error(key+" must be 0 or 1")
    return args


def mounts(source, target, readonly=True):
    return ["--mount", f"type=bind,source={source},target={target}"+(",readonly" if readonly else "")]


def envflag(key, value):
    return ["-e", key+"="+str(value)]


def docker_command(args, image, output=None, root=ROOT, environment=None):
    environment = os.environ if environment is None else environment
    suite = args.suite
    command = ["docker", "run"]
    if suite == "systemd":
        command += ["--detach", "--tty"]
    else:
        command += ["--rm"]
        if suite != "runtime" or args.pattern:
            command += ["--init"]
    command += ["--privileged", "--cgroupns=private", "--network="+("bridge" if suite == "live" else "none"),
                "--memory=5g", "--memory-swap=5g", "--cpus="+("2" if suite in ("observe", "static") else "6"), "--pids-limit=2304"]
    if args.platform == "kvm":
        command += ["--device", "/dev/kvm"]
    command += envflag("AGENTBOX_PLATFORM", args.platform) + envflag("PYTHONDONTWRITEBYTECODE", "1")
    if suite == "systemd":
        command += ["--tmpfs", "/run:rw,nosuid,nodev,mode=755", "--tmpfs", "/opt:rw,exec,nosuid,nodev,mode=755", "--stop-signal", "SIGRTMIN+3"]
        command += envflag("container", "docker") + envflag("SYSTEMD_LOG_TARGET", "console") + envflag("AGENTBOX_TEST_MODE", args.mode)
        return command + mounts(root, "/workspace") + ["--entrypoint", "/sbin/init", image, "--unit=basic.target"]
    if suite in ("observe", "static") or suite in RUNTIME - {"runtime"}:
        command += ["--tmpfs", "/tmp:rw,exec,nosuid,nodev,mode=1777,size=268435456"]
    if suite in ("observe", "static"):
        command += mounts(root, "/workspace") + mounts(output, "/results", False)
        if suite == "observe":
            command += ["--tmpfs", "/state:rw,nosuid,nodev,mode=700,size=134217728"]
            script = "container_validation.py"
        else:
            command += ["--tmpfs", "/lab:rw,exec,nosuid,nodev,mode=755,size=1073741824"]
            command += envflag("AGENTBOX_TEST_IMAGE_ROOTFS", environment.get("AGENTBOX_TEST_IMAGE_ROOTFS", "0"))
            command += envflag("AGENTBOX_EXPERIMENT_DURATION", args.duration) + envflag("AGENTBOX_EXPERIMENT_REPETITIONS", args.repetitions) + envflag("AGENTBOX_EXPERIMENT_COMPARISON", args.comparison)
            for key in ("AGENTBOX_SOURCE_REVISION", "AGENTBOX_SOURCE_DIRTY"):
                command += envflag(key, environment.get(key, "unknown"))
            script = "static_container.py"
        return command + ["--entrypoint", "python3", image, "-u", "/workspace/tests/observation/"+script]
    runtime_source, rootfs_source = "/runtime", "/rootfs"
    prepared = environment.get("AGENTBOX_TEST_DEPENDENCIES_TMPFS", "0")
    image_rootfs = environment.get("AGENTBOX_TEST_IMAGE_ROOTFS", "0")
    if prepared == "1":
        runtime_source = "/runtime-source"
        command += ["--tmpfs", "/runtime:rw,exec,nosuid,nodev,mode=755,size=536870912"]
        rootfs_source = "/rootfs-source"
        if image_rootfs != "1":
            command += ["--tmpfs", "/rootfs:rw,exec,nosuid,nodev,mode=755,size=536870912"]
    command += mounts(root / ".local/gvisor", runtime_source)
    if image_rootfs == "1":
        command += ["--tmpfs", "/rootfs:rw,exec,nosuid,nodev,mode=755,size=536870912"]
    else:
        command += mounts(root / ".local/rootfs", rootfs_source)
    command += envflag("AGENTBOX_TEST_DEPENDENCIES_TMPFS", prepared) + envflag("AGENTBOX_TEST_IMAGE_ROOTFS", image_rootfs)
    command += mounts(root / "bin", "/tests") + mounts(root / "scripts/lib/container-entrypoint.sh", "/entrypoint.sh")
    if suite == "live":
        settings = environment.get("AGENTBOX_DEEPSEEK_CONFIG", str(root / ".local/deepseek.env"))
        command += mounts(Path(settings).absolute(), "/run/agentbox-live/deepseek.env")
        command += envflag("AGENTBOX_LIVE_DEEPSEEK", "1") + envflag("AGENTBOX_DEEPSEEK_CONFIG", "/run/agentbox-live/deepseek.env")
    if suite == "benchmark":
        command += mounts(output, "/results", False)
        timeout = str(1200+(2*args.samples+args.rate-1)//args.rate)+"s"
        for key, value in {"AGENTBOX_BENCHMARK": 1, "AGENTBOX_BENCH_CREATE_CONCURRENCY": args.create_concurrency, "AGENTBOX_BENCH_SAMPLES": args.samples, "AGENTBOX_BENCH_RATE": args.rate,
                           "AGENTBOX_BENCH_TEST_TIMEOUT": timeout, "AGENTBOX_BENCH_OUTPUT": "/results",
                           "AGENTBOX_BENCH_ENVIRONMENT": "isolated-docker", "AGENTBOX_ROOTFS_IMAGE": image}.items():
            command += envflag(key, value)
    else:
        pattern = args.pattern or PATTERNS.get(suite)
        if pattern:
            command += envflag("AGENTBOX_TEST_SINGLE_RUN", "1") + envflag("AGENTBOX_TEST_RUN", pattern) + envflag("AGENTBOX_EXECUTION_MODE", args.mode)
        command += envflag("AGENTBOX_GATEWAY_MEASURE", environment.get("AGENTBOX_GATEWAY_MEASURE", "0"))
    return command + ["--entrypoint", "/bin/bash", image, "/entrypoint.sh"]


def run(command, capture=False):
    return subprocess.run([str(x) for x in command], cwd=ROOT, check=True, text=True, capture_output=capture)


def systemd(command, args):
    container = run(command, True).stdout.strip()
    try:
        for _ in range(100):
            if run(["docker", "inspect", "--format", "{{.State.Running}}", container], True).stdout.strip() != "true":
                raise RuntimeError("systemd container exited before readiness")
            if subprocess.run(["docker", "exec", container, "systemctl", "show-environment"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
                break
            time.sleep(.2)
        else:
            raise RuntimeError("systemd did not become available")
        script = "gateway_validation.py" if args.gateway else "container_validation.py"
        run(["docker", "exec", "-e", "PYTHONDONTWRITEBYTECODE=1", container, "python3", "-u", "/workspace/tests/systemd/"+script])
    except BaseException:
        for probe in (["inspect", "--format", "{{json .State}}"], ["logs", "--tail", "60"]):
            subprocess.run(["docker", *probe, container], check=False)
        for unit in ("agentbox.service", "agentbox-gateway.service"):
            subprocess.run(["docker", "exec", container, "journalctl", "-u", unit, "-n", "60", "--no-pager"], check=False)
        raise
    finally:
        run(["docker", "rm", "-f", container], True)


def main():
    args = arguments()
    image = os.environ.get("AGENTBOX_SYSTEMD_TEST_IMAGE", "kindest/node:v1.29.2") if args.suite == "systemd" else "<pinned-python-image>"
    output = ROOT / "artifacts/<new-results>"
    if args.dry_run:
        print(shlex.join([str(x) for x in docker_command(args, image, output)]))
        return 0
    for required in (".local/gvisor/runsc", ".local/rootfs/usr/local/bin/python3"):
        if not (ROOT / required).is_file():
            raise ValueError("prepare runsc/rootfs with scripts/setup.sh first")
    if args.suite == "live" and not Path(os.environ.get("AGENTBOX_DEEPSEEK_CONFIG", ROOT / ".local/deepseek.env")).is_file():
        raise ValueError("private DeepSeek settings are required")
    if args.suite == "systemd":
        image = run(["docker", "image", "inspect", image, "--format", "{{.Id}}"], True).stdout.strip()
    else:
        image = json.loads((ROOT / ".local/rootfs-image.json").read_text())[0]
    if args.suite in RUNTIME | {"benchmark"}:
        (ROOT / "bin").mkdir(exist_ok=True)
        build_environment = dict(os.environ, CGO_ENABLED="0")
        package = "benchmarks" if args.suite == "benchmark" else "integration"
        binary = "benchmark.test" if args.suite == "benchmark" else "integration.test"
        subprocess.run(["go", "test", "-tags=integration", "-c", "-o", "bin/"+binary, "./tests/"+package], cwd=ROOT, env=build_environment, check=True)
        if args.suite != "benchmark":
            binaries = ["agentbox-gateway", "agentbox-mcp"] + (["agentbox-agent"] if args.suite == "live" else [])
            for binary in binaries:
                subprocess.run(["go", "build", "-trimpath", "-o", "bin/"+binary, "./cmd/"+binary], cwd=ROOT, env=build_environment, check=True)
    else:
        needed = ["agentboxd"] + (["mockcore"] if args.suite in ("observe", "static") else []) + (["agentbox-gateway"] if args.gateway else [])
        if any(not (ROOT / "bin" / name).is_file() for name in needed):
            raise ValueError("run make build before this suite")
    environment = dict(os.environ)
    plot_python = os.environ.get("AGENTBOX_PLOT_PYTHON", str(ROOT / ".local/plot-venv/bin/python"))
    if not Path(plot_python).is_file():
        plot_python = sys.executable
    if args.suite == "static":
        if args.comparison == "static":
            run([plot_python, "-c", "import matplotlib"])
        environment["AGENTBOX_SOURCE_REVISION"] = run(["git", "rev-parse", "HEAD"], True).stdout.strip()
        environment["AGENTBOX_SOURCE_DIRTY"] = str(bool(run(["git", "status", "--porcelain"], True).stdout.strip())).lower()
    if args.suite in ("benchmark", "observe", "static"):
        (ROOT / "artifacts").mkdir(exist_ok=True)
        output = Path(tempfile.mkdtemp(prefix=args.suite+"-"+args.platform+"-", dir=ROOT / "artifacts"))
        print("Results: "+str(output), flush=True)
    command = docker_command(args, image, output, environment=environment)
    if args.suite == "systemd":
        systemd(command, args)
    else:
        run(command)
    if args.suite == "static" and args.comparison == "static":
        run([plot_python, ROOT / "scripts/experiment.py", "plot", output])
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError) as error:
        print("Container verification failed: "+str(error), file=sys.stderr)
        sys.exit(1)
