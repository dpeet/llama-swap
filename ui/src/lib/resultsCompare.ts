// The Results page's host comparison: one row per host for one model family
// (DGX production, then each RG GPU), so a quoted number reads against each
// machine at a glance. Owner decisions 2026-10-01: D2 (rows, not a dot strip)
// and D3 option 1 (each row is the host's serving config, plus a tick at the
// best measured value on that host).

import {
  aggregateNumber,
  byCodepoint,
  isFixedHarness,
  metricInfo,
  ratio,
  type Config,
  type Host,
  type Lane,
  type Measurement,
  type ResultsData,
} from "./results";

export interface HostValue {
  config: Config;
  measurement: Measurement;
  /** The mean of the measurement's runs. */
  value: number;
}

/**
 * - `measured`: the row's config has a value for the metric.
 * - `no-value`: the row has a config, but no value for the metric ("—").
 * - `no-profile`: the host serves no config of the family, but has measured some (only `best` is set).
 * - `not-measured`: neither.
 */
export type HostRowStatus = "measured" | "no-value" | "no-profile" | "not-measured";

export interface HostRow {
  /** A stable key: "dgx-production" or the RG host id. */
  key: string;
  host: Host | null;
  /** "DGX production", or the RG host's GPU ("GH200"). */
  label: string;
  /** The lane that picks the row's config: the production lane, or the RG lane with that node's serving profile. */
  lane: Lane | null;
  config: Config | null;
  value: HostValue | null;
  /** The best value on the host, set only when it beats the row's value (or the row has none). */
  best?: HostValue;
  status: HostRowStatus;
}

/**
 * The measurements a row compares: the fixed harness (the only one that
 * compares across hosts, plan D5), thinking off (the cross-host default), and
 * not superseded.
 */
function comparable(m: Measurement, metric: string): boolean {
  return m.metric === metric && isFixedHarness(m) && m.thinking_measured !== true && m.superseded_by === undefined;
}

/** Each config's newest comparable measurement of `metric`, as a value. */
function valuesByConfig(data: ResultsData, metric: string): Map<string, HostValue> {
  const out = new Map<string, HostValue>();
  for (const m of data.measurements) {
    const config = data.configs[m.config];
    const value = aggregateNumber(m, "mean");
    if (!config || value === null || !comparable(m, metric)) continue;
    const prior = out.get(config.id);
    const newer = !prior || byCodepoint(m.date ?? "", prior.measurement.date ?? "") > 0 ||
      (m.date === prior.measurement.date && byCodepoint(m.id, prior.measurement.id) > 0);
    if (newer) out.set(config.id, { config, measurement: m, value });
  }
  return out;
}

/** The lane's measured config, else its current one (render.py's lane resolution). */
function laneConfig(data: ResultsData, lane: Lane): Config | null {
  return data.configs[lane.measured_config ?? lane.current_config] ?? null;
}

/**
 * The RG lane carrying that node's serving profile for the family: a lane with
 * `rg_profile` on that host and family. catalog.toml has one profile per
 * (node, family) today, so the order below only makes a future second one
 * deterministic: featured first, then the lane id.
 */
function rgProfileLane(data: ResultsData, hostId: string, family: string): Lane | null {
  const lanes = Object.values(data.lanes).filter(
    (lane) => lane.host === hostId && lane.family === family && lane.rg_profile !== undefined,
  );
  lanes.sort((a, b) => Number(b.featured) - Number(a.featured) || byCodepoint(a.id, b.id));
  return lanes[0] ?? null;
}

function bestOnHost(values: Map<string, HostValue>, hostId: string, family: string, higherIsBetter: boolean): HostValue | null {
  let best: HostValue | null = null;
  for (const v of values.values()) {
    if (v.config.host !== hostId || v.config.family !== family) continue;
    if (!best || (higherIsBetter ? v.value > best.value : v.value < best.value)) best = v;
  }
  return best;
}

function makeRow(
  key: string,
  label: string,
  host: Host | null,
  hostId: string | null,
  lane: Lane | null,
  config: Config | null,
  values: Map<string, HostValue>,
  family: string,
  higherIsBetter: boolean,
): HostRow {
  const value = config ? (values.get(config.id) ?? null) : null;
  const top = hostId ? bestOnHost(values, hostId, family, higherIsBetter) : null;
  // Where the best equals the bar, the bar alone says it.
  const beats = top !== null && (value === null || (higherIsBetter ? top.value > value.value : top.value < value.value));
  const status: HostRowStatus = config ? (value ? "measured" : "no-value") : top ? "no-profile" : "not-measured";
  const row: HostRow = { key, host, label, lane, config, value, status };
  if (beats && top) row.best = top;
  return row;
}

/**
 * hostRows returns the comparison's rows for one family and metric: DGX
 * production (the family's `production` lane), then one row per RG host in
 * catalog order, each on its serving-profile lane. Every row carries the best
 * comparable value measured on that host for the family; for a lower-is-better
 * metric (TTFT) best is the lowest.
 */
export function hostRows(data: ResultsData, family: string, metric: string): HostRow[] {
  const { higherIsBetter } = metricInfo(metric);
  const values = valuesByConfig(data, metric);
  const production = Object.values(data.lanes)
    .filter((lane) => lane.family === family && lane.status === "production" && data.hosts[lane.host]?.site !== "rg")
    .sort((a, b) => byCodepoint(a.id, b.id))[0];
  const dgxHost = production?.host ?? null;
  const rows: HostRow[] = [
    makeRow(
      "dgx-production",
      "DGX production",
      dgxHost !== null ? (data.hosts[dgxHost] ?? null) : null,
      dgxHost,
      production ?? null,
      production ? laneConfig(data, production) : null,
      values,
      family,
      higherIsBetter,
    ),
  ];
  for (const host of Object.values(data.hosts)) {
    if (host.site !== "rg") continue;
    const lane = rgProfileLane(data, host.id, family);
    const config = lane ? laneConfig(data, lane) : null;
    rows.push(makeRow(host.id, shortGpu(host.gpu), host, host.id, lane, config, values, family, higherIsBetter));
  }
  return rows;
}

/** "H100 PCIe" → "H100": the row label names the GPU, and the form factor is in the host's tooltip. */
export function shortGpu(gpu: string): string {
  return gpu.replace(/\s+(PCIe|SXM\d*)$/i, "");
}

/** yours ÷ the row's value, or null when either is missing. */
export function rowRatio(row: HostRow, yours: number | null): number | null {
  return ratio(yours, row.value?.value ?? null);
}

/** A config's short name for the small labels: "V1-06 llama.cpp" for a matrix row, else its label. */
export function shortConfigName(config: Config): string {
  return config.matrix_id ? `${config.matrix_id} ${config.engine}` : plainText(config.label);
}

/** Inline-code backticks stripped, for text that isn't rendered as Markdown (aria labels, titles). */
export function plainText(md: string): string {
  return md.replace(/`/g, "");
}

/** The axis end: the smallest 1/1.2/1.5/2/2.5/3/4/5/6/8 × 10ⁿ at or above `value`, so the ticks read as round numbers. */
export function niceMax(value: number): number {
  if (!(value > 0)) return 1;
  const power = 10 ** Math.floor(Math.log10(value));
  for (const m of [1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) if (m * power >= value) return m * power;
  return 10 * power;
}

/**
 * The one x-scale end the host rows and the "show all" dot strip share, so yours sits at the same x in both:
 * the nice max just past the largest row value, best tick, plotted config and yours.
 */
export function compareDomainMax(rows: HostRow[], pointValues: number[], yours: number | null): number {
  const values = [yours ?? 0, ...pointValues, ...rows.flatMap((r) => [r.value?.value ?? 0, r.best?.value ?? 0])];
  return niceMax(Math.max(...values) * 1.04);
}
