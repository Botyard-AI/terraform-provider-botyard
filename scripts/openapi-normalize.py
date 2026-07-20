#!/usr/bin/env python3
"""Normalize the Botyard public OpenAPI spec for Go client generation.

The Botyard API emits OpenAPI 3.1 (FastAPI). oapi-codegen's loader
(kin-openapi) only supports 3.0, and generating a client for the *entire* API
pulls in unrelated schemas (chat, inbox, ...) that trigger oapi-codegen bugs.

Pipeline (in order):

1. ``collapse_nullable`` (top-down): rewrite 3.1 nullability to 3.0
   ``nullable: true``. This MUST run top-down: a parent ``anyOf: [X, {type:
   null}]`` is detected and collapsed while its ``{type: null}`` branch is still
   intact, before recursion could rewrite that branch into ``{nullable: true}``
   and hide it from the parent detector. (A bottom-up walk left the null branch
   in the union, producing oapi-codegen union wrappers with ``interface{}`` null
   branches.)
2. ``fix_scalars`` (any order, run after collapse): 3.1 numeric
   ``exclusiveMinimum``/``exclusiveMaximum`` -> 3.0 ``{bound, boolean}``, and
   strip 3.1-only ``contentMediaType``/``contentEncoding``. Running after the
   collapse ensures a numeric ``exclusiveMinimum`` merged up out of a collapsed
   union branch is still fixed.
3. ``prune``: keep only operations whose tag set intersects ``--keep-tags`` and
   prune ``components`` to the transitively-reachable schemas. Also drop the
   catch-all ``default`` response's ``application/json`` entry (see below).

(A final ``openapi-down-convert`` pass in generate-client.sh handles the version
bump and any remaining 3.0 cleanups.)

Usage:
    openapi-normalize.py <input.json> <output.json> --keep-tags bots skills ...
"""

from __future__ import annotations

import argparse
import json
from typing import Any

HTTP_METHODS = {"get", "put", "post", "delete", "patch", "head", "options", "trace"}
DROP_KEYS = {"contentMediaType", "contentEncoding"}


def is_null_branch(b: Any) -> bool:
    """True if b is a schema whose only type is JSON null (a 3.1 nullable
    marker inside a union), e.g. ``{"type": "null"}`` or ``{"type": ["null"]}``."""
    if not isinstance(b, dict):
        return False
    t = b.get("type")
    return t == "null" or (isinstance(t, list) and set(t) == {"null"})


def collapse_nullable(node: Any) -> None:
    """Top-down: resolve this node's nullability, then recurse into children."""
    if isinstance(node, dict):
        _collapse_here(node)
        for v in node.values():
            collapse_nullable(v)
    elif isinstance(node, list):
        for item in node:
            collapse_nullable(item)


def _collapse_here(d: dict[str, Any]) -> None:
    # type as a list (possibly containing "null"), or a bare "null" type.
    t = d.get("type")
    if isinstance(t, list):
        non_null = [x for x in t if x != "null"]
        if "null" in t:
            d["nullable"] = True
        if non_null:
            d["type"] = non_null[0]  # 3.0 has no multi-type; keep the first
        else:
            d.pop("type", None)
    elif t == "null":
        d.pop("type", None)
        d["nullable"] = True

    # anyOf/oneOf unions containing a null branch -> nullable + collapse.
    for key in ("anyOf", "oneOf"):
        arr = d.get(key)
        if not isinstance(arr, list):
            continue
        non_null = [b for b in arr if not is_null_branch(b)]
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


def fix_scalars(node: Any) -> None:
    """Fix 3.1 numeric exclusive bounds and strip 3.1-only annotations."""
    if isinstance(node, dict):
        for bound, excl in (("minimum", "exclusiveMinimum"), ("maximum", "exclusiveMaximum")):
            v = node.get(excl)
            if isinstance(v, (int, float)) and not isinstance(v, bool):
                node[bound] = v
                node[excl] = True
        for k in list(node.keys()):
            if k in DROP_KEYS:
                del node[k]
            else:
                fix_scalars(node[k])
    elif isinstance(node, list):
        for item in node:
            fix_scalars(item)


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


def _strip_default_json(op: dict[str, Any]) -> None:
    """Drop only ``default.content['application/json']``.

    oapi-codegen emits the ``default`` response's ``application/json`` case as
    ``Content-Type == "application/json"`` with no status check, ordered before
    the 200 case — so it shadows every successful application/json 200 (JSON200
    stays nil). We keep ``application/problem+json`` (an exact-match case that
    does not shadow 200), preserving typed error bodies.
    """
    responses = op.get("responses")
    if not isinstance(responses, dict):
        return
    default = responses.get("default")
    if not isinstance(default, dict):
        return
    content = default.get("content")
    if isinstance(content, dict):
        content.pop("application/json", None)
        if not content:
            responses.pop("default", None)


def prune(spec: dict[str, Any], keep_tags: set[str], exclude_paths: tuple[str, ...] = ()) -> None:
    new_paths: dict[str, Any] = {}
    for path, item in spec.get("paths", {}).items():
        if any(frag in path for frag in exclude_paths):
            continue  # explicitly excluded (e.g. endpoints whose schemas trip codegen)
        kept = {m: op for m, op in item.items() if m in HTTP_METHODS and keep_tags & set(op.get("tags", []))}
        if not kept:
            continue
        for op in kept.values():
            if isinstance(op, dict):
                _strip_default_json(op)
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
            continue
        keep = reachable.get(ctype, set())
        components[ctype] = {n: v for n, v in components[ctype].items() if n in keep}
    spec["components"] = components


def normalize_spec(
    spec: dict[str, Any], keep_tags: set[str], exclude_paths: tuple[str, ...] = ()
) -> dict[str, Any]:
    """Full in-place normalization; returns the same spec for convenience."""
    collapse_nullable(spec)
    fix_scalars(spec)
    prune(spec, keep_tags, exclude_paths)
    return spec


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("src")
    ap.add_argument("dst")
    ap.add_argument("--keep-tags", nargs="+", required=True)
    ap.add_argument(
        "--exclude-paths",
        nargs="*",
        default=[],
        help="Path substrings to drop even when tag-matched (schemas that trip codegen).",
    )
    args = ap.parse_args()

    spec = json.load(open(args.src))
    normalize_spec(spec, set(args.keep_tags), tuple(args.exclude_paths))
    json.dump(spec, open(args.dst, "w"))
    n_schemas = len(spec.get("components", {}).get("schemas", {}))
    print(f"normalized+pruned -> {args.dst} ({len(spec['paths'])} paths, {n_schemas} schemas)")


if __name__ == "__main__":
    main()
