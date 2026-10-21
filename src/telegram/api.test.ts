import { describe, expect, it } from "vitest";
import { HttpTelegramApi, TelegramApiError, type HttpTransport } from "./api.js";

// §13.2 «учитывать retry_after»: значение обязано доезживать из ответа
// Telegram до политики доставки (markRetryWait). Защита уровня парсинга:
// HttpTelegramApi с внедрённым transport-заглушкой, без сети.
// Итерация 2: закрывает выжившую мутацию М8 (потеря retry_after).

function jsonResponse(body: unknown, status = 200, statusText = ""): Response {
  return new Response(JSON.stringify(body), { status, statusText });
}

function signal(): AbortSignal {
  return new AbortController().signal;
}

function makeApi(transport: HttpTransport): HttpTelegramApi {
  return new HttpTelegramApi("TOKEN", "https://tg.example", transport);
}

async function captureError(fn: () => Promise<unknown>): Promise<unknown> {
  try {
    await fn();
    return null;
  } catch (error) {
    return error;
  }
}

describe("HttpTelegramApi.sendMessage (transport-заглушка)", () => {
  it("ok-ответ: message_id возвращается строкой; запрос уходит на /bot<token>/sendMessage с chat_id и text", async () => {
    let seen: { url: string; body: string } | undefined;
    const api = makeApi(async (url, init) => {
      seen = { url, body: init.body };
      return jsonResponse({ ok: true, result: { message_id: 4242 } });
    });

    const result = await api.sendMessage("500", "привет", signal());

    expect(result).toEqual({ messageId: "4242" });
    expect(seen!.url).toBe("https://tg.example/botTOKEN/sendMessage");
    expect(JSON.parse(seen!.body)).toEqual({ chat_id: "500", text: "привет" });
  });

  it("429 с parameters.retry_after: TelegramApiError с httpStatus=429 и retryAfterSec из ответа", async () => {
    const api = makeApi(async () =>
      jsonResponse(
        {
          ok: false,
          description: "Too Many Requests: retry after 7",
          parameters: { retry_after: 7 },
        },
        429,
      ),
    );

    const error = await captureError(() => api.sendMessage("500", "x", signal()));

    expect(error).toBeInstanceOf(TelegramApiError);
    const apiError = error as TelegramApiError;
    expect(apiError.httpStatus).toBe(429);
    expect(apiError.retryAfterSec).toBe(7);
    expect(apiError.message).toContain("Too Many Requests");
    expect(apiError.message).toContain("429");
  });

  it("4xx с description и без parameters: статус и описание доезживают, retryAfterSec=null", async () => {
    const api = makeApi(async () =>
      jsonResponse(
        { ok: false, description: "Bad Request: chat not found" },
        400,
      ),
    );

    const error = await captureError(() => api.sendMessage("500", "x", signal()));

    expect(error).toBeInstanceOf(TelegramApiError);
    const apiError = error as TelegramApiError;
    expect(apiError.httpStatus).toBe(400);
    expect(apiError.retryAfterSec).toBeNull();
    expect(apiError.message).toContain("Bad Request: chat not found");
  });

  it("ok:true, но result без message_id — явная ошибка, а не «лучшее усилие»", async () => {
    const api = makeApi(async () => jsonResponse({ ok: true, result: {} }));

    const error = await captureError(() => api.sendMessage("500", "x", signal()));

    expect(error).toBeInstanceOf(TelegramApiError);
    expect((error as TelegramApiError).message).toContain("message_id");
  });

  it("не-JSON тело (html от прокси при 502): ошибка с HTTP-статусом без падения парсинга", async () => {
    const api = makeApi(
      async () => new Response("<html>Bad Gateway</html>", { status: 502, statusText: "Bad Gateway" }),
    );

    const error = await captureError(() => api.sendMessage("500", "x", signal()));

    expect(error).toBeInstanceOf(TelegramApiError);
    expect((error as TelegramApiError).httpStatus).toBe(502);
  });
});

describe("HttpTelegramApi.getUpdates (transport-заглушка)", () => {
  it("ok-ответ: массив updates; offset, timeout и allowed_updates передаются явно", async () => {
    let seen: { url: string; body: string } | undefined;
    const api = makeApi(async (url, init) => {
      seen = { url, body: init.body };
      return jsonResponse({ ok: true, result: [{ update_id: 5 }, { update_id: 6 }] });
    });

    const updates = await api.getUpdates(5, 25, signal());

    expect(updates).toEqual([{ update_id: 5 }, { update_id: 6 }]);
    expect(seen!.url).toBe("https://tg.example/botTOKEN/getUpdates");
    expect(JSON.parse(seen!.body)).toEqual({
      offset: 5,
      timeout: 25,
      allowed_updates: ["message", "edited_message"],
    });
  });

  it("ok-ответ без result → пустой массив (нет обновлений — не ошибка)", async () => {
    const api = makeApi(async () => jsonResponse({ ok: true }));
    expect(await api.getUpdates(0, 1, signal())).toEqual([]);
  });

  it("ошибка API пробрасывается как TelegramApiError", async () => {
    const api = makeApi(async () => jsonResponse({ ok: false, description: "Unauthorized" }, 401));
    const error = await captureError(() => api.getUpdates(0, 1, signal()));
    expect(error).toBeInstanceOf(TelegramApiError);
    expect((error as TelegramApiError).httpStatus).toBe(401);
  });
});
