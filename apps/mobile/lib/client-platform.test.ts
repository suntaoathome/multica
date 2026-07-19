import { describe, expect, it } from "vitest";

import { resolveClientOS } from "./resolve-client-os";

describe("resolveClientOS", () => {
  it.each(["ios", "android"])("reports %s without changing its identity", (os) => {
    expect(resolveClientOS(os)).toBe(os);
  });
});
