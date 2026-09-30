#!/usr/bin/env python3

import argparse
import json
import math
import os
from pathlib import Path
import re
import secrets
import statistics
import subprocess
import tempfile
import time


RECORD = re.compile(r"(?P<user>[^:\n]+):\$2[aby]\$(?P<cost>[0-9]{2})\$[./A-Za-z0-9]{53}\n{1,2}")


def inspect_script(path, cost):
    encoded = f"{cost:02d}"
    script = path.read_text()
    pattern = f"auth_pattern='^[^:]+:\\$2[aby]\\${encoded}\\$[./A-Za-z0-9]{{53}}$'"
    generation = f'htpasswd -niBC {encoded} "$auth_user" < "$password_file" > "$auth_tmp"'
    if "${bcryptCost}" in script:
        raise ValueError("reconciliation script contains an unevaluated bcrypt cost")
    if pattern not in script or generation not in script:
        raise ValueError(f"reconciliation script does not enforce and generate cost {encoded}")


def validate_record(record, expected_cost):
    match = RECORD.fullmatch(record)
    if match is None or match.group("user") != "developer":
        raise ValueError("synthetic htpasswd record has an invalid shape")
    if match.group("cost") != f"{expected_cost:02d}":
        raise ValueError("synthetic htpasswd record has the wrong encoded cost")


def run_htpasswd(htpasswd, args, secret):
    return subprocess.run(
        [str(htpasswd), *args],
        input=secret + "\n",
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )


def make_fixture(htpasswd, directory, cost, secret):
    generated = run_htpasswd(htpasswd, ["-niBC", f"{cost:02d}", "developer"], secret)
    if generated.returncode != 0:
        raise RuntimeError(f"synthetic htpasswd generation failed at cost {cost}")
    validate_record(generated.stdout, cost)
    path = directory / f"cost-{cost}.htpasswd"
    path.write_text(generated.stdout)
    os.chmod(path, 0o600)
    return path, generated.stdout


def verify(htpasswd, path, secret):
    return run_htpasswd(htpasswd, ["-vi", str(path), "developer"], secret).returncode == 0


def measure(htpasswd, path, secret):
    if not verify(htpasswd, path, secret):
        raise RuntimeError("synthetic warmup verification failed")
    samples = []
    batches = []
    for _ in range(5):
        batch_start = time.perf_counter_ns()
        for _ in range(10):
            started = time.perf_counter_ns()
            if not verify(htpasswd, path, secret):
                raise RuntimeError("synthetic password verification failed")
            samples.append((time.perf_counter_ns() - started) / 1_000_000)
        batches.append((time.perf_counter_ns() - batch_start) / 1_000_000)
    ordered = sorted(samples)
    return {
        "sample_count": len(samples),
        "median_ms": statistics.median(samples),
        "p95_ms": ordered[math.ceil(0.95 * len(ordered)) - 1],
        "ten_verification_batch_ms": batches,
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--inspect-script", type=Path)
    parser.add_argument("--expected-cost", type=int)
    parser.add_argument("--htpasswd", type=Path)
    parser.add_argument("--apache-version")
    args = parser.parse_args()

    if args.inspect_script is not None:
        if args.expected_cost is None:
            parser.error("--inspect-script requires --expected-cost")
        inspect_script(args.inspect_script, args.expected_cost)
    if args.htpasswd is None:
        if args.inspect_script is None:
            parser.error("--htpasswd or --inspect-script is required")
        return
    if args.apache_version is None:
        parser.error("--htpasswd requires --apache-version")

    secret = secrets.token_hex(32)
    wrong_secret = ("0" if secret[0] != "0" else "1") + secret[1:]
    results = {}
    with tempfile.TemporaryDirectory(prefix="workspace-host-auth-") as temporary:
        for cost in (5, 12):
            path, record = make_fixture(args.htpasswd, Path(temporary), cost, secret)
            if cost == 12:
                try:
                    validate_record(record, 5)
                except ValueError:
                    pass
                else:
                    raise RuntimeError("cost-12 fixture passed the cost-5 gate")
            if not verify(args.htpasswd, path, secret) or verify(args.htpasswd, path, wrong_secret):
                raise RuntimeError(f"synthetic password verification behavior failed at cost {cost}")
            results[f"cost_{cost:02d}"] = {
                "declared_cost": cost,
                "encoded_cost": f"{cost:02d}",
                "correct_password": True,
                "wrong_password_rejected": True,
                **measure(args.htpasswd, path, secret),
            }
    results["cost_05_to_12_median_ratio"] = results["cost_12"]["median_ms"] / results["cost_05"]["median_ms"]
    print(json.dumps({
        "htpasswd_executable": str(args.htpasswd.resolve()),
        "apache_version": args.apache_version,
        "results": results,
    }, sort_keys=True, indent=2))


if __name__ == "__main__":
    main()
