// The Results page's table model: rows are configs, columns are config facts
// or metrics. Shared by ResultsTable (desktop) and ResultCards (phone).

import {
  aggregateNumber,
  formatMeasurement,
  harnessLabel,
  lanesForConfig,
  metricInfo,
  METRICS,
  type Config,
  type Lane,
  type Measurement,
  type ResultsData,
} from "./results";

export interface ColumnDef {
  id: string;
  /** Header text; `title` is the long form shown on hover and in the column menu. */
  label: string;
  title: string;
  kind: "metric" | "text";
  unit?: string;
}

export const TEXT_COLUMNS: ColumnDef[] = [
  { id: "host", label: "Host", title: "Host", kind: "text" },
  { id: "build", label: "Build", title: "Build key", kind: "text" },
  { id: "spec", label: "Spec", title: "Speculative decoding", kind: "text" },
  { id: "kv", label: "KV", title: "KV cache dtype", kind: "text" },
  { id: "context", label: "Context", title: "Context length", kind: "text" },
];

const SHORT_LABELS: Record<string, string> = {
  decode_short: "Short",
  decode_balanced: "Balanced",
  decode_4stream: "4-stream",
  decode_single: "Single",
  prefill_cold: "Prefill",
  ttft_cold: "TTFT",
  accept_len: "Accept len",
  steps_per_s: "Steps/s",
  image_decode: "Image decode",
  image_ttft: "Image TTFT",
  image_json_ok: "Image JSON ok",
  image_accept_len: "Image accept len",
  decode_prose: "Prose",
  decode_structured: "Structured",
  kv_pool_tokens: "KV pool",
  graded_set: "Graded",
};

export function metricColumn(metric: string): ColumnDef {
  const info = metricInfo(metric);
  return { id: metric, label: SHORT_LABELS[metric] ?? info.label, title: info.label, kind: "metric", unit: info.unit };
}

/** Every metric the data has, in the schema's order, then any the UI doesn't know yet. */
export function metricColumns(data: ResultsData): ColumnDef[] {
  const present = new Set(data.measurements.map((m) => m.metric));
  const known = Object.keys(METRICS).filter((m) => present.has(m));
  const unknown = [...present].filter((m) => !(m in METRICS)).sort();
  return [...known, ...unknown].map(metricColumn);
}

/**
 * The quoted numbers plus where each config ran. Build is left to the Columns
 * menu, because its long keys pushed Prefill and TTFT off a 1440 px screen with
 * the sidebar open (and the detail dialog shows it anyway).
 */
export const DEFAULT_VISIBLE_COLUMNS = ["host", "decode_short", "decode_balanced", "decode_4stream", "prefill_cold", "ttft_cold"];

/** Restores the persisted column list; anything but a list of strings falls back to the defaults. */
export function normalizeVisibleColumns(raw: unknown): string[] {
  if (!Array.isArray(raw) || !raw.every((v) => typeof v === "string")) return [...DEFAULT_VISIBLE_COLUMNS];
  return raw as string[];
}

export interface ResultRow {
  config: Config;
  hostLabel: string;
  lanes: Lane[];
  /** Shown measurements by metric (already filtered and date-sorted). */
  byMetric: Map<string, Measurement[]>;
  /** Harness badges: the distinct harnesses of the shown measurements. */
  harnesses: string[];
  /** False when no measurement passes the filters; such rows sort last and render dimmed. */
  measured: boolean;
}

export function buildRows(data: ResultsData, configs: Config[], shown: Map<string, Measurement[]>): ResultRow[] {
  return configs.map((config) => {
    const list = shown.get(config.id) ?? [];
    const byMetric = new Map<string, Measurement[]>();
    for (const m of list) byMetric.set(m.metric, [...(byMetric.get(m.metric) ?? []), m]);
    return {
      config,
      hostLabel: data.hosts[config.host]?.label ?? config.host,
      lanes: lanesForConfig(data, config.id),
      byMetric,
      harnesses: [...new Set(list.map((m) => harnessLabel(m.harness)))],
      measured: list.length > 0,
    };
  });
}

export function textCell(row: ResultRow, column: string): string {
  const c = row.config;
  switch (column) {
    case "host":
      return row.hostLabel;
    case "build":
      return c.build ?? "unrecorded";
    case "spec":
      return c.spec.label;
    case "kv":
      return c.kv_dtype ?? "—";
    case "context":
      return c.context !== undefined ? c.context.toLocaleString("en-US") : "—";
    default:
      return "—";
  }
}

export interface CellEntry {
  measurement: Measurement;
  /** Runs in order (render.py's default "runs" aggregate), at the recorded precision. */
  text: string;
  /** The id's qualifier (e.g. thinking-on, 120f), shown when a cell holds several measurements. */
  qualifier: string | null;
}

/**
 * The id's qualifier (the part after config/metric). With `inSharedCell`, an
 * unqualified id falls back to its date, because a cell holding several
 * sessions must label every one of them, not only the qualified ones.
 */
export function qualifierOf(m: Measurement, inSharedCell = false): string | null {
  const parts = m.id.split("/");
  if (parts.length > 2) return parts.slice(2).join("/");
  return inSharedCell ? (m.date ?? null) : null;
}

export function cellEntries(row: ResultRow, metric: string): CellEntry[] {
  const list = row.byMetric.get(metric) ?? [];
  return list.map((measurement) => ({
    measurement,
    text: formatMeasurement(measurement),
    qualifier: qualifierOf(measurement, list.length > 1),
  }));
}

export interface SortState {
  /** A column id, "config", or "" for catalog order. */
  key: string;
  dir: "asc" | "desc";
}

function sortValue(row: ResultRow, key: string, columns: Map<string, ColumnDef>): number | string | null {
  if (key === "config") return row.config.label.toLowerCase();
  if (key === "context") return row.config.context ?? null;
  if (columns.get(key)?.kind === "metric") {
    const first = row.byMetric.get(key)?.[0];
    return first ? aggregateNumber(first, "mean") : null;
  }
  const text = textCell(row, key);
  return text === "—" ? null : text.toLowerCase();
}

/**
 * sortRows orders rows by a column, keeping rows without a value for it (and
 * rows with no shown measurement at all) last in either direction, so a sort
 * always leads with real numbers.
 */
export function sortRows(rows: ResultRow[], sort: SortState, columns: ColumnDef[]): ResultRow[] {
  const byId = new Map(columns.map((c) => [c.id, c]));
  const indexed = rows.map((row, index) => ({ row, index, value: sort.key === "" ? null : sortValue(row, sort.key, byId) }));
  const sign = sort.dir === "asc" ? 1 : -1;
  indexed.sort((a, b) => {
    if (a.row.measured !== b.row.measured) return a.row.measured ? -1 : 1;
    if (sort.key === "") return a.index - b.index;
    if (a.value === null || b.value === null) {
      if (a.value === b.value) return a.index - b.index;
      return a.value === null ? 1 : -1;
    }
    const cmp = typeof a.value === "number" && typeof b.value === "number" ? a.value - b.value : String(a.value).localeCompare(String(b.value));
    return cmp * sign || a.index - b.index;
  });
  return indexed.map((x) => x.row);
}

/** The first direction a header click sorts in: best first for a metric (high, or low for a time), A–Z for text. */
export function firstSortDir(column: ColumnDef | undefined): SortState["dir"] {
  if (column?.kind !== "metric") return "asc";
  return metricInfo(column.id).higherIsBetter ? "desc" : "asc";
}

/** Next sort after clicking a header: first direction, then reversed, then back to catalog order. */
export function nextSort(current: SortState, key: string, first: SortState["dir"]): SortState {
  if (current.key !== key) return { key, dir: first };
  if (current.dir === first) return { key, dir: first === "asc" ? "desc" : "asc" };
  return { key: "", dir: "asc" };
}

/** The caveat marks a measurement carries, with their text, for a cell's superscript. */
export function caveatMarks(data: ResultsData, m: Measurement): { mark: string; text: string }[] {
  return m.caveats.map((id) => {
    const caveat = data.caveats[id];
    return caveat ? { mark: caveat.mark, text: caveat.text_md } : { mark: `[${id}]`, text: `Unknown caveat ${id}` };
  });
}
