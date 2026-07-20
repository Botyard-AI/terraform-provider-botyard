#!/usr/bin/env python3
"""Normalize the Botyard public OpenAPI spec for Go client generation.

The Botyard API emits OpenAPI 3.1 (FastAPI). oapi-codegen's loader
(kin-openapi) only supports 3.0, and generating a client for the *entire* API
pulls in unrelated schemas (chat, inbox, ...) that trigger oapi-codegen bugs.

This script performs two jobs, in order:

1. 3.1 -> 3.0 friendly normalization:
   - exclusiveMinimum/Maximum numbers -> {bound + boolean} (3.0 form),
   - nullable unions (`anyOf: [X, {type: null}]`), `type: [..., "null"]`,
     and bare `type: "null"` -> `nullable: true`,
   - strip 3.1-only `contentMediaType`/`contentEncoding` annotations.
   (A final `openapi-down-convert` pass in generate-client.sh handles the
   version bump and any remaining 3.0 cleanups.)

2. Tag-scoped pruning: keep only operations whose tag set intersects
   ``--keep-tags`` and prune ``components`` to the schemas transitively
   reachable from those operations. This keeps the generated client small and
   avoids codegen bugs in unrelated parts of the API. Coverage grows by adding
   tags here as resources are implemented.

Usage:
    openapi-normalize.py <input.json> <output.json> --keep-tags bots skills ...
"""

from __future__ import annotations

import argparse
import json
from typing import Any

HTTP_METHODS = {"get", "put", "post", "delete", "patch", "head", "options", "trace"}
DROP_KEYS = {"contentMediaType", "contentEncoding"}


def resolve_node(d: dict[str, Any]) -> None:
    """Rewrite a single schema/parameter node from 3.1 to 3.0-friendly form."""
    for bound, excl in (("minimum", "exclusiveMinimum"), ("maximum", "exclusiveMaximum")):
        v = d.get(excl)
        if isinstance(v, (int, float)) and not isinstance(v, bool):
            d[bound] = v
            d[excl] = True

    t = d.get("type")
    if isinstance(t, list):
        non_null = [x for x in t if x != "null"]
        if "null" in t:
            d["nullable"] = True
        if non_null:
            d["type"] = non_null[0]
        else:
            d.pop("type", None)
    elif t == "null":
        d.pop("type", None)
        d["nullable"] = True

    for key in ("anyOf", "oneOf"):
        arr = d.get(key)
        if not isinstance(arr, list):
            continue
        non_null = [b for b in arr if not (isinstance(b, dict) and b.get("type") == "null")]
        if len(non_null) != len(arr):
            d["nullable"] = True
        if len(non_null) == 0:
            d.pop(key, None)
        elif len(non_null) == 1:
            only = non_null[0]
            d.pop(key, None)
            if set(only.keys()) == {"$ref"}:
                d["allOf"] = [only]  # 3.0 idiom for a nullable $ref
            else:
                for k, v in only.items():
                    d.setdefault(k, v)
        else:
            d[key] = non_null


def normalize(node: Any) -> None:
    """Bottom-up walk: resolve children before parents so collapsed unions
    inherit already-normalized child values."""
    if isinstance(node, dict):
        for k in list(node.keys()):
            if k in DROP_KEYS:
                del node[k]
            else:
                normalize(node[k])
        resolve_node(node)
    elif isinstance(node, list):
        for item in node:
            normalize(item)


def collect_refs(node: Any, acc: set[str]) -> None:
    if isinstance(node, dict):
        ref = node.get("$ref")
        if isinstance(ref, str):
            acc.add(ref)
        for v in node.values():
            collect_refs(v, acc)
    elif isinstance(node, list):
        for v in node:
            collect_refs(v, acc)


def prune(spec: dict[str, Any], keep_tags: set[str]) -> None:
    """Keep only operations tagged with one of keep_tags and prune components
    to the transitively-reachable set, removing dangling paths so the loader
    sees a self-consistent spec."""
    new_paths: dict[str, Any] = {}
    for path, item in spec.get("paths", {}).items():
        kept = {m: op for m, op in item.items() if m in HTTP_METHODS and keep_tags & set(op.get("tags", []))}
        if not kept:
            continue
        # Drop the catch-all `default` (ProblemDetails) response. oapi-codegen
        # emits its case as `Content-Type == "application/json"` with no status
        # check, ordered before the 200 case — so it shadows every successful
        # application/json 200 (JSON200 stays nil). The provider surfaces errors
        # from the HTTP status + raw body instead of a typed default response.
        for op in kept.values():
            if isinstance(op, dict):
                op.get("responses", {}).pop("default", None)
        for meta in ("parameters", "summary", "description", "servers"):
            if meta in item:
                kept[meta] = item[meta]
        new_paths[path] = kept
    spec["paths"] = new_paths

    components = spec.get("components", {})
    reachable: dict[str, set[str]] = {}
    seed: set[str] = set()
    collect_refs(new_paths, seed)

    worklist = list(seed)
    while worklist:
        ref = worklist.pop()
        parts = ref.lstrip("#/").split("/")  # components/<ctype>/<name>
        if len(parts) != 3 or parts[0] != "components":
            continue
        ctype, name = parts[1], parts[2]
        seen = reachable.setdefault(ctype, set())
        if name in seen:
            continue
        seen.add(name)
        target = components.get(ctype, {}).get(name)
        if target is not None:
            sub: set[str] = set()
            collect_refs(target, sub)
            worklist.extend(sub)

    for ctype in list(components.keys()):
        if ctype == "securitySchemes":
            continue  # keep auth definitions regardless of operation refs
        keep = reachable.get(ctype, set())
        components[ctype] = {n: v for n, v in components[ctype].items() if n in keep}
    spec["components"] = components


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("src")
    ap.add_argument("dst")
    ap.add_argument("--keep-tags", nargs="+", required=True)
    args = ap.parse_args()

    spec = json.load(open(args.src))
    normalize(spec)
    prune(spec, set(args.keep_tags))
    json.dump(spec, open(args.dst, "w"))
    n_schemas = len(spec.get("components", {}).get("schemas", {}))
    print(f"normalized+pruned -> {args.dst} ({len(spec['paths'])} paths, {n_schemas} schemas)")


if __name__ == "__main__":
    main()
