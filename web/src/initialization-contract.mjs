/**
 * @typedef {{
 *   key: string,
 *   selector: string,
 *   all?: boolean,
 *   min?: number,
 *   max?: number,
 *   expected?: (values: Record<string, Element | Element[] | null>) => {min: number, max: number}
 * }} ElementContract
 */

/**
 * Resolve an interface from one declarative table and derive its diagnostic
 * checks from that same table, preventing selectors and guards from drifting.
 *
 * @param {ParentNode} root
 * @param {ElementContract[]} contracts
 */
export function collectRequiredElements(root, contracts) {
  /** @type {Record<string, Element | Element[] | null>} */
  const values = {};
  for (const contract of contracts) {
    values[contract.key] = contract.all
      ? Array.from(root.querySelectorAll(contract.selector))
      : root.querySelector(contract.selector);
  }

  const checks = contracts.map((contract) => {
    const value = values[contract.key];
    const actual = Array.isArray(value) ? value.length : Number(Boolean(value));
    const bounds = contract.expected?.(values) ?? {
      min: contract.min ?? 1,
      max: contract.max ?? 1,
    };
    const expected = bounds.min === bounds.max
      ? String(bounds.min)
      : bounds.max === Number.POSITIVE_INFINITY
        ? `at least ${bounds.min}`
        : `${bounds.min}..${bounds.max}`;
    return {
      key: contract.key,
      selector: contract.selector,
      expected,
      actual,
      valid: actual >= bounds.min && actual <= bounds.max,
    };
  });
  return {
    values,
    checks,
    mismatches: checks.filter((check) => !check.valid),
    valid: checks.every((check) => check.valid),
  };
}
