import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { AllPlatforms } from "./all-platforms";

vi.mock("../../i18n", () => ({
  useLocale: () => ({
    t: {
      download: {
        allPlatforms: {
          title: "All platforms",
          androidLabel: "Android · APK",
          androidMinVersion: "Android 7.0+",
          androidInternal: "Internal test build with a debug signature.",
          sha256Label: "SHA-256",
          macArm64Label: "macOS · Apple Silicon",
          macX64Label: "macOS · Intel",
          winX64Label: "Windows · x64",
          winArm64Label: "Windows · ARM64",
          linuxX64Label: "Linux · x64",
          linuxArm64Label: "Linux · ARM64",
          formatDmg: ".dmg",
          formatZip: ".zip",
          formatExe: ".exe",
          formatAppImage: ".AppImage",
          formatDeb: ".deb",
          formatRpm: ".rpm",
          formatApk: "Download APK",
          unavailable: "Not available",
        },
        footer: { allReleases: "View all releases" },
      },
    },
  }),
}));

describe("AllPlatforms", () => {
  it("shows the Android APK with version, size, and checksum", () => {
    render(
      <AllPlatforms
        assets={{}}
        android={{
          version: "0.1.0",
          apkUrl: "https://downloads.test/handoff.apk",
          sha256: "abc123",
          sizeBytes: 111_170_048,
        }}
        fallbackHref="https://github.test/releases"
      />,
    );

    expect(screen.getByRole("link", { name: "Download APK" })).toHaveAttribute(
      "href",
      "https://downloads.test/handoff.apk",
    );
    expect(screen.getByText("v0.1.0 · 106 MB · Android 7.0+")).toBeInTheDocument();
    expect(screen.getByText("abc123")).toBeInTheDocument();
  });

  it("lists Intel macOS downloads separately from Apple Silicon", () => {
    render(
      <AllPlatforms
        assets={{
          macArm64Dmg: "https://downloads.test/mac-arm64.dmg",
          macArm64Zip: "https://downloads.test/mac-arm64.zip",
          macX64Dmg: "https://downloads.test/mac-x64.dmg",
          macX64Zip: "https://downloads.test/mac-x64.zip",
        }}
        fallbackHref="https://github.test/releases"
      />,
    );

    const intelLabel = screen.getByText("macOS · Intel");
    const intelRow = intelLabel.parentElement?.parentElement?.parentElement;
    expect(intelRow).not.toBeNull();
    expect(within(intelRow!).getByRole("link", { name: ".dmg" })).toHaveAttribute(
      "href",
      "https://downloads.test/mac-x64.dmg",
    );
    expect(within(intelRow!).getByRole("link", { name: ".zip" })).toHaveAttribute(
      "href",
      "https://downloads.test/mac-x64.zip",
    );
  });
});
