// Filter state for the Results page, kept out of the components so matching,
// search and the hash-querystring round trip are unit testable.

import {
  aggregateNumber,
  byCodepoint,
  isFixedHarness,
  lanesForConfig,
  LANE_STATUSES,
  nearestByRatio,
  ratio,
  type Config,
  type Measurement,
  type ResultsData,
} from "./results";

export const THINKING = ["any", "on", "off"] as const;
export type Thinking = (typeof THINKING)[number];
export const COMPARABILITY = ["fixed", "all"] as const;
export type Comparability = (typeof COMPARABILITY)[number];
/** The lane-status filter's extra value for configs that no lane serves. */
export const UNSERVED = "unserved";

/** The compare strip's metrics: the numbers people quote (owner's job, plan D8). */
export const COMPARE_METRICS = ["decode_short", "decode_balanced", "decode_4stream", "prefill_cold", "ttft_cold"] as const;
export type CompareMetric = (typeof COMPARE_METRICS)[number];
export const COMPARE_METRIC_LABELS: Record<CompareMetric, string> = {
  decode_short: "Decode, short",
  decode_balanced: "Decode, balanced",
  decode_4stream: "Decode, 4-stream",
  prefill_cold: "Cold prefill",
  ttft_cold: "TTFT",
};

export interface ResultsFilters {
  /** "" means any, for every string filter below. */
  family: string;
  host: string;
  engine: string;
  /** Only configs with a shown measurement of this metric. */
  metric: string;
  thinking: Thinking;
  /** A lane status, UNSERVED, or "". */
  status: string;
  /** "fixed" (default) shows only fixed-harness measurements, because the others don't compare (plan D5). */
  comparability: Comparability;
  showSuperseded: boolean;
  search: string;
  compareMetric: CompareMetric;
  /** The typed "your number", kept as text so a cleared input stays empty. */
  yours: string;
}

export function emptyResultsFilters(): ResultsFilters {
  return {
    family: "",
    host: "",
    engine: "",
    metric: "",
    thinking: "any",
    status: "",
    comparability: "fixed",
    showSuperseded: false,
    search: "",
    compareMetric: "decode_short",
    yours: "",
  };
}

function oneOf<T extends string>(value: unknown, allowed: readonly T[], fallback: T): T {
  return typeof value === "string" && (allowed as readonly string[]).includes(value) ? (value as T) : fallback;
}

/**
 * normalizeResultsFilters coerces an unknown value (a restored object, possibly
 * from an older build) into valid ResultsFilters. Wrongly typed or unknown
 * values fall back to the defaults. Free-text filters (family, host, engine,
 * metric) are kept as given, because the data's lists grow.
 */
export function normalizeResultsFilters(raw: unknown): ResultsFilters {
  const filters = emptyResultsFilters();
  if (typeof raw !== "object" || raw === null) return filters;
  const source = raw as Record<string, unknown>;
  for (const key of ["family", "host", "engine", "metric", "search", "yours"] as const) {
    if (typeof source[key] === "string") filters[key] = source[key] as string;
  }
  filters.thinking = oneOf(source.thinking, THINKING, filters.thinking);
  filters.comparability = oneOf(source.comparability, COMPARABILITY, filters.comparability);
  filters.status = oneOf(source.status, [...LANE_STATUSES, UNSERVED, ""], "");
  filters.compareMetric = oneOf(source.compareMetric, COMPARE_METRICS, filters.compareMetric);
  if (typeof source.showSuperseded === "boolean") filters.showSuperseded = source.showSuperseded;
  return filters;
}

// Querystring keys, spelled out so a shared link reads plainly.
const QUERY_KEYS = {
  family: "family",
  host: "host",
  engine: "engine",
  metric: "metric",
  thinking: "thinking",
  status: "status",
  comparability: "harness",
  showSuperseded: "superseded",
  search: "q",
  compareMetric: "compare",
  yours: "yours",
} as const satisfies Record<keyof ResultsFilters, string>;

/** The hash querystring for `filters` (without "?"), writing only values that differ from the defaults. */
export function resultsFiltersToQuery(filters: ResultsFilters): string {
  const defaults = emptyResultsFilters();
  const query = new URLSearchParams();
  for (const [field, key] of Object.entries(QUERY_KEYS) as [keyof ResultsFilters, string][]) {
    const value = filters[field];
    if (value === defaults[field]) continue;
    if (typeof value === "boolean") query.set(key, value ? "1" : "0");
    else if (value.trim() !== "") query.set(key, value);
  }
  return query.toString();
}

/** Filters from a hash querystring (svelte-spa-router's `querystring`, with or without "?"). */
export function resultsFiltersFromQuery(querystring: string | undefined): ResultsFilters {
  const query = new URLSearchParams(querystring ?? "");
  const raw: Record<string, unknown> = {};
  for (const [field, key] of Object.entries(QUERY_KEYS) as [keyof ResultsFilters, string][]) {
    const value = query.get(key);
    if (value === null) continue;
    raw[field] = field === "showSuperseded" ? value === "1" : value;
  }
  return normalizeResultsFilters(raw);
}

/** How many table filters are set (the compare strip's metric and number aren't filters). */
export function activeFilterCount(filters: ResultsFilters): number {
  const defaults = emptyResultsFilters();
  const keys = ["family", "host", "engine", "metric", "thinking", "status", "comparability", "showSuperseded", "search"] as const;
  return keys.filter((key) => (key === "search" ? filters.search.trim() !== "" : filters[key] !== defaults[key])).length;
}

// ---- Matching ------------------------------------------------------------------

/** Whether a measurement passes the measurement-level filters (harness, thinking, superseded). */
export function measurementShown(m: Measurement, filters: ResultsFilters): boolean {
  if (filters.comparability === "fixed" && !isFixedHarness(m)) return false;
  if (!filters.showSuperseded && m.superseded_by !== undefined) return false;
  if (filters.thinking !== "any" && m.thinking_measured !== (filters.thinking === "on")) return false;
  return true;
}

/** Lower-cased, de-duplicated whitespace tokens; every one must match (AND). */
export function searchTokens(text: string): string[] {
  return [...new Set(text.toLowerCase().split(/\s+/).filter((t) => t !== ""))];
}

/**
 * The text a config is searched by: its id, label, model, lanes, build key,
 * checkpoint, flags, env, matrix id, notes, and the caveat text and notes of
 * its measurements.
 */
export function configHaystack(data: ResultsData, config: Config): string {
  const parts: (string | undefined)[] = [
    config.id,
    config.label,
    config.model,
    config.engine,
    config.engine_label,
    config.build ?? "build unrecorded",
    config.image,
    config.checkpoint,
    data.checkpoints[config.checkpoint]?.name,
    data.hosts[config.host]?.label ?? config.host,
    config.matrix_id,
    config.compose,
    config.kv_dtype,
    config.spec.label,
    config.notes_md,
    ...config.flags,
    ...Object.entries(config.env).map(([k, v]) => `${k}=${v}`),
  ];
  for (const lane of lanesForConfig(data, config.id)) {
    parts.push(lane.id, lane.label, lane.llama_swap_id, lane.rg_profile, lane.notes_md);
  }
  const caveatIds = new Set(config.caveats);
  for (const m of data.measurements) {
    if (m.config !== config.id) continue;
    m.caveats.forEach((id) => caveatIds.add(id));
    parts.push(m.note, m.history_section);
  }
  for (const id of caveatIds) parts.push(id, data.caveats[id]?.text_md);
  return parts.filter((p): p is string => p !== undefined).join("\n").toLowerCase();
}

/** Whether a config passes the config-level filters and the search (not the metric filter; see configsShown). */
export function configMatches(data: ResultsData, config: Config, filters: ResultsFilters, haystack?: string): boolean {
  if (filters.family !== "" && config.family !== filters.family) return false;
  if (filters.host !== "" && config.host !== filters.host) return false;
  if (filters.engine !== "" && config.engine !== filters.engine) return false;
  if (filters.status !== "") {
    const statuses = lanesForConfig(data, config.id).map((lane) => lane.status);
    if (filters.status === UNSERVED ? statuses.length > 0 : !statuses.includes(filters.status)) return false;
  }
  const tokens = searchTokens(filters.search);
  if (tokens.length > 0) {
    const text = haystack ?? configHaystack(data, config);
    if (!tokens.every((token) => text.includes(token))) return false;
  }
  return true;
}

/** Shown measurements per config id, sorted by date then id as render.py's select does. */
export function shownMeasurements(data: ResultsData, filters: ResultsFilters): Map<string, Measurement[]> {
  const out = new Map<string, Measurement[]>();
  for (const m of data.measurements) {
    if (!measurementShown(m, filters)) continue;
    const list = out.get(m.config) ?? [];
    list.push(m);
    out.set(m.config, list);
  }
  for (const list of out.values()) {
    list.sort((a, b) => byCodepoint(a.date ?? "", b.date ?? "") || byCodepoint(a.id, b.id));
  }
  return out;
}

/** The configs the table shows: config filters, search, and the metric filter (a shown measurement of it). */
export function configsShown(
  data: ResultsData,
  filters: ResultsFilters,
  shown: Map<string, Measurement[]> = shownMeasurements(data, filters),
): Config[] {
  return Object.values(data.configs).filter(
    (config) =>
      configMatches(data, config, filters) &&
      (filters.metric === "" || (shown.get(config.id) ?? []).some((m) => m.metric === filters.metric)),
  );
}

// ---- Compare strip ---------------------------------------------------------------

export interface ComparePoint {
  config: Config;
  measurement: Measurement;
  value: number;
  site: string; // "dgx" | "rg" | the host id when the host is unknown
  dgxProduction: boolean;
}

/**
 * pickForStrip chooses the one measurement a config contributes to the strip
 * when several pass the filters: thinking off first (the cross-host default),
 * then the newest.
 */
function pickForStrip(list: Measurement[]): Measurement | undefined {
  return [...list].sort((a, b) => {
    const thinking = Number(a.thinking_measured === true) - Number(b.thinking_measured === true);
    return thinking || (b.date ?? "").localeCompare(a.date ?? "");
  })[0];
}

/** One point per shown config with a value for the compare metric, sorted by value. */
export function comparePoints(data: ResultsData, filters: ResultsFilters, configs: Config[] = configsShown(data, filters)): ComparePoint[] {
  const shown = shownMeasurements(data, filters);
  const points: ComparePoint[] = [];
  for (const config of configs) {
    const m = pickForStrip((shown.get(config.id) ?? []).filter((r) => r.metric === filters.compareMetric));
    const value = m ? aggregateNumber(m, "mean") : null;
    if (!m || value === null) continue;
    const site = data.hosts[config.host]?.site ?? config.host;
    const dgxProduction = site === "dgx" && lanesForConfig(data, config.id).some((lane) => lane.status === "production");
    points.push({ config, measurement: m, value, site, dgxProduction });
  }
  return points.sort((a, b) => a.value - b.value);
}

export interface CompareRatio {
  point: ComparePoint;
  /** yours ÷ the point's value. */
  ratio: number;
  kind: "dgx-production" | "nearest-rg";
}

/** The ratios the strip labels: yours against every DGX production point, then the nearest RG point. */
export function compareRatios(points: ComparePoint[], yours: number | null): CompareRatio[] {
  if (yours === null || !(yours > 0)) return [];
  const out: CompareRatio[] = [];
  for (const point of points.filter((p) => p.dgxProduction)) {
    const r = ratio(yours, point.value);
    if (r !== null) out.push({ point, ratio: r, kind: "dgx-production" });
  }
  const nearest = nearestByRatio(
    points.filter((p) => p.site === "rg"),
    (p) => p.value,
    yours,
  );
  const r = nearest ? ratio(yours, nearest.value) : null;
  if (nearest && r !== null) out.push({ point: nearest, ratio: r, kind: "nearest-rg" });
  return out;
}

/** The typed number, or null when blank or not a positive number. Accepts "1,234" and "~95". */
export function parseYours(text: string): number | null {
  const cleaned = text.replace(/[,\s~≈]/g, "");
  if (cleaned === "") return null;
  const value = Number(cleaned);
  return Number.isFinite(value) && value > 0 ? value : null;
}
