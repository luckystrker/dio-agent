import { configDefaults, defineConfig } from "vitest/config";

// Гейт без БД (npm run test): юнит- и PBT-тесты.
// Интеграционные тесты с живым PostgreSQL (*.db.test.ts) — отдельный
// скрипт test:db и vitest.db.config.ts.
export default defineConfig({
  test: {
    include: ["src/**/*.test.ts"],
    exclude: [...configDefaults.exclude, "src/**/*.db.test.ts"],
    environment: "node",
  },
});
