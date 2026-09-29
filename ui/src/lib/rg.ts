// Pure helpers for the RG GPUs page (routes/RgGpus.svelte).

import type { RgFamilyChoice, RgHold, RgNode } from "./types";

/** The shortest hold rg-hold accepts; boot alone takes ~6 min. */
export const MIN_HOLD_MINUTES = 30;

/**
 * The duration text as the page sends it: trimmed and lower-cased, because
 * rg-api's pattern is case-sensitive and parseDuration accepts "2H".
 */
export function canonicalDuration(text: string): string {
  return text.trim().toLowerCase();
}

/**
 * Parse a hold duration such as "2h", "90m" or "1h30m" into minutes.
 * Returns null for anything else, and for less than MIN_HOLD_MINUTES.
 * The pattern matches rg-api's own check, so the page never sends what it
 * would reject.
 */
export function parseDuration(text: string): number | null {
  const match = /^(?:(\d+)h)?(?:(\d+)m)?$/.exec(canonicalDuration(text));
  if (!match || (match[1] === undefined && match[2] === undefined)) return null;
  const minutes = Number(match[1] ?? 0) * 60 + Number(match[2] ?? 0);
  return minutes >= MIN_HOLD_MINUTES ? minutes : null;
}

/**
 * How many cars to show for a decode speed against the DGX reference:
 * clamp(round(tokS / refTokS), 1, 8). `ratio` is the unrounded multiple,
 * for the "≈N× DGX" label. A missing reference counts as 1 car, 0×.
 */
export function speedCars(tokS: number, refTokS: number): { count: number; ratio: number } {
  if (!(tokS > 0) || !(refTokS > 0)) return { count: 1, ratio: 0 };
  const ratio = tokS / refTokS;
  return { count: Math.min(8, Math.max(1, Math.round(ratio))), ratio };
}

/**
 * True when rg-api could not read the hold list. Its `holds` is then empty
 * although holds may exist, and fetch_holds always prefixes its error "holds:".
 */
export function holdsUnavailable(errors: string[]): boolean {
  return errors.some((error) => error.startsWith("holds:"));
}

/** Format a remaining time as "1h 30m", "45m" or "<1m". */
export function formatTimeLeft(seconds: number): string {
  if (!(seconds >= 60)) return seconds > 0 ? "<1m" : "0m";
  const totalMinutes = Math.floor(seconds / 60);
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (hours === 0) return `${minutes}m`;
  return minutes === 0 ? `${hours}h` : `${hours}h ${minutes}m`;
}

/**
 * "~3 h" / "~2.5 h" for how long a busy node stays busy, measured from the
 * overview's own snapshot time (`generated_at`) rather than the client clock,
 * so it matches the data. Rounds half-up to 0.5 h; under 0.5 h reads
 * "< 30 min". Null when either time is missing or unparsable.
 */
export function busyForLabel(endIso: string | null | undefined, nowIso: string | null | undefined): string | null {
  const end = endIso ? Date.parse(endIso) : NaN;
  const now = nowIso ? Date.parse(nowIso) : NaN;
  if (Number.isNaN(end) || Number.isNaN(now)) return null;
  const hours = (end - now) / 3_600_000;
  if (hours < 0.5) return "< 30 min";
  return `~${Math.floor(hours * 2 + 0.5) / 2} h`;
}

export type BusyTone = "red" | "orange" | "yellow";

/**
 * How urgent a busy node's remaining time is, for the badge color: red at
 * 4 h or more (a long wait), orange from 1 h up to 4 h, yellow under 1 h
 * (including an end already past). Same hours math as `busyForLabel`; null when
 * either time is missing or unparsable, which keeps the neutral badge.
 */
export function busyTone(endIso: string | null | undefined, nowIso: string | null | undefined): BusyTone | null {
  const end = endIso ? Date.parse(endIso) : NaN;
  const now = nowIso ? Date.parse(nowIso) : NaN;
  if (Number.isNaN(end) || Number.isNaN(now)) return null;
  const hours = (end - now) / 3_600_000;
  if (hours >= 4) return "red";
  if (hours >= 1) return "orange";
  return "yellow";
}

export interface RgFamilyOption {
  value: RgFamilyChoice;
  label: string;
  /** True for the family of the node's default profile. */
  isDefault: boolean;
}

const familyLabels: Record<RgFamilyChoice, string> = {
  "flash-next": "Flash-Next",
  "27b": "Qwen 27B",
  "hold-only": "Hold only",
};

export function familyLabel(family: RgFamilyChoice): string {
  return familyLabels[family];
}

/**
 * The families a node can serve, the default profile's family first, then
 * "hold-only". A node with no profiles defaults to hold-only. Takes any object
 * with `profiles`, so the page can pass every node's profiles merged for
 * "best available".
 */
export function familyOptions(node: Pick<RgNode, "profiles">): RgFamilyOption[] {
  const defaultFamily = node.profiles.find((profile) => profile.default)?.family ?? null;
  const families = [...new Set(node.profiles.map((profile) => profile.family))];
  if (defaultFamily) families.sort((a, b) => Number(b === defaultFamily) - Number(a === defaultFamily));
  const options: RgFamilyOption[] = families.map((family) => ({
    value: family,
    label: familyLabels[family],
    isDefault: family === defaultFamily,
  }));
  options.push({ value: "hold-only", label: familyLabels["hold-only"], isDefault: defaultFamily === null });
  return options;
}

/**
 * Format an ISO timestamp as a local clock time, with the date when it is not
 * today ("14:30" or "Sep 29 14:30"). Null or unparsable input reads "unknown".
 */
export function formatClock(iso: string | null | undefined, now: Date = new Date()): string {
  const date = iso ? new Date(iso) : null;
  if (!date || Number.isNaN(date.getTime())) return "unknown";
  const time = date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  if (date.toDateString() === now.toDateString()) return time;
  return `${date.toLocaleDateString([], { month: "short", day: "numeric" })} ${time}`;
}

/**
 * Whole seconds since the overview was fetched, never negative. The page ticks
 * a "now" and advances every time-left by this, so labels count down between
 * fetches without a request.
 */
export function elapsedSeconds(fetchedAtMs: number, nowMs: number): number {
  return Math.max(0, Math.floor((nowMs - fetchedAtMs) / 1000));
}

/** An ISO time moved forward by `seconds`; undefined when missing or unparsable. */
export function advanceIso(iso: string | null | undefined, seconds: number): string | undefined {
  const ms = iso ? Date.parse(iso) : NaN;
  return Number.isNaN(ms) ? undefined : new Date(ms + seconds * 1000).toISOString();
}

/**
 * "1h 30m left", "< 1 min left" or "ending" for a hold's `time_left_s` as of
 * the fetch, advanced by the seconds elapsed since. Never negative. Null when
 * the time left is unknown.
 */
export function timeLeftText(timeLeftAtFetchS: number | null, elapsedS: number): string | null {
  if (timeLeftAtFetchS === null) return null;
  const left = timeLeftAtFetchS - elapsedS;
  if (!(left > 0)) return "ending";
  if (left < 60) return "< 1 min left";
  return `${formatTimeLeft(left)} left`;
}

/**
 * One line for a hold: "Pending, start by 14:30" or "Running · 1h 30m left".
 * Shared by the holds list and the node cards so the wording cannot diverge.
 * `elapsedS` counts the time left down since the overview was fetched.
 */
export function holdStatus(hold: Pick<RgHold, "state" | "start" | "time_left_s">, elapsedS = 0): string {
  if (hold.state === "PENDING") return `Pending, start by ${formatClock(hold.start)}`;
  const left = timeLeftText(hold.time_left_s, elapsedS);
  return `${hold.state[0]}${hold.state.slice(1).toLowerCase()}${left ? ` · ${left}` : ""}`;
}

/** Serving states of a launch still in progress, which naturally has no step yet. */
const LAUNCHING_STATES = new Set(["submitted", "pending", "running", "launching"]);

/**
 * Whether a hold shows the red "Idle" badge: idle holds only, except while its
 * serving launch is in progress. Hold-only, serving and unhealthy holds keep it.
 */
export function idleBadgeShown(hold: Pick<RgHold, "idle" | "serving">): boolean {
  if (!hold.idle) return false;
  return !(hold.serving && LAUNCHING_STATES.has(hold.serving.state.toLowerCase()));
}

/**
 * Background refresh runs every 2 min, and a page that turns visible refetches
 * at once when the last fetch is at least this old. One threshold for both
 * because a manual refresh just before a tick makes that tick redundant.
 */
export const REFRESH_INTERVAL_MS = 120_000;
export const MIN_REFRESH_AGE_MS = 60_000;

/**
 * Whether a background refetch should start now: the page is visible, no fetch
 * is in flight (fetches never overlap) and the last one is old enough.
 * `lastFetchAt` is null before the first fetch.
 */
export function refreshDue(
  state: { visible: boolean; inFlight: boolean; lastFetchAt: number | null; now: number },
  minAgeMs: number = MIN_REFRESH_AGE_MS,
): boolean {
  if (!state.visible || state.inFlight) return false;
  return state.lastFetchAt === null || state.now - state.lastFetchAt >= minAgeMs;
}

/**
 * Text for a failed grab or release. A proxy timeout or network error means
 * the action may still complete in rg-api, so the text says to check before a
 * retry (a retried "best" grab would place a second hold). Duck-types the
 * error code so lib/ does not import stores/.
 */
export function rgActionErrorText(cause: unknown, fallback: string): string {
  if (!(cause instanceof Error) && (typeof cause !== "object" || cause === null)) return fallback;
  const message = (cause as { message?: unknown }).message;
  const text = typeof message === "string" && message !== "" ? message : fallback;
  const code = (cause as { code?: unknown }).code;
  if (code === "rg_api_unreachable" || cause instanceof TypeError) {
    return `${text} — the action may still have gone through; check Your holds (refreshed) before trying again.`;
  }
  return cause instanceof Error ? cause.message : fallback;
}

/**
 * Whether the release dialog offers "Force release" after a refused release:
 * only for the reasons rg-api's forced teardown (`rg.sh down --force`) resolves,
 * never for another refusal, and never as the first action.
 */
export function forceReleaseOffered(code: string | null | undefined): boolean {
  return code === "down_failed" || code === "launch_in_progress" || code === "terminal_session";
}
