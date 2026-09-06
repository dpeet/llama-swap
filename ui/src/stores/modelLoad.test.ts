import { afterEach, describe, expect, it, vi } from "vitest";
import type { Model } from "../lib/types";

const { loadModel, unloadSingleModel } = vi.hoisted(() => ({
  loadModel: vi.fn(),
  unloadSingleModel: vi.fn(),
}));
vi.mock("./api", () => ({ loadModel, unloadSingleModel }));

import { loadingMessage, loadingPercent, onToggleLoad } from "./modelLoad";

function modelWith(fields: Partial<Model>): Model {
  return { id: "m", state: "stopped", name: "", description: "", unlisted: false, peerID: "", ...fields };
}

describe("loadingPercent", () => {
  it("rounds the reported fraction down to a whole percentage", () => {
    expect(loadingPercent(modelWith({ state: "starting", loadingProgress: 0.426 }))).toBe(42);
    expect(loadingPercent(modelWith({ state: "starting", loadingProgress: 0 }))).toBe(0);
    // kyojin caps progress at 0.999 until it is ready
    expect(loadingPercent(modelWith({ state: "starting", loadingProgress: 0.999 }))).toBe(99);
  });

  it("is undefined without progress or once the model is no longer starting", () => {
    expect(loadingPercent(modelWith({ state: "starting" }))).toBeUndefined();
    expect(loadingPercent(modelWith({ state: "ready", loadingProgress: 1 }))).toBeUndefined();
    expect(loadingPercent(undefined)).toBeUndefined();
  });
});

describe("loadingMessage", () => {
  it("is only shown while starting", () => {
    expect(loadingMessage(modelWith({ state: "starting", loadingMessage: "loading tensors" }))).toBe("loading tensors");
    expect(loadingMessage(modelWith({ state: "ready", loadingMessage: "loading tensors" }))).toBe("");
    expect(loadingMessage(modelWith({ state: "starting" }))).toBe("");
  });
});

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
