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
): Promise<string | null> {
  if (!budget) return null;
  const used = await budget.reservedToday("model:");
  if (used >= dailyCallsLimit) {
    throw new SkipWorkError(
      "дневной бюджет модельных вызовов исчерпан",
      `дневной бюджет модельных вызовов исчерпан (${used}/${dailyCallsLimit})`,
    );
  }
  // false = эта попытка уже оплачена (повтор того же work_item после сбоя)
  const operationId = `model:${workItemId}:${attempt}`;
  await budget.tryReserve(operationId);
  return operationId; // журнал хода §20: бюджетная бронь вызова
}
