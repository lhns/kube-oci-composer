#!/usr/bin/env python3
"""Assert the chart's bundled retention policy actually protects something.

Every one of these four settings fails by silently protecting NOTHING rather than by erroring, which
is how they were found: seven e2e runs that read like the registry was broken, when the policy simply
matched nothing. A rendered config that looks plausible is not evidence. See ADR 0031.

Reads `helm template` output on stdin.
"""

import io
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


def policy_of(cfg):
    return cfg["storage"]["retention"]["policies"][0]


def problems(cfg):
    policy = policy_of(cfg)
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
    # ONE entry carrying both rules. Rules within an entry are OR'ed, but zot stops at the first
    # entry whose patterns match, so a second entry also matching `.*` is dead configuration that
    # reads as protection -- which is exactly how `pushedWithin` came to be inert. ADR 0057.
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

    # keepUntagged is optional since ADR 0060 -- the controllers name everything they publish after
    # its own digest, so nothing live is untagged -- and configuring it is what pins every retired
    # manifest forever. What each shape still has to get right:
    keep_untagged = policy.get("keepUntagged")
    if keep_untagged is not None:
        # Configured: it has to protect what it claims to. Pull recency alone matches nothing for a
        # manifest that was just pushed and never pulled.
        if not keep_untagged.get("pulledWithin"):
            found.append("keepUntagged does not key on pulledWithin, so digest-pinned images are "
                         "unprotected")
        if not keep_untagged.get("pushedWithin"):
            found.append("keepUntagged does not key on pushedWithin, so freshly pushed untagged "
                         "content matches no rule")
    elif not cfg["storage"].get("gcDelay"):
        # Not configured: gcDelay is then the ONLY cover for a build's output between being pushed
        # and being named (ADR 0054). zot's default is shorter than that gap under load.
        found.append("keepUntagged is off and gcDelay is not set, so nothing covers a build's "
                     "output between its push and the controller naming it")

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

    A SUBSET match, deliberately: the page is abridged -- readTimeout, TLS and auth have sections of
    their own -- so omitting a key is fine and contradicting one is not. It published the dead
    two-entry keepTags shape for a while under the words "This is what the chart renders", which an
    operator running their own zot would have copied.
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
        print("the bundled retention policy would protect nothing:")
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

    if policy_of(cfg).get("keepUntagged") is None:
        print("OK: the policy keys on pull recency; live content is tagged, and untagged content is "
              "reclaimed after gcDelay.")
    else:
        print("OK: the policy keys on pull recency and covers both tags and digests.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
