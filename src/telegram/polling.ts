import type { Pool } from "pg";
import type { Logger } from "../runtime/logger.js";
import type { TelegramApi } from "./api.js";
import { ingestUpdate, readOffset } from "./ingest.js";
import type { NormalizedEvent } from "./normalize.js";

// Long polling (§6.1): updates обрабатываются в порядке update_id;
// при ошибке БД offset не двигается, остаток пачки — на retry.

export interface PendingEvent {
  eventId: string;
  chatId: string;
  chatType: NormalizedEvent["chatType"];
  kind: NormalizedEvent["kind"];
  text: string | null;
  occurredAt: Date;
}

export interface PollerOptions {
  /** long-poll таймаут getUpdates, сек */
  pollTimeoutSec?: number;
  /** пауза после ошибки, мс */
  errorBackoffMs?: number;
}

const DEFAULTS = {
  pollTimeoutSec: 25,
  errorBackoffMs: 3_000,
};

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export class TelegramPoller {
  private readonly pollTimeoutSec: number;
  private readonly errorBackoffMs: number;
  private stopped = false;
  private loopPromise?: Promise<void>;
  private currentCall?: AbortController;
  private offset: number | null = null;

  constructor(
    private readonly deps: {
      pool: Pool;
      api: TelegramApi;
      logger: Logger;
      onPendingEvent: (event: PendingEvent) => void;
    },
    options: PollerOptions = {},
  ) {
    this.pollTimeoutSec = options.pollTimeoutSec ?? DEFAULTS.pollTimeoutSec;
    this.errorBackoffMs = options.errorBackoffMs ?? DEFAULTS.errorBackoffMs;
  }

  async start(): Promise<void> {
    this.stopped = false;
    this.offset = await readOffset(this.deps.pool);
    this.deps.logger.info("polling запущен", { offset: this.offset });
    this.loopPromise = this.loop();
  }

  async stop(): Promise<void> {
    this.stopped = true;
    this.currentCall?.abort();
    if (this.loopPromise) await this.loopPromise.catch(() => undefined);
    this.loopPromise = undefined;
  }

  private async loop(): Promise<void> {
    while (!this.stopped) {
      try {
        const offset = this.offset ?? 0;
        this.currentCall = new AbortController();
        const updates = await this.deps.api.getUpdates(offset, this.pollTimeoutSec, this.currentCall.signal);
        this.currentCall = undefined;
        if (this.stopped) return;
        for (const update of updates) {
          const result = await ingestUpdate(this.deps.pool, update);
          this.offset = Number(result.event.telegramUpdateId) + 1;
          if (result.inserted && result.status === "pending" && result.eventId !== null) {
            this.deps.onPendingEvent({
              eventId: result.eventId,
              chatId: result.event.chatId,
              chatType: result.event.chatType,
              kind: result.event.kind,
              text: result.event.text,
              occurredAt: new Date(),
            });
          }
        }
      } catch (error) {
        if (this.stopped) return;
        this.deps.logger.error("polling: ошибка, повторю после паузы", {
          error: error instanceof Error ? error.message : String(error),
        });
        await sleep(this.errorBackoffMs);
      }
    }
  }
}
