#!/usr/bin/env python3
"""Regression tests for scripts/openapi-normalize.py.

Run: python3 scripts/openapi_normalize_test.py
"""

import importlib.util
import pathlib
import unittest
from typing import Any

_MOD_PATH = pathlib.Path(__file__).with_name("openapi-normalize.py")
_spec = importlib.util.spec_from_file_location("openapi_normalize", _MOD_PATH)
assert _spec and _spec.loader
norm = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(norm)


def find_null_branches(node: Any) -> list[Any]:
    """Return every residual JSON-null construct: a bare/array null type, or a
    null branch left inside an anyOf/oneOf union."""
    out: list[Any] = []
    if isinstance(node, dict):
        t = node.get("type")
        if t == "null" or (isinstance(t, list) and "null" in t):
            out.append(node)
        for key in ("anyOf", "oneOf"):
            arr = node.get(key)
            if isinstance(arr, list):
                out.extend(b for b in arr if norm.is_null_branch(b))
        for v in node.values():
            out.extend(find_null_branches(v))
    elif isinstance(node, list):
        for item in node:
            out.extend(find_null_branches(item))
    return out


def collapse(d: dict) -> dict:
    norm.collapse_nullable(d)
    return d


class TestCollapseNullable(unittest.TestCase):
    def test_scalar_nullable_union(self):
        d = collapse({"anyOf": [{"type": "string"}, {"type": "null"}]})
        self.assertEqual(d.get("type"), "string")
        self.assertTrue(d.get("nullable"))
        self.assertNotIn("anyOf", d)
        self.assertEqual(find_null_branches(d), [])

    def test_ref_nullable_union(self):
        d = collapse({"anyOf": [{"$ref": "#/components/schemas/Foo"}, {"type": "null"}]})
        self.assertTrue(d.get("nullable"))
        self.assertEqual(d.get("allOf"), [{"$ref": "#/components/schemas/Foo"}])
        self.assertNotIn("anyOf", d)
        self.assertEqual(find_null_branches(d), [])

    def test_type_array_with_null(self):
        d = collapse({"type": ["string", "null"]})
        self.assertEqual(d.get("type"), "string")
        self.assertTrue(d.get("nullable"))
        self.assertEqual(find_null_branches(d), [])

    def test_bare_null_type(self):
        d = collapse({"type": "null"})
        self.assertNotIn("type", d)
        self.assertTrue(d.get("nullable"))
        self.assertEqual(find_null_branches(d), [])

    def test_multi_branch_union_keeps_non_null(self):
        d = collapse({"anyOf": [{"type": "string"}, {"type": "integer"}, {"type": "null"}]})
        self.assertTrue(d.get("nullable"))
        self.assertEqual(d.get("anyOf"), [{"type": "string"}, {"type": "integer"}])
        self.assertEqual(find_null_branches(d), [])

    def test_nested_union_in_property(self):
        # The regression Caliper hit: a null branch nested under properties must
        # also be collapsed (top-down walk reaches it).
        d = collapse(
            {
                "type": "object",
                "properties": {"x": {"anyOf": [{"type": "integer"}, {"type": "null"}]}},
            }
        )
        self.assertEqual(find_null_branches(d), [])
        self.assertTrue(d["properties"]["x"].get("nullable"))


class TestFixScalars(unittest.TestCase):
    def test_exclusive_minimum_number_to_bool(self):
        d = {"type": "integer", "exclusiveMinimum": 0}
        norm.fix_scalars(d)
        self.assertEqual(d["minimum"], 0)
        self.assertIs(d["exclusiveMinimum"], True)

    def test_exclusive_minimum_survives_union_collapse(self):
        # Numeric exclusiveMinimum inside a collapsed union branch must still be
        # fixed by the scalar pass that runs after collapse.
        d = {"anyOf": [{"type": "integer", "exclusiveMinimum": 5}, {"type": "null"}]}
        norm.collapse_nullable(d)
        norm.fix_scalars(d)
        self.assertEqual(d.get("minimum"), 5)
        self.assertIs(d.get("exclusiveMinimum"), True)
        self.assertTrue(d.get("nullable"))

    def test_strips_content_media_type(self):
        d = {"type": "string", "format": "byte", "contentMediaType": "application/octet-stream"}
        norm.fix_scalars(d)
        self.assertNotIn("contentMediaType", d)


class TestNormalizeSpec(unittest.TestCase):
    def _spec(self) -> dict:
        return {
            "openapi": "3.1.0",
            "paths": {
                "/v1/orgs/{org_id}/bots/{slug}": {
                    "get": {
                        "tags": ["bots"],
                        "responses": {
                            "200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Bot"}}}},
                            "default": {
                                "content": {
                                    "application/json": {"schema": {"$ref": "#/components/schemas/ProblemDetails"}},
                                    "application/problem+json": {"schema": {"$ref": "#/components/schemas/ProblemDetails"}},
                                }
                            },
                        },
                    }
                },
                "/v1/chat/stream": {"get": {"tags": ["chat"], "responses": {"200": {"description": "ok"}}}},
            },
            "components": {
                "schemas": {
                    "Bot": {
                        "type": "object",
                        "properties": {"owner": {"anyOf": [{"type": "string"}, {"type": "null"}]}},
                    },
                    "ProblemDetails": {"type": "object"},
                    "Unused": {"type": "object"},
                },
            },
        }

    def test_exclude_paths(self):
        spec = self._spec()
        # Add a second bots-tagged path we want to drop despite the kept tag.
        spec["paths"]["/v1/orgs/{org_id}/bots/{slug}/catalog"] = {
            "get": {"tags": ["bots"], "responses": {"200": {"description": "ok"}}}
        }
        out = norm.normalize_spec(spec, {"bots"}, ("/v1/orgs/{org_id}/bots/{slug}/catalog",))
        assert "/v1/orgs/{org_id}/bots/{slug}/catalog" not in out["paths"]
        assert "/v1/orgs/{org_id}/bots/{slug}" in out["paths"]  # exact match: sibling kept

    def test_exclude_operations(self):
        spec = self._spec()
        # Add a sibling write operation on the kept path that we want to drop
        # while keeping the path's read operation.
        spec["paths"]["/v1/orgs/{org_id}/bots/{slug}"]["post"] = {
            "tags": ["bots"],
            "operationId": "create_bot",
            "responses": {"200": {"description": "ok"}},
        }
        spec["paths"]["/v1/orgs/{org_id}/bots/{slug}"]["get"]["operationId"] = "get_bot"
        out = norm.normalize_spec(spec, {"bots"}, (), ("create_bot",))
        path = out["paths"]["/v1/orgs/{org_id}/bots/{slug}"]
        assert "get" in path  # read operation kept
        assert "post" not in path  # excluded write operation dropped

    def test_full_normalization(self):
        spec = norm.normalize_spec(self._spec(), {"bots"})
        # No residual null branches anywhere.
        self.assertEqual(find_null_branches(spec), [])
        # Only the bots-tagged path survives.
        self.assertEqual(list(spec["paths"].keys()), ["/v1/orgs/{org_id}/bots/{slug}"])
        # default.application/json dropped; application/problem+json retained.
        default = spec["paths"]["/v1/orgs/{org_id}/bots/{slug}"]["get"]["responses"]["default"]
        self.assertNotIn("application/json", default["content"])
        self.assertIn("application/problem+json", default["content"])
        # Unreferenced schema pruned; referenced ones kept.
        schemas = spec["components"]["schemas"]
        self.assertIn("Bot", schemas)
        self.assertIn("ProblemDetails", schemas)
        self.assertNotIn("Unused", schemas)


if __name__ == "__main__":
    unittest.main()
