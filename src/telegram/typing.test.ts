import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TelegramTypingPulse } from "./typing.js";

// 8.4 (§13.3): typing показывается, пока реально ожидается ответ, с обновлением
// ~раз в 4 секунды; сбой пульса не роняет ход; повторный start заменяет таймер.

const fakeApi = () => {
  const calls: string[] = [];
  return {
    calls,
    sendChatAction: async (chatId: string, action: string): Promise<void> => {
      calls.push(`${chatId}:${action}`);
    },
  };
};

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("TelegramTypingPulse (8.4, §13.3)", () => {
  it("start: сразу шлёт typing и обновляет каждые 4 секунды", async () => {
    const api = fakeApi();
    const pulse = new TelegramTypingPulse(api as never);
    pulse.start("100");
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(8_000);
    expect(api.calls).toEqual(["100:typing", "100:typing", "100:typing"]);
    pulse.stop("100");
  });

  it("stop: обновления прекращаются", async () => {
    const api = fakeApi();
    const pulse = new TelegramTypingPulse(api as never);
    pulse.start("200");
    await vi.advanceTimersByTimeAsync(0);
    pulse.stop("200");
    const afterStop = api.calls.length;
    await vi.advanceTimersByTimeAsync(12_000);
    expect(api.calls.length).toBe(afterStop);
  });

  it("повторный start того же чата не плодит таймеры", async () => {
    const api = fakeApi();
    const pulse = new TelegramTypingPulse(api as never);
    pulse.start("300");
    pulse.start("300");
    await vi.advanceTimersByTimeAsync(4_000);
    // два немедленных пульса + один тик таймера (без замены было бы два тика)
    expect(api.calls).toEqual(["300:typing", "300:typing", "300:typing"]);
    pulse.stop("300");
  });

  it("ошибка api не роняет пульс", async () => {
    const pulse = new TelegramTypingPulse({
      sendChatAction: async () => {
        throw new Error("сеть лежит");
      },
    } as never);
    expect(() => pulse.start("400")).not.toThrow();
    await vi.advanceTimersByTimeAsync(8_000);
    expect(() => pulse.stop("400")).not.toThrow();
  });
});
