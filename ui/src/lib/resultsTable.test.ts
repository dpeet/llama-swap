import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { normalizeResults } from "./results";
import { configsShown, emptyResultsFilters, shownMeasurements } from "./resultsFilters";
import {
  buildRows,
  caveatMarks,
  cellEntries,
  DEFAULT_VISIBLE_COLUMNS,
  firstSortDir,
  metricColumn,
  metricColumns,
  nextSort,
  normalizeVisibleColumns,
  qualifierOf,
  sortRows,
  TEXT_COLUMNS,
} from "./resultsTable";

const fixtureDir = fileURLToPath(new URL("./__fixtures__/results/", import.meta.url));
const data = normalizeResults(
  JSON.parse(readFileSync(fixtureDir + "catalog.json", "utf8")),
  readFileSync(fixtureDir + "measurements.jsonl", "utf8"),
);

function rows(fields = {}) {
  const filters = { ...emptyResultsFilters(), ...fields };
  const shown = shownMeasurements(data, filters);
  return buildRows(data, configsShown(data, filters, shown), shown);
}

describe("columns", () => {
  it("lists the data's metrics in schema order", () => {
    const ids = metricColumns(data).map((c) => c.id);
    expect(ids.slice(0, 3)).toEqual(["decode_short", "decode_balanced", "decode_4stream"]);
    expect(ids).toContain("kv_pool_tokens");
  });

  it("labels an unknown metric by its id", () => {
    expect(metricColumn("new_metric")).toMatchObject({ label: "new_metric", unit: "" });
  });

  it("restores the persisted list or falls back to the defaults", () => {
    expect(normalizeVisibleColumns(["host"])).toEqual(["host"]);
    expect(normalizeVisibleColumns("host")).toEqual(DEFAULT_VISIBLE_COLUMNS);
    expect(normalizeVisibleColumns([1])).toEqual(DEFAULT_VISIBLE_COLUMNS);
  });
});

describe("rows", () => {
  it("formats cells as ordered runs at the recorded precision", () => {
    const v1 = rows().find((r) => r.config.id === "V1-09b")!;
    expect(cellEntries(v1, "ttft_cold").map((e) => e.text)).toEqual(["0.331 / 0.330"]);
    expect(v1.harnesses).toEqual(["fixed"]);
    expect(v1.lanes.map((l) => l.id)).toEqual(["rg-flash-next-h100"]);
  });

  it("names a measurement's qualifier from its id", () => {
    const m = data.measurements.find((r) => r.id === "dgx-sglang-27b@0928/accept_len/thinking-on")!;
    expect(qualifierOf(m)).toBe("thinking-on");
    expect(qualifierOf(data.measurements[0])).toBeNull();
  });

  it("labels an unqualified measurement by its date when it shares a cell", () => {
    const unqualified = { ...data.measurements[0], date: "2026-09-28" };
    expect(qualifierOf(unqualified, true)).toBe("2026-09-28");
    expect(qualifierOf({ ...unqualified, date: undefined }, true)).toBeNull();
  });

  it("shows caveat marks, and the raw id for an unknown caveat", () => {
    const m = { ...data.measurements[0], caveats: ["rg-on-node", "nope"] };
    expect(caveatMarks(data, m).map((c) => c.mark)).toEqual(["ʰ", "[nope]"]);
  });
});

describe("sorting", () => {
  it("sorts a metric best first and keeps rows without a value last", () => {
    const sorted = sortRows(rows(), { key: "decode_short", dir: "desc" }, metricColumns(data));
    const values = sorted.map((r) => r.byMetric.get("decode_short")?.[0]?.runs?.[0] ?? null);
    const firstNull = values.indexOf(null);
    const numbers = values.slice(0, firstNull === -1 ? undefined : firstNull) as number[];
    expect(numbers).toEqual([...numbers].sort((a, b) => b - a));
    if (firstNull !== -1) expect(values.slice(firstNull).every((v) => v === null)).toBe(true);
    expect(sorted[0].config.id).toBe("V1-09b");
  });

  it("puts unmeasured rows last even in catalog order", () => {
    const sorted = sortRows(rows(), { key: "", dir: "asc" }, []);
    const firstUnmeasured = sorted.findIndex((r) => !r.measured);
    expect(firstUnmeasured).toBeGreaterThan(0);
    expect(sorted.slice(firstUnmeasured).every((r) => !r.measured)).toBe(true);
  });

  it("cycles first direction → reversed → catalog order, TTFT starting low", () => {
    const ttft = metricColumn("ttft_cold");
    expect(firstSortDir(ttft)).toBe("asc");
    expect(firstSortDir(metricColumn("decode_short"))).toBe("desc");
    expect(firstSortDir(TEXT_COLUMNS[0])).toBe("asc");
    let sort = nextSort({ key: "", dir: "asc" }, "ttft_cold", "asc");
    expect(sort).toEqual({ key: "ttft_cold", dir: "asc" });
    sort = nextSort(sort, "ttft_cold", "asc");
    expect(sort).toEqual({ key: "ttft_cold", dir: "desc" });
    expect(nextSort(sort, "ttft_cold", "asc")).toEqual({ key: "", dir: "asc" });
  });
});
