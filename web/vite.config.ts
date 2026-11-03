import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The Go server embeds the build from server/internal/web/dist.
export default defineConfig({
  plugins: [react()],
  build: { outDir: "../server/internal/web/dist", emptyOutDir: true },
  server: { proxy: { "/api": "http://localhost:8080" } },
});
