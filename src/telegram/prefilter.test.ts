import { describe, expect, it } from "vitest";
import { decideIngestStatus } from "./prefilter.js";
import type { NormalizedEvent } from "./normalize.js";

function event(overrides: Partial<NormalizedEvent>): NormalizedEvent {
  return {
    telegramUpdateId: "1",
    chatId: "1",
    chatType: "private",
    actorId: "1",
    kind: "message",
    text: "x",
    supported: true,
    ...overrides,
  };
}

describe("decideIngestStatus", () => {
  it("личка — всегда process, даже обычное сообщение", () => {
    expect(decideIngestStatus(event({ chatType: "private" }))).toBe("pending");
  });

  it("пассивная группа без обращения — journal_only", () => {
    expect(decideIngestStatus(event({ chatType: "group", kind: "message" }))).toBe("journal_only");
  });

  it("reply / mention / command в группе — process (прямое обращение)", () => {
    for (const kind of ["reply", "mention", "command"] as const) {
      expect(decideIngestStatus(event({ chatType: "group", kind }))).toBe("pending");
    }
  });

  it("правка сообщения — journal_only", () => {
    expect(decideIngestStatus(event({ chatType: "private", kind: "edited" }))).toBe("pending");
    expect(decideIngestStatus(event({ chatType: "group", kind: "edited" }))).toBe("journal_only");
  });

  it("неподдерживаемое — journal_only всегда", () => {
    expect(decideIngestStatus(event({ supported: false }))).toBe("journal_only");
  });
});
