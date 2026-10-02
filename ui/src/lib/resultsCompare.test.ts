import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { normalizeResults } from "./results";
import { compareDomainMax, hostRows, niceMax, rowRatio, shortConfigName, shortGpu, type HostRow } from "./resultsCompare";

function load(dir: string) {
  const fixtureDir = fileURLToPath(new URL(dir, import.meta.url));
  return normalizeResults(
    JSON.parse(readFileSync(fixtureDir + "catalog.json", "utf8")),
    readFileSync(fixtureDir + "measurements.jsonl", "utf8"),
  );
}

// Logic tests read the stable trial fixture: DGX GB10 plus one RG host (violet1).
const trial = load("./__fixtures__/results/trial/");

const round = (n: number) => Number(n.toFixed(3));
const summary = (rows: HostRow[]) =>
  rows.map((r) => ({
    label: r.label,
    config: r.config?.id ?? null,
    value: r.value ? round(r.value.value) : null,
    best: r.best ? [r.best.config.id, round(r.best.value)] : null,
    status: r.status,
  }));

describe("hostRows (trial fixture)", () => {
  it("puts DGX production first, on the production lane's measured config, then each RG host's serving profile", () => {
    const rows = hostRows(trial, "flash-next", "decode_short");
    expect(rows.map((r) => r.key)).toEqual(["dgx-production", "violet1"]);
    expect(rows[0].lane?.id).toBe("dgx-vllm-fn");
    // measured_config wins over current_config (dgx-vllm-fn@v030-arma-e29f has no measurements)
    expect(rows[0].config?.id).toBe("dgx-vllm-fn@v030-arma");
    expect(rows[0].value?.value).toBeCloseTo(32.16, 5);
    expect(rows[1].lane?.rg_profile).toBe("flash-next-h100");
    expect(rows[1].config?.id).toBe("V1-09b");
    expect(rows[1].label).toBe("H100");
    expect(rows[1].value?.value).toBeCloseTo(95.235, 5);
  });

  it("omits the best tick where the bar is already the host's best", () => {
    const rows = hostRows(trial, "flash-next", "decode_short");
    expect(rows.map((r) => r.best)).toEqual([undefined, undefined]);
  });

  it("ticks a better config on the same host (decode_balanced: rollback 8a72 beats production)", () => {
    const [dgx] = hostRows(trial, "flash-next", "decode_balanced");
    expect(dgx.value?.value).toBeCloseTo(28.575, 5);
    expect(dgx.best?.config.id).toBe("dgx-vllm-fn@8a72");
    expect(dgx.best?.value).toBeCloseTo(32.58, 5);
  });

  it("takes the lowest as best for a lower-is-better metric (TTFT)", () => {
    // production 1.751 s beats 8a72's 2.165 s, so there is no tick; a max-is-best rule would tick 8a72.
    const [dgx] = hostRows(trial, "flash-next", "ttft_cold");
    expect(dgx.value?.value).toBeCloseTo(1.751, 5);
    expect(dgx.best).toBeUndefined();
  });

  it("marks a host with no profile or measurement for the family as not measured", () => {
    const rows = hostRows(trial, "27b", "decode_short");
    expect(summary(rows)).toEqual([
      { label: "DGX production", config: "dgx-sglang-27b@0928", value: 22.94, best: null, status: "measured" },
      { label: "H100", config: null, value: null, best: null, status: "not-measured" },
    ]);
  });

  it("keeps a configured row whose metric is missing, with its best tick", () => {
    // decode_prose exists only on the retired llama.cpp 27B, not on production
    const [dgx] = hostRows(trial, "27b", "decode_prose");
    expect(dgx.status).toBe("no-value");
    expect(dgx.config?.id).toBe("dgx-sglang-27b@0928");
    expect(dgx.best).toBeUndefined(); // not the fixed harness, so not comparable
    const [noLane] = hostRows(trial, "no-such-family", "decode_short");
    expect(noLane.status).toBe("not-measured");
  });

  it("gives yours ÷ the row's value, and null without one", () => {
    const rows = hostRows(trial, "flash-next", "decode_short");
    expect(rowRatio(rows[0], 150)).toBeCloseTo(150 / 32.16, 5);
    expect(rowRatio(rows[0], null)).toBeNull();
    expect(rowRatio(hostRows(trial, "27b", "decode_short")[1], 150)).toBeNull();
  });

  it("names configs and GPUs shortly", () => {
    expect(shortGpu("H100 PCIe")).toBe("H100");
    expect(shortGpu("GH200")).toBe("GH200");
    expect(shortConfigName(trial.configs["V1-09b"])).toBe("V1-09b vllm");
  });
});

describe("compareDomainMax", () => {
  it("rounds the axis end up to a nice number", () => {
    expect(niceMax(0)).toBe(1);
    expect(niceMax(0.9)).toBeCloseTo(1, 12);
    expect(niceMax(221.25 * 1.04)).toBe(250);
    expect(niceMax(1.751 * 1.04)).toBe(2);
    expect(niceMax(100)).toBe(100);
  });

  it("covers rows, best ticks, every plotted config and yours, so rows and the dot strip share one scale", () => {
    const rows = hostRows(trial, "flash-next", "decode_short");
    expect(compareDomainMax(rows, [], null)).toBe(100); // H100 95.235 × 1.04 = 99.04
    expect(compareDomainMax(rows, [130], null)).toBe(150); // a plotted config past every row
    expect(compareDomainMax(rows, [], 150)).toBe(200); // yours past everything: 156 → 200
  });
});

describe("hostRows (live fixture)", () => {
  // The live fixture is regen.py's copy of docs/results; these values were computed from it on
  // 2026-10-01 (fixed harness, thinking off, newest per config, mean of runs) and need updating
  // when a regen changes a serving profile or adds a faster config.
  const live = load("./__fixtures__/results/");

  it("shows the four Flash-Next short-decode rows", () => {
    expect(summary(hostRows(live, "flash-next", "decode_short"))).toEqual([
      { label: "DGX production", config: "dgx-vllm-fn@v030-arma", value: 32.16, best: null, status: "measured" },
      { label: "GH200", config: "gh200-vllm-fn@69d3", value: 209.58, best: ["gh200-vllm-fn@2026-09-27", 221.25], status: "measured" },
      { label: "H100", config: "V1-09b", value: 95.235, best: ["V1-06", 109.23], status: "measured" },
      { label: "A100", config: "V2-06i", value: 51.9, best: ["V2-06b", 55.255], status: "measured" },
    ]);
  });
});
