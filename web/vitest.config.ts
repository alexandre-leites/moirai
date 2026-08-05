import { defineConfig } from "vitest/config";

// Deliberately separate from vite.config.ts: the react refresh plugin has no
// place in a test run. The default environment is node, because most tests
// render with react-dom/server; the files that mount a component and let its
// effects fetch opt into jsdom with a `// @vitest-environment jsdom` docblock.
export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    // Only active with `--coverage` (`make coverage-web` / `npm run test --
    // --coverage`); a plain `npm test` run is unaffected. Thresholds sit
    // below the suite's actual numbers (statements 84.8%, branches 77.2%,
    // functions 83.2%, lines 87.9% as of issue #372) so the gate that adds
    // coverage reporting does not itself fail CI on a routine run-to-run
    // wobble. Raise a threshold only in the same PR that raises the real
    // number -- see the Go equivalent in scripts/go-coverage.sh /
    // Makefile#coverage-go for the same rule applied to the Go modules.
    coverage: {
      provider: "v8",
      reporter: ["text", "text-summary"],
      thresholds: {
        statements: 70,
        branches: 65,
        functions: 70,
        lines: 75,
      },
    },
  },
});
