import { afterEach, describe, expect, it, vi } from "vitest";
import { detectOS } from "./os-detect";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("detectOS", () => {
  it("detects Android before the Linux fallback", async () => {
    vi.stubGlobal("navigator", {
      userAgent:
        "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/126 Mobile Safari/537.36",
      platform: "Linux armv81",
    });

    await expect(detectOS()).resolves.toMatchObject({
      os: "android",
    });
  });

  it("detects Android from Chromium high-entropy platform data", async () => {
    vi.stubGlobal("navigator", {
      userAgent: "Mozilla/5.0",
      platform: "Linux",
      userAgentData: {
        getHighEntropyValues: vi.fn().mockResolvedValue({
          platform: "Android",
          architecture: "arm",
        }),
      },
    });

    await expect(detectOS()).resolves.toEqual({
      os: "android",
      arch: "arm64",
      archConfident: true,
    });
  });
});
