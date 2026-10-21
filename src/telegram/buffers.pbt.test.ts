import { describe, expect, it } from "vitest";
import fc from "fast-check";
import { MAX_BATCH_TEXT_CHARS, MAX_EVENTS_PER_BATCH, windowsFor } from "./buffers.js";
import type { EventKind } from "./normalize.js";

// PBT по §6.2: окна буферов для трёх классов сценариев.
// Таблица спецификации: личка idle 1–2 с / max 3 с; пассивная группа
// idle 3–5 с / max 6 с; reply/mention/command idle 0–300 мс / max 1 с.
// Дополнительно: batch закрывается при 20 событиях и лимите входного текста.

const chatTypeArb = fc.constantFrom("private", "group");
const kindArb = fc.constantFrom("message", "reply", "mention", "command", "edited", "other");
const eventArb = fc.record({ chatType: chatTypeArb, kind: kindArb });

const DIRECT = { idleMin: 1_000, idleMax: 2_000, maxMs: 3_000 };
const PASSIVE = { idleMin: 3_000, idleMax: 5_000, maxMs: 6_000 };
const FAST = { idleMin: 0, idleMax: 300, maxMs: 1_000 };

function expectWindows(
  event: { chatType: "private" | "group"; kind: EventKind },
  bounds: { idleMin: number; idleMax: number; maxMs: number },
): void {
  const windows = windowsFor(event);
  expect(windows.idleMs).toBeGreaterThanOrEqual(bounds.idleMin);
  expect(windows.idleMs).toBeLessThanOrEqual(bounds.idleMax);
  expect(windows.maxMs).toBe(bounds.maxMs);
}

describe("PBT: windowsFor — окна классов §6.2", () => {
  it("личка — окно direct: idle 1–2 с, max ровно 3 с (любой kind)", async () => {
    await fc.assert(
      fc.property(kindArb, (kind) => {
        expectWindows({ chatType: "private", kind }, DIRECT);
      }),
    );
  });

  it("группа с reply/mention/command — окно fast: idle 0–300 мс, max ровно 1 с", async () => {
    await fc.assert(
      fc.property(
        fc.constantFrom("reply", "mention", "command"),
        (kind) => {
          expectWindows({ chatType: "group", kind }, FAST);
        },
      ),
    );
  });

  it("пассивная группа (message/edited/other) — idle 3–5 с, max ровно 6 с", async () => {
    await fc.assert(
      fc.property(
        fc.constantFrom("message", "edited", "other"),
        (kind) => {
          expectWindows({ chatType: "group", kind }, PASSIVE);
        },
      ),
    );
  });

  it("для любого события idle не длиннее max: поток не может переносить закрытие бесконечно", async () => {
    await fc.assert(
      fc.property(eventArb, (event) => {
        const windows = windowsFor(event);
        expect(windows.idleMs).toBeLessThanOrEqual(windows.maxMs);
        expect(windows.maxMs).toBeGreaterThan(0);
        expect(windows.idleMs).toBeGreaterThan(0);
      }),
    );
  });

  it("возвращаемые окна — копия: мутация результата не заражает общие константы", async () => {
    await fc.assert(
      fc.property(eventArb, eventArb, (first, second) => {
        windowsFor(first).idleMs = -1;
        expect(windowsFor(second).idleMs).toBeGreaterThan(0);
      }),
    );
  });

  it("лимиты закрытия batch соответствуют §6.2: 20 событий, лимит текста положительный", () => {
    expect(MAX_EVENTS_PER_BATCH).toBe(20);
    expect(MAX_BATCH_TEXT_CHARS).toBeGreaterThan(0);
  });
});
