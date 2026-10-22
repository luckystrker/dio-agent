import { describe, expect, it } from "vitest";
import {
  TelegramMediaFetcher,
  photoFileIdFromUpdate,
  type MediaTransport,
} from "./media.js";

// Фото из Telegram: выбор крупнейшего размера и скачивание без записи на диск.

describe("photoFileIdFromUpdate", () => {
  it("берёт самый крупный PhotoSize", () => {
    const raw = {
      update_id: 1,
      message: {
        message_id: 10,
        chat: { id: 42, type: "private" },
        photo: [
          { file_id: "small", width: 90, height: 60 },
          { file_id: "big", width: 1280, height: 853 },
          { file_id: "medium", width: 320, height: 213 },
        ],
      },
    };
    expect(photoFileIdFromUpdate(raw)).toBe("big");
  });

  it("null, если фото нет или update не объект", () => {
    expect(photoFileIdFromUpdate(null)).toBeNull();
    expect(photoFileIdFromUpdate({ update_id: 1 })).toBeNull();
    expect(
      photoFileIdFromUpdate({ message: { chat: { id: 1 }, text: "привет" } }),
    ).toBeNull();
  });

  it("находит фото в edited_message", () => {
    const raw = {
      edited_message: { photo: [{ file_id: "edited", width: 10, height: 10 }] },
    };
    expect(photoFileIdFromUpdate(raw)).toBe("edited");
  });
});

describe("TelegramMediaFetcher", () => {
  it("скачивает файл и отдаёт mime + base64", async () => {
    const calls: string[] = [];
    const transport: MediaTransport = async (url) => {
      calls.push(url);
      if (url.includes("/getFile")) {
        return new Response(
          JSON.stringify({ ok: true, result: { file_path: "photos/file_1.jpg" } }),
          { status: 200 },
        );
      }
      return new Response(new Uint8Array([0xff, 0xd8, 0xff, 0xd9]), { status: 200 });
    };
    const fetcher = new TelegramMediaFetcher("TOKEN", "https://api.telegram.org", transport);

    const photo = await fetcher.fetchPhoto("file-id");

    expect(photo).toEqual({ mime: "image/jpeg", base64: "/9j/2Q==" });
    expect(calls[0]).toContain("/botTOKEN/getFile?file_id=file-id");
    expect(calls[1]).toBe("https://api.telegram.org/file/botTOKEN/photos/file_1.jpg");
  });

  it("null на ошибку getFile и на пустой файл", async () => {
    const failing: MediaTransport = async () =>
      new Response(JSON.stringify({ ok: false }), { status: 400 });
    expect(
      await new TelegramMediaFetcher("T", "https://api.telegram.org", failing).fetchPhoto("x"),
    ).toBeNull();

    const empty: MediaTransport = async (url) =>
      url.includes("/getFile")
        ? new Response(JSON.stringify({ ok: true, result: { file_path: "p.jpg" } }), { status: 200 })
        : new Response(new Uint8Array([]), { status: 200 });
    expect(
      await new TelegramMediaFetcher("T", "https://api.telegram.org", empty).fetchPhoto("x"),
    ).toBeNull();
  });
});
