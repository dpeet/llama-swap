# /// script
# requires-python = ">=3.12"
# ///
"""Regenerate this directory from the live results data: `uv run --script regen.py [/opt/ai/artisanal-inference]`.

Copies docs/results/catalog.json and measurements.jsonl, then writes expected-format.json: render.py's
format_value for (a) every view metric cell (each metric/parts column x each measurement a row selects
for it, with the column's display spec) and (b) every measurement with no spec (view null), which is
what the table, cards and dialog show. The TS test asserts formatMeasurement matches all of them.
"""

import json
import shutil
import sys
from pathlib import Path

repo = Path(sys.argv[1] if len(sys.argv) > 1 else "/opt/ai/artisanal-inference")
here = Path(__file__).resolve().parent
sys.path.insert(0, str(repo / "serve" / "results"))
sys.dont_write_bytecode = True  # because a __pycache__ in the repo or fixture dir is litter

from resultslib import data as d  # noqa: E402
from resultslib import render as r  # noqa: E402

paths = d.Paths(repo)
for src in (paths.catalog, paths.measurements):
    shutil.copyfile(src, here / src.name)

data = d.load(paths)
renderer = r.Renderer(data)
entries: list[dict] = []
seen: set[tuple[str, str, str]] = set()


def add_cells(view: str, col: dict, metric: str, where: dict | None, spec_row, superseded: bool) -> None:
    spec = {k: col[k] for k in ("aggregate", "precision", "approx_prefix") if k in col}
    for rec in renderer.row_select(spec_row, metric, where, superseded):
        # the page applies the record's own display[view] override itself, as metric_cell does here
        merged = {**spec, **rec.get("display", {}).get(view, {})}
        key = (view, col["header"], rec["id"])
        if key in seen:
            continue
        seen.add(key)
        entries.append(
            {"view": view, "header": col["header"], "measurement": rec["id"],
             "expected": r.format_value(rec, merged)}
        )


for name, view in data.catalog["views"].items():
    cols = view.get("columns")
    if view["kind"] == "lane-cards" or not cols or "section" in view["rows"]:
        continue
    superseded = bool(view.get("include_superseded"))
    for spec_row in renderer.row_specs(name, view):
        if spec_row.heading is not None:
            continue
        for col in cols:
            if "metric" in col:
                add_cells(name, col, col["metric"], col.get("where"), spec_row, superseded)
            for part in col.get("parts", []):
                add_cells(name, col, part["metric"], part.get("where"), spec_row, superseded)

view_cells = len(entries)
for _, rec in data.measurements:
    entries.append({"view": None, "header": None, "measurement": rec["id"], "expected": r.format_value(rec, {})})

(here / "expected-format.json").write_text(json.dumps(entries, indent=1, ensure_ascii=False) + "\n")
print(f"{view_cells} view cells, {len(entries) - view_cells} default cells")
