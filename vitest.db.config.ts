import { defineConfig } from "vitest/config";

// Интеграционные тесты с живым PostgreSQL (npm run test:db).
// Отдельная база dio_test поднимается и мигрируется в globalSetup.
// Файлы выполняются последовательно: все пишут в одну тестовую базу.
export default defineConfig({
  test: {
    include: ["src/**/*.db.test.ts"],
    environment: "node",
    fileParallelism: false,
    testTimeout: 30_000,
    hookTimeout: 60_000,
    globalSetup: ["./src/test-support/db-global-setup.ts"],
  },
});
