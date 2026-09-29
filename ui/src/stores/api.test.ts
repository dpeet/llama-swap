import { get } from "svelte/store";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  activeProfile,
  activityRevision,
  fetchPlaygroundModels,
  fetchProfiles,
  fetchTailcatStatus,
  getActivity,
  getHardware,
  getRgOverview,
  grabRg,
  releaseRg,
  RgApiError,
  handleAPIEventMessage,
  hasListedModels,
  inFlightRequests,
  inflightRequestEntries,
  models,
  playgroundModels,
  profileModels,
  profiles,
  selectorModels,
  setActiveProfile,
  tailcatStatus,
  uiConfig,
} from "./api";

afterEach(() => {
  vi.unstubAllGlobals();
  models.set([]);
  playgroundModels.set([]);
  profiles.set([]);
  activeProfile.set(null);
  tailcatStatus.set({ enabled: false, address: "", models: [] });
});

describe("tailcat api", () => {
  it("fetches status and publishes it for conditional navigation", async () => {
    const status = { enabled: true, address: "tcCaseSensitiveToken", models: ["chat"] };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => status }));

    await expect(fetchTailcatStatus()).resolves.toEqual(status);
    expect(fetch).toHaveBeenCalledWith("/api/tailcat");
    expect(get(tailcatStatus)).toEqual(status);
  });

  it("requests literal Tailcat source-prefix pagination", async () => {
    const page = { data: [], page: 2, limit: 10, total: 0, total_pages: 0 };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => page }));

    await getActivity({ srcPrefix: "tc:", page: 2, limit: 10, sort: "src", order: "asc" });
    expect(fetch).toHaveBeenCalledWith(
      "/api/metrics/activity?page=2&limit=10&sort=src&order=asc&src_prefix=tc%3A"
    );
  });
});

describe("hardware api", () => {
  it("fetches the hardware snapshot", async () => {
    const snapshot = {
      schema_version: 1,
      captured_at: "2026-08-03T12:00:00Z",
      capture: { scope: "inference_host", method: "detected", detector: { name: "llama-swap", version: "246" } },
      architecture: { name: "x86_64" },
      operating_system: { family: "linux", name: "Ubuntu", version: "24.04", kernel: "6.8" },
      environment: { kind: "native", name: null, version: null },
      cpu: { vendor: "AMD", model: "Ryzen", socket_count: 1, physical_core_count: 16, logical_thread_count: 32 },
      memory: { capacity_bytes: 1024 },
      accelerators: [],
    };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => snapshot }));
    await expect(getHardware()).resolves.toEqual(snapshot);
    expect(fetch).toHaveBeenCalledWith("/api/hardware");
  });

  it("rejects unavailable hardware", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 503 }));
    await expect(getHardware()).rejects.toThrow("Failed to fetch hardware: 503");
  });
});

describe("rg api", () => {
  const jsonResponse = (status: number, body: unknown) => ({ ok: status < 400, status, json: async () => body });

  it("fetches the overview", async () => {
    const overview = { nodes: [], holds: [], errors: [] };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, overview)));
    await expect(getRgOverview()).resolves.toEqual(overview);
    expect(fetch).toHaveBeenCalledWith("/api/rg/overview", undefined);
  });

  it("posts a grab as JSON", async () => {
    const placed = { status: "placed", job: "1" };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(202, placed)));
    const req = { node: "best", family: "flash-next", duration: "2h", caller: "page", mode: "page" } as const;
    await expect(grabRg(req)).resolves.toEqual(placed);
    expect(fetch).toHaveBeenCalledWith("/api/rg/grab", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
    });
  });

  it("posts a release with confirm", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, { ok: true })));
    await releaseRg("165537");
    expect(fetch).toHaveBeenCalledWith("/api/rg/release", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ job: "165537", confirm: true }),
    });
  });

  it("posts a forced release with force: true, and only when asked", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, { ok: true })));
    await releaseRg("165537", true);
    expect(fetch).toHaveBeenCalledWith("/api/rg/release", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ job: "165537", confirm: true, force: true }),
    });
  });

  it("throws the body's detail, keeping the code", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(jsonResponse(502, { error: "rg_api_unreachable", detail: "connection refused" })),
    );
    const error = await getRgOverview().catch((e) => e);
    expect(error).toBeInstanceOf(RgApiError);
    expect(error.message).toBe("connection refused");
    expect(error.code).toBe("rg_api_unreachable");
    expect(error.status).toBe(502);
  });

  it("rejects a 200 whose body is not JSON, rather than resolving to null", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => { throw new SyntaxError("Unexpected token <"); } }));
    const error = await getRgOverview().catch((e) => e);
    expect(error).toBeInstanceOf(RgApiError);
    expect(error.code).toBe("bad_response");
  });

  it("falls back to the reason, then the error code, then the status", async () => {
    const req = { node: "best", family: "27b", duration: "1h", caller: "page", mode: "page" } as const;
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(409, { status: "refused", reason: "refused:NODE_NOT_FREE" })));
    await expect(grabRg(req)).rejects.toThrow("refused:NODE_NOT_FREE");
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(404, { error: "rg_api_not_configured" })));
    await expect(getRgOverview()).rejects.toThrow("rg_api_not_configured");
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 500, json: async () => { throw new Error("not json"); } }));
    await expect(getRgOverview()).rejects.toThrow("RG request failed: 500");
  });
});

describe("api store event handling", () => {
  it("anchors model uptime to the browser clock on receipt", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-26T12:00:00Z"));
    try {
      handleAPIEventMessage(
        JSON.stringify({
          type: "modelStatus",
          data: JSON.stringify([
            // readySince is an hour in the future, as a server clock that runs
            // ahead would report it; uptimeMs is what the UI counts from.
            { id: "ready", state: "ready", readySince: "2026-09-26T13:00:00Z", uptimeMs: 90_000 },
            { id: "stopped", state: "stopped" },
          ]),
        })
      );
      const byId = Object.fromEntries(get(models).map((m) => [m.id, m]));
      expect(byId.ready.readyAt).toBe(Date.parse("2026-09-26T12:00:00Z") - 90_000);
      expect(byId.stopped.readyAt).toBeUndefined();
    } finally {
      vi.useRealTimers();
    }
  });

  it("parses inflight request entries", () => {
    inFlightRequests.set(0);
    inflightRequestEntries.set([]);

    handleAPIEventMessage(
      JSON.stringify({
        type: "inflight",
        data: JSON.stringify({
          operation: "snapshot",
          requests: [
            {
              id: "7",
              timestamp: "2026-07-03T00:00:00Z",
              model: "m1",
              req_path: "/v1/chat/completions",
              method: "POST",
              req_headers: { "User-Agent": "test-agent" },
              remote_ip: "203.0.113.9",
              resp_headers: {},
              resp_bytes: 0,
              elapsed_ms: 125,
              metadata: { source: "test" },
            },
          ],
        }),
      })
    );

    expect(get(inFlightRequests)).toBe(1);
    expect(get(inflightRequestEntries)).toEqual([
      {
        id: "7",
        timestamp: "2026-07-03T00:00:00Z",
        model: "m1",
        req_path: "/v1/chat/completions",
        method: "POST",
        req_headers: { "User-Agent": "test-agent" },
        remote_ip: "203.0.113.9",
        resp_headers: {},
        resp_bytes: 0,
        elapsed_ms: 125,
        client_received_at_ms: expect.any(Number),
        metadata: { source: "test" },
      },
    ]);
  });

  it("upserts and removes inflight entries by id", () => {
    handleAPIEventMessage(JSON.stringify({
      type: "inflight",
      data: JSON.stringify({
        operation: "upsert",
        request: {
          id: "7",
          timestamp: "2026-07-03T00:00:00Z",
          model: "m1",
          req_path: "/v1/chat/completions",
          method: "POST",
          req_headers: {},
          remote_ip: "203.0.113.9",
          resp_headers: { "Content-Type": "text/event-stream" },
          resp_bytes: 42,
          elapsed_ms: 250,
        },
      }),
    }));

    expect(get(inflightRequestEntries)).toHaveLength(1);
    expect(get(inflightRequestEntries)[0].resp_bytes).toBe(42);

    handleAPIEventMessage(JSON.stringify({
      type: "inflight",
      data: JSON.stringify({ operation: "remove", id: "7" }),
    }));
    expect(get(inflightRequestEntries)).toEqual([]);
    expect(get(inFlightRequests)).toBe(0);
  });

  it("parses UI activity configuration", () => {
    handleAPIEventMessage(JSON.stringify({
      type: "uiConfig",
      data: JSON.stringify({ activity: { session_id: ["X-Trace-ID"] } }),
    }));
    expect(get(uiConfig).activity.session_id).toEqual(["X-Trace-ID"]);
  });

  it("increments activity revision for activity events", () => {
    activityRevision.set(0);

    handleAPIEventMessage(
      JSON.stringify({
        type: "activity",
        data: JSON.stringify({ id: 42 }),
      })
    );

    expect(get(activityRevision)).toBe(1);
  });

  it("applies profile change events", () => {
    activeProfile.set(null);
    handleAPIEventMessage(JSON.stringify({
      type: "profileChanged",
      data: JSON.stringify({ active: "coding" }),
    }));
    expect(get(activeProfile)).toBe("coding");
  });

  it("loads and switches profiles", async () => {
    const mockFetch = vi.fn()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          active: null,
          profiles: [{ id: "coding", description: "Coding", pins: { llm: "real" } }],
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ active: "coding" }),
      });
    vi.stubGlobal("fetch", mockFetch);

    await fetchProfiles();
    expect(get(profiles)).toHaveLength(1);
    expect(get(activeProfile)).toBeNull();

    await setActiveProfile("coding");
    expect(get(activeProfile)).toBe("coding");
    expect(mockFetch).toHaveBeenLastCalledWith("/api/profiles/active", expect.objectContaining({
      method: "PUT",
      body: JSON.stringify({ name: "coding" }),
    }));
  });

  it("loads Playground models and virtual model types from v1/models", async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        data: [
          {
            id: "real",
            name: "Real",
            capabilities: { vision: true },
            architecture: { input_modalities: ["text", "image"], output_modalities: ["text"] },
            meta: { llamaswap: { type: "model", aliases: ["variant", "alternate"] } },
          },
          {
            id: "variant",
            meta: { llamaswap: { type: "alias", modelID: "real" } },
          },
          {
            id: "remote/remote-model",
            meta: { llamaswap: { type: "peer", peerID: "remote" } },
          },
          {
            id: "pool",
            meta: {
              llamaswap: {
                type: "selector",
                strategy: "spillover",
                targets: ["real", "remote/remote-model"],
                spillover: 4,
              },
            },
          },
          {
            id: "public",
            meta: { llamaswap: { type: "profile" } },
          },
        ],
      }),
    });
    vi.stubGlobal("fetch", mockFetch);

    await fetchPlaygroundModels();

    expect(mockFetch).toHaveBeenCalledWith("/v1/models");
    expect(get(playgroundModels).map((model) => model.id)).not.toContain("variant");
    expect(get(playgroundModels).find((model) => model.id === "real")).toMatchObject({
      aliases: ["variant", "alternate"],
      capabilities: { vision: true },
      modalities: { in: ["text", "image"], out: ["text"] },
      playgroundType: "model",
    });
    // Models without an architecture block report no modalities at all.
    expect(get(playgroundModels).find((model) => model.id === "remote/remote-model")).toMatchObject({
      modalities: { in: [], out: [] },
    });
    expect(get(playgroundModels).find((model) => model.id === "remote/remote-model")).toMatchObject({
      peerID: "remote",
      playgroundType: "peer",
    });
    expect(get(selectorModels).map((model) => model.id)).toEqual(["pool"]);
    expect(get(selectorModels)[0]).toMatchObject({
      strategy: "spillover",
      targets: ["real", "remote/remote-model"],
      spillover: 4,
    });
    expect(get(profileModels).map((model) => model.id)).toEqual(["public"]);
    expect(get(hasListedModels)).toBe(true);

    playgroundModels.set([]);
    expect(get(selectorModels)).toEqual([]);
    expect(get(profileModels)).toEqual([]);
    expect(get(hasListedModels)).toBe(false);
  });

  it("coalesces overlapping Playground model refreshes", async () => {
    type ModelResponse = {
      ok: boolean;
      json: () => Promise<{ data: [] }>;
    };
    let resolveFirst!: (response: ModelResponse) => void;
    const firstResponse = new Promise<ModelResponse>((resolve) => {
      resolveFirst = resolve;
    });
    const mockFetch = vi.fn()
      .mockReturnValueOnce(firstResponse)
      .mockResolvedValue({
        ok: true,
        json: async () => ({ data: [] }),
      });
    vi.stubGlobal("fetch", mockFetch);

    const first = fetchPlaygroundModels();
    const overlapping = fetchPlaygroundModels();

    expect(overlapping).toBe(first);
    expect(mockFetch).toHaveBeenCalledTimes(1);

    resolveFirst({
      ok: true,
      json: async () => ({ data: [] }),
    });
    await first;

    await vi.waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2));
  });
});
