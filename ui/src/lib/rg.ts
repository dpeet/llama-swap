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
 * One line for a hold: "Pending, start by 14:30" or "Running · 1h 30m left".
 * Shared by the holds list and the node cards so the wording cannot diverge.
 */
export function holdStatus(hold: Pick<RgHold, "state" | "start" | "time_left_s">): string {
  if (hold.state === "PENDING") return `Pending, start by ${formatClock(hold.start)}`;
  const left = hold.time_left_s === null ? "" : ` · ${formatTimeLeft(hold.time_left_s)} left`;
  return `${hold.state[0]}${hold.state.slice(1).toLowerCase()}${left}`;
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
