import path from "node:path"
import { defineConfig } from "vitest/config"
import react from "@vitejs/plugin-react"
import tailwindcss from "@tailwindcss/vite"
import { tanstackRouter } from "@tanstack/router-plugin/vite"

// The Go server (quai server) listens on :8080 and serves /api, /auth and /healthz. In
// development Vite proxies them, so the browser sees one origin and the session cookie works.
const backend = process.env.QUAI_BACKEND ?? "http://localhost:8080"

export default defineConfig({
  plugins: [
    tanstackRouter({ target: "react", autoCodeSplitting: true }), // before react()
    react(),
    tailwindcss(),
  ],
  resolve: { alias: { "@": path.resolve(import.meta.dirname, "./src") } },
  build: {
    rolldownOptions: {
      // Libraries change less often than the app: their chunks stay cached across releases.
      output: {
        codeSplitting: {
          groups: [
            { name: "react", test: /node_modules[\\/](react|react-dom|scheduler)[\\/]/, priority: 3 },
            { name: "tanstack", test: /node_modules[\\/]@tanstack[\\/]/, priority: 2 },
            { name: "ui", test: /node_modules[\\/](radix-ui|@radix-ui|lucide-react|sonner)[\\/]/, priority: 1 },
            // Loaded with the pages that use them: the designer's canvas, and the forms' CEL.
            { name: "flow", test: /node_modules[\\/](@xyflow|d3-[a-z-]+|classcat)[\\/]/, priority: 1 },
            { name: "cel", test: /node_modules[\\/]@marcbachmann[\\/]/, priority: 1 },
            { name: "vendor", test: /node_modules/ },
          ],
        },
      },
    },
  },
  server: {
    proxy: {
      "/api": { target: backend, changeOrigin: false },
      "/auth": { target: backend, changeOrigin: false },
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    css: false,
  },
})
