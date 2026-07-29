import { rm } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const assetsDirectory = resolve(webRoot, "dist", "assets");

await rm(assetsDirectory, { force: true, recursive: true });
console.log(`Cleaned ${assetsDirectory}`);
