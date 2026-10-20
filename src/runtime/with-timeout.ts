// Таймаут-обёртка для внешних вызовов (ARCHITECTURE §5.2, §16).
// Отмена — через AbortSignal; отменённый результат не применяется.
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
  const timer = setTimeout(() => controller.abort(new TimeoutError(timeoutMs)), timeoutMs);
  try {
    return await fn(controller.signal);
  } finally {
    clearTimeout(timer);
  }
}
