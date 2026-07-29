import { defineConfig } from "vitest/config";

// Deliberately separate from vite.config.ts: the react refresh plugin has no
// place in a test run. The default environment is node, because most tests
// render with react-dom/server; the files that mount a component and let its
// effects fetch opt into jsdom with a `// @vitest-environment jsdom` docblock.
export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
  },
});
