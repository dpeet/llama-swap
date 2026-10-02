import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import {
  aggregateNumber,
  byCodepoint,
  formatMeasurement,
  formatNumber,
  formatRatio,
  LANE_STATUSES,
  lanesForConfig,
  METRICS,
  nearestByRatio,
  normalizeResults,
  predecessorsOf,
  ratio,
  type Measurement,
} from "./results";

// The fixture is a copy of artisanal-inference docs/results/ (catalog.json and
// measurements.jsonl at 2f4e8d4). expected-format.json holds render.py's
// format_value output for every view metric column × matching measurement of
// those files, so the page and RESULTS.md can't disagree on a number.
const fixtureDir = fileURLToPath(new URL("./__fixtures__/results/", import.meta.url));
const catalog: unknown = JSON.parse(readFileSync(fixtureDir + "catalog.json", "utf8"));
const jsonl = readFileSync(fixtureDir + "measurements.jsonl", "utf8");
const expectedFormat = JSON.parse(readFileSync(fixtureDir + "expected-format.json", "utf8")) as {
  view: string;
  header: string;
  measurement: string;
  expected: string;
}[];

function minimalCatalog(extra: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    version: 1,
    hosts: { h: { label: "Host", site: "dgx", gpu: "GB10" } },
    builds: {},
    checkpoints: { ck: { name: "Checkpoint" } },
    configs: {
      c: {
        label: "Config",
        model: "M",
        family: "flash-next",
        host: "h",
        build: null,
        engine: "vllm",
        checkpoint: "ck",
        spec: { method: "none", label: "none" },
      },
    },
    lanes: {},
    caveats: {},
    campaigns: {},
    views: {},
    refs: {},
    ...extra,
  };
}

function line(record: Record<string, unknown>): string {
  return JSON.stringify({ id: "c/decode_short", config: "c", metric: "decode_short", unit: "tok/s", runs: [1], ...record });
}

function measurement(fields: Partial<Measurement>): Measurement {
  return {
    id: "c/decode_short",
    config: "c",
    metric: "decode_short",
    unit: "tok/s",
    approx: false,
    conditions: {},
    caveats: [],
    evidence: [],
    display: {},
    ...fields,
  };
}

describe("normalizeResults on the real files", () => {
  const data = normalizeResults(catalog, jsonl);

  it("loads every record without rejections", () => {
    expect(data.rejected).toEqual([]);
    const lineCount = jsonl.split("\n").filter((l) => l.trim() !== "").length;
    expect(data.measurements).toHaveLength(lineCount);
    expect(Object.keys(data.configs).length).toBeGreaterThan(5);
    expect(Object.keys(data.lanes).length).toBeGreaterThan(5);
  });

  it("keeps a null build and the lane's measured_config", () => {
    expect(data.configs["dgx-sglang-27b-bf16@unrecorded"].build).toBeNull();
    expect(data.lanes["dgx-vllm-fn"].measured_config).toBe("dgx-vllm-fn@v030-arma");
    expect(lanesForConfig(data, "dgx-vllm-fn@v030-arma").map((l) => l.id)).toContain("dgx-vllm-fn");
  });

  it("formats every view metric cell exactly as render.py does", () => {
    expect(expectedFormat.length).toBeGreaterThan(20);
    for (const want of expectedFormat) {
      const column = data.views[want.view].columns.find((c) => c.header === want.header);
      const m = data.measurements.find((r) => r.id === want.measurement);
      expect(column, want.header).toBeDefined();
      expect(m, want.measurement).toBeDefined();
      expect(formatMeasurement(m!, column, want.view), `${want.view} / ${want.header} / ${want.measurement}`).toBe(
        want.expected,
      );
    }
  });
});

describe("normalizeResults rejections", () => {
  it("rejects a catalog that is not an object", () => {
    const data = normalizeResults([], "");
    expect(data.rejected).toEqual([{ id: "catalog.json", reason: "catalog: not a JSON object" }]);
    expect(data.configs).toEqual({});
  });

  it("rejects a section that is not an object", () => {
    const data = normalizeResults(minimalCatalog({ lanes: [] }), "");
    expect(data.rejected).toEqual([{ id: "lanes", reason: "catalog: lanes is not an object" }]);
  });

  it("rejects a record that is not an object, or misses a required field", () => {
    const cat = minimalCatalog({
      hosts: { h: { label: "Host", site: "dgx", gpu: "GB10" }, bad: "x", nogpu: { label: "L", site: "rg" } },
    });
    const data = normalizeResults(cat, "");
    expect(data.rejected).toEqual([
      { id: "bad", reason: "host: not an object" },
      { id: "nogpu", reason: "host: missing or invalid gpu" },
    ]);
    expect(Object.keys(data.hosts)).toEqual(["h"]);
  });

  it("rejects a config with a bad build or spec", () => {
    const base = (minimalCatalog().configs as Record<string, Record<string, unknown>>).c;
    const data = normalizeResults(
      minimalCatalog({ configs: { c: base, b: { ...base, build: 3 }, s: { ...base, spec: { method: "mtp" } } } }),
      "",
    );
    expect(data.rejected).toEqual([
      { id: "b", reason: "config: missing or invalid build" },
      { id: "s", reason: "config: missing or invalid spec.label" },
    ]);
  });

  it("rejects a lane naming an unknown config", () => {
    const lanes = {
      ok: { label: "L", host: "h", family: "flash-next", status: "production", current_config: "c" },
      gone: { label: "L", host: "h", family: "flash-next", status: "production", current_config: "nope" },
      gone2: { label: "L", host: "h", family: "flash-next", status: "production", current_config: "c", measured_config: "nope" },
    };
    const data = normalizeResults(minimalCatalog({ lanes }), "");
    expect(Object.keys(data.lanes)).toEqual(["ok"]);
    expect(data.rejected).toEqual([
      { id: "gone", reason: "lane: unknown current_config nope" },
      { id: "gone2", reason: "lane: unknown measured_config nope" },
    ]);
  });

  it("skips blank lines and rejects invalid JSON and non-objects by line", () => {
    const text = ["", line({}), "   ", "{not json", "[1,2]", ""].join("\n");
    const data = normalizeResults(minimalCatalog(), text);
    expect(data.measurements.map((m) => m.id)).toEqual(["c/decode_short"]);
    expect(data.rejected).toEqual([
      { id: "measurements.jsonl line 4", reason: "measurement: invalid JSON" },
      { id: "measurements.jsonl line 5", reason: "measurement: not an object" },
    ]);
  });

  it("keeps the first of a duplicate id", () => {
    const text = [line({ runs: [1] }), line({ runs: [2] })].join("\n");
    const data = normalizeResults(minimalCatalog(), text);
    expect(data.measurements.map((m) => m.runs)).toEqual([[1]]);
    expect(data.rejected).toEqual([
      { id: "c/decode_short", reason: "measurement: duplicate id (measurements.jsonl line 2); the first is kept" },
    ]);
  });

  it("rejects runs and range together or neither, and malformed values", () => {
    const text = [
      line({ id: "c/a", range: [1, 2] }),
      JSON.stringify({ id: "c/b", config: "c", metric: "decode_short", unit: "tok/s" }),
      line({ id: "c/c", runs: [] }),
      line({ id: "c/d", runs: [1, "2"] }),
      JSON.stringify({ id: "c/e", config: "c", metric: "decode_short", unit: "tok/s", range: [1, 2, 3] }),
    ].join("\n");
    const data = normalizeResults(minimalCatalog(), text);
    expect(data.measurements).toEqual([]);
    expect(data.rejected.map((r) => r.reason)).toEqual([
      "measurement: needs exactly one of runs and range",
      "measurement: needs exactly one of runs and range",
      "measurement: runs must be a non-empty list of numbers",
      "measurement: runs must be a non-empty list of numbers",
      "measurement: range must be [lo, hi]",
    ]);
  });

  it("rejects a measurement with no id, a missing unit, or an unknown config", () => {
    const text = [
      JSON.stringify({ config: "c", metric: "decode_short", unit: "tok/s", runs: [1] }),
      line({ id: "c/u", unit: undefined }),
      line({ id: "x/decode_short", config: "x" }),
    ].join("\n");
    const data = normalizeResults(minimalCatalog(), text);
    expect(data.rejected).toEqual([
      { id: "measurements.jsonl line 1", reason: "measurement: missing or invalid id" },
      { id: "c/u", reason: "measurement: missing or invalid unit" },
      { id: "x/decode_short", reason: "measurement: unknown config x" },
    ]);
  });

  it("keeps an unknown caveat id and drops a malformed optional field", () => {
    const text = line({ caveats: ["nope", 3], thinking_measured: "yes", precision: 1.5 });
    const data = normalizeResults(minimalCatalog(), text);
    expect(data.rejected).toEqual([]);
    const m = data.measurements[0];
    expect(m.caveats).toEqual(["nope"]);
    expect(m.thinking_measured).toBeUndefined();
    expect(m.precision).toBeUndefined();
  });

  it("treats a missing JSONL as an empty set", () => {
    expect(normalizeResults(minimalCatalog(), "").measurements).toEqual([]);
  });
});

describe("formatMeasurement (render.py format_value)", () => {
  // Expected strings produced by render.py format_value on the same inputs.
  const cases: [Partial<Measurement>, Record<string, unknown>, string][] = [
    [{ runs: [95.24, 95.23] }, { aggregate: "mean", precision: 2 }, "95.24"],
    [{ runs: [95.24, 95.23] }, { aggregate: "mean" }, "95.24"],
    [{ runs: [2169, 2174] }, { aggregate: "mean", precision: -1, approx_prefix: true }, "~2,170"],
    [{ runs: [11492, 11539] }, { aggregate: "range", precision: 0 }, "11,492–11,539"],
    [{ runs: [0.331, 0.33], precision: 3 }, {}, "0.331 / 0.330"],
    [{ runs: [114.8, 115.0], precision: 1 }, {}, "114.8 / 115.0"],
    [{ runs: [274.3, 308.6] }, { aggregate: "run:2" }, "308.6"],
    [{ runs: [274.3, 308.6] }, { aggregate: "min" }, "274.3"],
    [{ runs: [274.3, 308.6] }, { aggregate: "max" }, "308.6"],
    [{ runs: [5, 5] }, { aggregate: "range" }, "5"],
    [{ range: [478, 631] }, { aggregate: "mean" }, "478–631"],
    [{ runs: [569302, 540175, 603725] }, { aggregate: "mean", precision: -3 }, "571,000"],
    [{ runs: [0.125, 0.135] }, { aggregate: "mean", precision: 2 }, "0.13"],
    [{ runs: [2520.5, 2619.7] }, { precision: 0 }, "2,521 / 2,620"],
    [{ runs: [1.005] }, { precision: 2 }, "1.01"],
    [{ runs: [-1.25] }, { precision: 1 }, "-1.3"],
    [{ runs: [2.8], approx: true }, { precision: 2 }, "~2.80"],
  ];

  it.each(cases)("%j under %j → %s", (fields, spec, want) => {
    expect(formatMeasurement(measurement(fields), spec)).toBe(want);
  });

  it("applies a view's display override over the column spec", () => {
    const m = measurement({ runs: [244.0, 307.3], display: { "fastest-way": { aggregate: "run:2", approx_prefix: true } } });
    expect(formatMeasurement(m, { aggregate: "mean", precision: 0 }, "fastest-way")).toBe("~307");
    expect(formatMeasurement(m, { aggregate: "mean", precision: 0 }, "cross-host")).toBe("276");
  });

  it("returns a dash for a run past the last one", () => {
    expect(formatMeasurement(measurement({ runs: [1] }), { aggregate: "run:3" })).toBe("—");
  });

  it("formats plain numbers with separators", () => {
    expect(formatNumber(140700)).toBe("140,700");
    expect(formatNumber(1e21)).toBe("1,000,000,000,000,000,000,000");
    expect(formatNumber(0.000001)).toBe("0.000001");
  });
});

describe("aggregateNumber", () => {
  it("covers mean, a range midpoint, min, max and run:<n>", () => {
    expect(aggregateNumber(measurement({ runs: [1, 2, 6] }))).toBe(3);
    expect(aggregateNumber(measurement({ range: [478, 632] }))).toBe(555);
    expect(aggregateNumber(measurement({ runs: [3, 1] }), "min")).toBe(1);
    expect(aggregateNumber(measurement({ runs: [3, 1] }), "max")).toBe(3);
    expect(aggregateNumber(measurement({ runs: [3, 1] }), "run:2")).toBe(1);
    expect(aggregateNumber(measurement({ runs: [3, 1] }), "run:3")).toBeNull();
    expect(aggregateNumber(measurement({ range: [1, 2] }), "run:1")).toBeNull();
  });
});

describe("ratios", () => {
  it("divides yours by the reference and refuses a non-positive reference", () => {
    expect(ratio(220, 32)).toBeCloseTo(6.875);
    expect(ratio(1, 0)).toBeNull();
    expect(ratio(null, 3)).toBeNull();
  });

  it("formats to two significant figures", () => {
    expect(formatRatio(6.875)).toBe("6.9×");
    expect(formatRatio(0.7143)).toBe("0.71×");
    expect(formatRatio(12.4)).toBe("12×");
    expect(formatRatio(1)).toBe("1×");
  });

  it("finds the nearest value in ratio terms", () => {
    const values = [50, 190, 400];
    expect(nearestByRatio(values, (v) => v, 100)).toBe(190);
    expect(nearestByRatio(values, (v) => v, 80)).toBe(50);
    expect(nearestByRatio(values, (v) => v, 300)).toBe(400);
    expect(nearestByRatio(values, (v) => v, 0)).toBeNull();
    expect(nearestByRatio([], (v: number) => v, 10)).toBeNull();
  });
});

describe("predecessorsOf", () => {
  it("finds measurements superseded by the given ids", () => {
    const text = [line({ id: "c/new" }), line({ id: "c/old", superseded_by: "c/new" })].join("\n");
    const data = normalizeResults(minimalCatalog(), text);
    expect(predecessorsOf(data, new Set(["c/new"])).map((m) => m.id)).toEqual(["c/old"]);
  });
});

// The live schema, not a fixture copy, because the point is to catch the UI's
// lists falling behind the schema; skipped when docs/results isn't mounted.
const liveSchemaPath = "/opt/ai/artisanal-inference/docs/results/schema.json";

describe.skipIf(!existsSync(liveSchemaPath))("UI lists match schema.json", () => {
  const schema = JSON.parse(readFileSync(liveSchemaPath, "utf8")) as {
    $defs: {
      metric: { enum: string[]; "x-metrics": Record<string, { unit: string; label: string }> };
      lane: { properties: { status: { enum: string[] } } };
    };
  };

  it("knows every metric, with the schema's label and unit", () => {
    const { enum: metricEnum, "x-metrics": xMetrics } = schema.$defs.metric;
    for (const metric of new Set([...metricEnum, ...Object.keys(xMetrics)])) {
      expect(Object.hasOwn(METRICS, metric), `METRICS lacks ${metric}`).toBe(true);
      if (xMetrics[metric]) {
        expect(METRICS[metric].label, metric).toBe(xMetrics[metric].label);
        expect(METRICS[metric].unit, metric).toBe(xMetrics[metric].unit);
      }
    }
  });

  it("knows every lane status", () => {
    const known: readonly string[] = LANE_STATUSES;
    for (const status of schema.$defs.lane.properties.status.enum) expect(known, status).toContain(status);
  });
});

describe("normalizeResults with Object.prototype names as ids", () => {
  const base = minimalCatalog().configs as Record<string, Record<string, unknown>>;
  // JSON.parse, because an object literal's "__proto__" key sets the prototype instead of an own key.
  const catalog = JSON.parse(
    JSON.stringify({
      ...minimalCatalog(),
      configs: { constructor: { ...base.c, label: "Ctor" }, toString: { ...base.c, label: "ToString" } },
    }).replace('"constructor"', '"__proto__":' + JSON.stringify({ ...base.c, label: "Proto" }) + ',"constructor"'),
  );
  catalog.lanes = { l: { label: "L", host: "h", family: "flash-next", status: "production", current_config: "hasOwnProperty" } };
  const jsonl = [
    line({ id: "constructor/decode_short", config: "constructor" }),
    line({ id: "__proto__/decode_short", config: "__proto__" }),
    line({ id: "valueOf/decode_short", config: "valueOf" }),
  ].join("\n");
  const data = normalizeResults(catalog, jsonl);

  it("keeps such ids as plain records", () => {
    expect(Object.keys(data.configs).sort()).toEqual(["__proto__", "constructor", "toString"]);
    expect(data.configs["__proto__"].label).toBe("Proto");
    expect(data.configs["constructor"].label).toBe("Ctor");
    expect(data.measurements.map((m) => m.config)).toEqual(["constructor", "__proto__"]);
  });

  it("rejects references to inherited names that aren't records", () => {
    expect(data.rejected.map((r) => r.id)).toEqual(["l", "valueOf/decode_short"]);
    expect(data.hosts["constructor"]).toBeUndefined();
  });
});

describe("byCodepoint", () => {
  it("orders by codepoint as Python's sort does, not by locale", () => {
    expect(["b", "a", "B", "_x", "10", "9"].sort(byCodepoint)).toEqual(["10", "9", "B", "_x", "a", "b"]);
  });
});
