import { describe, expect, it } from "vitest";
import { NormalizationError, normalizeUpdate } from "./normalize.js";

function messageUpdate(overrides: Record<string, unknown> = {}): unknown {
  return {
    update_id: 100,
    message: {
      message_id: 1,
      from: { id: 42, is_bot: false },
      chat: { id: 500, type: "private" },
      date: 1700000000,
      text: "привет",
      ...overrides,
    },
  };
}

describe("normalizeUpdate", () => {
  it("личное сообщение: private + kind=message", () => {
    const event = normalizeUpdate(messageUpdate());
    expect(event).toMatchObject({
      telegramUpdateId: "100",
      chatId: "500",
      chatType: "private",
      actorId: "42",
      kind: "message",
      text: "привет",
      supported: true,
    });
  });

  it("группа: chatType=group", () => {
    const event = normalizeUpdate(
      messageUpdate({ chat: { id: -100200, type: "supergroup" } }),
    );
    expect(event.chatType).toBe("group");
    expect(event.chatId).toBe("-100200");
  });

  it("reply определяется по reply_to_message", () => {
    const event = normalizeUpdate(messageUpdate({ reply_to_message: { message_id: 9 } }));
    expect(event.kind).toBe("reply");
  });

  it("command определяется по entities", () => {
    const event = normalizeUpdate(
      messageUpdate({
        text: "/start",
        entities: [{ type: "bot_command", offset: 0, length: 6 }],
      }),
    );
    expect(event.kind).toBe("command");
  });

  it("mention определяется по entities", () => {
    const event = normalizeUpdate(
      messageUpdate({
        text: "@dioAgent привет",
        entities: [{ type: "mention", offset: 0, length: 10 }],
      }),
    );
    expect(event.kind).toBe("mention");
  });

  it("edited_message даёт kind=edited", () => {
    const raw = messageUpdate();
    const { message, ...rest } = raw as { message: unknown } & Record<string, unknown>;
    const event = normalizeUpdate({ ...rest, edited_message: message });
    expect(event.kind).toBe("edited");
  });

  it("неподдерживаемый callback_query: supported=false, но нормализуется", () => {
    const event = normalizeUpdate({ update_id: 101, callback_query: { id: "cb1" } });
    expect(event.supported).toBe(false);
    expect(event.telegramUpdateId).toBe("101");
    expect(event.kind).toBe("other");
  });

  it("без update_id — NormalizationError (нельзя записать даже в журнал)", () => {
    expect(() => normalizeUpdate({ message: {} })).toThrow(NormalizationError);
    expect(() => normalizeUpdate(null)).toThrow(NormalizationError);
  });

  it("сообщение без чата — unsupported, offset не застрянет", () => {
    const event = normalizeUpdate({ update_id: 102, message: { message_id: 1, text: "x" } });
    expect(event.supported).toBe(false);
  });

  it("текст отсутствует (стикер/фото) — text=null, но событие поддержано", () => {
    const event = normalizeUpdate(messageUpdate({ text: undefined, sticker: { file_id: "f" } }));
    expect(event.text).toBeNull();
    expect(event.supported).toBe(true);
  });
});
