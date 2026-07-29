import assert from "node:assert/strict";
import test from "node:test";

import {
  createChatEventReducerState,
  reduceChatEventChunk,
} from "./chat-event-reducer.mjs";

test("chat event reduction preserves partial lines across chunks", () => {
  const first = reduceChatEventChunk(
    createChatEventReducerState(),
    '{"type":"delta","text":"hel',
  );
  assert.deepEqual(first.events, []);
  assert.equal(first.state.buffer, '{"type":"delta","text":"hel');

  const second = reduceChatEventChunk(
    first.state,
    'lo"}\n{"type":"done"}\n',
  );
  assert.deepEqual(second.events, [
    { type: "delta", text: "hello" },
    { type: "done" },
  ]);
  assert.equal(second.state.buffer, "");
});

test("one malformed event costs one event rather than the remaining answer", () => {
  const result = reduceChatEventChunk(
    createChatEventReducerState(),
    [
      '{"type":"delta","text":"before"}',
      '{"type":',
      '{"type":"delta","text":"after"}',
      '{"type":"done"}',
      "",
    ].join("\n"),
  );
  assert.deepEqual(result.events.map((event) => event.type), [
    "delta",
    "delta",
    "done",
  ]);
  assert.equal(result.malformed.length, 1);
  assert.equal(result.state.malformed, 1);
});

test("the final unterminated event is reduced when the stream closes", () => {
  const result = reduceChatEventChunk(
    createChatEventReducerState(),
    '{"type":"done"}',
    true,
  );
  assert.deepEqual(result.events, [{ type: "done" }]);
});
