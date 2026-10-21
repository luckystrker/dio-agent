import { describe, expect, it } from "vitest";
import fc from "fast-check";
import { decideIngestStatus } from "./prefilter.js";
import type { NormalizedEvent } from "./normalize.js";

// PBT по §6.3: дешёвые правила process vs journal_only. Главный инвариант —
// прямой запрос не отбрасывается: личка всегда process, прямое обращение
// в группе (reply/mention/command) тоже. Шум группы — только журнал.

const chatTypeArb = fc.constantFrom("private", "group");
const kindArb = fc.constantFrom("message", "reply", "mention", "command", "edited", "other");

const supportedEventArb = fc.record({
  chatType: chatTypeArb,
  kind: kindArb,
});

type PartialEvent = { supported: boolean; chatType?: "private" | "group"; kind?: string };

const eventArb: fc.Arbitrary<PartialEvent> = fc.boolean().chain((supported): fc.Arbitrary<PartialEvent> =>
  supported
    ? supportedEventArb.map((fields): PartialEvent => ({ supported: true, ...fields }))
    : fc.record({ supported: fc.constant(false), kind: fc.constantFrom("other", "message") as fc.Arbitrary<string> }),
);

function fullEvent(partial: PartialEvent): NormalizedEvent {
  return {
    telegramUpdateId: "1",
    chatId: "1",
    chatType: partial.chatType ?? "private",
    actorId: "1",
    kind: (partial.kind as NormalizedEvent["kind"]) ?? "message",
    text: "x",
    supported: partial.supported,
  };
}

describe("PBT: decideIngestStatus (§6.3 prefilter)", () => {
  it("личка — всегда pending, что бы ни было в kind (прямой запрос не отбрасывается)", async () => {
    await fc.assert(
      fc.asyncProperty(kindArb, async (kind) => {
        expect(
          decideIngestStatus(fullEvent({ supported: true, chatType: "private", kind })),
        ).toBe("pending");
      }),
    );
  });

  it("решение всегда ровно одно из двух значений", async () => {
    await fc.assert(
      fc.asyncProperty(eventArb, async (partial) => {
        const decision = decideIngestStatus(fullEvent(partial));
        expect(["pending", "journal_only"]).toContain(decision);
      }),
    );
  });

  it("в группе: reply/mention/command — pending, message/edited/other — journal_only", async () => {
    await fc.assert(
      fc.asyncProperty(kindArb, async (kind) => {
        const decision = decideIngestStatus(fullEvent({ supported: true, chatType: "group", kind }));
        const expected = kind === "reply" || kind === "mention" || kind === "command" ? "pending" : "journal_only";
        expect(decision).toBe(expected);
      }),
    );
  });

  it("неподдерживаемое событие — всегда journal_only", async () => {
    await fc.assert(
      fc.asyncProperty(kindArb, chatTypeArb, async (kind, chatType) => {
        expect(
          decideIngestStatus(fullEvent({ supported: false, kind, chatType })),
        ).toBe("journal_only");
      }),
    );
  });
});
