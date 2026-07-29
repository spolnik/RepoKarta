export function boundedPollRetry(
  failureCount: number,
  options?: {
    maximumFailures?: number;
    baseDelayMS?: number;
    maximumDelayMS?: number;
  }
): { retry: boolean; delayMS: number };
