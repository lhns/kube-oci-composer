#!/usr/bin/env python3
"""Assert the chart's bundled retention policy actually protects something.

Each setting checked here fails by silently protecting nothing rather than by erroring (ADR 0031).
Reads `helm template` output on stdin; an optional argument is a Markdown file whose first ```json
block must agree with the rendered config.
"""

import io
import json
import sys

import yaml


def config_from(stream):
    # Read to EOF first: returning early would SIGPIPE `helm template` and fail a pipefail pipeline.
    for doc in yaml.safe_load_all(stream.read()):
        if not doc or doc.get("kind") != "ConfigMap":
            continue
        if doc["metadata"]["name"].endswith("-registry"):
            return json.loads(doc["data"]["config.json"])
    raise SystemExit("no bundled registry ConfigMap rendered")


def policy_of(cfg):
    return cfg["storage"]["retention"]["policies"][0]


def problems(cfg):
    policy = policy_of(cfg)
    found = []

    # Without the search extension no pull is recorded, and every tag expires however often pulled.
    if not cfg.get("extensions", {}).get("search", {}).get("enable"):
        found.append(
            "extensions.search is off, so no pull is ever recorded and pulledWithin matches nothing"
        )

    # zot keeps `patterns` AND (pulledWithin OR ...): an entry without patterns matches no tag.
    keep_tags = policy.get("keepTags") or []
    if not keep_tags:
        found.append("no keepTags entry, so every tag is a deletion candidate")
    for entry in keep_tags:
        if not entry.get("patterns"):
            found.append("a keepTags entry has no patterns, so it protects no tag at all")
    # ONE entry with both rules: zot stops at the first entry whose patterns match. ADR 0057.
    if len(keep_tags) > 1:
        found.append(
            f"{len(keep_tags)} keepTags entries; only the first whose patterns match is ever "
            "evaluated, so the rest protect nothing"
        )
    if keep_tags and not keep_tags[0].get("pulledWithin"):
        found.append("the keepTags entry does not key on pulledWithin, so refreshing protects nothing")
    if keep_tags and not keep_tags[0].get("pushedWithin"):
        found.append("the keepTags entry does not key on pushedWithin, so a tag pushed and never "
                     "pulled is protected by nothing")

    # Nothing live is untagged (ADR 0060), and a configured keepUntagged keeps every retired
    # manifest forever, so the default policy must not have it.
    if "keepUntagged" in policy:
        found.append("keepUntagged is configured, which keeps every manifest whose last tag expired "
                     "-- and every layer it references -- forever (ADR 0060)")
    # Nothing reclaims retired manifests without it.
    if policy.get("deleteUntagged") is not True:
        found.append("deleteUntagged is not true, so a manifest whose last tag expired is never "
                     "reclaimed")
    # gcDelay is the only cover for a build's output between push and naming (ADR 0054).
    if not cfg["storage"].get("gcDelay"):
        found.append("gcDelay is not set, so nothing covers a build's output between its push and "
                     "the controller naming it")

    return found


DOC_FENCE = "```json"


def documented_config(path):
    """The first ```json block in a Markdown file, or None if there is none."""
    text = io.open(path, encoding="utf-8").read()
    start = text.find(DOC_FENCE)
    if start < 0:
        return None
    start = text.index("\n", start) + 1
    end = text.index("\n```", start)
    return json.loads(text[start:end])


def documented_drift(doc, rendered, path=""):
    """Every key the doc shows must equal what the chart renders.

    A subset match: the page is abridged, so omitting a key is fine and contradicting one is not.
    """
    found = []
    if isinstance(doc, dict):
        if not isinstance(rendered, dict):
            return [f"{path or 'config'}: documented as an object, rendered as {type(rendered).__name__}"]
        for key, value in doc.items():
            where = f"{path}.{key}" if path else key
            if key not in rendered:
                found.append(f"{where}: documented, not rendered")
                continue
            found += documented_drift(value, rendered[key], where)
    elif isinstance(doc, list):
        if not isinstance(rendered, list):
            return [f"{path}: documented as a list, rendered as {type(rendered).__name__}"]
        if len(doc) != len(rendered):
            found.append(f"{path}: {len(doc)} documented, {len(rendered)} rendered")
        for i, (d, r) in enumerate(zip(doc, rendered)):
            found += documented_drift(d, r, f"{path}[{i}]")
    elif doc != rendered:
        found.append(f"{path}: documented {doc!r}, renders {rendered!r}")
    return found


def main():
    cfg = config_from(sys.stdin)
    found = problems(cfg)
    if found:
        print("the bundled retention policy would lose live content or keep dead content forever:")
        for f in found:
            print("  -", f)
        return 1

    # Optional second argument: a Markdown file claiming to show what the chart renders.
    if len(sys.argv) > 1:
        doc = documented_config(sys.argv[1])
        if doc is None:
            print(f"no ```json block in {sys.argv[1]}")
            return 1
        drift = documented_drift(doc, cfg)
        if drift:
            print(f"{sys.argv[1]} no longer matches what the chart renders:")
            for d in drift:
                print("  -", d)
            print('It says "This is what the chart renders", so anyone running their own '
                  "registry copies it.")
            return 1
        print(f"OK: {sys.argv[1]} agrees with the rendered config.")

    print("OK: the policy keys on pull recency; live content is tagged, and what nothing names is "
          "reclaimed after gcDelay.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
