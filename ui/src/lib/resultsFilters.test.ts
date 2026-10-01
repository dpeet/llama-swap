import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { normalizeResults } from "./results";
import {
  activeFilterCount,
  compareRatios,
  comparePoints,
  configHaystack,
  configsShown,
  emptyResultsFilters,
  measurementShown,
  normalizeResultsFilters,
  parseYours,
  resultsFiltersFromQuery,
  resultsFiltersToQuery,
  searchTokens,
  shownMeasurements,
  UNSERVED,
  type ResultsFilters,
} from "./resultsFilters";

const fixtureDir = fileURLToPath(new URL("./__fixtures__/results/", import.meta.url));
const data = normalizeResults(
  JSON.parse(readFileSync(fixtureDir + "catalog.json", "utf8")),
  readFileSync(fixtureDir + "measurements.jsonl", "utf8"),
);

function filters(fields: Partial<ResultsFilters> = {}): ResultsFilters {
  return { ...emptyResultsFilters(), ...fields };
}

const ids = (list: { id: string }[]) => list.map((c) => c.id);

describe("normalizeResultsFilters", () => {
  it("returns defaults for non-objects", () => {
    expect(normalizeResultsFilters(null)).toEqual(emptyResultsFilters());
    expect(normalizeResultsFilters("x")).toEqual(emptyResultsFilters());
  });

  it("keeps valid fields and drops invalid ones", () => {
    const got = normalizeResultsFilters({
      family: "27b",
      thinking: "sideways",
      comparability: "all",
      status: "bogus",
      compareMetric: "accept_len",
      showSuperseded: "yes",
      search: 3,
      yours: "150",
    });
    expect(got).toEqual(filters({ family: "27b", comparability: "all", yours: "150" }));
  });

  it("accepts the unserved status", () => {
    expect(normalizeResultsFilters({ status: UNSERVED }).status).toBe(UNSERVED);
  });
});

describe("querystring round trip", () => {
  it("writes nothing for the defaults", () => {
    expect(resultsFiltersToQuery(emptyResultsFilters())).toBe("");
    expect(resultsFiltersFromQuery(undefined)).toEqual(emptyResultsFilters());
  });

  it("round-trips every field", () => {
    const set = filters({
      family: "flash-next",
      host: "violet1",
      engine: "vllm",
      metric: "prefill_cold",
      thinking: "off",
      status: "production",
      comparability: "all",
      showSuperseded: true,
      search: "fp8 kv",
      compareMetric: "ttft_cold",
      yours: "0.4",
    });
    const query = resultsFiltersToQuery(set);
    expect(query).toBe(
      "family=flash-next&host=violet1&engine=vllm&metric=prefill_cold&thinking=off&status=production&harness=all&superseded=1&q=fp8+kv&compare=ttft_cold&yours=0.4",
    );
    expect(resultsFiltersFromQuery(query)).toEqual(set);
    expect(resultsFiltersFromQuery("?" + query)).toEqual(set);
  });

  it("drops blank text and ignores junk values", () => {
    expect(resultsFiltersToQuery(filters({ search: "   " }))).toBe("");
    expect(resultsFiltersFromQuery("thinking=maybe&compare=nope&superseded=0")).toEqual(emptyResultsFilters());
  });
});

describe("activeFilterCount", () => {
  it("counts table filters, not the compare strip's metric or number", () => {
    expect(activeFilterCount(emptyResultsFilters())).toBe(0);
    expect(activeFilterCount(filters({ compareMetric: "ttft_cold", yours: "3" }))).toBe(0);
    expect(activeFilterCount(filters({ family: "27b", comparability: "all", search: " x " }))).toBe(3);
  });
});

describe("search", () => {
  it("tokenizes on whitespace, lower-cased and de-duplicated", () => {
    expect(searchTokens("  FP8  kv fp8 ")).toEqual(["fp8", "kv"]);
  });

  it("matches flags, matrix id, checkpoint, lane label and caveat text, all tokens required", () => {
    const shown = (q: string) => ids(configsShown(data, filters({ search: q })));
    expect(shown("--max-num-batched-tokens")).toContain("V1-09b");
    expect(shown("v1-09b")).toEqual(["V1-09b"]);
    expect(shown("awq-w4a16")).toContain("V1-09b");
    expect(shown("previous production")).toEqual(["dgx-vllm-fn@8a72"]);
    // "first-batch warmup" is caveat text on V1-09b's 4-stream measurement.
    expect(shown("first-batch warmup")).toContain("V1-09b");
    expect(shown("v1-09b dgx-sglang")).toEqual([]);
  });

  it("includes the build key and notes", () => {
    const text = configHaystack(data, data.configs["dgx-vllm-fn@v030-arma"]);
    expect(text).toContain("vllm-0.30-arma");
    expect(text).toContain("dgx-vllm-fn");
  });
});

describe("filters", () => {
  it("shows only fixed-harness measurements by default", () => {
    const accept = data.measurements.find((m) => m.metric === "accept_len")!;
    expect(measurementShown(accept, filters())).toBe(false);
    expect(measurementShown(accept, filters({ comparability: "all" }))).toBe(true);
  });

  it("filters thinking and hides superseded measurements unless asked", () => {
    const off = data.measurements.find((m) => m.thinking_measured === false && m.harness === "fixed-2026-09-27")!;
    expect(measurementShown(off, filters({ thinking: "on" }))).toBe(false);
    expect(measurementShown(off, filters({ thinking: "off" }))).toBe(true);
    const superseded = { ...off, superseded_by: "x/decode_short" };
    expect(measurementShown(superseded, filters())).toBe(false);
    expect(measurementShown(superseded, filters({ showSuperseded: true }))).toBe(true);
  });

  it("filters by family, host, engine and lane status", () => {
    expect(ids(configsShown(data, filters({ host: "violet1" })))).toEqual(["V1-09b"]);
    expect(configsShown(data, filters({ family: "27b" })).every((c) => c.family === "27b")).toBe(true);
    expect(configsShown(data, filters({ engine: "llama.cpp" })).every((c) => c.engine === "llama.cpp")).toBe(true);
    expect(ids(configsShown(data, filters({ status: "rollback" })))).toEqual(["dgx-vllm-fn@8a72"]);
    // V1-09b is served by an RG lane; a config no lane names is unserved.
    expect(ids(configsShown(data, filters({ status: UNSERVED })))).not.toContain("dgx-vllm-fn@8a72");
  });

  it("keeps only configs with a shown measurement of the metric filter", () => {
    const got = ids(configsShown(data, filters({ metric: "prefill_cold" })));
    expect(got).toContain("V1-09b");
    expect(got).not.toContain("dgx-sglang-gpt-oss-120b@unrecorded");
  });

  it("sorts each config's shown measurements by date then id", () => {
    for (const list of shownMeasurements(data, filters({ comparability: "all" })).values()) {
      const keys = list.map((m) => `${m.date ?? ""} ${m.id}`);
      expect(keys).toEqual([...keys].sort((a, b) => a.localeCompare(b)));
    }
  });
});

describe("compare strip", () => {
  it("plots one point per config, flags DGX production, and sorts by value", () => {
    const points = comparePoints(data, filters({ family: "flash-next" }));
    expect(points.length).toBeGreaterThanOrEqual(3);
    expect(points.map((p) => p.value)).toEqual([...points.map((p) => p.value)].sort((a, b) => a - b));
    const production = points.filter((p) => p.dgxProduction).map((p) => p.config.id);
    expect(production).toEqual(["dgx-vllm-fn@v030-arma"]);
    expect(points.find((p) => p.config.id === "V1-09b")?.site).toBe("rg");
  });

  it("labels yours against DGX production and the nearest RG config", () => {
    const points = comparePoints(data, filters({ family: "flash-next" }));
    const ratios = compareRatios(points, 150);
    expect(ratios.map((r) => [r.kind, r.point.config.id])).toEqual([
      ["dgx-production", "dgx-vllm-fn@v030-arma"],
      ["nearest-rg", "V1-09b"],
    ]);
    expect(ratios[0].ratio).toBeCloseTo(150 / 32.16, 5);
    expect(ratios[1].ratio).toBeCloseTo(150 / 95.235, 5);
    expect(compareRatios(points, null)).toEqual([]);
  });

  it("parses the typed number leniently", () => {
    expect(parseYours("1,234")).toBe(1234);
    expect(parseYours(" ~95 ")).toBe(95);
    expect(parseYours("")).toBeNull();
    expect(parseYours("-3")).toBeNull();
    expect(parseYours("abc")).toBeNull();
  });
});
