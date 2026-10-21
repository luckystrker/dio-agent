import { describe, expect, it } from "vitest";
import fc from "fast-check";
import { NormalizationError, normalizeUpdate, type EventKind } from "./normalize.js";

// PBT по §6.1 (шаг 1 «нормализовать тип и безопасно извлечь метаданные»):
// результат детерминирован, сохраняет update_id/chat/text, классификация
// reply/mention/command/edited не зависит от содержимого текста сообщения.

const chatTypeArb = fc.constantFrom("private", "group", "supergroup", "channel");

const entityArb = fc.record({
  type: fc.constantFrom("bot_command", "mention", "url", "bold", "italic", "hashtag"),
  offset: fc.nat(),
  length: fc.integer({ min: 0, max: 200 }),
});

const messageArb = fc.record({
  message_id: fc.nat(),
  from: fc.option(fc.record({ id: fc.integer(), is_bot: fc.constant(false) }), {
    nil: undefined,
  }),
  chat: fc.record({ id: fc.integer(), type: chatTypeArb }),
  date: fc.nat(),
  text: fc.option(fc.string({ maxLength: 40 }), { nil: undefined }),
  entities: fc.option(fc.array(entityArb, { maxLength: 3 }), { nil: undefined }),
  reply_to_message: fc.option(fc.record({ message_id: fc.nat() }), { nil: undefined }),
});

const updateIdArb = fc.integer({ min: 0, max: Number.MAX_SAFE_INTEGER });

const messageUpdateArb = fc.record({ update_id: updateIdArb, message: messageArb });
const editedUpdateArb = fc.record({ update_id: updateIdArb, edited_message: messageArb });
const updateArb = fc.oneof(messageUpdateArb, editedUpdateArb);

function messageOf(update: unknown): {
  text?: string;
  entities?: Array<{ type?: string }>;
  reply_to_message?: unknown;
  chat: { id: number; type: string };
  from?: { id: number };
} {
  const record = update as { message?: unknown; edited_message?: unknown };
  return (record.message ?? record.edited_message) as ReturnType<typeof messageOf>;
}

describe("PBT: normalizeUpdate — детерминированность и сохранение данных", () => {
  it("для любого валидного message-update результат детерминирован", async () => {
    await fc.assert(
      fc.property(updateArb, (update) => {
        expect(normalizeUpdate(update)).toEqual(normalizeUpdate(update));
      }),
    );
  });

  it("сохраняет update_id, chat, text, actor и chatType для любого валидного update", async () => {
    await fc.assert(
      fc.property(updateArb, (update) => {
        const event = normalizeUpdate(update);
        const message = messageOf(update);
        expect(event.telegramUpdateId).toBe(
          String((update as { update_id: number }).update_id),
        );
        expect(event.chatId).toBe(String(message.chat.id));
        expect(event.text).toBe(typeof message.text === "string" ? message.text : null);
        expect(event.chatType).toBe(message.chat.type === "private" ? "private" : "group");
        expect(event.actorId).toBe(
          typeof message.from?.id === "number" ? String(message.from.id) : null,
        );
        expect(event.supported).toBe(true);
      }),
    );
  });
});

describe("PBT: normalizeUpdate — классификация", () => {
  it("инвариантна к содержимому текста: замена text не меняет kind/chatType/supported", async () => {
    await fc.assert(
      fc.property(
        updateArb,
        fc.option(fc.string({ maxLength: 40 }), { nil: null }),
        (update, replacement) => {
          const base = normalizeUpdate(update);
          const key = "message" in update ? "message" : "edited_message";
          const mutated = {
            ...update,
            [key]: { ...messageOf(update), text: replacement ?? undefined },
          };
          const other = normalizeUpdate(mutated);
          expect(other.kind).toBe(base.kind);
          expect(other.chatType).toBe(base.chatType);
          expect(other.supported).toBe(base.supported);
        },
      ),
    );
  });

  it("edited_message классифицируется как edited независимо от reply и entities", async () => {
    await fc.assert(
      fc.property(editedUpdateArb, (update) => {
        expect(normalizeUpdate(update).kind).toBe("edited");
      }),
    );
  });

  it("reply_to_message в обычном сообщении даёт kind=reply", async () => {
    await fc.assert(
      fc.property(messageUpdateArb, (update) => {
        fc.pre(messageOf(update).reply_to_message !== undefined);
        expect(normalizeUpdate(update).kind).toBe("reply");
      }),
    );
  });

  it("entities с bot_command (без reply) дают kind=command", async () => {
    await fc.assert(
      fc.property(messageUpdateArb, (update) => {
        const message = messageOf(update);
        fc.pre(
          message.reply_to_message === undefined &&
            (message.entities ?? []).some((entity) => entity.type === "bot_command"),
        );
        expect(normalizeUpdate(update).kind).toBe("command");
      }),
    );
  });

  it("entities с mention (без reply и bot_command) дают kind=mention", async () => {
    await fc.assert(
      fc.property(messageUpdateArb, (update) => {
        const message = messageOf(update);
        fc.pre(
          message.reply_to_message === undefined &&
            !(message.entities ?? []).some((entity) => entity.type === "bot_command") &&
            (message.entities ?? []).some((entity) => entity.type === "mention"),
        );
        expect(normalizeUpdate(update).kind).toBe("mention");
      }),
    );
  });

  it("kind принимает только значения из шести классов; приоритет edited > reply > command > mention > message", async () => {
    const KINDS: EventKind[] = ["message", "reply", "mention", "command", "edited", "other"];
    await fc.assert(
      fc.property(updateArb, (update) => {
        const event = normalizeUpdate(update);
        expect(KINDS).toContain(event.kind);
        // Независимый прогноз по приоритетам §6.1/§19 (edit — отдельное событие)
        const message = messageOf(update);
        const isEdited = "edited_message" in update;
        const expected: EventKind = isEdited
          ? "edited"
          : message.reply_to_message !== undefined
            ? "reply"
            : (message.entities ?? []).some((entity) => entity.type === "bot_command")
              ? "command"
              : (message.entities ?? []).some((entity) => entity.type === "mention")
                ? "mention"
                : "message";
        expect(event.kind).toBe(expected);
      }),
    );
  });
});

describe("PBT: normalizeUpdate — неподдерживаемые и неразборчивые", () => {
  it("callback_query для любого содержимого: supported=false, фиктивный чат 0, offset не застрянет", async () => {
    const payloadArb = fc.record({
      id: fc.string({ maxLength: 10 }),
      data: fc.option(fc.string({ maxLength: 10 }), { nil: undefined }),
    });
    await fc.assert(
      fc.asyncProperty(updateIdArb, payloadArb, async (updateId, payload) => {
        const event = normalizeUpdate({ update_id: updateId, callback_query: payload });
        expect(event.supported).toBe(false);
        expect(event.chatId).toBe("0");
        expect(event.kind).toBe("other");
        expect(event.text).toBeNull();
        expect(event.telegramUpdateId).toBe(String(updateId));
      }),
    );
  });

  it("сообщение без chat.id для любого остального содержимого: supported=false", async () => {
    await fc.assert(
      fc.asyncProperty(updateIdArb, messageArb, async (updateId, message) => {
        const event = normalizeUpdate({
          update_id: updateId,
          message: { ...message, chat: undefined },
        });
        expect(event.supported).toBe(false);
        expect(event.telegramUpdateId).toBe(String(updateId));
      }),
    );
  });

  it("без целочисленного update_id — NormalizationError (в журнал не записать)", async () => {
    await fc.assert(
      fc.asyncProperty(
        fc.oneof(
          fc.constantFrom(null, undefined, "100", 1.5, Number.NaN),
          fc.record({ message: messageArb }),
          fc.integer(),
        ),
        async (junk) => {
          expect(() => normalizeUpdate(junk)).toThrow(NormalizationError);
        },
      ),
    );
  });
});
