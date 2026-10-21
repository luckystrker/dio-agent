import { describe, expect, it } from "vitest";
import { chatScope, intersectScopes, isValidScope, visibleFrom } from "./scope.js";

describe("chatScope", () => {
  it("строит chat-scope из id чата", () => {
    expect(chatScope("111777")).toBe("chat:111777");
    expect(chatScope("-100200")).toBe("chat:-100200");
  });
});

describe("isValidScope", () => {
  it("разрешает public, internal, chat:<id>", () => {
    expect(isValidScope("public")).toBe(true);
    expect(isValidScope("internal")).toBe(true);
    expect(isValidScope("chat:42")).toBe(true);
    expect(isValidScope("chat:-100")).toBe(true);
  });

  it("отклоняет мусор", () => {
    expect(isValidScope("group")).toBe(false);
    expect(isValidScope("chat:abc")).toBe(false);
    expect(isValidScope("")).toBe(false);
  });
});

describe("intersectScopes (§9.1)", () => {
  it("один scope остаётся собой", () => {
    expect(intersectScopes(["chat:1"])).toBe("chat:1");
    expect(intersectScopes(["public"])).toBe("public");
  });

  it("пусто — internal; missing provenance — internal", () => {
    expect(intersectScopes([])).toBe("internal");
  });

  it("internal не расширяет доступ", () => {
    expect(intersectScopes(["internal", "chat:1"])).toBe("internal");
  });

  it("public не сужает: пересечение остальных", () => {
    expect(intersectScopes(["public", "chat:1"])).toBe("chat:1");
    expect(intersectScopes(["public", "public"])).toBe("public");
  });

  it("две разные приватные аудитории без общего чата — internal", () => {
    expect(intersectScopes(["chat:1", "chat:2"])).toBe("internal");
    expect(intersectScopes(["chat:1", "public", "chat:2"])).toBe("internal");
  });
});

describe("visibleFrom", () => {
  it("public виден всем; свой scope виден себе", () => {
    expect(visibleFrom("public", "chat:1")).toBe(true);
    expect(visibleFrom("chat:1", "chat:1")).toBe(true);
  });

  it("чужой чат не виден; internal не виден нигде кроме internal", () => {
    expect(visibleFrom("chat:1", "chat:2")).toBe(false);
    expect(visibleFrom("internal", "chat:1")).toBe(false);
    expect(visibleFrom("internal", "internal")).toBe(true);
  });
});
