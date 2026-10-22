import { describe, expect, it } from "vitest";
import { fitHistory, type HistoryMessage } from "./recent-history.js";

// 8.1: в контексте хода — недавний разговор обеих сторон (до 1000 сообщений),
// при превышении бюджета объёма первыми теряются самые старые сообщения.

const msg = (role: "user" | "assistant", content: string): HistoryMessage => ({ role, content });

describe("fitHistory (8.1: окно недавнего разговора)", () => {
  it("всё помещается — ничего не обрезается", () => {
    const history = [msg("user", "привет"), msg("assistant", "о, привет")];
    expect(fitHistory(history, 1000)).toEqual(history);
  });

  it("бюджет мал — теряются самые старые, свежие остаются", () => {
    const history = [
      msg("user", "старое длинное сообщение длиной 40 символов!!!!!!"),
      msg("assistant", "среднее сообщение"),
      msg("user", "свежее"),
    ];
    const fitted = fitHistory(history, 30);
    expect(fitted).toEqual([msg("assistant", "среднее сообщение"), msg("user", "свежее")]);
  });

  it("самое свежее сообщение попадает всегда, даже если само больше бюджета", () => {
    const history = [msg("user", "а"), msg("assistant", "очень длинное сообщение больше бюджета целиком")];
    expect(fitHistory(history, 5)).toEqual([msg("assistant", "очень длинное сообщение больше бюджета целиком")]);
  });

  it("пустая история — пустой результат", () => {
    expect(fitHistory([], 100)).toEqual([]);
  });
});
