/**
 * @typedef {import("./generated/api-contract").ConversationEvent} ConversationEvent
 * @typedef {{buffer: string, malformed: number}} ChatEventReducerState
 */

/** @returns {ChatEventReducerState} */
export function createChatEventReducerState() {
  return { buffer: "", malformed: 0 };
}

/**
 * Reduce one decoded stream chunk into complete, minimally validated events.
 * A malformed line is reported and skipped; it never consumes later events.
 *
 * @param {ChatEventReducerState} state
 * @param {string} chunk
 * @param {boolean} [done]
 */
export function reduceChatEventChunk(state, chunk, done = false) {
  const combined = state.buffer + chunk;
  const lines = combined.split("\n");
  const buffer = done ? "" : lines.pop() ?? "";
  if (done && lines.at(-1) === "") {
    lines.pop();
  }
  /** @type {ConversationEvent[]} */
  const events = [];
  /** @type {Array<{line: string, reason: string}>} */
  const malformed = [];
  for (const line of lines) {
    if (!line.trim()) {
      continue;
    }
    try {
      const value = /** @type {unknown} */ (JSON.parse(line));
      if (
        !value ||
        typeof value !== "object" ||
        Array.isArray(value) ||
        typeof /** @type {{type?: unknown}} */ (value).type !== "string"
      ) {
        throw new TypeError("event must be an object with a string type");
      }
      events.push(/** @type {ConversationEvent} */ (value));
    } catch (error) {
      malformed.push({
        line,
        reason: error instanceof Error ? error.message : String(error),
      });
    }
  }
  return {
    state: {
      buffer,
      malformed: state.malformed + malformed.length,
    },
    events,
    malformed,
  };
}
