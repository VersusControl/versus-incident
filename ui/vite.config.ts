/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import path from "node:path";
import { availableParallelism } from "node:os";

// Dev server proxies /api/* to the Go backend so we don't need to deal with
// CORS while running `./run.sh` locally.
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "@": path.resolve(import.meta.dirname, "./src"),
    },
  },
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: process.env.VITE_API_PROXY_TARGET || "http://localhost:3000",
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: "node",
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    // DOM-heavy suites retain full assertions without oversubscribing the runner.
    maxWorkers: Math.min(4, availableParallelism()),
  },
});
