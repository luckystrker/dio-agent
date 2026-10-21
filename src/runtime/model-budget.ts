import type { Budget } from "./budgets.js";
import { SkipWorkError } from "./worker.js";

// Общий резерв модельного вызова (§16, чек-лист 1.9): один механизм для
// разговорных и фоновых вызовов. operation_id детерминирован на попытку —
// retry после сбоя не расходует лимит дважды.

export async function reserveModelCall(
  budget: Budget | null,
  dailyCallsLimit: number,
  workItemId: string,
  attempt: number,
): Promise<void> {
  if (!budget) return;
  const used = await budget.reservedToday();
  if (used >= dailyCallsLimit) {
    throw new SkipWorkError(
      "дневной бюджет модельных вызовов исчерпан",
      `дневной бюджет модельных вызовов исчерпан (${used}/${dailyCallsLimit})`,
    );
  }
  // false = эта попытка уже оплачена (повтор того же work_item после сбоя)
  await budget.tryReserve(`model:${workItemId}:${attempt}`);
}
