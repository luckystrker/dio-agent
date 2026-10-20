// Таймаут-обёртка для внешних вызовов (ARCHITECTURE §5.2, §16).
// Жёсткая стена: Promise.race с дедлайном — обёртка завершается ошибкой
// TimeoutError в границу, даже если fn игнорирует AbortSignal.
// Отменённый (осиротевший) fn может дополнить работу после дедлайна —
// его результат дальше не применяется (§5.2: «отменённый результат
// не применяется»).
export class TimeoutError extends Error {
  constructor(timeoutMs: number) {
    super(`превышен таймаут ${timeoutMs} мс`);
    this.name = "TimeoutError";
  }
}

export async function withTimeout<T>(
  fn: (signal: AbortSignal) => Promise<T>,
  timeoutMs: number,
): Promise<T> {
  const controller = new AbortController();
  let timer: NodeJS.Timeout | undefined;
  const deadline = new Promise<never>((_, reject) => {
    timer = setTimeout(() => {
      const error = new TimeoutError(timeoutMs);
      controller.abort(error);
      reject(error);
    }, timeoutMs);
  });
  try {
    return await Promise.race([fn(controller.signal), deadline]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}
