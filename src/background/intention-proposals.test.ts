import { describe, expect, it } from "vitest";
import {
  IntentionProposalError,
  intentionDedupeKey,
  MAX_INTENTIONS_PER_TURN,
  validateIntentionProposals,
} from "./intention-proposals.js";

const VALID = { kind: "check_in", topic: "как проходит ремонт", reason: "обещал спросить", delay_hours: 24 };

describe("validateIntentionProposals (§10.1, §14.1)", () => {
  it("принимает валидное предложение и тримит тексты", () => {
    const [proposal] = validateIntentionProposals([{ ...VALID, topic: "  тема  " }]);
    expect(proposal).toEqual({ kind: "check_in", topic: "тема", reason: "обещал спросить", delayHours: 24 });
  });

  it("пустой список валиден; больше 2 — отклонение", () => {
    expect(validateIntentionProposals([])).toEqual([]);
    const three = [VALID, { ...VALID, topic: "2" }, { ...VALID, topic: "3" }];
    expect(() => validateIntentionProposals(three)).toThrow(/2 намерений/);
    expect(MAX_INTENTIONS_PER_TURN).toBe(2);
  });

  it("все четыре вида разрешены", () => {
    for (const kind of ["reply_later", "check_in", "share", "explore"]) {
      expect(validateIntentionProposals([{ ...VALID, kind }])[0]!.kind).toBe(kind);
    }
  });

  it("неизвестный вид и посторонние поля отклоняются", () => {
    expect(() => validateIntentionProposals([{ ...VALID, kind: "vibe" }])).toThrow(/#0.*vibe/);
    expect(() => validateIntentionProposals([{ ...VALID, entityIds: [] } as never])).toThrow(/постороннее поле/);
  });

  it("delay_hours границы: 3 минуты и 7 дней включительно, вне — отклонение", () => {
    expect(validateIntentionProposals([{ ...VALID, delay_hours: 0.05 }])).toHaveLength(1);
    expect(validateIntentionProposals([{ ...VALID, delay_hours: 168 }])).toHaveLength(1);
    expect(() => validateIntentionProposals([{ ...VALID, delay_hours: 0.01 }])).toThrow(IntentionProposalError);
    expect(() => validateIntentionProposals([{ ...VALID, delay_hours: 169 }])).toThrow(IntentionProposalError);
  });

  it("пустые topic/reason и не-число часов отклоняются", () => {
    expect(() => validateIntentionProposals([{ ...VALID, topic: "" }])).toThrow(/topic/);
    expect(() => validateIntentionProposals([{ ...VALID, reason: " " }])).toThrow(/reason/);
    expect(() => validateIntentionProposals([{ ...VALID, delay_hours: "soon" as never }])).toThrow(/delay_hours/);
  });
});

describe("intentionDedupeKey (§14.1: тип+тема+чат)", () => {
  it("перефразировка регистра не размножает поиск; разный вид/чат — разный ключ", () => {
    expect(intentionDedupeKey("explore", "Горы", "111")).toBe(intentionDedupeKey("explore", "горы", "111"));
    expect(intentionDedupeKey("explore", "горы", "111")).not.toBe(intentionDedupeKey("share", "горы", "111"));
    expect(intentionDedupeKey("explore", "горы", "111")).not.toBe(intentionDedupeKey("explore", "горы", "222"));
  });
});
