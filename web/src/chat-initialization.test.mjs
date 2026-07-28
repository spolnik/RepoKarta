import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { JSDOM } from "jsdom";
import { createServer } from "vite";

const sourceDirectory = path.dirname(fileURLToPath(import.meta.url));
const webDirectory = path.resolve(sourceDirectory, "..");
const chatTemplate = await readFile(
  path.resolve(webDirectory, "templates", "chat.html"),
  "utf8"
);

const debugDrawer = `
  <aside id="conversation-debug" hidden>
    <button type="button" data-debug-close>Close</button>
    <button type="button" data-debug-copy>Copy</button>
    <button type="button" data-debug-clear>Clear</button>
    <ol id="debug-entries"></ol>
    <p data-debug-empty>No diagnostics yet.</p>
  </aside>
`;

function renderFixture({ scopeButtons, removeProvider = false }) {
  let html = chatTemplate
    .replace(/\{\{\/\*[\s\S]*?\*\/\}\}/g, "")
    .replace(/\{\{[^}]+\}\}/g, "");
  if (scopeButtons === 2) {
    html = html.replace(
      /(<button type="button" data-conversation-scope="own"[^>]*>Mine<\/button>)/,
      '$1<button type="button" data-conversation-scope="all" aria-pressed="false">All</button>'
    );
  }
  if (removeProvider) {
    html = html.replace('id="conversation-provider"', 'id="missing-conversation-provider"');
  }
  return html.replace("</body>", `${debugDrawer}</body>`);
}

function jsonResponse(body) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" }
  });
}

function installBrowser(html, canViewAll) {
  const dom = new JSDOM(html, {
    pretendToBeVisual: true,
    url: "http://127.0.0.1:7331/chat"
  });
  class XPathEvaluatorCompat extends dom.window.XPathEvaluator {
    createExpression(expression, resolver) {
      const compiled = super.createExpression(expression, resolver);
      return {
        evaluate(contextNode, type = dom.window.XPathResult.ANY_TYPE, result = null) {
          return compiled.evaluate(contextNode, type, result);
        }
      };
    }
  }
  const requests = [];
  const globals = {
    window: dom.window,
    document: dom.window.document,
    location: dom.window.location,
    navigator: dom.window.navigator,
    Node: dom.window.Node,
    Element: dom.window.Element,
    Document: dom.window.Document,
    DocumentFragment: dom.window.DocumentFragment,
    DOMParser: dom.window.DOMParser,
    CustomEvent: dom.window.CustomEvent,
    ShadowRoot: dom.window.ShadowRoot,
    XPathEvaluator: XPathEvaluatorCompat,
    XPathResult: dom.window.XPathResult,
    HTMLElement: dom.window.HTMLElement,
    HTMLAnchorElement: dom.window.HTMLAnchorElement,
    HTMLButtonElement: dom.window.HTMLButtonElement,
    HTMLDialogElement: dom.window.HTMLDialogElement,
    HTMLFormElement: dom.window.HTMLFormElement,
    HTMLInputElement: dom.window.HTMLInputElement,
    HTMLOptionElement: dom.window.HTMLOptionElement,
    HTMLSelectElement: dom.window.HTMLSelectElement,
    HTMLTextAreaElement: dom.window.HTMLTextAreaElement,
    Option: dom.window.Option,
    File: dom.window.File,
    FileReader: dom.window.FileReader
  };
  const originals = new Map();
  for (const [name, value] of Object.entries(globals)) {
    originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, {
      configurable: true,
      writable: true,
      value
    });
  }
  dom.window.matchMedia = (query) => ({
    matches: false,
    media: query,
    onchange: null,
    addEventListener() {},
    removeEventListener() {},
    addListener() {},
    removeListener() {},
    dispatchEvent() {
      return false;
    }
  });
  if (!dom.window.HTMLDialogElement.prototype.showModal) {
    dom.window.HTMLDialogElement.prototype.showModal = function showModal() {
      this.open = true;
    };
  }
  if (!dom.window.HTMLDialogElement.prototype.close) {
    dom.window.HTMLDialogElement.prototype.close = function close() {
      this.open = false;
    };
  }
  const fetchStub = async (input) => {
    const target = String(input);
    requests.push(target);
    if (target === "/api/providers") {
      return jsonResponse({ providers: [] });
    }
    if (target.startsWith("/api/conversations?")) {
      return jsonResponse({
        conversations: [],
        viewer: { id: "test:viewer", name: "Viewer", provider: "test" },
        can_view_all: canViewAll,
        scope: "own"
      });
    }
    if (target === "/api/contexts/named") {
      return jsonResponse({ named_contexts: [] });
    }
    if (target === "/api/contexts/resolve") {
      return jsonResponse({ contexts: [], named_contexts: [] });
    }
    if (target === "/api/whoami") {
      return jsonResponse({ admin: canViewAll });
    }
    return jsonResponse({});
  };
  originals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  Object.defineProperty(globalThis, "fetch", {
    configurable: true,
    writable: true,
    value: fetchStub
  });
  dom.window.fetch = fetchStub;

  return {
    document: dom.window.document,
    requests,
    cleanup() {
      dom.window.close();
      for (const [name, descriptor] of originals) {
        if (descriptor) {
          Object.defineProperty(globalThis, name, descriptor);
        } else {
          delete globalThis[name];
        }
      }
    }
  };
}

async function waitFor(predicate, message) {
  const deadline = Date.now() + 2_000;
  while (!predicate()) {
    if (Date.now() >= deadline) {
      assert.fail(message);
    }
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}

async function runScenario(options) {
  const browser = installBrowser(renderFixture(options), options.scopeButtons === 2);
  const vite = await createServer({
    root: webDirectory,
    logLevel: "silent",
    server: { middlewareMode: true },
    appType: "custom"
  });
  try {
    await vite.ssrLoadModule(`/src/main.ts?scenario=${options.name}`);
    return await options.assertions(browser);
  } finally {
    await vite.close();
    browser.cleanup();
  }
}

test("chat initialization supports owner-only and view-all scope chrome", async () => {
  for (const scopeButtons of [1, 2]) {
    await runScenario({
      name: `scope-${scopeButtons}`,
      scopeButtons,
      async assertions({ document, requests }) {
        await waitFor(
          () => requests.includes("/api/conversations?scope=own") &&
            document.querySelector("[data-conversation-history-empty]").textContent !==
              "Loading saved chats…",
          `scope count ${scopeButtons} did not request saved conversations`
        );
        assert.equal(
          document.querySelector("[data-conversation-history-empty]").textContent,
          "You have no saved chats yet."
        );
        const buttons = [...document.querySelectorAll("[data-conversation-scope]")];
        assert.equal(buttons.length, scopeButtons);
        assert.equal(buttons[0].disabled, scopeButtons === 1);
        assert.equal(buttons[0].getAttribute("aria-pressed"), "true");
        if (scopeButtons === 2) {
          assert.equal(buttons[1].disabled, false);
          assert.equal(buttons[1].hidden, false);
        }
      }
    });
  }
});

test("chat initialization guard reports selector mismatches and shows an error", async () => {
  await runScenario({
    name: "missing-provider",
    scopeButtons: 1,
    removeProvider: true,
    async assertions({ document, requests }) {
      const error = document.querySelector("[data-conversation-initialization-error]");
      assert.ok(error);
      assert.match(error.textContent, /Chat failed to initialise/);
      assert.equal(
        document.querySelector("[data-conversation-history-empty]").textContent,
        "Chat failed to initialise. Open Debug for details."
      );
      assert.equal(requests.some((request) => request.startsWith("/api/conversations?")), false);
      const debugEvent = [...document.querySelectorAll(".debug-entry-event")]
        .find((entry) => entry.textContent === "chat.initialization.failed");
      assert.ok(debugEvent);
      assert.match(debugEvent.parentElement.querySelector(".debug-entry-details").textContent, /#conversation-provider/);
      assert.equal(document.querySelector("#conversation-debug").hidden, false);
    }
  });
});
