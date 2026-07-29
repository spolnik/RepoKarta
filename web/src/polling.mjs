/**
 * @param {number} failureCount
 * @param {{maximumFailures?: number, baseDelayMS?: number, maximumDelayMS?: number}} [options]
 */
export function boundedPollRetry(failureCount, options = {}) {
  const maximumFailures = options.maximumFailures ?? 5;
  const baseDelayMS = options.baseDelayMS ?? 500;
  const maximumDelayMS = options.maximumDelayMS ?? 8_000;
  const failures = Math.max(1, failureCount);
  return {
    retry: failures <= maximumFailures,
    delayMS: Math.min(maximumDelayMS, baseDelayMS * (2 ** (failures - 1))),
  };
}
