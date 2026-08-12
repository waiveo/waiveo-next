import { describe, it, expect } from "vitest";
import { can } from "./can";

// The role ranking this console hides controls by (SEC-010). It MIRRORS the
// server's own `roleRank` in internal/app/auth — duplicated because no wire
// field publishes the ORDER, only a role name.
describe("can", () => {
  it("ranks viewer < operator < admin < owner, exactly as the server does", () => {
    expect(can("owner", "admin")).toBe(true);
    expect(can("admin", "admin")).toBe(true);
    expect(can("operator", "admin")).toBe(false);
    expect(can("viewer", "operator")).toBe(false);
    expect(can("operator", "viewer")).toBe(true);
  });

  // A role this build does not know reaches NOTHING. A future role name is far
  // likelier to be narrower than broader, and hiding a control from someone
  // entitled to it is a support question — showing one they are not is a 403
  // an operator reads as the product being broken.
  it("gives an unknown or absent role no authority", () => {
    expect(can(undefined, "viewer")).toBe(false);
    expect(can("auditor" as never, "viewer")).toBe(false);
  });
});
