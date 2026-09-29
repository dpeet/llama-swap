import { describe, it, expect } from "vitest";
import {
  parseDuration,
  canonicalDuration,
  rgActionErrorText,
  holdStatus,
  speedCars,
  formatTimeLeft,
  formatClock,
  familyOptions,
  familyLabel,
  holdsUnavailable,
  forceReleaseOffered,
  busyForLabel,
  busyTone,
  elapsedSeconds,
  advanceIso,
  timeLeftText,
  refreshDue,
  idleBadgeShown,
  nodeDisplayState,
  startByBound,
  queuedHoldLine,
  MIN_REFRESH_AGE_MS,
  busyTarget,
  busyBadgeLabel,
  nodePopoverDetails,
} from "./rg";
import type { RgProfile } from "./types";

function profile(id: string, family: RgProfile["family"], isDefault = false): RgProfile {
  return { id, family, label: id, tok_s: 100, speedup: 3, ctx_limit: 1000, default: isDefault };
}

describe("parseDuration", () => {
  it("parses hours, minutes and both", () => {
    expect(parseDuration("2h")).toBe(120);
    expect(parseDuration("90m")).toBe(90);
    expect(parseDuration("1h30m")).toBe(90);
    expect(parseDuration("24h")).toBe(1440);
  });

  it("accepts the 30 minute floor and rejects less", () => {
    expect(parseDuration("30m")).toBe(30);
    expect(parseDuration("29m")).toBeNull();
    expect(parseDuration("0m")).toBeNull();
    expect(parseDuration("0h29m")).toBeNull();
  });

  it("ignores surrounding whitespace and case", () => {
    expect(parseDuration("  2H ")).toBe(120);
  });

  it("rejects empty and malformed input", () => {
    expect(parseDuration("")).toBeNull();
    expect(parseDuration("   ")).toBeNull();
    expect(parseDuration("2x")).toBeNull();
    expect(parseDuration("2")).toBeNull();
    expect(parseDuration("m30")).toBeNull();
    expect(parseDuration("1m30h")).toBeNull();
    expect(parseDuration("1.5h")).toBeNull();
    expect(parseDuration("-2h")).toBeNull();
  });
});

describe("speedCars", () => {
  it("rounds tok/s over the reference", () => {
    const { count, ratio } = speedCars(209.6, 30.1);
    expect(count).toBe(7);
    expect(ratio).toBeCloseTo(6.96, 2);
  });

  it("clamps to at least 1 car", () => {
    expect(speedCars(10, 30.1).count).toBe(1);
    expect(speedCars(10, 30.1).ratio).toBeCloseTo(0.33, 2);
  });

  it("clamps to at most 8 cars and keeps the true ratio", () => {
    const { count, ratio } = speedCars(500, 30);
    expect(count).toBe(8);
    expect(ratio).toBeCloseTo(16.67, 2);
  });

  it("rounds at the .5 edges", () => {
    expect(speedCars(45, 30).count).toBe(2);
    expect(speedCars(44.9, 30).count).toBe(1);
  });

  it("degrades to 1 car when the inputs are unusable", () => {
    expect(speedCars(0, 30)).toEqual({ count: 1, ratio: 0 });
    expect(speedCars(100, 0)).toEqual({ count: 1, ratio: 0 });
    expect(speedCars(NaN, 30)).toEqual({ count: 1, ratio: 0 });
  });
});

describe("formatTimeLeft", () => {
  it("formats hours and minutes", () => {
    expect(formatTimeLeft(5400)).toBe("1h 30m");
    expect(formatTimeLeft(7200)).toBe("2h");
    expect(formatTimeLeft(45 * 60)).toBe("45m");
  });

  it("handles the small and empty edges", () => {
    expect(formatTimeLeft(59)).toBe("<1m");
    expect(formatTimeLeft(60)).toBe("1m");
    expect(formatTimeLeft(0)).toBe("0m");
    expect(formatTimeLeft(-5)).toBe("0m");
    expect(formatTimeLeft(NaN)).toBe("0m");
  });
});

describe("familyOptions", () => {
  it("lists the default family first, then the rest, then hold-only", () => {
    const options = familyOptions({
      profiles: [profile("q27", "27b"), profile("fn", "flash-next", true)],
    });
    expect(options.map((o) => o.value)).toEqual(["flash-next", "27b", "hold-only"]);
    expect(options.filter((o) => o.isDefault).map((o) => o.value)).toEqual(["flash-next"]);
  });

  it("does not repeat a family that has several profiles", () => {
    const options = familyOptions({
      profiles: [profile("a", "flash-next", true), profile("b", "flash-next")],
    });
    expect(options.map((o) => o.value)).toEqual(["flash-next", "hold-only"]);
  });

  it("defaults to hold-only when the node has no profiles", () => {
    const options = familyOptions({ profiles: [] });
    expect(options).toEqual([{ value: "hold-only", label: "Hold only", isDefault: true }]);
  });

  it("labels each family", () => {
    expect(familyLabel("flash-next")).toBe("Flash-Next");
    expect(familyLabel("27b")).toBe("Qwen 27B");
  });
});

describe("formatClock", () => {
  const now = new Date(2026, 8, 28, 12, 0, 0);

  it("reads unknown for missing or unparsable input", () => {
    expect(formatClock(null, now)).toBe("unknown");
    expect(formatClock(undefined, now)).toBe("unknown");
    expect(formatClock("", now)).toBe("unknown");
    expect(formatClock("soon", now)).toBe("unknown");
  });

  it("shows only the time for today", () => {
    const today = new Date(2026, 8, 28, 14, 30, 0).toISOString();
    expect(formatClock(today, now)).toBe(new Date(today).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }));
  });

  it("adds the date for another day", () => {
    const later = new Date(2026, 8, 29, 9, 5, 0).toISOString();
    const text = formatClock(later, now);
    expect(text).toContain(new Date(later).toLocaleDateString([], { month: "short", day: "numeric" }));
    expect(text.length).toBeGreaterThan(formatClock(new Date(2026, 8, 28, 9, 5, 0).toISOString(), now).length);
  });
});

describe("canonicalDuration", () => {
  it("trims and lower-cases, so what the page sends matches rg-api's case-sensitive pattern", () => {
    const sent = canonicalDuration("  2H ");
    expect(sent).toBe("2h");
    expect(sent).toMatch(/^([0-9]+h)?([0-9]+m)?$/);
  });
});

describe("rgActionErrorText", () => {
  it("warns that the action may have gone through after a proxy timeout", () => {
    const cause = { code: "rg_api_unreachable", message: "net/http: timeout awaiting response headers" };
    expect(rgActionErrorText(cause, "Grab failed")).toContain("may still have gone through");
    expect(rgActionErrorText(cause, "Grab failed")).toContain("net/http: timeout awaiting response headers");
  });

  it("warns the same after a fetch network error", () => {
    expect(rgActionErrorText(new TypeError("Failed to fetch"), "Grab failed")).toContain("may still have gone through");
  });

  it("returns other errors' messages unchanged, and the fallback for non-errors", () => {
    const refused = Object.assign(new Error("refused:NODE_NOT_FREE"), { code: "refused:NODE_NOT_FREE" });
    expect(rgActionErrorText(refused, "Grab failed")).toBe("refused:NODE_NOT_FREE");
    expect(rgActionErrorText("boom", "Release failed")).toBe("Release failed");
  });
});

describe("holdStatus", () => {
  it("says when a pending hold should start", () => {
    expect(holdStatus({ state: "PENDING", start: null, time_left_s: null })).toBe("Pending, start by unknown");
  });

  it("shows time left for a running hold, and omits it when unknown", () => {
    expect(holdStatus({ state: "RUNNING", start: null, time_left_s: 5400 })).toBe("Running · 1h 30m left");
    expect(holdStatus({ state: "RUNNING", start: null, time_left_s: null })).toBe("Running");
  });
});

describe("holdsUnavailable", () => {
  it("is true when the hold-status source failed", () => {
    expect(holdsUnavailable(["holds: rg-hold status exit 3"])).toBe(true);
    expect(holdsUnavailable(["catalog: x", "holds: y"])).toBe(true);
  });
  it("is false for other source failures or none", () => {
    expect(holdsUnavailable(["catalog: x"])).toBe(false);
    expect(holdsUnavailable([])).toBe(false);
  });
});

describe("forceReleaseOffered", () => {
  it("is offered only for the refusals a scripted forced teardown resolves", () => {
    expect(forceReleaseOffered("down_failed")).toBe(true);
    expect(forceReleaseOffered("launch_in_progress")).toBe(true);
    expect(forceReleaseOffered("terminal_session")).toBe(true);
  });

  it("is not offered for any other refusal or error, nor without a code", () => {
    expect(forceReleaseOffered("release_in_progress")).toBe(false);
    expect(forceReleaseOffered("refused:NOT_LIVE")).toBe(false);
    expect(forceReleaseOffered("endpoint_error")).toBe(false);
    expect(forceReleaseOffered("rg_api_unreachable")).toBe(false);
    expect(forceReleaseOffered("forbidden")).toBe(false);
    expect(forceReleaseOffered(null)).toBe(false);
    expect(forceReleaseOffered(undefined)).toBe(false);
  });
});

describe("busyForLabel", () => {
  const now = "2026-09-29T12:00:00Z";
  // End time `hours` after `now`, as rg-api sends it (UTC with Z).
  const endIn = (hours: number) => new Date(Date.parse(now) + hours * 3_600_000).toISOString();

  it("shows exact hours without a trailing .0", () => {
    expect(busyForLabel(endIn(3), now)).toBe("~3 h");
    expect(busyForLabel(endIn(12), now)).toBe("~12 h");
  });

  it("rounds half-up to the nearest 0.5 h", () => {
    expect(busyForLabel(endIn(2.24), now)).toBe("~2 h");
    expect(busyForLabel(endIn(2.25), now)).toBe("~2.5 h");
    expect(busyForLabel(endIn(2.5), now)).toBe("~2.5 h");
    expect(busyForLabel(endIn(2.74), now)).toBe("~2.5 h");
    expect(busyForLabel(endIn(2.75), now)).toBe("~3 h");
    expect(busyForLabel(endIn(11.9), now)).toBe("~12 h");
  });

  it("says under 30 min when less than half an hour is left or the end has passed", () => {
    expect(busyForLabel(endIn(0.2), now)).toBe("< 30 min");
    expect(busyForLabel(endIn(0.49), now)).toBe("< 30 min");
    expect(busyForLabel(endIn(-1), now)).toBe("< 30 min");
    expect(busyForLabel(endIn(0), now)).toBe("< 30 min");
  });

  it("shows ~0.5 h from exactly half an hour", () => {
    expect(busyForLabel(endIn(0.5), now)).toBe("~0.5 h");
    expect(busyForLabel(endIn(0.74), now)).toBe("~0.5 h");
  });

  it("prefixes with ≥ instead of ~ when incomplete", () => {
    expect(busyForLabel(endIn(3), now, true)).toBe("≥3 h");
  });

  it("says the end is unknown when incomplete and the known part is under 30 min", () => {
    expect(busyForLabel(endIn(0.2), now, true)).toBe("end unknown");
    expect(busyForLabel(endIn(-1), now, true)).toBe("end unknown");
    expect(busyForLabel(endIn(0.5), now, true)).toBe("≥0.5 h");
  });

  it("returns null for a missing or invalid end or snapshot time", () => {
    expect(busyForLabel(null, now)).toBeNull();
    expect(busyForLabel(undefined, now)).toBeNull();
    expect(busyForLabel("", now)).toBeNull();
    expect(busyForLabel("not a date", now)).toBeNull();
    expect(busyForLabel(endIn(3), "not a date")).toBeNull();
  });
});

describe("busyTone", () => {
  const now = "2026-09-29T12:00:00Z";
  const endIn = (hours: number) => new Date(Date.parse(now) + hours * 3_600_000).toISOString();

  it("is red from 4 h up", () => {
    expect(busyTone(endIn(12), now)).toBe("red");
    expect(busyTone(endIn(4), now)).toBe("red");
  });

  it("is orange from 1 h up to just under 4 h", () => {
    expect(busyTone(endIn(3.99), now)).toBe("orange");
    expect(busyTone(endIn(1), now)).toBe("orange");
  });

  it("is yellow under 1 h, including an end already past", () => {
    expect(busyTone(endIn(0.99), now)).toBe("yellow");
    expect(busyTone(endIn(-1), now)).toBe("yellow");
  });

  it("is null for a missing or invalid end or snapshot time", () => {
    expect(busyTone(null, now)).toBeNull();
    expect(busyTone(undefined, now)).toBeNull();
    expect(busyTone("not a date", now)).toBeNull();
    expect(busyTone(endIn(3), "not a date")).toBeNull();
  });
});

describe("elapsedSeconds", () => {
  it("is the whole seconds between the fetch and now", () => {
    expect(elapsedSeconds(1_000, 31_000)).toBe(30);
  });

  it("never goes negative, such as after a clock step back", () => {
    expect(elapsedSeconds(31_000, 1_000)).toBe(0);
  });
});

describe("advanceIso", () => {
  it("moves an ISO time forward by seconds", () => {
    expect(advanceIso("2026-09-29T12:00:00Z", 90)).toBe("2026-09-29T12:01:30.000Z");
  });

  it("passes a missing or unparsable time through as undefined", () => {
    expect(advanceIso(undefined, 30)).toBeUndefined();
    expect(advanceIso("not a date", 30)).toBeUndefined();
  });
});

describe("timeLeftText", () => {
  it("counts down from the time left at fetch", () => {
    expect(timeLeftText(5400, 0)).toBe("1h 30m left");
    expect(timeLeftText(5400, 1800)).toBe("1h left");
    expect(timeLeftText(5400, 4500)).toBe("15m left");
  });

  it("floors at under a minute, then ending, never negative", () => {
    expect(timeLeftText(100, 45)).toBe("< 1 min left");
    expect(timeLeftText(100, 100)).toBe("ending");
    expect(timeLeftText(100, 5000)).toBe("ending");
    expect(timeLeftText(0, 0)).toBe("ending");
  });

  it("is null when the time left is unknown", () => {
    expect(timeLeftText(null, 30)).toBeNull();
  });
});

describe("countdown of the busy label and tone", () => {
  const generatedAt = "2026-09-29T12:00:00Z";
  const end = "2026-09-29T16:00:00Z"; // 4 h after the snapshot

  it("ticks the label and tone down as time passes since the fetch", () => {
    expect(busyForLabel(end, advanceIso(generatedAt, 0))).toBe("~4 h");
    expect(busyTone(end, advanceIso(generatedAt, 0))).toBe("red");
    expect(busyForLabel(end, advanceIso(generatedAt, 1800))).toBe("~3.5 h");
    expect(busyTone(end, advanceIso(generatedAt, 1800))).toBe("orange");
    expect(busyTone(end, advanceIso(generatedAt, 3 * 3600 + 60))).toBe("yellow");
  });

  it("floors at under 30 min once the end has passed", () => {
    expect(busyForLabel(end, advanceIso(generatedAt, 5 * 3600))).toBe("< 30 min");
    expect(busyTone(end, advanceIso(generatedAt, 5 * 3600))).toBe("yellow");
  });
});

describe("holdStatus countdown", () => {
  it("advances the time left by the elapsed seconds", () => {
    expect(holdStatus({ state: "RUNNING", start: null, time_left_s: 5400 }, 1800)).toBe("Running · 1h left");
    expect(holdStatus({ state: "RUNNING", start: null, time_left_s: 5400 }, 6000)).toBe("Running · ending");
  });

  it("leaves a pending hold alone", () => {
    expect(holdStatus({ state: "PENDING", start: null, time_left_s: null }, 600)).toBe("Pending, start by unknown");
  });
});

describe("refreshDue", () => {
  const now = 1_000_000;
  const base = { visible: true, inFlight: false, lastFetchAt: now - MIN_REFRESH_AGE_MS, now };

  it("is due when visible, idle and the last fetch is old enough", () => {
    expect(refreshDue(base)).toBe(true);
    expect(refreshDue({ ...base, lastFetchAt: null })).toBe(true);
  });

  it("is not due while hidden or while a fetch is in flight", () => {
    expect(refreshDue({ ...base, visible: false })).toBe(false);
    expect(refreshDue({ ...base, inFlight: true })).toBe(false);
  });

  it("is not due when the last fetch is fresher than the minimum age", () => {
    expect(refreshDue({ ...base, lastFetchAt: now - MIN_REFRESH_AGE_MS + 1 })).toBe(false);
  });
});

describe("idleBadgeShown", () => {
  const serving = (state: string) => ({ profile: "p", state, port: 1, error: null });

  it("is hidden while a launch is in progress", () => {
    for (const state of ["submitted", "pending", "running", "launching", "Running"]) {
      expect(idleBadgeShown({ idle: true, serving: serving(state) })).toBe(false);
    }
  });

  it("stays for hold-only and for serving or unhealthy holds", () => {
    expect(idleBadgeShown({ idle: true, serving: null })).toBe(true);
    expect(idleBadgeShown({ idle: true, serving: serving("serving") })).toBe(true);
    expect(idleBadgeShown({ idle: true, serving: serving("unhealthy") })).toBe(true);
  });

  it("is hidden for a hold that is not idle", () => {
    expect(idleBadgeShown({ idle: false, serving: null })).toBe(false);
  });
});

describe("nodeDisplayState", () => {
  const pending = { state: "PENDING" };
  const running = { state: "RUNNING" };

  it("maps the other availabilities straight through", () => {
    expect(nodeDisplayState("free", [])).toBe("free");
    expect(nodeDisplayState("busy", [])).toBe("busy");
    expect(nodeDisplayState("unavailable", [])).toBe("unavailable");
    expect(nodeDisplayState("unknown", [])).toBe("unknown");
  });

  it("is queued when ours only because our hold is pending", () => {
    expect(nodeDisplayState("ours", [pending])).toBe("queued");
  });

  it("is yours when our hold is running, even with another pending", () => {
    expect(nodeDisplayState("ours", [running])).toBe("yours");
    expect(nodeDisplayState("ours", [pending, running])).toBe("yours");
  });

  it("stays yours when no hold is listed, because the hold list may have failed", () => {
    expect(nodeDisplayState("ours", [])).toBe("yours");
  });

  it("ignores holds unless availability says ours", () => {
    expect(nodeDisplayState("busy", [pending])).toBe("busy");
  });
});

describe("startByBound", () => {
  const now = "2026-09-29T12:00:00Z";
  const startIn = (hours: number) => new Date(Date.parse(now) + hours * 3_600_000).toISOString();

  it("marks busyForLabel's rounded hours as an upper bound", () => {
    expect(startByBound(startIn(3), now)).toBe("≤ ~3 h");
    expect(startByBound(startIn(2.6), now)).toBe("≤ ~2.5 h");
  });

  it("reads under 30 min, also once the estimate has passed", () => {
    expect(startByBound(startIn(0.2), now)).toBe("≤ 30 min");
    expect(startByBound(startIn(-1), now)).toBe("≤ 30 min");
  });

  it("is null when a time is missing or unparsable", () => {
    expect(startByBound(null, now)).toBeNull();
    expect(startByBound("garbage", now)).toBeNull();
    expect(startByBound(startIn(3), undefined)).toBeNull();
  });

  it("counts down with the tick", () => {
    const start = startIn(4);
    expect(startByBound(start, advanceIso(now, 0))).toBe("≤ ~4 h");
    expect(startByBound(start, advanceIso(now, 1800))).toBe("≤ ~3.5 h");
  });
});

describe("busyBadgeLabel", () => {
  const now = "2026-09-29T12:00:00Z";
  const endIn = (hours: number) => new Date(Date.parse(now) + hours * 3_600_000).toISOString();

  it("reads Busy with the estimate", () => {
    expect(busyBadgeLabel(endIn(3), now)).toBe("Busy ~3 h");
    expect(busyBadgeLabel(endIn(3), now, true)).toBe("Busy ≥3 h");
    expect(busyBadgeLabel(endIn(0.2), now)).toBe("Busy < 30 min");
  });

  it("reads Busy, end unknown when incomplete and under 30 min", () => {
    expect(busyBadgeLabel(endIn(0.2), now, true)).toBe("Busy, end unknown");
  });

  it("reads plain Busy without a usable time", () => {
    expect(busyBadgeLabel(null, now)).toBe("Busy");
    expect(busyBadgeLabel(endIn(3), undefined)).toBe("Busy");
  });
});

describe("busyTarget", () => {
  const end = "2026-09-29T13:00:00Z";
  const freeBy = "2026-09-29T16:00:00Z";

  it("uses free_by when busy and available", () => {
    const node = { running: { job: "1", user: "a", end }, free_by: freeBy, free_by_complete: false };
    expect(busyTarget(node, "busy")).toEqual({ iso: freeBy, incomplete: true });
  });

  it("falls back to running.end when free_by is missing", () => {
    const node = { running: { job: "1", user: "a", end }, free_by: null };
    expect(busyTarget(node, "busy")).toEqual({ iso: end, incomplete: false });
  });

  it("uses running.end when queued, ignoring free_by", () => {
    const node = { running: { job: "1", user: "a", end }, free_by: freeBy, free_by_complete: true };
    expect(busyTarget(node, "queued")).toEqual({ iso: end, incomplete: false });
  });

  it("returns undefined without running or free_by", () => {
    const node = { running: null, free_by: null };
    expect(busyTarget(node, "busy")).toEqual({ iso: undefined, incomplete: false });
  });
});

describe("nodePopoverDetails", () => {
  const now = new Date("2026-09-29T12:00:00Z");
  const busyNow = now.toISOString();

  it("formats running, queued, and free_by lines", () => {
    const node = {
      running: { job: "1", user: "alice", end: "2026-09-29T13:00:00Z" },
      queue: [
        { job: "2", user: "bob", start_by: "2026-09-29T13:00:00Z", end_by: "2026-09-29T14:30:00Z" },
      ],
      free_by: "2026-09-29T14:30:00Z",
      free_by_complete: true,
    };
    const rows = nodePopoverDetails(node, busyNow, now);
    expect(rows).toEqual([
      "Running: alice · ends in ~1 h",
      `Queued: bob · starts by ${formatClock("2026-09-29T13:00:00Z", now)} · ends by ${formatClock("2026-09-29T14:30:00Z", now)}`,
      `Free for a new hold by ${formatClock("2026-09-29T14:30:00Z", now)} (~2.5 h, if every job runs its full limit)`,
    ]);
  });

  it("uses ≥ when free_by_complete is false", () => {
    const node = {
      running: null,
      queue: [],
      free_by: "2026-09-29T15:00:00Z",
      free_by_complete: false,
    };
    const rows = nodePopoverDetails(node, busyNow, now);
    expect(rows).toEqual([
      `Free for a new hold no earlier than ${formatClock("2026-09-29T15:00:00Z", now)} (≥3 h, if every job runs its full limit)`,
    ]);
  });

  it("does not claim a free time when incomplete and under 30 min", () => {
    const node = { running: null, queue: [], free_by: "2026-09-29T12:10:00Z", free_by_complete: false };
    expect(nodePopoverDetails(node, busyNow, now)).toEqual(["Free time unknown: some job ends are not known"]);
  });

  it("omits rows and parts that are missing", () => {
    const node = {
      running: { job: "1", user: "alice", end: "2026-09-29T13:00:00Z" },
      queue: [{ job: "2", user: "bob", start_by: null }],
      free_by: null,
    };
    const rows = nodePopoverDetails(node, busyNow, now);
    expect(rows).toEqual([
      "Running: alice · ends in ~1 h",
      "Queued: bob",
    ]);
  });
});

describe("queuedHoldLine", () => {
  const now = "2026-09-29T12:00:00Z";
  const start = "2026-09-29T15:00:00Z";

  it("gives the start-by time and its upper bound", () => {
    expect(queuedHoldLine({ start }, now)).toBe(`queued · start by ${formatClock(start)} (≤ ~3 h)`);
  });

  it("drops the bound when it cannot be computed", () => {
    expect(queuedHoldLine({ start }, undefined)).toBe(`queued · start by ${formatClock(start)}`);
  });

  it("says so when Slurm gave no estimate", () => {
    expect(queuedHoldLine({ start: null }, now)).toBe("queued · no start estimate");
  });
});

describe("holdStatus start-by bound", () => {
  const now = "2026-09-29T12:00:00Z";
  const start = "2026-09-29T15:00:00Z";

  it("adds the upper bound to a pending hold when now is given", () => {
    expect(holdStatus({ state: "PENDING", start, time_left_s: null }, 0, now)).toBe(
      `Pending, start by ${formatClock(start)} (≤ ~3 h)`,
    );
  });

  it("leaves a running hold alone", () => {
    expect(holdStatus({ state: "RUNNING", start, time_left_s: 5400 }, 0, now)).toBe("Running · 1h 30m left");
  });
});
