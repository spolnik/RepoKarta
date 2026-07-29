import assert from "node:assert/strict";
import test from "node:test";
import { JSDOM } from "jsdom";

import { createRepositoryPicker } from "./repository-picker.mjs";

function fixture() {
  const dom = new JSDOM(`
    <select><option value="">Fleet</option><option value="7">Service</option></select>
    <div data-picker data-open="false">
      <button data-trigger aria-expanded="false"></button>
      <span data-current></span><span data-meta></span>
      <div data-popover hidden><input data-search>
        <button data-option data-value="" data-label="Fleet"></button>
        <button data-option data-value="7" data-label="Service"></button>
      </div>
      <div data-backdrop></div>
    </div>
  `);
  const document = dom.window.document;
  const select = document.querySelector("select");
  const picker = document.querySelector("[data-picker]");
  const trigger = document.querySelector("[data-trigger]");
  const current = document.querySelector("[data-current]");
  const meta = document.querySelector("[data-meta]");
  const backdrop = document.querySelector("[data-backdrop]");
  const popover = document.querySelector("[data-popover]");
  const search = document.querySelector("[data-search]");
  const options = Array.from(document.querySelectorAll("[data-option]"));
  return { dom, select, picker, trigger, current, meta, backdrop, popover, search, options };
}

test("repository picker owns selection, filtering, and accessible open state", () => {
  const value = fixture();
  const picker = createRepositoryPicker({
    ...value,
    fallbackToFirst: true,
    describe: (selected) => ({
      label: selected?.dataset.label || "Repository",
      meta: selected?.dataset.value ? "Single repository" : "All repositories"
    })
  });
  picker.sync();
  assert.equal(value.current.textContent, "Fleet");
  value.trigger.click();
  assert.equal(value.popover.hidden, false);
  assert.equal(value.trigger.getAttribute("aria-expanded"), "true");

  value.search.value = "service";
  value.search.dispatchEvent(new value.dom.window.Event("input", { bubbles: true }));
  assert.equal(value.options[0].hidden, true);
  assert.equal(value.options[1].hidden, false);

  value.options[1].click();
  assert.equal(value.select.value, "7");
  assert.equal(value.current.textContent, "Service");
  assert.equal(value.options[1].getAttribute("aria-selected"), "true");
  assert.equal(value.popover.hidden, true);
});

test("repository picker mirrors a disabled native selector", () => {
  const value = fixture();
  value.select.disabled = true;
  const picker = createRepositoryPicker({
    ...value,
    syncDisabled: true,
    describe: (selected, nativeOption) => ({
      label: selected?.dataset.label || nativeOption?.textContent || "Choose",
      meta: selected ? "Workspace" : "Select indexed code"
    })
  });
  picker.sync();
  assert.equal(value.trigger.disabled, true);
  value.trigger.click();
  assert.equal(value.popover.hidden, true);
});
