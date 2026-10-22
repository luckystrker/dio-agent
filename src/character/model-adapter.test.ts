import { describe, expect, it, vi } from "vitest";
import { ModelError, OpenAICompatModelAdapter, type ModelTransport } from "./model-adapter.js";
import { TurnResultSchema, TURN_RESULT_JSON_SCHEMA } from "./turn-result.js";

function toolCallResponse(argumentsJson: string, usage?: object): Response {
  return new Response(
    JSON.stringify({
      choices: [
        {
          message: {
            tool_calls: [
              {
                type: "function",
                function: { name: "turn_result", arguments: argumentsJson },
              },
            ],
          },
        },
      ],
      ...(usage ? { usage } : {}),
    }),
    { status: 200 },
  );
}

const VALID_ARGUMENTS = JSON.stringify({
  decision: "answer",
  messages: ["привет!"],
  notes: ["пользователь поздоровался"],
});

function makeAdapter(transport: ModelTransport, overrides: Record<string, unknown> = {}) {
  return new OpenAICompatModelAdapter({
    endpoint: "https://model.example/v1",
    apiKey: "key",
    model: "test-model",
    transport,
    maxRetries: 1,
    ...overrides,
  });
}

const INPUT = {
  turnId: "t1",
  context: { system: "ты Dio", messages: [{ role: "user" as const, content: "привет" }] },
};

describe("OpenAICompatModelAdapter", () => {
  it("валидный tool-вызов разбирается в TurnResult и считает usage", async () => {
    const onUsage = vi.fn();
    const adapter = makeAdapter(
      async () => toolCallResponse(VALID_ARGUMENTS, { prompt_tokens: 10, completion_tokens: 5 }),
      { onUsage },
    );
    const result = await adapter.runTurn(INPUT);
    expect(result.decision).toBe("answer");
    expect(result.messages).toEqual(["привет!"]);
    expect(onUsage).toHaveBeenCalledWith({ promptTokens: 10, completionTokens: 5 });
  });

  it("дефолты полей применяются: messages/notes становятся пустыми массивами", async () => {
    const adapter = makeAdapter(async () => toolCallResponse(JSON.stringify({ decision: "ignore" })));
    const result = await adapter.runTurn(INPUT);
    expect(result.messages).toEqual([]);
    expect(result.notes).toEqual([]);
  });

  it("onUsage вызова замещает колбэк адаптера — usage привязывается к конкретному ходу (§20)", async () => {
    const adapterOnUsage = vi.fn();
    const callOnUsage = vi.fn();
    const adapter = makeAdapter(
      async () => toolCallResponse(VALID_ARGUMENTS, { prompt_tokens: 7, completion_tokens: 3 }),
      { onUsage: adapterOnUsage },
    );
    await adapter.runTurn({ ...INPUT, onUsage: callOnUsage });
    expect(callOnUsage).toHaveBeenCalledWith({ promptTokens: 7, completionTokens: 3 });
    expect(adapterOnUsage).not.toHaveBeenCalled();
  });

  it("невалидная схема — retry, затем явная ошибка (не «лучшее усилие»)", async () => {
    const calls: string[] = [];
    const adapter = makeAdapter(async (_url, init) => {
      calls.push(init.body);
      return toolCallResponse(JSON.stringify({ decision: "explode", messages: "не массив" }));
    });
    await expect(adapter.runTurn(INPUT)).rejects.toThrow(ModelError);
    expect(calls).toHaveLength(2); // первая попытка + один retry
  });

  it("HTTP 401 не ретраится — ошибка сразу", async () => {
    const calls: string[] = [];
    const adapter = makeAdapter(async (_url, init) => {
      calls.push(init.body);
      return new Response("unauthorized", { status: 401 });
    });
    await expect(adapter.runTurn(INPUT)).rejects.toThrow(/HTTP 401/);
    expect(calls).toHaveLength(1);
  });

  it("HTTP 429 ретраится", async () => {
    const calls: string[] = [];
    let answered = false;
    const adapter = makeAdapter(async (_url, init) => {
      calls.push(init.body);
      if (!answered) {
        answered = true;
        return new Response("rate limited", { status: 429 });
      }
      return toolCallResponse(VALID_ARGUMENTS);
    });
    const result = await adapter.runTurn(INPUT);
    expect(result.decision).toBe("answer");
    expect(calls).toHaveLength(2);
  });

  it("ответ без tool-вызова — retry и ошибка", async () => {
    let calls = 0;
    const adapter = makeAdapter(async () => {
      calls++;
      return new Response(JSON.stringify({ choices: [{ message: { content: "просто текст" } }] }), {
        status: 200,
      });
    });
    await expect(adapter.runTurn(INPUT)).rejects.toThrow(/turn_result/);
    expect(calls).toBe(2);
  });

  it("транспортная ошибка после ретраев даёт ModelError", async () => {
    const adapter = makeAdapter(async () => {
      throw new Error("network down");
    });
    await expect(adapter.runTurn(INPUT)).rejects.toThrow(/network down/);
  });

  it("запрос уходит на {endpoint}/chat/completions с Bearer и forced tool_choice", async () => {
    let seen: { url: string; headers: Record<string, string>; body: string } | undefined;
    const adapter = makeAdapter(async (url, init) => {
      seen = { url, headers: init.headers, body: init.body };
      return toolCallResponse(VALID_ARGUMENTS);
    });
    await adapter.runTurn(INPUT);
    const body = JSON.parse(seen!.body);
    expect(seen!.url).toBe("https://model.example/v1/chat/completions");
    expect(seen!.headers["authorization"]).toBe("Bearer key");
    expect(body.tool_choice).toEqual({ type: "function", function: { name: "turn_result" } });
    expect(body.tools[0].function.name).toBe("turn_result");
    expect(body.max_tokens).toBe(1000);
  });

  it("битый JSON с двойным двоеточием чинится без повторного вызова", async () => {
    const transport = vi.fn(async () =>
      toolCallResponse(
        '{"decision": "answer", "messages": ["ок"], "notes": [], "reason":: "тест"}',
      ),
    );
    const adapter = makeAdapter(transport);
    const result = await adapter.runTurn(INPUT);
    expect(result.messages).toEqual(["ок"]);
    expect(result.reason).toBe("тест");
    expect(transport).toHaveBeenCalledTimes(1);
  });

  it("400 «Thinking mode does not support this tool_choice» → повтор без forced tool_choice", async () => {
    let calls = 0;
    const transport = vi.fn(async (_url: string, init: { body: string }) => {
      calls++;
      if (calls === 1) {
        return new Response(
          JSON.stringify({
            error: { message: "Upstream request failed: Thinking mode does not support this tool_choice" },
          }),
          { status: 400 },
        );
      }
      const body = JSON.parse(init.body) as { tool_choice?: unknown };
      expect(body.tool_choice).toBeUndefined();
      return toolCallResponse(VALID_ARGUMENTS);
    });
    const adapter = makeAdapter(transport);
    const result = await adapter.runTurn(INPUT);
    expect(result.decision).toBe("answer");
    expect(calls).toBe(2);
  });

  it("хвостовой слэш endpoint не дублируется", async () => {
    let url = "";
    const adapter = new OpenAICompatModelAdapter({
      endpoint: "https://model.example/v1/",
      apiKey: "key",
      model: "m",
      transport: async (u) => {
        url = u;
        return toolCallResponse(VALID_ARGUMENTS);
      },
    });
    await adapter.runTurn(INPUT);
    expect(url).toBe("https://model.example/v1/chat/completions");
  });
});

describe("согласованность TURN_RESULT_JSON_SCHEMA со схемой zod", () => {
  it("enum решений в JSON Schema совпадает с zod", () => {
    const zodEnum = TurnResultSchema.shape.decision.options;
    const jsonEnum = TURN_RESULT_JSON_SCHEMA.properties.decision.enum;
    expect([...jsonEnum].sort()).toEqual([...zodEnum].sort());
  });
});
