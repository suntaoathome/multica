import { describe, expect, it } from "vitest";
import { createEnDict } from "../../i18n/en";
import { resolveContent } from "./hero";

describe("resolveContent", () => {
  it("links Android devices directly to the configured APK", () => {
    const content = resolveContent(
      { os: "android", arch: "arm64", archConfident: false },
      {},
      false,
      createEnDict(true).download.hero,
      {
        version: "0.1.0",
        apkUrl: "https://downloads.test/handoff.apk",
      },
    );

    expect(content.primary).toEqual({
      href: "https://downloads.test/handoff.apk",
      label: "Download APK",
      disabled: false,
    });
    expect(content.title).toBe("Handoff for Android");
  });

  it("links confidently detected Intel Macs to the x64 installers", () => {
    const content = resolveContent(
      { os: "mac", arch: "x64", archConfident: true },
      {
        macArm64Dmg: "https://downloads.test/mac-arm64.dmg",
        macArm64Zip: "https://downloads.test/mac-arm64.zip",
        macX64Dmg: "https://downloads.test/mac-x64.dmg",
        macX64Zip: "https://downloads.test/mac-x64.zip",
      },
      false,
      createEnDict(true).download.hero,
    );

    expect(content.primary).toEqual({
      href: "https://downloads.test/mac-x64.dmg",
      label: "Download (.dmg)",
      disabled: false,
    });
    expect(content.alt).toEqual({
      href: "https://downloads.test/mac-x64.zip",
      label: "or download .zip",
    });
  });
});
