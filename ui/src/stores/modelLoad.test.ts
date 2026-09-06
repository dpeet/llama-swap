import { afterEach, describe, expect, it, vi } from "vitest";
import type { Model } from "../lib/types";

const { loadModel, unloadSingleModel } = vi.hoisted(() => ({
  loadModel: vi.fn(),
  unloadSingleModel: vi.fn(),
}));
vi.mock("./api", () => ({ loadModel, unloadSingleModel }));

import { onToggleLoad } from "./modelLoad";

function model(state: Model["state"], id = "m"): Model {
  return { id, state } as Model;
}

afterEach(() => {
  loadModel.mockReset();
  unloadSingleModel.mockReset();
});

describe("onToggleLoad", () => {
  it("cancels a starting model via the unload API, not just a client abort", () => {
    onToggleLoad(model("starting"));
    // Aborting the client request alone would leave the swap running server-side.
    expect(unloadSingleModel).toHaveBeenCalledWith("m");
    expect(loadModel).not.toHaveBeenCalled();
  });

  it("loads a stopped, non-pending model", () => {
    onToggleLoad(model("stopped"));
    expect(loadModel).toHaveBeenCalledWith("m", expect.anything());
    expect(unloadSingleModel).not.toHaveBeenCalled();
  });

  it("unloads a ready model", () => {
    onToggleLoad(model("ready"));
    expect(unloadSingleModel).toHaveBeenCalledWith("m");
    expect(loadModel).not.toHaveBeenCalled();
  });
});
