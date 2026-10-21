import { TurnResultSchema, TURN_RESULT_JSON_SCHEMA, type TurnResult } from "./turn-result.js";
import { withTimeout } from "../runtime/with-timeout.js";

/**
 * Тонкий собственный адаптер поверх OpenAI-совместимого протокола
 * (ARCHITECTURE §17). Всё, что видит модель, приложение собирает само
 * и передаёт явно; ответ валидируется по схеме до применения.
 */

export interface ChatMessage {
  role: "user" | "assistant";
  content: string;
}

export interface ModelTurnContext {
  system: string;
  messages: ChatMessage[];
}

export interface ModelTurnInput {
  turnId: string;
  context: ModelTurnContext;
  constraints?: Record<string, unknown>;
  signal?: AbortSignal;
  /** Usage конкретного вызова (§20: журнал хода) — приоритетнее колбэка адаптера. */
  onUsage?: (usage: ModelUsage) => void;
}

export interface CharacterModel {
  runTurn(input: ModelTurnInput): Promise<TurnResult>;
}

export interface ModelUsage {
  promptTokens: number;
  completionTokens: number;
}

export type ModelTransport = (
  url: string,
  init: { method: "POST"; headers: Record<string, string>; body: string; signal: AbortSignal },
) => Promise<Response>;

export class ModelError extends Error {
  constructor(
    message: string,
    public readonly retryable: boolean,
  ) {
    super(message);
    this.name = "ModelError";
  }
}

export interface AdapterOptions {
  endpoint: string; // базовый URL, например https://.../v1
  apiKey: string;
  model: string;
  maxOutputTokens?: number;
  timeoutMs?: number;
  maxRetries?: number;
  transport?: ModelTransport;
  onUsage?: (usage: ModelUsage) => void;
}

const DEFAULTS = {
  maxOutputTokens: 1000,
  timeoutMs: 60_000,
  maxRetries: 1,
  /** пауза между попытками внутри адаптера, мс * 2^номер (§12.1) */
  retryBaseMs: 1_000,
};

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function combineSignals(outer: AbortSignal | undefined, inner: AbortSignal): AbortSignal {
  if (!outer) return inner;
  const combined = new AbortController();
  const abort = (reason?: unknown): void => combined.abort(reason);
  if (outer.aborted) abort(outer.reason);
  else outer.addEventListener("abort", () => abort(outer.reason), { once: true });
  if (inner.aborted) abort(inner.reason);
  else inner.addEventListener("abort", () => abort(inner.reason), { once: true });
  return combined.signal;
}

function extractToolCallArguments(json: unknown): string {
  const choices = (json as { choices?: unknown[] })?.["choices"];
  if (!Array.isArray(choices) || choices.length === 0) {
    throw new ModelError("в ответе нет choices", true);
  }
  const message = (choices[0] as { message?: { tool_calls?: unknown[] } })?.["message"];
  const toolCalls = message?.["tool_calls"];
  if (!Array.isArray(toolCalls) || toolCalls.length === 0) {
    throw new ModelError("модель не вызвала tool turn_result", true);
  }
  const functionCall = (toolCalls[0] as { function?: { arguments?: unknown } })?.["function"];
  const arguments_ = functionCall?.["arguments"];
  if (typeof arguments_ !== "string") {
    throw new ModelError("у tool-вызова нет строковых arguments", true);
  }
  return arguments_;
}

export class OpenAICompatModelAdapter implements CharacterModel {
  private readonly endpoint: string;
  private readonly apiKey: string;
  private readonly model: string;
  private readonly maxOutputTokens: number;
  private readonly timeoutMs: number;
  private readonly maxRetries: number;
  private readonly transport: ModelTransport;
  private readonly onUsage?: (usage: ModelUsage) => void;

  constructor(options: AdapterOptions) {
    this.endpoint = options.endpoint.replace(/\/+$/, "");
    this.apiKey = options.apiKey;
    this.model = options.model;
    this.maxOutputTokens = options.maxOutputTokens ?? DEFAULTS.maxOutputTokens;
    this.timeoutMs = options.timeoutMs ?? DEFAULTS.timeoutMs;
    this.maxRetries = options.maxRetries ?? DEFAULTS.maxRetries;
    this.transport = options.transport ?? ((url, init) => fetch(url, init));
    this.onUsage = options.onUsage;
  }

  async runTurn(input: ModelTurnInput): Promise<TurnResult> {
    const systemMessage = input.constraints
      ? `${input.context.system}\n\nОграничения: ${JSON.stringify(input.constraints)}`
      : input.context.system;

    const body = {
      model: this.model,
      messages: [
        { role: "system", content: systemMessage },
        ...input.context.messages.map((message) => ({ role: message.role, content: message.content })),
      ],
      max_tokens: this.maxOutputTokens,
      tools: [
        {
          type: "function",
          function: {
            name: "turn_result",
            description:
              "Зафиксировать решение хода: answer/ignore/defer, сообщения и заметки о состоянии.",
            parameters: TURN_RESULT_JSON_SCHEMA,
          },
        },
      ],
      tool_choice: { type: "function", function: { name: "turn_result" } },
    };

    return withTimeout(async (timeoutSignal) => {
      const signal = combineSignals(input.signal, timeoutSignal);
      let lastError: ModelError = new ModelError("адаптер не выполнял запросов", false);

      for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
        if (attempt > 0) {
          // §12.1: пауза между попытками, растущая с номером —
          // мгновенный повтор сжигает попытки при кратковременном 429
          await sleep(DEFAULTS.retryBaseMs * 2 ** (attempt - 1));
        }
        let response: Response;
        try {
          response = await this.transport(`${this.endpoint}/chat/completions`, {
            method: "POST",
            headers: {
              authorization: `Bearer ${this.apiKey}`,
              "content-type": "application/json",
            },
            body: JSON.stringify(body),
            signal,
          });
        } catch (error) {
          lastError = new ModelError(
            `транспортная ошибка: ${error instanceof Error ? error.message : String(error)}`,
            true,
          );
          continue;
        }

        if (!response.ok) {
          const text = await response.text().catch(() => "");
          lastError = new ModelError(
            `HTTP ${response.status}: ${text.slice(0, 300)}`,
            response.status === 429 || response.status >= 500,
          );
          if (!lastError.retryable) throw lastError;
          continue;
        }

        const json: unknown = await response.json().catch(() => {
          throw new ModelError("ответ не является JSON", true);
        });

        try {
          const argumentsJson: unknown = JSON.parse(extractToolCallArguments(json));
          const parsed = TurnResultSchema.safeParse(argumentsJson);
          if (!parsed.success) {
            throw new ModelError(
              `ответ не прошёл схему TurnResult: ${JSON.stringify(parsed.error.issues).slice(0, 300)}`,
              true,
            );
          }
          this.reportUsage(json, input);
          return parsed.data;
        } catch (error) {
          if (error instanceof ModelError) {
            lastError = error;
            continue;
          }
          throw error;
        }
      }

      throw lastError;
    }, this.timeoutMs);
  }

  private reportUsage(json: unknown, input: ModelTurnInput): void {
    const callback = input.onUsage ?? this.onUsage;
    if (!callback) return;
    const usage = (json as { usage?: { prompt_tokens?: number; completion_tokens?: number } })?.["usage"];
    if (!usage) return;
    callback({
      promptTokens: usage["prompt_tokens"] ?? 0,
      completionTokens: usage["completion_tokens"] ?? 0,
    });
  }
}
