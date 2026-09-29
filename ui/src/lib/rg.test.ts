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
