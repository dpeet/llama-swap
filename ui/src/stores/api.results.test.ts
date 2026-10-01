import { afterEach, describe, expect, it, vi } from "vitest";
import { getResults, ResultsApiError } from "./api";

function response(status: number, body: string, contentType = "application/json") {
  return { ok: status < 400, status, text: async () => body, headers: new Headers({ "Content-Type": contentType }) };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("getResults", () => {
  it("fetches both files in parallel and parses the catalog", async () => {
    const fetchMock = vi.fn((url: string) =>
      Promise.resolve(url.endsWith("catalog.json") ? response(200, '{"version":1}') : response(200, "{}\n", "application/x-ndjson")),
    );
    vi.stubGlobal("fetch", fetchMock);
    await expect(getResults()).resolves.toEqual({ catalog: { version: 1 }, measurementsText: "{}\n" });
    expect(fetchMock.mock.calls.map((c) => c[0])).toEqual(["/api/results/catalog.json", "/api/results/measurements.jsonl"]);
  });

  it("throws the server's code and detail", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(response(404, '{"error":"results_not_configured","detail":"LLAMA_SWAP_RESULTS_DIR is unset"}')),
    );
    const error = await getResults().catch((e) => e);
    expect(error).toBeInstanceOf(ResultsApiError);
    expect(error.code).toBe("results_not_configured");
    expect(error.message).toBe("LLAMA_SWAP_RESULTS_DIR is unset");
    expect(error.status).toBe(404);
  });

  it("falls back to the status when the error body isn't JSON", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response(502, "Bad Gateway", "text/plain")));
    const error = await getResults().catch((e) => e);
    expect(error.code).toBeNull();
    expect(error.message).toBe("Results request failed: 502");
  });

  it("rejects a 2xx catalog that isn't JSON, and an HTML page", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response(200, "<html>", "text/plain")));
    expect((await getResults().catch((e) => e)).code).toBe("bad_response");
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response(200, "<html></html>", "text/html; charset=utf-8")));
    const error = await getResults().catch((e) => e);
    expect(error.code).toBe("bad_response");
    expect(error.message).toContain("HTML");
  });
});
