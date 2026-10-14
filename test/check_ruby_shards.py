"""Require complete, unique coverage across independently executed Ruby shards."""

import collections
import json
import sys


def validate(paths):
    suites = collections.defaultdict(list)
    for path in paths:
        with open(path, encoding="utf-8") as stream:
            report = json.load(stream)
        if report.get("schema") != 1 or not report.get("coverageComplete"):
            raise ValueError(f"incomplete Ruby shard: {path}")
        if sorted(test["identity"] for test in report["tests"]) != report["selected"]:
            raise ValueError(f"invalid executed manifest: {path}")
        if any(not test["passed"] and not test["skipped"] for test in report["tests"]):
            raise ValueError(f"failed Ruby shard: {path}")
        suites[report["suite"]].append(report)
    for suite, reports in suites.items():
        discovered = reports[0]["discovered"]
        if any(report["discovered"] != discovered for report in reports):
            raise ValueError(f"different discovered manifests: {suite}")
        shards = [report["shard"] for report in reports]
        counts = {int(shard.split("/")[1]) for shard in shards}
        if len(counts) != 1 or sorted(shards) != sorted(f"{i}/{next(iter(counts))}" for i in range(next(iter(counts)))):
            raise ValueError(f"missing or duplicated shards: {suite}")
        executed = [test["identity"] for report in reports for test in report["tests"]]
        if sorted(executed) != discovered:
            raise ValueError(f"missing or duplicated tests: {suite}")
        print(f"{suite}: {len(executed)} tests, {len(reports)} shards, complete coverage")


if __name__ == "__main__":
    validate(sys.argv[1:])
