import htmx from "htmx.org";
import hljs from "highlight.js/lib/common";
import DOMPurify from "dompurify";
import { marked } from "marked";
import "./styles.css";

htmx.config.allowEval = false;

function connectIndexEvents(): void {
  const repositoryList = document.querySelector<HTMLElement>("#repository-list");
  if (!repositoryList || !("EventSource" in window)) {
    return;
  }

  const events = new EventSource("/events");
  events.addEventListener("repositories", () => {
    void htmx.ajax("get", "/repositories", {
      target: "#repository-list",
      swap: "outerHTML"
    });
  });
  window.addEventListener("pagehide", () => events.close(), { once: true });
}

function enableQueryChips(): void {
  const form = document.querySelector<HTMLFormElement>('form[action="/search"]');
  const input = document.querySelector<HTMLInputElement>("#search-query");
  if (!form || !input) {
    return;
  }

  document.querySelectorAll<HTMLButtonElement>("[data-query]").forEach((button) => {
    button.addEventListener("click", () => {
      input.value = button.dataset.query ?? "";
      form.requestSubmit();
    });
  });
}

function enableCopyButtons(): void {
  document.querySelectorAll<HTMLButtonElement>("[data-copy]").forEach((button) => {
    button.addEventListener("click", async () => {
      const value = button.dataset.copy;
      if (!value) {
        return;
      }
      const originalLabel = button.textContent;
      try {
        await navigator.clipboard.writeText(value);
        button.textContent = "Copied";
      } catch {
        button.textContent = "Copy failed";
      }
      window.setTimeout(() => {
        button.textContent = originalLabel;
      }, 1600);
    });
  });
}

function highlightSource(): void {
  document.querySelectorAll<HTMLElement>("code[data-highlight-language]").forEach((element) => {
    hljs.highlightElement(element);
  });
}

function focusSourceLine(): void {
  const hashTarget = /^#L\d+$/.test(location.hash)
    ? document.getElementById(location.hash.slice(1))
    : null;
  const target = hashTarget ?? document.querySelector<HTMLElement>(".source-line-focused");
  if (!target) {
    return;
  }
  window.requestAnimationFrame(() => {
    target.scrollIntoView({ block: "center", inline: "nearest" });
  });
}

type MatchRange = {
  start: number;
  end: number;
};

function parseMatchRanges(value: string | undefined): MatchRange[] {
  if (!value) {
    return [];
  }
  return value
    .split(",")
    .map((item) => {
      const [start, end] = item.split(":").map(Number);
      return { start, end };
    })
    .filter(({ start, end }) => Number.isInteger(start) && Number.isInteger(end) && start >= 0 && end > start);
}

function restoreMatchHighlights(element: HTMLElement, ranges: MatchRange[]): void {
  if (ranges.length === 0) {
    return;
  }
  const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
  const textNodes: Array<{ node: Text; start: number; end: number }> = [];
  let offset = 0;
  while (walker.nextNode()) {
    const node = walker.currentNode as Text;
    const end = offset + node.data.length;
    textNodes.push({ node, start: offset, end });
    offset = end;
  }

  for (const item of textNodes) {
    const overlaps = ranges
      .map((range) => ({
        start: Math.max(0, range.start - item.start),
        end: Math.min(item.node.data.length, range.end - item.start)
      }))
      .filter((range) => range.end > range.start)
      .sort((left, right) => left.start - right.start);
    if (overlaps.length === 0) {
      continue;
    }

    const replacement = document.createDocumentFragment();
    let position = 0;
    for (const overlap of overlaps) {
      const start = Math.max(position, overlap.start);
      if (overlap.end <= start) {
        continue;
      }
      if (start > position) {
        replacement.append(item.node.data.slice(position, start));
      }
      const mark = document.createElement("mark");
      mark.className = "search-highlight";
      mark.textContent = item.node.data.slice(start, overlap.end);
      replacement.append(mark);
      position = overlap.end;
    }
    if (position < item.node.data.length) {
      replacement.append(item.node.data.slice(position));
    }
    item.node.replaceWith(replacement);
  }
}

function highlightSearchResults(root: ParentNode = document): void {
  root.querySelectorAll<HTMLElement>("code[data-search-highlight-language]").forEach((element) => {
    if (element.dataset.syntaxHighlighted === "true") {
      return;
    }
    const source = element.textContent ?? "";
    const language = (element.dataset.searchHighlightLanguage ?? "").toLowerCase();
    const highlighted = language && hljs.getLanguage(language)
      ? hljs.highlight(source, { language, ignoreIllegals: true })
      : hljs.highlightAuto(source);
    element.innerHTML = highlighted.value;
    element.classList.add("hljs");
    element.dataset.syntaxHighlighted = "true";
    restoreMatchHighlights(element, parseMatchRanges(element.dataset.matchRanges));
  });
}

type ConversationEvent = {
  type: "meta" | "delta" | "sources" | "done" | "error";
  conversation_id?: string;
  text?: string;
  sources?: Array<{ label: string; url: string }>;
};

type ProviderStatus = {
  id: string;
  name: string;
  available: boolean;
  authenticated: boolean;
  detail?: string;
  models?: string[];
  model_placeholder?: string;
  efforts?: string[];
};

type DebugLevel = "info" | "warn" | "error";

type DebugEntry = {
  timestamp: string;
  level: DebugLevel;
  event: string;
  details?: unknown;
};

type DebugLogger = {
  add: (level: DebugLevel, event: string, details?: unknown) => void;
  open: () => void;
};

type MermaidAPI = typeof import("mermaid")["default"];

let mermaidLoader: Promise<MermaidAPI> | undefined;
let mermaidRenderQueue: Promise<void> = Promise.resolve();
let mermaidDiagramID = 0;
const markdownRenderRevisions = new WeakMap<HTMLElement, number>();

function describeError(error: unknown): Record<string, unknown> {
  if (error instanceof Error) {
    return {
      name: error.name,
      message: error.message,
      stack: error.stack
    };
  }
  return { value: String(error) };
}

function formatDebugDetails(value: unknown): string {
  const seen = new WeakSet<object>();
  try {
    return JSON.stringify(value, (_key, item: unknown) => {
      if (typeof item === "object" && item !== null) {
        if (seen.has(item)) {
          return "[Circular]";
        }
        seen.add(item);
      }
      return item;
    }, 2);
  } catch {
    return String(value);
  }
}

function enableDebugLogger(): DebugLogger | undefined {
  const panel = document.querySelector<HTMLElement>("#conversation-debug");
  const list = document.querySelector<HTMLOListElement>("#debug-entries");
  const empty = document.querySelector<HTMLElement>("[data-debug-empty]");
  const toggle = document.querySelector<HTMLButtonElement>("[data-debug-toggle]");
  const count = document.querySelector<HTMLElement>("[data-debug-count]");
  const close = document.querySelector<HTMLButtonElement>("[data-debug-close]");
  const copy = document.querySelector<HTMLButtonElement>("[data-debug-copy]");
  const clear = document.querySelector<HTMLButtonElement>("[data-debug-clear]");
  if (!panel || !list || !empty || !toggle || !count || !close || !copy || !clear) {
    return undefined;
  }

  const entries: DebugEntry[] = [];

  const setOpen = (open: boolean): void => {
    panel.hidden = !open;
    toggle.setAttribute("aria-expanded", String(open));
    if (open) {
      list.scrollTop = list.scrollHeight;
    }
  };

  const add = (level: DebugLevel, event: string, details?: unknown): void => {
    const entry: DebugEntry = {
      timestamp: new Date().toISOString(),
      level,
      event,
      details
    };
    entries.push(entry);
    if (entries.length > 250) {
      entries.shift();
      list.firstElementChild?.remove();
    }
    if (level === "error") {
      toggle.classList.add("debug-toggle-debug-error");
    }

    const item = document.createElement("li");
    item.className = `debug-entry debug-entry-${level}`;
    const time = document.createElement("time");
    time.dateTime = entry.timestamp;
    time.textContent = entry.timestamp.slice(11, 23);
    const levelLabel = document.createElement("span");
    levelLabel.className = "debug-entry-level";
    levelLabel.textContent = level;
    const eventLabel = document.createElement("span");
    eventLabel.className = "debug-entry-event";
    eventLabel.textContent = event;
    item.append(time, levelLabel, eventLabel);
    if (details !== undefined) {
      const detail = document.createElement("pre");
      detail.className = "debug-entry-details";
      detail.textContent = formatDebugDetails(details);
      item.append(detail);
    }
    list.append(item);
    empty.hidden = true;
    count.textContent = String(entries.length);
    if (!panel.hidden) {
      list.scrollTop = list.scrollHeight;
    }
    if (level === "error") {
      setOpen(true);
    }
  };

  toggle.addEventListener("click", () => setOpen(panel.hasAttribute("hidden")));
  close.addEventListener("click", () => setOpen(false));
  clear.addEventListener("click", () => {
    entries.length = 0;
    list.replaceChildren();
    empty.hidden = false;
    count.textContent = "0";
    toggle.classList.remove("debug-toggle-debug-error");
  });
  copy.addEventListener("click", async () => {
    const original = copy.textContent;
    try {
      await navigator.clipboard.writeText(entries.map((entry) => {
        const heading = `[${entry.timestamp}] ${entry.level.toUpperCase()} ${entry.event}`;
        return entry.details === undefined ? heading : `${heading}\n${formatDebugDetails(entry.details)}`;
      }).join("\n\n"));
      copy.textContent = "Copied";
    } catch (error: unknown) {
      add("error", "debug.copy.failed", describeError(error));
      copy.textContent = "Copy failed";
    }
    window.setTimeout(() => {
      copy.textContent = original;
    }, 1600);
  });

  window.addEventListener("error", (event) => {
    add("error", "browser.error", {
      message: event.message,
      filename: event.filename,
      line: event.lineno,
      column: event.colno,
      error: describeError(event.error)
    });
  });
  window.addEventListener("unhandledrejection", (event) => {
    add("error", "browser.unhandled-rejection", describeError(event.reason));
  });
  window.addEventListener("offline", () => add("warn", "browser.offline"));
  window.addEventListener("online", () => add("info", "browser.online"));

  add("info", "debug.session.started", {
    path: location.pathname,
    online: navigator.onLine,
    userAgent: navigator.userAgent
  });

  return {
    add,
    open: () => setOpen(true)
  };
}

async function probeServerHealth(debug: DebugLogger): Promise<void> {
  const started = performance.now();
  debug.add("info", "server.health.probe.started");
  try {
    const response = await fetch("/healthz", {
      cache: "no-store",
      headers: { Accept: "application/json" }
    });
    const body = await response.text();
    debug.add(response.ok ? "info" : "warn", "server.health.probe.completed", {
      status: response.status,
      duration_ms: Math.round(performance.now() - started),
      body: body.slice(0, 500)
    });
  } catch (error: unknown) {
    debug.add("error", "server.health.probe.failed", {
      duration_ms: Math.round(performance.now() - started),
      ...describeError(error)
    });
  }
}

async function loadMermaid(): Promise<MermaidAPI> {
  if (!mermaidLoader) {
    mermaidLoader = import("mermaid")
      .then(({ default: mermaid }) => {
        mermaid.initialize({
          startOnLoad: false,
          securityLevel: "strict",
          suppressErrorRendering: true,
          maxTextSize: 50_000,
          maxEdges: 500,
          htmlLabels: false,
          theme: "base",
          themeVariables: {
            background: "#080b10",
            primaryColor: "#111827",
            primaryTextColor: "#e2e8f0",
            primaryBorderColor: "#34d399",
            lineColor: "#64748b",
            secondaryColor: "#1e1b4b",
            tertiaryColor: "#101318",
            noteBkgColor: "#172033",
            noteBorderColor: "#8b5cf6",
            noteTextColor: "#e2e8f0",
            fontFamily: "Inter, ui-sans-serif, system-ui, sans-serif"
          },
          flowchart: {
            htmlLabels: false,
            useMaxWidth: true
          },
          sequence: {
            useMaxWidth: true
          }
        });
        return mermaid;
      })
      .catch((error: unknown) => {
        mermaidLoader = undefined;
        throw error;
      });
  }
  return mermaidLoader;
}

async function renderMermaidDiagram(
  target: HTMLElement,
  canvas: HTMLElement,
  source: string,
  revision: number,
  index: number,
  debug?: DebugLogger
): Promise<void> {
  const started = performance.now();
  try {
    const mermaid = await loadMermaid();
    const result = await mermaid.render(`repokarta-mermaid-${++mermaidDiagramID}`, source);
    if (markdownRenderRevisions.get(target) !== revision || !canvas.isConnected) {
      return;
    }
    // Strict mode is locked in the site config, so diagram directives cannot
    // enable HTML labels or click actions in this generated SVG.
    canvas.innerHTML = result.svg;
    canvas.classList.remove("mermaid-diagram-loading");
    canvas.setAttribute("aria-label", `${result.diagramType} diagram`);
    debug?.add("info", "chat.diagram.rendered", {
      index,
      type: result.diagramType,
      duration_ms: Math.round(performance.now() - started)
    });
  } catch (error: unknown) {
    if (markdownRenderRevisions.get(target) !== revision || !canvas.isConnected) {
      return;
    }
    canvas.classList.remove("mermaid-diagram-loading");
    canvas.classList.add("mermaid-diagram-error");
    canvas.removeAttribute("role");
    canvas.textContent = "This Mermaid diagram could not be rendered. Its source is available below.";
    const sourceDetails = canvas.parentElement?.querySelector<HTMLDetailsElement>(".mermaid-diagram-source");
    if (sourceDetails) {
      sourceDetails.open = true;
    }
    debug?.add("warn", "chat.diagram.render-failed", {
      index,
      error_type: error instanceof Error ? error.name : "UnknownError",
      duration_ms: Math.round(performance.now() - started)
    });
  }
}

function renderAssistantMarkdown(
  target: HTMLElement,
  markdown: string,
  debug?: DebugLogger,
  renderDiagrams = false
): void {
  const revision = (markdownRenderRevisions.get(target) ?? 0) + 1;
  markdownRenderRevisions.set(target, revision);
  const rendered = marked.parse(markdown, {
    async: false,
    breaks: false,
    gfm: true
  });
  target.innerHTML = DOMPurify.sanitize(rendered, {
    FORBID_ATTR: ["style"],
    FORBID_TAGS: ["button", "embed", "form", "iframe", "img", "input", "object", "select", "style", "svg", "textarea"],
    USE_PROFILES: { html: true }
  });

  target.querySelectorAll<HTMLAnchorElement>("a").forEach((link) => {
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    link.classList.add("conversation-source-link");
  });
  target.querySelectorAll<HTMLElement>("pre code").forEach((code, index) => {
    if (code.classList.contains("language-mermaid")) {
      if (!renderDiagrams) {
        return;
      }
      const pre = code.closest("pre");
      if (!pre) {
        return;
      }
      const source = code.textContent ?? "";
      const figure = document.createElement("figure");
      figure.className = "mermaid-diagram";
      const canvas = document.createElement("div");
      canvas.className = "mermaid-diagram-canvas mermaid-diagram-loading";
      canvas.setAttribute("role", "img");
      canvas.setAttribute("aria-label", "Rendering Mermaid diagram");
      canvas.textContent = "Rendering diagram…";
      const sourceDetails = document.createElement("details");
      sourceDetails.className = "mermaid-diagram-source";
      const sourceSummary = document.createElement("summary");
      sourceSummary.textContent = "View Mermaid source";
      sourceDetails.append(sourceSummary, pre.cloneNode(true));
      figure.append(canvas, sourceDetails);
      pre.replaceWith(figure);
      mermaidRenderQueue = mermaidRenderQueue.then(
        () => renderMermaidDiagram(target, canvas, source, revision, index + 1, debug)
      );
      return;
    }
    const languageClass = Array.from(code.classList).find((className) => className.startsWith("language-"));
    const language = languageClass?.slice("language-".length);
    if (language && !hljs.getLanguage(language)) {
      return;
    }
    hljs.highlightElement(code);
  });
}

function conversationMessage(role: "user" | "assistant" | "error", text = ""): HTMLElement {
  const wrapper = document.createElement("article");
  wrapper.className = `conversation-message conversation-message-${role}`;
  const label = document.createElement("p");
  label.className = "conversation-role";
  label.textContent = role === "user" ? "You" : role === "assistant" ? "RepoKarta" : "Provider error";
  const content = document.createElement("div");
  content.className = "conversation-content";
  if (text) {
    content.textContent = text;
  }
  wrapper.append(label, content);
  return wrapper;
}

function enableConversations(debug?: DebugLogger): void {
  const form = document.querySelector<HTMLFormElement>("#conversation-form");
  const messages = document.querySelector<HTMLElement>("#conversation-messages");
  const empty = document.querySelector<HTMLElement>("[data-conversation-empty]");
  const provider = document.querySelector<HTMLSelectElement>("#conversation-provider");
  const model = document.querySelector<HTMLInputElement>("#conversation-model");
  const modelOptions = document.querySelector<HTMLDataListElement>("#conversation-model-options");
  const effort = document.querySelector<HTMLSelectElement>("#conversation-effort");
  const input = document.querySelector<HTMLTextAreaElement>("#conversation-message");
  const submit = document.querySelector<HTMLButtonElement>("#conversation-submit");
  const detail = document.querySelector<HTMLElement>("#provider-detail");
  const newConversation = document.querySelector<HTMLButtonElement>("[data-new-conversation]");
  if (!form || !messages || !provider || !model || !modelOptions || !effort || !input || !submit || !detail || !newConversation) {
    return;
  }

  let conversationID = "";
  let busy = false;
  let statuses: ProviderStatus[] = [];
  let configuredProviderID = "";
  const providerPreferences = new Map<string, { model: string; effort: string }>();

  const configureProvider = (): void => {
    if (configuredProviderID) {
      providerPreferences.set(configuredProviderID, {
        model: model.value,
        effort: effort.value
      });
    }
    configuredProviderID = provider.value;
    const status = statuses.find((candidate) => candidate.id === provider.value);
    const preferences = providerPreferences.get(provider.value);

    model.value = preferences?.model ?? "";
    model.placeholder = status?.model_placeholder ?? "Provider default";
    modelOptions.replaceChildren();
    for (const modelID of status?.models ?? []) {
      const option = document.createElement("option");
      option.value = modelID;
      modelOptions.append(option);
    }

    effort.replaceChildren();
    const defaultEffort = document.createElement("option");
    defaultEffort.value = "";
    defaultEffort.textContent = "Provider default";
    effort.append(defaultEffort);
    for (const effortID of status?.efforts ?? []) {
      const option = document.createElement("option");
      option.value = effortID;
      option.textContent = effortID.charAt(0).toUpperCase() + effortID.slice(1);
      effort.append(option);
    }
    effort.value = status?.efforts?.includes(preferences?.effort ?? "") ? preferences?.effort ?? "" : "";

    const ready = Boolean(status?.available && status.authenticated);
    model.disabled = !ready;
    effort.disabled = !ready || !status?.efforts?.length;
    detail.textContent = status?.detail ?? "Choose an authenticated local provider.";
    debug?.add("info", "ui.provider.configured", {
      provider: status?.id || null,
      ready,
      model: model.value || "provider-default",
      effort: effort.value || "provider-default"
    });
  };

  debug?.add("info", "providers.request.started", { endpoint: "/api/providers" });
  void fetch("/api/providers", { headers: { Accept: "application/json" } })
    .then(async (response) => {
      debug?.add(response.ok ? "info" : "warn", "providers.response.received", {
        status: response.status,
        content_type: response.headers.get("content-type")
      });
      if (!response.ok) {
        const body = await response.text();
        throw new Error(body || `Provider check failed (${response.status})`);
      }
      return response.json() as Promise<{ providers: ProviderStatus[] }>;
    })
    .then((result) => {
      statuses = result.providers;
      debug?.add("info", "providers.loaded", {
        providers: statuses.map((status) => ({
          id: status.id,
          available: status.available,
          authenticated: status.authenticated
        }))
      });
      provider.replaceChildren();
      for (const status of statuses) {
        const option = document.createElement("option");
        option.value = status.id;
        option.disabled = !status.available || !status.authenticated;
        const state = status.authenticated ? "ready" : status.available ? "login required" : "not installed";
        option.textContent = `${status.name} — ${state}`;
        provider.append(option);
      }
      const firstReady = statuses.find((status) => status.available && status.authenticated);
      provider.value = firstReady?.id ?? "";
      provider.disabled = !firstReady;
      submit.disabled = !firstReady;
      configureProvider();
    })
    .catch((error: unknown) => {
      debug?.add("error", "providers.request.failed", describeError(error));
      detail.textContent = error instanceof Error ? error.message : "Could not check providers.";
    });

  provider.addEventListener("change", configureProvider);
  model.addEventListener("change", () => {
    debug?.add("info", "ui.model.changed", {
      provider: provider.value,
      model: model.value.trim() || "provider-default"
    });
  });
  effort.addEventListener("change", () => {
    debug?.add("info", "ui.effort.changed", {
      provider: provider.value,
      effort: effort.value || "provider-default"
    });
  });
  document.querySelectorAll<HTMLButtonElement>("[data-chat-prompt]").forEach((button) => {
    button.addEventListener("click", () => {
      input.value = button.dataset.chatPrompt ?? "";
      input.focus();
      debug?.add("info", "ui.starter.selected", {
        message_length: input.value.length
      });
    });
  });
  input.addEventListener("keydown", (event) => {
    if (event.key !== "Enter" || event.isComposing) {
      return;
    }
    if (event.shiftKey) {
      debug?.add("info", "ui.message.newline");
      return;
    }
    event.preventDefault();
    debug?.add("info", "ui.message.submit-key");
    form.requestSubmit();
  });
  newConversation.addEventListener("click", () => {
    debug?.add("info", "ui.conversation.new", {
      previous_conversation: conversationID || null,
      provider: provider.value
    });
    conversationID = "";
    provider.disabled = false;
    const status = statuses.find((candidate) => candidate.id === provider.value);
    model.disabled = false;
    effort.disabled = !status?.efforts?.length;
    messages.replaceChildren();
    const replacement = document.createElement("div");
    replacement.className = "conversation-empty";
    replacement.dataset.conversationEmpty = "";
    replacement.textContent = "Start a fresh read-only conversation about the indexed code.";
    messages.append(replacement);
    input.focus();
  });

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const question = input.value.trim();
    if (!question || !provider.value || busy) {
      debug?.add("warn", "chat.submit.ignored", {
        has_message: Boolean(question),
        has_provider: Boolean(provider.value),
        busy
      });
      return;
    }
    const requestStarted = performance.now();
    let deltaEvents = 0;
    let answerCharacters = 0;
    let streamCompleted = false;
    busy = true;
    providerPreferences.set(provider.value, {
      model: model.value,
      effort: effort.value
    });
    submit.disabled = true;
    submit.textContent = "Thinking…";
    empty?.remove();
    messages.querySelector("[data-conversation-empty]")?.remove();
    messages.append(conversationMessage("user", question));
    const assistant = conversationMessage("assistant");
    const answer = assistant.querySelector<HTMLElement>(".conversation-content");
    let answerText = "";
    messages.append(assistant);
    input.value = "";
    messages.scrollTop = messages.scrollHeight;
    debug?.add("info", "chat.request.started", {
      endpoint: "/api/chat",
      provider: provider.value,
      model: model.value.trim() || "provider-default",
      effort: effort.value || "provider-default",
      conversation: conversationID ? "continuation" : "new",
      message_length: question.length
    });

    try {
      const response = await fetch("/api/chat", {
        method: "POST",
        headers: {
          Accept: "application/x-ndjson",
          "Content-Type": "application/json"
        },
        body: JSON.stringify({
          conversation_id: conversationID,
          provider: provider.value,
          model: model.value.trim(),
          effort: effort.value,
          message: question
        })
      });
      debug?.add(response.ok ? "info" : "warn", "chat.response.received", {
        status: response.status,
        status_text: response.statusText,
        content_type: response.headers.get("content-type"),
        duration_ms: Math.round(performance.now() - requestStarted)
      });
      if (!response.ok || !response.body) {
        const body = await response.text();
        throw new Error(body || `Conversation failed (${response.status})`);
      }
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      for (;;) {
        const { value, done } = await reader.read();
        buffer += decoder.decode(value, { stream: !done });
        const lines = buffer.split("\n");
        buffer = lines.pop() ?? "";
        for (const line of lines) {
          if (!line.trim()) {
            continue;
          }
          let message: ConversationEvent;
          try {
            message = JSON.parse(line) as ConversationEvent;
          } catch (error: unknown) {
            debug?.add("error", "chat.stream.decode-failed", {
              line_length: line.length,
              ...describeError(error)
            });
            throw error;
          }
          if (message.type === "meta" && message.conversation_id) {
            conversationID = message.conversation_id;
            provider.disabled = true;
            model.disabled = true;
            effort.disabled = true;
            debug?.add("info", "chat.stream.started", {
              conversation_id: conversationID
            });
          } else if (message.type === "delta" && message.text && answer) {
            deltaEvents++;
            answerCharacters += message.text.length;
            answerText += message.text;
            renderAssistantMarkdown(answer, answerText, debug);
          } else if (message.type === "sources" && message.sources?.length) {
            debug?.add("info", "chat.stream.sources", {
              count: message.sources.length
            });
            let sources = assistant.querySelector<HTMLElement>(".conversation-sources");
            if (!sources) {
              sources = document.createElement("div");
              sources.className = "conversation-sources";
              assistant.append(sources);
            }
            sources.replaceChildren();
            for (const source of message.sources) {
              const link = document.createElement("a");
              link.href = source.url;
              link.textContent = source.label;
              link.target = "_blank";
              link.rel = "noreferrer";
              sources.append(link);
            }
          } else if (message.type === "done") {
            streamCompleted = true;
            if (answer) {
              renderAssistantMarkdown(answer, answerText, debug, true);
            }
            debug?.add("info", "chat.stream.completed", {
              delta_events: deltaEvents,
              answer_characters: answerCharacters,
              duration_ms: Math.round(performance.now() - requestStarted)
            });
          } else if (message.type === "error") {
            debug?.add("error", "chat.stream.provider-error", {
              conversation_id: message.conversation_id || conversationID || null,
              message: message.text || "The provider could not complete this turn."
            });
            throw new Error(message.text || "The provider could not complete this turn.");
          }
        }
        messages.scrollTop = messages.scrollHeight;
        if (done) {
          if (!streamCompleted) {
            if (answer) {
              renderAssistantMarkdown(answer, answerText, debug, true);
            }
            debug?.add("warn", "chat.stream.closed-without-done", {
              delta_events: deltaEvents,
              answer_characters: answerCharacters
            });
          }
          break;
        }
      }
    } catch (error: unknown) {
      debug?.add("error", "chat.request.failed", {
        endpoint: "/api/chat",
        online: navigator.onLine,
        duration_ms: Math.round(performance.now() - requestStarted),
        ...describeError(error)
      });
      if (debug && (error instanceof TypeError || (error instanceof Error && /fetch|network/i.test(error.message)))) {
        await probeServerHealth(debug);
      }
      assistant.remove();
      messages.append(conversationMessage("error", error instanceof Error ? error.message : "Conversation failed."));
    } finally {
      busy = false;
      submit.disabled = false;
      submit.textContent = "Ask RepoKarta";
      input.focus();
      debug?.add("info", "chat.request.settled", {
        duration_ms: Math.round(performance.now() - requestStarted),
        conversation_id: conversationID || null
      });
    }
  });
}

connectIndexEvents();
enableQueryChips();
enableCopyButtons();
highlightSource();
focusSourceLine();
highlightSearchResults();
const debugLogger = enableDebugLogger();
enableConversations(debugLogger);

document.body.addEventListener("htmx:afterSwap", (event) => {
  const target = (event as CustomEvent<{ target?: ParentNode }>).detail?.target;
  highlightSearchResults(target ?? document);
});
