import { defineConfig } from "vitest/config";

// Deliberately separate from vite.config.ts: the tests render components with
// react-dom/server and need neither the react refresh plugin nor a DOM.
export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
  },
});
