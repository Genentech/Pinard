import { defineConfig } from "vitest/config";
import { resolve } from "path";

export default defineConfig({
  resolve: {
    alias: {
      "@pinard/logic": resolve(__dirname, "../lib/logic.ts"),
      "@pinard/classify": resolve(__dirname, "../lib/classify.ts"),
      "@pinard/teaching": resolve(__dirname, "../lib/teaching.ts"),
      "@pinard/log-rotate": resolve(__dirname, "../lib/logRotate.ts"),
    },
  },
  test: {
    globals: true,
    testTimeout: 30_000,
    hookTimeout: 15_000,
    include: ["unit/**/*.test.ts", "integration/**/*.test.ts", "contract/**/*.test.ts"],
    globalSetup: ["./setup/global-nats-setup.ts"],
  },
});
