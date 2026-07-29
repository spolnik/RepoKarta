import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const readJSON = (name) =>
  JSON.parse(readFileSync(resolve(webRoot, name), "utf8"));

const boundedSemver = /^(?:\^|~)?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;

/**
 * @param {string} spec
 * @param {string | undefined} locked
 */
function overrideMatchesLock(spec, locked) {
  if (!locked) {
    return false;
  }
  const requested = spec.replace(/^[~^]/, "").split("-", 1)[0].split(".");
  const actual = locked.split("-", 1)[0].split(".");
  if (spec.startsWith("^")) {
    return requested[0] === actual[0];
  }
  if (spec.startsWith("~")) {
    return requested[0] === actual[0] && requested[1] === actual[1];
  }
  return spec === locked;
}

/**
 * Validate every override leaf against the same bounded-registry policy as a
 * direct dependency and prove that the committed lockfile resolved it.
 *
 * @param {Record<string, unknown>} overrides
 * @param {Record<string, {version?: string}>} packages
 * @returns {string[]}
 */
export function validateOverrides(overrides, packages) {
  const errors = [];
  const visit = (entries, parent = "") => {
    for (const [name, value] of Object.entries(entries)) {
      const coordinate = parent ? `${parent} > ${name}` : name;
      if (typeof value === "string") {
        if (!boundedSemver.test(value)) {
          errors.push(
            `override ${coordinate} uses unbounded or non-registry spec ${JSON.stringify(value)}`,
          );
          continue;
        }
        const lockedVersions = Object.entries(packages)
          .filter(([path]) =>
            path === `node_modules/${name}` ||
            path.endsWith(`/node_modules/${name}`)
          )
          .map(([, entry]) => entry.version)
          .filter((version) => typeof version === "string");
        if (
          lockedVersions.length === 0 ||
          lockedVersions.some((locked) => !overrideMatchesLock(value, locked))
        ) {
          errors.push(
            `override ${coordinate} (${value}) does not match package-lock.json (${lockedVersions.join(", ") || "missing"})`,
          );
        }
        continue;
      }
      if (value && typeof value === "object" && !Array.isArray(value)) {
        visit(/** @type {Record<string, unknown>} */ (value), coordinate);
        continue;
      }
      errors.push(`override ${coordinate} must be a bounded registry version`);
    }
  };
  visit(overrides);
  return errors;
}

export function checkDependencyPolicy() {
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

  errors.push(...validateOverrides(
    manifest.overrides ?? {},
    lockfile.packages ?? {},
  ));

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
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  checkDependencyPolicy();
}
