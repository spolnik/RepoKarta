import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";

const webRoot = fileURLToPath(new URL(".", import.meta.url));

export default defineConfig({
  plugins: [tailwindcss()],
  build: {
    outDir: "dist",
    // Keep the tracked placeholder so a fresh clone can compile the Go embed
    // package before its first frontend build. Asset names are deterministic.
    emptyOutDir: false,
    rollupOptions: {
      input: resolve(webRoot, "src/main.ts"),
      output: {
        entryFileNames: "assets/app.js",
        assetFileNames: (asset) =>
          asset.names.some((name) => name.endsWith(".css"))
            ? "assets/app.css"
            : "assets/[name][extname]"
      }
    }
  }
});
