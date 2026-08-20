#!/usr/bin/env python3
"""Assert the chart's bundled retention policy actually protects something.

Every one of these four settings fails by silently protecting NOTHING rather than by erroring, which
is how they were found: seven e2e runs that read like the registry was broken, when the policy simply
matched nothing. A rendered config that looks plausible is not evidence. See ADR 0031.

Reads `helm template` output on stdin.
"""

import json
import sys

import yaml


def config_from(stream):
    # Read stdin to EOF before parsing. Returning early leaves the writer with a closed pipe, and
    # `helm template ... | this` then dies of SIGPIPE -- exit 141, under `set -o pipefail`, after
    # this script has already printed OK. The failure looks like the check failing when it passed.
    for doc in yaml.safe_load_all(stream.read()):
        if not doc or doc.get("kind") != "ConfigMap":
            continue
        if doc["metadata"]["name"].endswith("-registry"):
            return json.loads(doc["data"]["config.json"])
    raise SystemExit("no bundled registry ConfigMap rendered")


def problems(cfg):
    policy = cfg["storage"]["retention"]["policies"][0]
    found = []

    # Pull recency is only recorded when the metadata database exists. Without it every tag expires
    # however often it is fetched, and the refresh becomes a no-op that logs success.
    if not cfg.get("extensions", {}).get("search", {}).get("enable"):
        found.append(
            "extensions.search is off, so no pull is ever recorded and pulledWithin matches nothing"
        )

    # zot retains `patterns` AND (pulledWithin OR ...), so an entry without patterns matches no tags.
    keep_tags = policy.get("keepTags") or []
    if not keep_tags:
        found.append("no keepTags entry, so every tag is a deletion candidate")
    for entry in keep_tags:
        if not entry.get("patterns"):
            found.append("a keepTags entry has no patterns, so it protects no tag at all")
        if not entry.get("pulledWithin"):
            found.append("a keepTags entry does not key on pulledWithin, so refreshing cannot protect it")

    # Tagged and untagged manifests are governed independently, and ADR 0010 has workloads pin
    # digests -- so an untagged manifest may be exactly what a rescheduled pod pulls.
    if not policy.get("keepUntagged", {}).get("pulledWithin"):
        found.append("keepUntagged does not key on pulledWithin, so digest-pinned images are unprotected")

    return found


def main():
    found = problems(config_from(sys.stdin))
    if found:
        print("the bundled retention policy would protect nothing:")
        for f in found:
            print("  -", f)
        return 1
    print("OK: the policy keys on pull recency and covers both tags and digests.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
