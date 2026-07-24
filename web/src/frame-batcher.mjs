/**
 * Coalesces an arbitrary number of streamed text fragments into at most one
 * write per animation frame. The scheduler is injected so this stays
 * deterministic under Node's built-in test runner.
 *
 * @param {{
 *   schedule: (callback: () => void) => unknown,
 *   cancel: (handle: unknown) => void,
 *   write: (text: string) => void
 * }} options
 */
export function createFrameBatcher(options) {
  let pending = "";
  let scheduled;
  let active = true;
  let flushes = 0;

  const flush = () => {
    scheduled = undefined;
    if (!active || !pending) {
      return;
    }
    const text = pending;
    pending = "";
    options.write(text);
    flushes++;
  };

  return {
    append(text) {
      if (!active || !text) {
        return;
      }
      pending += text;
      if (scheduled === undefined) {
        scheduled = options.schedule(flush);
      }
    },
    finish() {
      if (!active) {
        return flushes;
      }
      if (scheduled !== undefined) {
        options.cancel(scheduled);
        scheduled = undefined;
      }
      flush();
      active = false;
      return flushes;
    },
    cancel() {
      active = false;
      pending = "";
      if (scheduled !== undefined) {
        options.cancel(scheduled);
        scheduled = undefined;
      }
    }
  };
}
