// Приёмочный тест по спецификации 8.4 (§13.3), не по коду. Углы, не покрытые
// в typing.test.ts:
// - параллельные чаты не мешают друг другу (каденции независимы, stop одного
//   не задевает другой, сбой api по одному чату не глушит пульс соседа);
// - stop чата без запущенного пульса безвреден;
// - длинная серия start/stop не копит таймеры и не теряет немедленные пульсы.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TelegramTypingPulse } from "./typing.js";

interface Recorder {
  calls: string[];
  sendChatAction(chatId: string, action: string): Promise<void>;
}

const recorder = (): Recorder => {
  const calls: string[] = [];
  return {
    calls,
    sendChatAction: async (chatId, action) => {
      calls.push(`${chatId}:${action}`);
    },
  };
};

/** api, который падает только для указанного чата (остальные записывает). */
const flakyRecorder = (brokenChatId: string): Recorder => {
  const good = recorder();
  return {
    calls: good.calls,
    sendChatAction: async (chatId, action) => {
      if (chatId === brokenChatId) throw new Error(`чат ${chatId} недоступен`);
      await good.sendChatAction(chatId, action);
    },
  };
};

const callsFor = (calls: string[], chatId: string): string[] =>
  calls.filter((call) => call.startsWith(`${chatId}:`));

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("TelegramTypingPulse (8.4, §13.3 — приёмка по спеке)", () => {
  it("два чата идут каждый в своём ритме; stop одного не задевает другой", async () => {
    const api = recorder();
    const pulse = new TelegramTypingPulse(api as never);

    pulse.start("111");
    await vi.advanceTimersByTimeAsync(2_000);
    pulse.start("222");
    await vi.advanceTimersByTimeAsync(6_000); // t = 8000

    // «111»: пульсы в t=0, 4000, 8000; «222»: в t=2000, 6000
    expect(callsFor(api.calls, "111")).toHaveLength(3);
    expect(callsFor(api.calls, "222")).toHaveLength(2);

    pulse.stop("111");
    await vi.advanceTimersByTimeAsync(8_000); // t = 16000

    // «111» замолчал, «222» продолжил: t=10000, 14000
    expect(callsFor(api.calls, "111")).toHaveLength(3);
    expect(callsFor(api.calls, "222")).toHaveLength(4);

    pulse.stop("222");
  });

  it("stop чата, который не был запущен, безвреден и ничего не глушит", async () => {
    const api = recorder();
    const pulse = new TelegramTypingPulse(api as never);

    expect(() => pulse.stop("999")).not.toThrow();
    expect(api.calls).toEqual([]);

    pulse.start("555");
    await vi.advanceTimersByTimeAsync(0);
    expect(() => pulse.stop("999")).not.toThrow();
    await vi.advanceTimersByTimeAsync(8_000);

    // соседний чат продолжал пульсировать: t=0, 4000, 8000
    expect(callsFor(api.calls, "555")).toHaveLength(3);
    expect(api.calls.every((call) => call.startsWith("555:"))).toBe(true);

    pulse.stop("555");
  });

  it("длинная серия start/stop на двух чатах: ровно один немедленный пульс на start, после серии тишина", async () => {
    const api = recorder();
    const pulse = new TelegramTypingPulse(api as never);

    for (let i = 0; i < 25; i++) {
      pulse.start("777");
      pulse.start("888");
      await vi.advanceTimersByTimeAsync(0);
      pulse.stop("777");
      pulse.stop("888");
    }
    // каждый start дал ровно один немедленный пульс — таймеры не копились
    expect(callsFor(api.calls, "777")).toHaveLength(25);
    expect(callsFor(api.calls, "888")).toHaveLength(25);

    await vi.advanceTimersByTimeAsync(20_000);
    expect(api.calls).toHaveLength(50); // после stop — тишина
  });

  it("постоянные сбои api по одному чату не глушат пульс ни его, ни соседа", async () => {
    const api = flakyRecorder("666");
    const pulse = new TelegramTypingPulse(api as never);

    pulse.start("666");
    pulse.start("444");
    await vi.advanceTimersByTimeAsync(12_000);

    // сбойный чат не прекращает попыток (12 с — три пульса), сосед не задет
    expect(callsFor(api.calls, "444")).toHaveLength(4); // t=0, 4000, 8000, 12000
    expect(() => pulse.stop("666")).not.toThrow();
    expect(() => pulse.stop("444")).not.toThrow();
    await vi.advanceTimersByTimeAsync(4_000);
    expect(callsFor(api.calls, "444")).toHaveLength(4); // оба остановлены
  });
});
