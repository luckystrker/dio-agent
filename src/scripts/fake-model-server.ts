// Локальный фейковый OpenAI-совместимый сервер для dry-run разработки:
// отвечает на POST /v1/chat/completions валидным tool-вызовом turn_result.
// Использование: npm run fake-model (порт 8787), затем MODEL_ENDPOINT=http://127.0.0.1:8787/v1

import { createServer } from "node:http";

const PORT = Number(process.env["FAKE_MODEL_PORT"] ?? 8787);

const server = createServer((request, response) => {
  if (request.method !== "POST" || !request.url?.includes("/chat/completions")) {
    response.writeHead(404).end();
    return;
  }
  let body = "";
  request.on("data", (chunk) => {
    body += chunk;
  });
  request.on("end", () => {
    const parsed = JSON.parse(body || "{}") as { messages?: Array<{ role: string; content: string }> };
    const userText = (parsed.messages ?? [])
      .filter((message) => message.role === "user")
      .map((message) => message.content)
      .join(" | ");
    const arguments_ = JSON.stringify({
      decision: "answer",
      messages: [`Привет! Я Dio. Получил пачку: ${userText.slice(0, 120)}`, "Как сам?"],
      notes: ["пользователь поздоровался пачкой сообщений"],
      reason: "прямое обращение в личке",
    });
    response.writeHead(200, { "content-type": "application/json" });
    response.end(
      JSON.stringify({
        choices: [
          {
            message: {
              role: "assistant",
              tool_calls: [
                {
                  type: "function",
                  function: { name: "turn_result", arguments: arguments_ },
                },
              ],
            },
          },
        ],
        usage: { prompt_tokens: 50, completion_tokens: 25 },
      }),
    );
  });
});

server.listen(PORT, "127.0.0.1", () => {
  console.log(`fake-model слушает http://127.0.0.1:${PORT}/v1/chat/completions`);
});
