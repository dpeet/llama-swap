// Benchmark results (artisanal-inference docs/results/) for the Results page:
// types, a never-throwing normalizer for catalog.json + measurements.jsonl, and
// the display aggregates. The aggregates mirror serve/results/resultslib/render.py
// (format_value) so a number reads the same here as in RESULTS.md; plan:
// artisanal-inference docs/todo/results-explorer.md.

// ---- Types -------------------------------------------------------------------

/** Lists the schema enumerates today. Values outside them are kept (the schema grows), only labelled raw. */
export const FAMILIES = ["flash-next", "27b", "other"] as const;
export const LANE_STATUSES = ["production", "rollback", "registered", "retired", "a-b", "superseded"] as const;
/** The comparable harness (plan D5): the page shows only these by default. */
export const FIXED_HARNESS = "fixed-2026-09-27";

export interface MetricInfo {
  label: string;
  unit: string;
  /** False for times (TTFT), where a smaller number is better. */
  higherIsBetter: boolean;
}

/** Copied from schema.json `x-metrics`. A metric missing here still loads, labelled by its id. */
export const METRICS: Record<string, MetricInfo> = {
  decode_short: { label: "Short decode", unit: "tok/s", higherIsBetter: true },
  decode_balanced: { label: "Balanced decode", unit: "tok/s", higherIsBetter: true },
  decode_4stream: { label: "4-stream aggregate", unit: "tok/s", higherIsBetter: true },
  decode_single: { label: "Single-stream decode (harness-specific prompt)", unit: "tok/s", higherIsBetter: true },
  prefill_cold: { label: "Cold prefill (~3.8k)", unit: "tok/s", higherIsBetter: true },
  ttft_cold: { label: "TTFT (~3.8k)", unit: "s", higherIsBetter: false },
  accept_len: { label: "Accept length", unit: "tokens/step", higherIsBetter: true },
  steps_per_s: { label: "Verify steps/s", unit: "steps/s", higherIsBetter: true },
  image_decode: { label: "Image-request decode", unit: "tok/s", higherIsBetter: true },
  image_ttft: { label: "Image-request TTFT", unit: "s", higherIsBetter: false },
  image_json_ok: { label: "Image items read correctly (valid JSON)", unit: "items", higherIsBetter: true },
  image_accept_len: { label: "Image-request accept length", unit: "tokens/step", higherIsBetter: true },
  decode_prose: { label: "Prose decode", unit: "tok/s", higherIsBetter: true },
  decode_structured: { label: "Structured decode", unit: "tok/s", higherIsBetter: true },
  kv_pool_tokens: { label: "KV pool", unit: "tokens", higherIsBetter: true },
  graded_set: { label: "Graded set correct", unit: "items", higherIsBetter: true },
};

export function metricInfo(metric: string): MetricInfo {
  return METRICS[metric] ?? { label: metric, unit: "", higherIsBetter: true };
}

export interface Host {
  id: string;
  label: string;
  site: string; // "dgx" | "rg"
  gpu: string;
  memory_gb?: number;
  arch?: string;
  notes_md?: string;
}

export interface Build {
  id: string;
  engine: string;
  version: string;
  image?: string;
  torch?: string;
  flashinfer?: string;
  recorded?: string;
  notes_md?: string;
}

export interface Checkpoint {
  id: string;
  name: string;
  source?: string;
  revision?: string;
  precision?: string;
  notes_md?: string;
}

export interface Spec {
  method: string;
  label: string;
  depth?: number;
  draft_vocab?: number;
  draft_checkpoint?: string;
}

export interface Config {
  id: string;
  label: string;
  model: string;
  family: string;
  host: string;
  /** null when the build was not recorded. */
  build: string | null;
  engine: string;
  checkpoint: string;
  spec: Spec;
  image?: string;
  image_digest?: string;
  engine_label?: string;
  kv_dtype?: string;
  context?: number;
  thinking_default?: string;
  flags: string[];
  env: Record<string, string>;
  compose?: string;
  matrix_id?: string;
  caveats: string[];
  notes_md?: string;
}

export interface Lane {
  id: string;
  label: string;
  host: string;
  family: string;
  status: string;
  current_config: string;
  measured_config?: string;
  llama_swap_id?: string;
  rg_profile?: string;
  port?: number;
  since?: string;
  featured: boolean;
  notes_md?: string;
}

export interface Caveat {
  id: string;
  mark: string;
  text_md: string;
}

export interface Campaign {
  id: string;
  label: string;
  dates?: string;
  plan?: string;
  raw_output?: string;
  notes_md?: string;
}

export type Aggregate = string; // runs | mean | range | min | max | run:<n>

export interface DisplaySpec {
  aggregate?: Aggregate;
  precision?: number;
  approx_prefix?: boolean;
}

export interface Conditions {
  prompt_tokens?: number;
  output_tokens?: number;
  frames?: number;
  context?: number;
  concurrency?: number;
  items?: number;
  reasoning_effort?: string;
  max_tokens?: number;
}

export interface Measurement {
  id: string;
  config: string;
  metric: string;
  unit: string;
  /** Exactly one of runs and range is set. */
  runs?: number[];
  range?: [number, number];
  approx: boolean;
  precision?: number;
  thinking_measured?: boolean;
  access?: string;
  residency?: string;
  harness?: string;
  harness_commit?: string;
  decode_definition?: string;
  conditions: Conditions;
  date?: string;
  campaign?: string;
  caveats: string[];
  evidence: string[];
  history_section?: string;
  superseded_by?: string;
  display: Record<string, DisplaySpec>;
  note?: string;
}

export interface ViewColumn {
  header: string;
  metric?: string;
  aggregate?: Aggregate;
  precision?: number;
  approx_prefix?: boolean;
}

export interface View {
  id: string;
  kind: string;
  title?: string;
  columns: ViewColumn[];
}

export interface Rejected {
  id: string;
  reason: string;
}

export interface ResultsData {
  hosts: Record<string, Host>;
  builds: Record<string, Build>;
  checkpoints: Record<string, Checkpoint>;
  configs: Record<string, Config>;
  lanes: Record<string, Lane>;
  caveats: Record<string, Caveat>;
  campaigns: Record<string, Campaign>;
  views: Record<string, View>;
  measurements: Measurement[];
  rejected: Rejected[];
}

// ---- Coercers (activityFilters.ts style: unknown in, valid value or null out) ----

type Obj = Record<string, unknown>;

function isObj(value: unknown): value is Obj {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function str(value: unknown): string | undefined {
  return typeof value === "string" && value !== "" ? value : undefined;
}

function num(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function int(value: unknown): number | undefined {
  const n = num(value);
  return n !== undefined && Number.isInteger(n) ? n : undefined;
}

function bool(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined;
}

function strList(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((v): v is string => typeof v === "string" && v !== "") : [];
}

function strMap(value: unknown): Record<string, string> {
  const out: Record<string, string> = Object.create(null);
  if (!isObj(value)) return out;
  for (const [k, v] of Object.entries(value)) if (typeof v === "string") out[k] = v;
  return out;
}

/** Copies the optional string fields that are present and well typed. */
function optStrings<T extends string>(source: Obj, keys: readonly T[]): Partial<Record<T, string>> {
  const out: Partial<Record<T, string>> = {};
  for (const key of keys) {
    const value = str(source[key]);
    if (value !== undefined) out[key] = value;
  }
  return out;
}

/** A required-field failure, thrown only inside the per-record coercers and caught by section(). */
class Reject extends Error {}

function need<T>(value: T | undefined, field: string): T {
  if (value === undefined) throw new Reject(`missing or invalid ${field}`);
  return value;
}

function coerceDisplay(value: unknown): DisplaySpec | null {
  if (!isObj(value)) return null;
  const spec: DisplaySpec = {};
  const agg = str(value.aggregate);
  if (agg !== undefined && AGGREGATE_RE.test(agg)) spec.aggregate = agg;
  const precision = int(value.precision);
  if (precision !== undefined) spec.precision = precision;
  const approx = bool(value.approx_prefix);
  if (approx !== undefined) spec.approx_prefix = approx;
  return spec;
}

const AGGREGATE_RE = /^(runs|mean|range|min|max|run:[1-9][0-9]*)$/;

function coerceHost(id: string, v: Obj): Host {
  return {
    id,
    label: need(str(v.label), "label"),
    site: need(str(v.site), "site"),
    gpu: need(str(v.gpu), "gpu"),
    ...(num(v.memory_gb) !== undefined ? { memory_gb: num(v.memory_gb) } : {}),
    ...optStrings(v, ["arch", "notes_md"] as const),
  };
}

function coerceBuild(id: string, v: Obj): Build {
  return {
    id,
    engine: need(str(v.engine), "engine"),
    version: need(str(v.version), "version"),
    ...optStrings(v, ["image", "torch", "flashinfer", "recorded", "notes_md"] as const),
  };
}

function coerceCheckpoint(id: string, v: Obj): Checkpoint {
  return {
    id,
    name: need(str(v.name), "name"),
    ...optStrings(v, ["source", "revision", "precision", "notes_md"] as const),
  };
}

function coerceSpec(value: unknown): Spec {
  if (!isObj(value)) throw new Reject("missing or invalid spec");
  const spec: Spec = { method: need(str(value.method), "spec.method"), label: need(str(value.label), "spec.label") };
  const depth = int(value.depth);
  if (depth !== undefined) spec.depth = depth;
  const vocab = int(value.draft_vocab);
  if (vocab !== undefined) spec.draft_vocab = vocab;
  const draft = str(value.draft_checkpoint);
  if (draft !== undefined) spec.draft_checkpoint = draft;
  return spec;
}

function coerceConfig(id: string, v: Obj): Config {
  if (!(v.build === null || str(v.build) !== undefined)) throw new Reject("missing or invalid build");
  const config: Config = {
    id,
    label: need(str(v.label), "label"),
    model: need(str(v.model), "model"),
    family: need(str(v.family), "family"),
    host: need(str(v.host), "host"),
    build: (v.build as string | null) ?? null,
    engine: need(str(v.engine), "engine"),
    checkpoint: need(str(v.checkpoint), "checkpoint"),
    spec: coerceSpec(v.spec),
    flags: strList(v.flags),
    env: strMap(v.env),
    caveats: strList(v.caveats),
    ...optStrings(v, [
      "image",
      "image_digest",
      "engine_label",
      "kv_dtype",
      "thinking_default",
      "compose",
      "matrix_id",
      "notes_md",
    ] as const),
  };
  const context = int(v.context);
  if (context !== undefined) config.context = context;
  return config;
}

function coerceLane(id: string, v: Obj): Lane {
  const lane: Lane = {
    id,
    label: need(str(v.label), "label"),
    host: need(str(v.host), "host"),
    family: need(str(v.family), "family"),
    status: need(str(v.status), "status"),
    current_config: need(str(v.current_config), "current_config"),
    featured: bool(v.featured) ?? false,
    ...optStrings(v, ["measured_config", "llama_swap_id", "rg_profile", "since", "notes_md"] as const),
  };
  const port = int(v.port);
  if (port !== undefined) lane.port = port;
  return lane;
}

function coerceCaveat(id: string, v: Obj): Caveat {
  return { id, mark: need(str(v.mark), "mark"), text_md: need(str(v.text_md), "text_md") };
}

function coerceCampaign(id: string, v: Obj): Campaign {
  return { id, label: need(str(v.label), "label"), ...optStrings(v, ["dates", "plan", "raw_output", "notes_md"] as const) };
}

function coerceView(id: string, v: Obj): View {
  const columns: ViewColumn[] = [];
  if (Array.isArray(v.columns)) {
    for (const col of v.columns) {
      if (!isObj(col) || str(col.header) === undefined) continue;
      columns.push({ header: col.header as string, ...optStrings(col, ["metric"] as const), ...coerceDisplay(col) });
    }
  }
  return { id, kind: need(str(v.kind), "kind"), ...optStrings(v, ["title"] as const), columns };
}

function coerceNumbers(value: unknown, field: string): number[] {
  if (!Array.isArray(value) || value.length === 0) throw new Reject(`${field} must be a non-empty list of numbers`);
  const out = value.map(num);
  if (out.some((n) => n === undefined)) throw new Reject(`${field} must be a non-empty list of numbers`);
  return out as number[];
}

function coerceConditions(value: unknown): Conditions {
  const out: Conditions = {};
  if (!isObj(value)) return out;
  for (const key of ["prompt_tokens", "output_tokens", "frames", "context", "concurrency", "items", "max_tokens"] as const) {
    const n = int(value[key]);
    if (n !== undefined) out[key] = n;
  }
  const effort = str(value.reasoning_effort);
  if (effort !== undefined) out.reasoning_effort = effort;
  return out;
}

function coerceMeasurement(v: Obj): Measurement {
  const id = need(str(v.id), "id");
  const hasRuns = v.runs !== undefined;
  const hasRange = v.range !== undefined;
  if (hasRuns === hasRange) throw new Reject("needs exactly one of runs and range");
  const m: Measurement = {
    id,
    config: need(str(v.config), "config"),
    metric: need(str(v.metric), "metric"),
    unit: need(str(v.unit), "unit"),
    approx: bool(v.approx) ?? false,
    conditions: coerceConditions(v.conditions),
    caveats: strList(v.caveats),
    evidence: strList(v.evidence),
    display: {},
    ...optStrings(v, [
      "access",
      "residency",
      "harness",
      "harness_commit",
      "decode_definition",
      "date",
      "campaign",
      "history_section",
      "superseded_by",
      "note",
    ] as const),
  };
  if (hasRuns) m.runs = coerceNumbers(v.runs, "runs");
  else {
    const range = coerceNumbers(v.range, "range");
    if (range.length !== 2) throw new Reject("range must be [lo, hi]");
    m.range = [range[0], range[1]];
  }
  const precision = int(v.precision);
  if (precision !== undefined) m.precision = precision;
  const thinking = bool(v.thinking_measured);
  if (thinking !== undefined) m.thinking_measured = thinking;
  if (isObj(v.display)) {
    for (const [view, spec] of Object.entries(v.display)) {
      const coerced = coerceDisplay(spec);
      if (coerced) m.display[view] = coerced;
    }
  }
  return m;
}

function reasonOf(cause: unknown): string {
  return cause instanceof Reject ? cause.message : `unreadable (${String(cause)})`;
}

/** One catalog section (an id → record object), each bad record rejected with "<kind>: <reason>". */
function section<T>(
  catalog: Obj,
  key: string,
  kind: string,
  coerce: (id: string, value: Obj) => T,
  rejected: Rejected[],
): Record<string, T> {
  // Null-prototype, because record ids are data: "__proto__" would otherwise
  // set the prototype and "constructor" would resolve to Object's.
  const out: Record<string, T> = Object.create(null);
  const raw = catalog[key];
  if (raw === undefined) return out;
  if (!isObj(raw)) {
    rejected.push({ id: key, reason: `catalog: ${key} is not an object` });
    return out;
  }
  for (const [id, value] of Object.entries(raw)) {
    if (!isObj(value)) {
      rejected.push({ id, reason: `${kind}: not an object` });
      continue;
    }
    try {
      out[id] = coerce(id, value);
    } catch (cause) {
      rejected.push({ id, reason: `${kind}: ${reasonOf(cause)}` });
    }
  }
  return out;
}

/**
 * normalizeResults coerces the parsed catalog.json and the raw measurements.jsonl
 * text into ResultsData. It never throws: a bad record is left out and listed in
 * `rejected` with a reason, so one typo can't blank the page. Blank JSONL lines
 * are skipped; on a duplicate measurement id the first wins. Records whose
 * references can't be followed (a lane or measurement naming an unknown config)
 * are rejected; an unknown host, build, checkpoint or caveat is kept and shown by
 * its raw id, because lint (not the page) owns referential integrity.
 */
export function normalizeResults(catalog: unknown, jsonlText: string): ResultsData {
  const rejected: Rejected[] = [];
  const cat: Obj = isObj(catalog) ? catalog : {};
  if (!isObj(catalog)) rejected.push({ id: "catalog.json", reason: "catalog: not a JSON object" });

  const configs = section(cat, "configs", "config", coerceConfig, rejected);
  const lanes: Record<string, Lane> = Object.create(null);
  for (const [id, lane] of Object.entries(section(cat, "lanes", "lane", coerceLane, rejected))) {
    if (!Object.hasOwn(configs, lane.current_config)) {
      rejected.push({ id, reason: `lane: unknown current_config ${lane.current_config}` });
      continue;
    }
    if (lane.measured_config !== undefined && !Object.hasOwn(configs, lane.measured_config)) {
      rejected.push({ id, reason: `lane: unknown measured_config ${lane.measured_config}` });
      continue;
    }
    lanes[id] = lane;
  }

  const measurements: Measurement[] = [];
  const seen = new Set<string>();
  const lines = typeof jsonlText === "string" ? jsonlText.split("\n") : [];
  lines.forEach((rawLine, index) => {
    const line = rawLine.trim();
    if (line === "") return;
    const where = `measurements.jsonl line ${index + 1}`;
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      rejected.push({ id: where, reason: "measurement: invalid JSON" });
      return;
    }
    if (!isObj(parsed)) {
      rejected.push({ id: where, reason: "measurement: not an object" });
      return;
    }
    const id = str(parsed.id) ?? where;
    try {
      const m = coerceMeasurement(parsed);
      if (seen.has(m.id)) throw new Reject(`duplicate id (${where}); the first is kept`);
      if (!Object.hasOwn(configs, m.config)) throw new Reject(`unknown config ${m.config}`);
      seen.add(m.id);
      measurements.push(m);
    } catch (cause) {
      rejected.push({ id, reason: `measurement: ${reasonOf(cause)}` });
    }
  });

  return {
    hosts: section(cat, "hosts", "host", coerceHost, rejected),
    builds: section(cat, "builds", "build", coerceBuild, rejected),
    checkpoints: section(cat, "checkpoints", "checkpoint", coerceCheckpoint, rejected),
    configs,
    lanes,
    caveats: section(cat, "caveats", "caveat", coerceCaveat, rejected),
    campaigns: section(cat, "campaigns", "campaign", coerceCampaign, rejected),
    views: section(cat, "views", "view", coerceView, rejected),
    measurements,
    rejected,
  };
}

// ---- Exact decimal formatting (render.py uses Decimal) -------------------------

// A value as an integer count of 10^-scale, e.g. 95.24 → {int: 9524n, scale: 2}.
// Exact decimal arithmetic, because binary floats would round a mean of
// 95.24 and 95.23 (95.235) down where render.py's ROUND_HALF_UP rounds it up.
interface Dec {
  int: bigint;
  scale: number;
}

/** The shortest round-trip decimal of a float, which is what Python's Decimal(str(v)) sees. */
function toDec(value: number): Dec {
  const text = String(value);
  const match = /^(-?)(\d+)(?:\.(\d+))?(?:e([+-]?\d+))?$/i.exec(text);
  if (!match) return { int: 0n, scale: 0 };
  const [, sign, whole, frac = "", exp = "0"] = match;
  let digits = BigInt(whole + frac);
  let scale = frac.length - Number(exp);
  if (scale < 0) {
    digits *= 10n ** BigInt(-scale);
    scale = 0;
  }
  return { int: sign ? -digits : digits, scale };
}

function rescale(dec: Dec, scale: number): bigint {
  return dec.int * 10n ** BigInt(scale - dec.scale);
}

/** num/den rounded half away from zero (Python's ROUND_HALF_UP) to `precision` places, as a count of 10^-precision. */
function roundRational(num: bigint, den: bigint, precision: number): bigint {
  let n = num < 0n ? -num : num;
  let d = den < 0n ? -den : den;
  if (precision >= 0) n *= 10n ** BigInt(precision);
  else d *= 10n ** BigInt(-precision);
  const q = (2n * n + d) / (2n * d);
  return num < 0n !== den < 0n ? -q : q;
}

function groupThousands(digits: string): string {
  return digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

/** A count of 10^-scale (scale may be negative: tens, hundreds) as `format(Decimal, ",f")` prints it. */
function formatScaled(value: bigint, scale: number): string {
  const negative = value < 0n;
  let abs = negative ? -value : value;
  if (scale < 0) {
    abs *= 10n ** BigInt(-scale);
    scale = 0;
  }
  const text = abs.toString().padStart(scale + 1, "0");
  const whole = text.slice(0, text.length - scale);
  const frac = scale > 0 ? "." + text.slice(text.length - scale) : "";
  return (negative ? "-" : "") + groupThousands(whole) + frac;
}

function formatDec(dec: Dec, precision: number | undefined): string {
  if (precision === undefined) return formatScaled(dec.int, dec.scale);
  return formatScaled(roundRational(dec.int, 10n ** BigInt(dec.scale), precision), precision);
}

/** A number as render.py's fmt_number prints it: thousands separators, HALF_UP rounding when a precision is set. */
export function formatNumber(value: number, precision?: number): string {
  return formatDec(toDec(value), precision);
}

// ---- Aggregates ----------------------------------------------------------------

/**
 * formatMeasurement is render.py format_value: one measurement under a display
 * spec (runs, mean, range, min, max, run:<n>). The spec's precision wins, else
 * the measurement's recorded precision, else each value as recorded. With a
 * `view`, that view's per-measurement `display` override is applied on top, as
 * render.py's metric_cell does. Returns "—" for a run:<n> past the last run
 * (render.py raises there; lint keeps it out of the data).
 */
export function formatMeasurement(m: Measurement, spec: DisplaySpec = {}, view?: string): string {
  const merged: DisplaySpec = { ...spec, ...(view !== undefined ? m.display[view] : undefined) };
  const precision = merged.precision ?? m.precision;
  const approx = merged.approx_prefix || m.approx ? "~" : "";
  if (m.range) {
    return `${approx}${formatNumber(m.range[0], precision)}–${formatNumber(m.range[1], precision)}`;
  }
  const runs = (m.runs ?? []).map(toDec);
  if (runs.length === 0) return "—";
  const agg = merged.aggregate ?? "runs";
  if (agg === "runs") return approx + runs.map((r) => formatDec(r, precision)).join(" / ");
  if (agg === "mean") {
    const scale = Math.max(...runs.map((r) => r.scale));
    const sum = runs.reduce((acc, r) => acc + rescale(r, scale), 0n);
    const places = precision ?? scale;
    const den = BigInt(runs.length) * 10n ** BigInt(scale);
    return approx + formatScaled(roundRational(sum, den, places), places);
  }
  const sorted = [...runs].sort((a, b) => compareDec(a, b));
  const lo = sorted[0];
  const hi = sorted[sorted.length - 1];
  if (agg === "range") {
    if (compareDec(lo, hi) === 0) return approx + formatDec(lo, precision);
    return `${approx}${formatDec(lo, precision)}–${formatDec(hi, precision)}`;
  }
  if (agg === "min") return approx + formatDec(lo, precision);
  if (agg === "max") return approx + formatDec(hi, precision);
  const n = Number(agg.slice("run:".length));
  if (!Number.isInteger(n) || n < 1 || n > runs.length) return "—";
  return approx + formatDec(runs[n - 1], precision);
}

function compareDec(a: Dec, b: Dec): number {
  const scale = Math.max(a.scale, b.scale);
  const x = rescale(a, scale);
  const y = rescale(b, scale);
  return x < y ? -1 : x > y ? 1 : 0;
}

/**
 * aggregateNumber is render.py aggregate_value as a float, for plotting, sorting
 * and ratios: the mean of the runs (a range's midpoint), min, max or run:<n>.
 * null when the aggregate doesn't apply.
 */
export function aggregateNumber(m: Measurement, agg: Aggregate = "mean"): number | null {
  const values = m.runs ?? m.range ?? [];
  if (values.length === 0) return null;
  if (agg === "mean" || agg === "runs" || (m.range && agg === "range")) {
    return values.reduce((a, b) => a + b, 0) / values.length;
  }
  if (agg === "min") return Math.min(...values);
  if (agg === "max") return Math.max(...values);
  if (agg.startsWith("run:") && m.runs) {
    const n = Number(agg.slice("run:".length));
    return n >= 1 && n <= m.runs.length ? m.runs[n - 1] : null;
  }
  return null;
}

// ---- Ratios ----------------------------------------------------------------------

/** yours ÷ reference; null when either is missing or the reference is not positive. */
export function ratio(yours: number | null, reference: number | null): number | null {
  if (yours === null || reference === null || !Number.isFinite(yours) || !(reference > 0)) return null;
  return yours / reference;
}

/** "6.9×", "0.71×", "12×": two significant figures, so a label stays short and doesn't overclaim. */
export function formatRatio(value: number): string {
  if (value >= 10) return `${Math.round(value)}×`;
  return `${Number(value.toPrecision(2))}×`;
}

/**
 * nearestByRatio returns the candidate whose value is closest to `target` in
 * ratio terms (|log(value / target)|), so 100 is as near 200 as 50 is, which
 * matches how the page phrases a comparison. Non-positive values are skipped.
 */
export function nearestByRatio<T>(candidates: T[], value: (c: T) => number, target: number): T | null {
  if (!(target > 0)) return null;
  let best: T | null = null;
  let bestDistance = Infinity;
  for (const candidate of candidates) {
    const v = value(candidate);
    if (!(v > 0)) continue;
    const distance = Math.abs(Math.log(v / target));
    if (distance < bestDistance) {
      best = candidate;
      bestDistance = distance;
    }
  }
  return best;
}

// ---- Lookups -------------------------------------------------------------------

/** Lanes a config stands for: those it is the current or measured config of (render.py lane_for, all matches). */
/** Codepoint order, as Python's str sort, so the page orders ties the way render.py does (localeCompare doesn't). */
export function byCodepoint(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

export function lanesForConfig(data: ResultsData, configId: string): Lane[] {
  return Object.values(data.lanes).filter(
    (lane) => lane.current_config === configId || lane.measured_config === configId,
  );
}

export function isFixedHarness(m: Measurement): boolean {
  return m.harness === FIXED_HARNESS;
}

/** A short harness badge label: "fixed" for the comparable harness, else the harness id. */
export function harnessLabel(harness: string | undefined): string {
  if (harness === undefined) return "harness unrecorded";
  return harness === FIXED_HARNESS ? "fixed" : harness;
}

/** Measurements whose superseded_by points at one of `ids` (a config's predecessors). */
export function predecessorsOf(data: ResultsData, ids: Set<string>): Measurement[] {
  return data.measurements.filter((m) => m.superseded_by !== undefined && ids.has(m.superseded_by));
}
