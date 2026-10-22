// Фото из Telegram (§6.1): file_id из raw_update, скачивание через Bot API.
// Картинка живёт только в памяти хода: на диск не пишем, в историю не кладём.

export interface PhotoMedia {
  mime: string;
  /** base64 без data:-префикса */
  base64: string;
}

export interface MediaFetcher {
  /** null — фото недоступно (нет file_id, ошибка API, слишком большой файл). */
  fetchPhoto(fileId: string): Promise<PhotoMedia | null>;
}

/** Потолок: больше в контекст не тянем, Telegram-фото обычно 50–300 КБ. */
const MAX_PHOTO_BYTES = 6 * 1024 * 1024;

/** Крупнейший PhotoSize из update (Telegram отдаёт размеры по возрастанию). */
export function photoFileIdFromUpdate(raw: unknown): string | null {
  if (typeof raw !== "object" || raw === null) return null;
  const record = raw as Record<string, unknown>;
  const message = (record["message"] ??
    record["edited_message"] ??
    record["channel_post"] ??
    record["edited_channel_post"]) as Record<string, unknown> | undefined;
  if (!message) return null;
  const photos = message["photo"];
  if (!Array.isArray(photos) || photos.length === 0) return null;

  let bestId: string | null = null;
  let bestArea = -1;
  for (const entry of photos) {
    if (typeof entry !== "object" || entry === null) continue;
    const photo = entry as { file_id?: unknown; width?: unknown; height?: unknown };
    if (typeof photo.file_id !== "string" || photo.file_id.length === 0) continue;
    const area =
      (typeof photo.width === "number" ? photo.width : 0) *
      (typeof photo.height === "number" ? photo.height : 0);
    if (area > bestArea) {
      bestArea = area;
      bestId = photo.file_id;
    }
  }
  return bestId;
}

export type MediaTransport = (url: string, init?: { signal?: AbortSignal }) => Promise<Response>;

export class TelegramMediaFetcher implements MediaFetcher {
  private readonly base: string;

  constructor(
    private readonly botToken: string,
    apiBase: string = "https://api.telegram.org",
    private readonly transport: MediaTransport = (url, init) => fetch(url, init),
  ) {
    this.base = apiBase.replace(/\/+$/, "");
  }

  async fetchPhoto(fileId: string): Promise<PhotoMedia | null> {
    const fileResponse = await this.transport(
      `${this.base}/bot${this.botToken}/getFile?file_id=${encodeURIComponent(fileId)}`,
      { signal: AbortSignal.timeout(15_000) },
    );
    if (!fileResponse.ok) return null;
    const json = (await fileResponse.json().catch(() => null)) as
      | { ok?: boolean; result?: { file_path?: unknown } }
      | null;
    const filePath = json?.result?.["file_path"];
    if (!json?.ok || typeof filePath !== "string" || filePath.length === 0) return null;

    const response = await this.transport(`${this.base}/file/bot${this.botToken}/${filePath}`, {
      signal: AbortSignal.timeout(20_000),
    });
    if (!response.ok) return null;

    const buffer = Buffer.from(await response.arrayBuffer());
    if (buffer.length === 0 || buffer.length > MAX_PHOTO_BYTES) return null;

    const mime = filePath.toLowerCase().endsWith(".png") ? "image/png" : "image/jpeg";
    return { mime, base64: buffer.toString("base64") };
  }
}
