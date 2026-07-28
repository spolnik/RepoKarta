import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const readJSON = (name) =>
  JSON.parse(readFileSync(resolve(webRoot, name), "utf8"));

const manifest = readJSON("package.json");
const lockfile = readJSON("package-lock.json");
const policy = readJSON("dependency-policy.json");
const npmrc = readFileSync(resolve(webRoot, "..", ".npmrc"), "utf8");
const directDependencies = {
  ...manifest.dependencies,
  ...manifest.devDependencies,
  ...manifest.optionalDependencies,
};
const lockRoot = lockfile.packages?.[""] ?? {};
const lockedDirectDependencies = {
  ...lockRoot.dependencies,
  ...lockRoot.devDependencies,
  ...lockRoot.optionalDependencies,
};
const errors = [];

const boundedSemver = /^(?:\^|~)?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;
for (const [name, spec] of Object.entries(directDependencies)) {
  if (!boundedSemver.test(spec)) {
    errors.push(
      `${name} uses unbounded or non-registry spec ${JSON.stringify(spec)}`,
    );
  }
  if (lockedDirectDependencies[name] !== spec) {
    errors.push(
      `${name} differs between package.json (${spec}) and package-lock.json (${lockedDirectDependencies[name] ?? "missing"})`,
    );
  }
}

const configuredReleaseAge = Number(
  npmrc.match(/^\s*min-release-age\s*=\s*(\d+)\s*$/m)?.[1],
);
if (
  !Number.isInteger(configuredReleaseAge) ||
  configuredReleaseAge < policy.minimumReleaseAgeDays
) {
  errors.push(
    `.npmrc min-release-age must be at least ${policy.minimumReleaseAgeDays} days`,
  );
}
if (!/^\s*save-exact\s*=\s*true\s*$/m.test(npmrc)) {
  errors.push(".npmrc must keep save-exact=true");
}

for (const [name, approval] of Object.entries(
  policy.approvedDependencies ?? {},
)) {
  const manifestSpec = directDependencies[name];
  if (manifestSpec !== approval.manifestRange) {
    errors.push(
      `${name} must remain ${approval.manifestRange}; found ${manifestSpec ?? "missing"}`,
    );
  }

  const lockedPackage = lockfile.packages?.[`node_modules/${name}`];
  const lockedMajor = Number(lockedPackage?.version?.split(".", 1)[0]);
  if (lockedMajor !== approval.major) {
    errors.push(
      `${name} lockfile major must remain ${approval.major}; found ${lockedPackage?.version ?? "missing"}`,
    );
  }
}

for (const [path, lockedPackage] of Object.entries(lockfile.packages ?? {})) {
  if (
    path &&
    lockedPackage.resolved?.startsWith("https://registry.npmjs.org/") &&
    !lockedPackage.integrity
  ) {
    errors.push(`${path} is missing lockfile integrity metadata`);
  }
}

if (errors.length > 0) {
  console.error("npm dependency policy failed:");
  for (const error of errors) {
    console.error(`- ${error}`);
  }
  process.exitCode = 1;
} else {
  console.log(
    `npm dependency policy passed (${configuredReleaseAge}-day release-age window)`,
  );
}
