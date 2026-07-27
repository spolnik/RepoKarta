/**
 * Apply a server-provided completion. Browser string offsets are UTF-16 code
 * units, which is also the convention used by selectionStart/selectionEnd.
 */
export function applyQueryCompletion(value, completion) {
  const start = Math.max(0, Math.min(value.length, completion.replace_start));
  const end = Math.max(start, Math.min(value.length, completion.replace_end));
  const nextValue = value.slice(0, start) + completion.insert_text + value.slice(end);
  return {
    value: nextValue,
    cursor: start + completion.insert_text.length
  };
}
