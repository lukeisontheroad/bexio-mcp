#!/usr/bin/env python3
"""Refresh openapi.json from https://docs.bexio.com/.

The docs page is pre-rendered Redoc with the full OpenAPI spec embedded in
`__redoc_state`. Code samples, examples and response schemas are dropped to
keep the embedded file small, as are path items bexio hides from the public
docs (they come through empty).
"""
import json
import urllib.request

html = urllib.request.urlopen("https://docs.bexio.com/").read().decode()
marker = "__redoc_state = "
state, _ = json.JSONDecoder().raw_decode(html, html.index(marker) + len(marker))
spec = state["spec"]["data"]

DROP = {"x-codeSamples", "x-alias", "example", "examples", "responses"}


def strip(node, in_properties=False):
    if isinstance(node, dict):
        return {k: strip(v, k == "properties") for k, v in node.items() if in_properties or k not in DROP}
    if isinstance(node, list):
        return [strip(v) for v in node]
    return node


methods = {"get", "post", "put", "patch", "delete"}
spec["paths"] = {p: v for p, v in spec["paths"].items() if methods & v.keys()}
with open("openapi.json", "w") as f:
    json.dump(strip(spec), f, separators=(",", ":"))
