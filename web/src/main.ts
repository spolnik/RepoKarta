import htmx from "htmx.org";
import hljs from "highlight.js/lib/common";
import DOMPurify from "dompurify";
import { marked } from "marked";
import { createFrameBatcher } from "./frame-batcher.mjs";
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

function enableRepositoryDrawer(): void {
  const drawer = document.querySelector<HTMLElement>("[data-repository-drawer]");
  const toggle = document.querySelector<HTMLButtonElement>("[data-repository-drawer-toggle]");
  const scrim = document.querySelector<HTMLButtonElement>("[data-repository-drawer-scrim]");
  const panel = document.querySelector<HTMLElement>("#repository-drawer-panel");
  if (!drawer || !toggle || !scrim || !panel) {
    return;
  }

  const setExpanded = (expanded: boolean): void => {
    drawer.dataset.expanded = String(expanded);
    toggle.setAttribute("aria-expanded", String(expanded));
    toggle.setAttribute("aria-label", expanded ? "Close repositories" : "Open repositories");
    panel.setAttribute("aria-hidden", String(!expanded));
    panel.toggleAttribute("inert", !expanded);
    scrim.hidden = !expanded;
  };

  toggle.addEventListener("click", () => {
    setExpanded(drawer.dataset.expanded !== "true");
  });
  scrim.addEventListener("click", () => {
    setExpanded(false);
    toggle.focus();
  });
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape" || drawer.dataset.expanded !== "true") {
      return;
    }
    setExpanded(false);
    toggle.focus();
  });

  setExpanded(false);
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
  type: "meta" | "activity" | "delta" | "sources" | "images" | "context" | "usage" | "interrupted" | "done" | "error";
  conversation_id?: string;
  title?: string;
  activity?: "thinking";
  segment_id?: string;
  text?: string;
  sources?: Array<{ label: string; url: string }>;
  images?: ConversationImage[];
  context?: ContextUsage;
  usage?: TokenUsage;
};

type ContextUsage = {
  used_tokens: number;
  max_tokens: number;
  percentage: number;
  model?: string;
};

type ConversationImage = {
  name: string;
  media_type: string;
  data: string;
};

type TokenUsage = {
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  budget_tokens?: number;
};

type ConversationRecord = {
  id: string;
  title: string;
  provider: string;
  model?: string;
  effort?: string;
  created_at: string;
  updated_at: string;
  message_count: number;
  input_tokens: number;
  output_tokens: number;
  messages?: ConversationRecordMessage[];
};

type ConversationRecordMessage = {
  id: number;
  role: "user" | "assistant";
  text?: string;
  images?: ConversationImage[];
  sources?: Array<{ label: string; url: string }>;
  status?: string;
  error?: string;
  input_tokens?: number;
  output_tokens?: number;
  created_at: string;
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
  image_input: boolean;
  image_output: boolean;
  interrupt: boolean;
  context_usage: boolean;
  token_usage: boolean;
  token_budget: boolean;
};

const supportedImageTypes = new Set(["image/gif", "image/jpeg", "image/png", "image/webp"]);
const maximumImagesPerTurn = 4;
const maximumImageBytes = 8 * 1024 * 1024;

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
const mermaidDiagramSources = new WeakMap<HTMLElement, string>();

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

function conversationErrorMessage(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error || "Conversation failed.");
  if (/401 unauthorized|authentication_error|invalid x-api-key/i.test(message)) {
    return "Provider authentication failed. Check the configured API key or sign-in, then try again.";
  }
  if (/429|rate.?limit|too many requests/i.test(message)) {
    return "The provider rate limit was reached. Wait a moment, then try again.";
  }
  if (/timeout|deadline exceeded|context canceled/i.test(message)) {
    return "The provider did not finish before the timeout. Increase the timeout in Run settings or try a narrower question.";
  }
  return message;
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
    const expand = canvas.parentElement?.querySelector<HTMLButtonElement>("[data-mermaid-expand]");
    if (expand) {
      expand.disabled = false;
    }
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
): number {
  const started = performance.now();
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
      mermaidDiagramSources.set(figure, source);
      const toolbar = document.createElement("div");
      toolbar.className = "mermaid-diagram-toolbar";
      const expand = document.createElement("button");
      expand.type = "button";
      expand.className = "mermaid-diagram-expand";
      expand.disabled = true;
      expand.setAttribute("data-mermaid-expand", "");
      expand.setAttribute("aria-label", "Expand diagram");
      expand.innerHTML = `
        <svg viewBox="0 0 20 20" aria-hidden="true">
          <path d="M7.5 3.5h-4v4M12.5 3.5h4v4M16.5 12.5v4h-4M7.5 16.5h-4v-4"></path>
        </svg>
        <span>Expand</span>
      `;
      toolbar.append(expand);
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
      figure.append(toolbar, canvas, sourceDetails);
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
  const duration = performance.now() - started;
  debug?.add("info", "chat.markdown.rendered", {
    characters: markdown.length,
    code_blocks: target.querySelectorAll("pre code").length,
    diagrams: renderDiagrams,
    duration_ms: Math.round(duration)
  });
  return duration;
}

function enableMermaidViewer(debug?: DebugLogger): void {
  const dialog = document.querySelector<HTMLDialogElement>("#mermaid-viewer");
  const canvas = dialog?.querySelector<HTMLElement>("[data-mermaid-viewer-canvas]");
  const stage = dialog?.querySelector<HTMLElement>("[data-mermaid-viewer-stage]");
  const title = dialog?.querySelector<HTMLElement>("[data-mermaid-viewer-title]");
  const status = dialog?.querySelector<HTMLElement>("[data-mermaid-viewer-status]");
  const zoomSelect = dialog?.querySelector<HTMLSelectElement>("[data-mermaid-zoom]");
  const zoomOut = dialog?.querySelector<HTMLButtonElement>("[data-mermaid-zoom-out]");
  const zoomIn = dialog?.querySelector<HTMLButtonElement>("[data-mermaid-zoom-in]");
  const close = dialog?.querySelector<HTMLButtonElement>("[data-mermaid-viewer-close]");
  if (!dialog || !canvas || !stage || !title || !status || !zoomSelect || !zoomOut || !zoomIn || !close) {
    return;
  }

  const zoomLevels = [50, 75, 100, 125, 150, 200];
  let intrinsicWidth = 1200;
  let viewerRequest = 0;

  const updateZoomButtons = (): void => {
    const zoom = Number.parseInt(zoomSelect.value, 10);
    zoomOut.disabled = zoomSelect.value !== "fit" && zoom <= zoomLevels[0];
    zoomIn.disabled = zoomSelect.value !== "fit" && zoom >= zoomLevels[zoomLevels.length - 1];
  };

  const applyZoom = (value: string, announce = true): void => {
    const svg = canvas.querySelector<SVGSVGElement>("svg");
    if (!svg) {
      return;
    }
    if (value === "fit") {
      canvas.style.width = `min(100%, ${Math.ceil(intrinsicWidth)}px)`;
      status.textContent = "Fit to window";
    } else {
      const zoom = Number.parseInt(value, 10);
      if (!Number.isFinite(zoom)) {
        return;
      }
      canvas.style.width = `${Math.max(1, Math.round(intrinsicWidth * zoom / 100))}px`;
      status.textContent = `${zoom}% zoom`;
    }
    zoomSelect.value = value;
    stage.scrollTo({ left: 0, top: 0, behavior: "instant" });
    updateZoomButtons();
    if (announce) {
      debug?.add("info", "chat.diagram.viewer.zoomed", { zoom: value });
    }
  };

  const stepZoom = (direction: -1 | 1): void => {
    if (zoomSelect.value === "fit") {
      applyZoom(direction > 0 ? "100" : "75");
      return;
    }
    const current = Number.parseInt(zoomSelect.value, 10);
    const currentIndex = Math.max(0, zoomLevels.indexOf(current));
    const nextIndex = Math.min(zoomLevels.length - 1, Math.max(0, currentIndex + direction));
    applyZoom(String(zoomLevels[nextIndex]));
  };

  const openViewer = async (figure: HTMLElement): Promise<void> => {
    const source = mermaidDiagramSources.get(figure);
    if (!source) {
      return;
    }
    const request = ++viewerRequest;
    const embeddedCanvas = figure.querySelector<HTMLElement>(".mermaid-diagram-canvas");
    title.textContent = embeddedCanvas?.getAttribute("aria-label") ?? "Mermaid diagram";
    status.textContent = "Rendering full-resolution diagram…";
    canvas.className = "mermaid-viewer-canvas mermaid-viewer-canvas-loading";
    canvas.style.removeProperty("width");
    canvas.textContent = "Rendering diagram…";
    zoomSelect.value = "fit";
    zoomSelect.disabled = true;
    zoomOut.disabled = true;
    zoomIn.disabled = true;
    if (!dialog.open) {
      dialog.showModal();
    }

    const started = performance.now();
    let result: Awaited<ReturnType<MermaidAPI["render"]>> | undefined;
    let renderError: unknown;
    const render = async (): Promise<void> => {
      try {
        const mermaid = await loadMermaid();
        result = await mermaid.render(`repokarta-mermaid-viewer-${++mermaidDiagramID}`, source);
      } catch (error: unknown) {
        renderError = error;
      }
    };
    mermaidRenderQueue = mermaidRenderQueue.then(render, render);
    await mermaidRenderQueue;
    if (request !== viewerRequest || !dialog.open) {
      return;
    }
    if (!result || renderError) {
      canvas.className = "mermaid-viewer-canvas mermaid-viewer-canvas-error";
      canvas.textContent = "This diagram could not be opened. Try the embedded view or inspect its Mermaid source.";
      status.textContent = "Unable to render";
      debug?.add("warn", "chat.diagram.viewer.render-failed", {
        error_type: renderError instanceof Error ? renderError.name : "UnknownError"
      });
      return;
    }

    canvas.innerHTML = result.svg;
    canvas.className = "mermaid-viewer-canvas";
    const svg = canvas.querySelector<SVGSVGElement>("svg");
    const viewBox = svg?.viewBox.baseVal;
    const declaredWidth = Number.parseFloat(svg?.getAttribute("width") ?? "");
    intrinsicWidth = viewBox?.width || declaredWidth || Math.max(svg?.getBoundingClientRect().width ?? 0, 1200);
    zoomSelect.disabled = false;
    applyZoom("fit", false);
    debug?.add("info", "chat.diagram.viewer.opened", {
      type: result.diagramType,
      intrinsic_width: Math.round(intrinsicWidth),
      duration_ms: Math.round(performance.now() - started)
    });
  };

  document.addEventListener("click", (event) => {
    const button = (event.target as Element | null)?.closest<HTMLButtonElement>("[data-mermaid-expand]");
    if (!button || button.disabled) {
      return;
    }
    const figure = button.closest<HTMLElement>(".mermaid-diagram");
    if (figure) {
      void openViewer(figure);
    }
  });
  zoomSelect.addEventListener("change", () => applyZoom(zoomSelect.value));
  zoomOut.addEventListener("click", () => stepZoom(-1));
  zoomIn.addEventListener("click", () => stepZoom(1));
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog) {
      dialog.close();
    }
  });
  dialog.addEventListener("close", () => {
    viewerRequest += 1;
    canvas.replaceChildren();
    canvas.style.removeProperty("width");
  });
}

type StreamingRenderMetrics = {
  characters: number;
  dom_flushes: number;
  markdown_render_ms: number;
};

type StreamingMarkdownRenderer = {
  append: (text: string) => void;
  finish: (renderDiagrams?: boolean) => StreamingRenderMetrics;
  cancel: () => void;
};

type AssistantTimelineRenderer = {
  append: (text: string, segmentID?: string) => void;
  thinking: () => void;
  finish: () => StreamingRenderMetrics;
  cancel: () => void;
};

function createStreamingMarkdownRenderer(
  target: HTMLElement,
  debug?: DebugLogger,
  onFlush?: () => void
): StreamingMarkdownRenderer {
  const textNode = document.createTextNode("");
  target.replaceChildren(textNode);
  target.classList.add("conversation-content-streaming");
  let markdown = "";
  let finalMetrics: StreamingRenderMetrics | undefined;
  const batcher = createFrameBatcher({
    schedule: (callback) => window.requestAnimationFrame(callback),
    cancel: (handle) => window.cancelAnimationFrame(handle as number),
    write: (text) => {
      textNode.appendData(text);
      onFlush?.();
    }
  });

  return {
    append: (text: string): void => {
      if (finalMetrics || !text) {
        return;
      }
      markdown += text;
      batcher.append(text);
    },
    finish: (renderDiagrams = false): StreamingRenderMetrics => {
      if (finalMetrics) {
        return finalMetrics;
      }
      const domFlushes = batcher.finish();
      target.classList.remove("conversation-content-streaming");
      const markdownRenderMS = renderAssistantMarkdown(target, markdown, debug, renderDiagrams);
      onFlush?.();
      finalMetrics = {
        characters: markdown.length,
        dom_flushes: domFlushes,
        markdown_render_ms: Math.round(markdownRenderMS)
      };
      return finalMetrics;
    },
    cancel: (): void => {
      batcher.cancel();
      target.classList.remove("conversation-content-streaming");
    }
  };
}

function createAssistantTimelineRenderer(
  target: HTMLElement,
  composerActivity: HTMLElement,
  debug?: DebugLogger,
  onFlush?: () => void
): AssistantTimelineRenderer {
  const composerLabel = composerActivity.querySelector<HTMLElement>("[data-activity-label]");
  const composerElapsed = composerActivity.querySelector<HTMLTimeElement>("[data-activity-elapsed]");
  let currentRenderer: StreamingMarkdownRenderer | undefined;
  let currentSegmentID = "";
  let activityRow: HTMLElement | undefined;
  let activityStarted = 0;
  let phaseStarted = 0;
  let phase: "thinking" | "answering" | undefined;
  let timer = 0;
  let finished = false;
  const metrics: StreamingRenderMetrics = {
    characters: 0,
    dom_flushes: 0,
    markdown_render_ms: 0
  };

  target.replaceChildren();

  const setComposerPhase = (nextPhase?: "thinking" | "answering", elapsed = 0): void => {
    phase = nextPhase;
    if (!nextPhase) {
      composerActivity.hidden = true;
      composerActivity.removeAttribute("data-phase");
      return;
    }
    composerActivity.hidden = false;
    composerActivity.dataset.phase = nextPhase;
    if (composerLabel) {
      composerLabel.textContent = nextPhase === "thinking" ? "Thinking" : "Answering";
    }
    if (composerElapsed) {
      composerElapsed.textContent = formatElapsed(elapsed);
      composerElapsed.dateTime = `PT${Math.max(0, elapsed / 1000).toFixed(1)}S`;
    }
  };

  const updateTimer = (): void => {
    const now = performance.now();
    if (activityRow) {
      const elapsed = now - activityStarted;
      const rowElapsed = activityRow.querySelector<HTMLTimeElement>("[data-activity-elapsed]");
      if (rowElapsed) {
        rowElapsed.textContent = formatElapsed(elapsed);
        rowElapsed.dateTime = `PT${Math.max(0, elapsed / 1000).toFixed(1)}S`;
      }
      setComposerPhase("thinking", elapsed);
    } else if (phase === "answering") {
      setComposerPhase("answering", now - phaseStarted);
    }
  };

  const ensureTimer = (): void => {
    if (!timer) {
      timer = window.setInterval(updateTimer, 100);
    }
  };

  const mergeMetrics = (next?: StreamingRenderMetrics): void => {
    if (!next) {
      return;
    }
    metrics.characters += next.characters;
    metrics.dom_flushes += next.dom_flushes;
    metrics.markdown_render_ms += next.markdown_render_ms;
  };

  const finishCurrentSegment = (): void => {
    if (!currentRenderer) {
      return;
    }
    mergeMetrics(currentRenderer.finish(true));
    currentRenderer = undefined;
    currentSegmentID = "";
  };

  const completeThinking = (preserve: boolean): void => {
    if (!activityRow) {
      return;
    }
    const elapsed = performance.now() - activityStarted;
    if (preserve) {
      activityRow.dataset.state = "complete";
      const label = activityRow.querySelector<HTMLElement>("[data-activity-label]");
      const elapsedElement = activityRow.querySelector<HTMLTimeElement>("[data-activity-elapsed]");
      if (label) {
        label.textContent = "Thought for";
      }
      if (elapsedElement) {
        elapsedElement.textContent = formatElapsed(elapsed);
        elapsedElement.dateTime = `PT${Math.max(0, elapsed / 1000).toFixed(1)}S`;
      }
    } else {
      activityRow.remove();
    }
    activityRow = undefined;
  };

  const startThinking = (): void => {
    if (finished || activityRow) {
      return;
    }
    finishCurrentSegment();
    activityStarted = performance.now();
    phaseStarted = activityStarted;
    activityRow = document.createElement("div");
    activityRow.className = "conversation-thinking";
    activityRow.dataset.state = "active";
    activityRow.innerHTML = [
      '<span class="conversation-thinking-dots" aria-hidden="true"><i></i><i></i><i></i></span>',
      '<span data-activity-label>Thinking</span>',
      '<time data-activity-elapsed datetime="PT0S">0.0s</time>'
    ].join("");
    target.append(activityRow);
    setComposerPhase("thinking");
    ensureTimer();
    onFlush?.();
  };

  return {
    append: (text: string, segmentID = ""): void => {
      if (finished || !text) {
        return;
      }
      const normalizedSegmentID = segmentID || currentSegmentID || "answer";
      if (currentRenderer && normalizedSegmentID !== currentSegmentID) {
        finishCurrentSegment();
      }
      completeThinking(true);
      if (!currentRenderer) {
        const segment = document.createElement("section");
        segment.className = "conversation-response-segment";
        target.append(segment);
        currentSegmentID = normalizedSegmentID;
        currentRenderer = createStreamingMarkdownRenderer(segment, debug, onFlush);
        phaseStarted = performance.now();
      }
      setComposerPhase("answering", performance.now() - phaseStarted);
      ensureTimer();
      currentRenderer.append(text);
    },
    thinking: startThinking,
    finish: (): StreamingRenderMetrics => {
      if (finished) {
        return metrics;
      }
      finished = true;
      completeThinking(false);
      finishCurrentSegment();
      if (timer) {
        window.clearInterval(timer);
        timer = 0;
      }
      setComposerPhase();
      onFlush?.();
      return metrics;
    },
    cancel: (): void => {
      if (finished) {
        return;
      }
      finished = true;
      completeThinking(false);
      currentRenderer?.cancel();
      if (timer) {
        window.clearInterval(timer);
        timer = 0;
      }
      setComposerPhase();
    }
  };
}

function formatImageSize(bytes: number): string {
  if (bytes < 1024 * 1024) {
    return `${Math.max(1, Math.round(bytes / 1024))} KB`;
  }
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function formatElapsed(milliseconds: number): string {
  const seconds = Math.max(0, milliseconds) / 1000;
  if (seconds < 60) {
    return `${seconds.toFixed(1)}s`;
  }
  const wholeSeconds = Math.floor(seconds);
  const minutes = Math.floor(wholeSeconds / 60);
  return `${minutes}:${String(wholeSeconds % 60).padStart(2, "0")}`;
}

function formatTokenCount(tokens: number): string {
  if (tokens < 1000) {
    return Math.max(0, Math.round(tokens)).toLocaleString();
  }
  if (tokens < 1_000_000) {
    return `${(tokens / 1000).toFixed(tokens < 10_000 ? 1 : 0)}k`;
  }
  return `${(tokens / 1_000_000).toFixed(1)}m`;
}

function imageDataURL(image: ConversationImage): string | undefined {
  const mediaType = image.media_type.toLowerCase();
  const maximumEncodedLength = Math.ceil(maximumImageBytes / 3) * 4 + 4;
  if (
    !supportedImageTypes.has(mediaType) ||
    !image.data ||
    image.data.length > maximumEncodedLength ||
    !/^[A-Za-z0-9+/]*={0,2}$/.test(image.data)
  ) {
    return undefined;
  }
  return `data:${mediaType};base64,${image.data}`;
}

function readConversationImage(file: File): Promise<ConversationImage> {
  return new Promise((resolve, reject) => {
    const mediaType = file.type.toLowerCase();
    if (!supportedImageTypes.has(mediaType)) {
      reject(new Error(`${file.name} is not a supported PNG, JPEG, WebP, or GIF image.`));
      return;
    }
    if (file.size > maximumImageBytes) {
      reject(new Error(`${file.name} exceeds the ${maximumImageBytes / (1024 * 1024)} MB image limit.`));
      return;
    }
    const reader = new FileReader();
    reader.addEventListener("error", () => reject(new Error(`${file.name} could not be read.`)), { once: true });
    reader.addEventListener("load", () => {
      const result = typeof reader.result === "string" ? reader.result : "";
      const separator = result.indexOf(",");
      if (separator < 0) {
        reject(new Error(`${file.name} could not be encoded.`));
        return;
      }
      resolve({
        name: file.name,
        media_type: mediaType,
        data: result.slice(separator + 1)
      });
    }, { once: true });
    reader.readAsDataURL(file);
  });
}

function appendConversationImages(
  message: HTMLElement,
  images: ConversationImage[],
  kind: "input" | "output"
): number {
  let gallery = message.querySelector<HTMLElement>(`[data-conversation-images="${kind}"]`);
  if (!gallery) {
    gallery = document.createElement("div");
    gallery.className = `conversation-image-gallery conversation-image-gallery-${kind}`;
    gallery.dataset.conversationImages = kind;
    message.append(gallery);
  }
  let appended = 0;
  for (const image of images) {
    const source = imageDataURL(image);
    if (!source) {
      continue;
    }
    const card = document.createElement("figure");
    card.className = "conversation-image-card";
    const preview = document.createElement("a");
    preview.href = source;
    preview.target = "_blank";
    preview.rel = "noopener noreferrer";
    preview.title = `Open ${image.name || "image"}`;
    const element = document.createElement("img");
    element.src = source;
    element.alt = image.name || (kind === "input" ? "Attached image" : "Generated image");
    element.loading = "lazy";
    element.decoding = "async";
    preview.append(element);
    const caption = document.createElement("figcaption");
    const name = document.createElement("span");
    name.textContent = image.name || (kind === "input" ? "Attached image" : "Generated image");
    caption.append(name);
    if (kind === "output") {
      const download = document.createElement("a");
      download.href = source;
      download.download = image.name || "repokarta-image";
      download.textContent = "Download";
      caption.append(download);
    }
    card.append(preview, caption);
    gallery.append(card);
    appended++;
  }
  if (appended === 0 && gallery.childElementCount === 0) {
    gallery.remove();
  }
  return appended;
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
  const timeout = document.querySelector<HTMLSelectElement>("#conversation-timeout");
  const tokenBudget = document.querySelector<HTMLSelectElement>("#conversation-token-budget");
  const input = document.querySelector<HTMLTextAreaElement>("#conversation-message");
  const imageInput = document.querySelector<HTMLInputElement>("#conversation-image-input");
  const attachButton = document.querySelector<HTMLButtonElement>("[data-image-attach]");
  const attachmentTray = document.querySelector<HTMLElement>("#conversation-attachments");
  const composerActivity = document.querySelector<HTMLElement>("#conversation-activity");
  const imageSupportDetail = document.querySelector<HTMLElement>("#image-support-detail");
  const submit = document.querySelector<HTMLButtonElement>("#conversation-submit");
  const interrupt = document.querySelector<HTMLButtonElement>("#conversation-interrupt");
  const runtime = document.querySelector<HTMLElement>("#conversation-runtime");
  const contextValue = document.querySelector<HTMLElement>("#conversation-context-value");
  const contextMeter = document.querySelector<HTMLElement>("#conversation-context-meter");
  const usageValue = document.querySelector<HTMLElement>("#conversation-usage-value");
  const detail = document.querySelector<HTMLElement>("#provider-detail");
  const newConversationButtons = Array.from(document.querySelectorAll<HTMLButtonElement>("[data-new-conversation]"));
  const history = document.querySelector<HTMLOListElement>("#conversation-history");
  const historyEmpty = document.querySelector<HTMLElement>("[data-conversation-history-empty]");
  const historyFilter = document.querySelector<HTMLInputElement>("[data-conversation-filter]");
  const workspace = document.querySelector<HTMLElement>("[data-chat-workspace]");
  const sessionPanel = document.querySelector<HTMLElement>("[data-session-panel]");
  const sessionPanelOpen = document.querySelector<HTMLButtonElement>("[data-session-panel-open]");
  const sessionPanelClose = document.querySelector<HTMLButtonElement>("[data-session-panel-close]");
  const sessionPanelScrim = document.querySelector<HTMLButtonElement>("[data-session-panel-scrim]");
  const inspector = document.querySelector<HTMLElement>("[data-inspector]");
  const inspectorToggle = document.querySelector<HTMLButtonElement>("[data-inspector-toggle]");
  const inspectorClose = document.querySelector<HTMLButtonElement>("[data-inspector-close]");
  const inspectorScrim = document.querySelector<HTMLButtonElement>("[data-inspector-scrim]");
  const title = document.querySelector<HTMLElement>("#conversation-title");
  const titleEdit = document.querySelector<HTMLButtonElement>("[data-conversation-title-edit]");
  const headerStatus = document.querySelector<HTMLElement>("#conversation-header-status");
  const providerLabel = document.querySelector<HTMLElement>("#conversation-provider-label");
  const settings = document.querySelector<HTMLDetailsElement>(".conversation-settings");
  const evidenceList = document.querySelector<HTMLOListElement>("#conversation-evidence-list");
  const evidenceEmpty = document.querySelector<HTMLElement>("[data-evidence-empty]");
  const evidenceCounts = Array.from(document.querySelectorAll<HTMLElement>("[data-evidence-count]"));
  if (
    !form ||
    !messages ||
    !provider ||
    !model ||
    !modelOptions ||
    !effort ||
    !timeout ||
    !tokenBudget ||
    !input ||
    !imageInput ||
    !attachButton ||
    !attachmentTray ||
    !composerActivity ||
    !imageSupportDetail ||
    !submit ||
    !interrupt ||
    !runtime ||
    !contextValue ||
    !contextMeter ||
    !usageValue ||
    !detail ||
    newConversationButtons.length === 0 ||
    !history ||
    !historyEmpty ||
    !historyFilter ||
    !workspace ||
    !sessionPanel ||
    !sessionPanelOpen ||
    !sessionPanelClose ||
    !sessionPanelScrim ||
    !inspector ||
    !inspectorToggle ||
    !inspectorClose ||
    !inspectorScrim ||
    !title ||
    !titleEdit ||
    !headerStatus ||
    !providerLabel ||
    !settings ||
    !evidenceList ||
    !evidenceEmpty ||
    evidenceCounts.length === 0
  ) {
    return;
  }

  let conversationID = "";
  let busy = false;
  let attachedImages: ConversationImage[] = [];
  let attachmentFeedback = "";
  let statuses: ProviderStatus[] = [];
  let conversationSummaries: ConversationRecord[] = [];
  let configuredProviderID = "";
  let runtimeTimer = 0;
  let messageScrollFrame = 0;
  const providerPreferences = new Map<string, { model: string; effort: string }>();
  const evidenceSources = new Map<string, { label: string; url: string }>();
  const emptyStateTemplate = empty?.cloneNode(true) as HTMLElement | undefined;

  const scheduleMessageScroll = (): void => {
    if (messageScrollFrame) {
      return;
    }
    messageScrollFrame = window.requestAnimationFrame(() => {
      messageScrollFrame = 0;
      messages.scrollTop = messages.scrollHeight;
    });
  };

  const setSessionPanelOpen = (open: boolean): void => {
    sessionPanel.dataset.open = String(open);
    sessionPanelScrim.hidden = !open;
    sessionPanelOpen.setAttribute("aria-expanded", String(open));
  };

  const setInspectorOpen = (open: boolean): void => {
    inspector.dataset.open = String(open);
    inspector.setAttribute("aria-hidden", String(!open));
    inspector.inert = !open;
    inspectorToggle.setAttribute("aria-expanded", String(open));
    workspace.dataset.inspectorOpen = String(open);
    inspectorScrim.hidden = !open || window.matchMedia("(min-width: 1280px)").matches;
  };

  const renderEvidenceSources = (): void => {
    evidenceList.replaceChildren();
    evidenceEmpty.hidden = evidenceSources.size > 0;
    for (const source of evidenceSources.values()) {
      const item = document.createElement("li");
      const link = document.createElement("a");
      link.href = source.url;
      link.target = "_blank";
      link.rel = "noreferrer";
      const label = document.createElement("strong");
      label.textContent = source.label;
      const location = document.createElement("span");
      location.textContent = source.url;
      link.append(label, location);
      item.append(link);
      evidenceList.append(item);
    }
    for (const count of evidenceCounts) {
      count.textContent = String(evidenceSources.size);
    }
  };

  const addEvidenceSources = (sources: Array<{ label: string; url: string }> = []): void => {
    for (const source of sources) {
      if (source.url) {
        evidenceSources.set(source.url, source);
      }
    }
    renderEvidenceSources();
  };

  const clearEvidenceSources = (): void => {
    evidenceSources.clear();
    renderEvidenceSources();
  };

  const syncConversationChrome = (): void => {
    const summary = conversationSummaries.find((candidate) => candidate.id === conversationID);
    title.textContent = summary?.title || "New conversation";
    titleEdit.hidden = !summary;
    const status = statuses.find((candidate) => candidate.id === provider.value);
    const providerName = status?.name || provider.value || "Choose a provider";
    providerLabel.textContent = model.value.trim() ? `${providerName} · ${model.value.trim()}` : providerName;
    settings.dataset.ready = String(Boolean(status?.available && status.authenticated));
  };

  const stopRuntimeTimer = (): void => {
    if (runtimeTimer) {
      window.clearInterval(runtimeTimer);
      runtimeTimer = 0;
    }
  };

  const startRuntimeTimer = (started: number): void => {
    stopRuntimeTimer();
    const update = (): void => {
      const elapsed = formatElapsed(performance.now() - started);
      runtime.textContent = `Working · ${elapsed}`;
      headerStatus.textContent = `Reading indexed code · ${elapsed}`;
    };
    update();
    runtime.classList.add("conversation-telemetry-active");
    runtimeTimer = window.setInterval(update, 100);
  };

  const finishRuntimeTimer = (started: number): void => {
    stopRuntimeTimer();
    const elapsed = formatElapsed(performance.now() - started);
    runtime.textContent = `Last turn · ${elapsed}`;
    headerStatus.textContent = `Answer complete · ${elapsed}`;
    runtime.classList.remove("conversation-telemetry-active");
  };

  const renderContextUsage = (usage?: ContextUsage): void => {
    if (!usage || usage.max_tokens <= 0) {
      const status = statuses.find((candidate) => candidate.id === provider.value);
      contextValue.textContent = status?.context_usage ? "After first turn" : "Unavailable";
      contextValue.title = status?.context_usage
        ? "The provider will report context usage after it processes a turn."
        : "This provider harness does not expose context window usage.";
      contextMeter.style.setProperty("--context-usage", "0%");
      contextMeter.dataset.state = status?.context_usage ? "pending" : "unavailable";
      return;
    }
    const percentage = Math.min(100, Math.max(0, usage.percentage || usage.used_tokens * 100 / usage.max_tokens));
    contextValue.textContent = `${Math.round(percentage)}% · ${formatTokenCount(usage.used_tokens)} / ${formatTokenCount(usage.max_tokens)}`;
    contextValue.title = `${usage.used_tokens.toLocaleString()} of ${usage.max_tokens.toLocaleString()} context tokens${usage.model ? ` · ${usage.model}` : ""}`;
    contextMeter.style.setProperty("--context-usage", `${percentage}%`);
    contextMeter.dataset.state = percentage >= 90 ? "critical" : percentage >= 75 ? "warning" : "ready";
  };

  const renderTokenUsage = (usage?: TokenUsage): void => {
    const status = statuses.find((candidate) => candidate.id === provider.value);
    if (!usage) {
      usageValue.textContent = status?.token_usage ? "After first turn" : "Provider managed";
      usageValue.title = status?.token_usage
        ? "Input and output token usage appears after the provider completes a turn."
        : "This provider harness does not report token usage to RepoKarta.";
      return;
    }
    usageValue.textContent = `${formatTokenCount(usage.input_tokens)} in · ${formatTokenCount(usage.output_tokens)} out`;
    const budget = usage.budget_tokens ? ` · ${usage.output_tokens.toLocaleString()} / ${usage.budget_tokens.toLocaleString()} output budget` : "";
    usageValue.title = `${usage.total_tokens.toLocaleString()} total tokens${budget}`;
  };

  const setConversationURL = (id: string): void => {
    const url = new URL(window.location.href);
    if (id) {
      url.searchParams.set("conversation", id);
    } else {
      url.searchParams.delete("conversation");
    }
    window.history.replaceState(null, "", url);
  };

  const setNewConversationDisabled = (disabled: boolean): void => {
    for (const button of newConversationButtons) {
      button.disabled = disabled;
    }
  };

  const renderAttachmentTray = (): void => {
    attachmentTray.replaceChildren();
    attachmentTray.hidden = attachedImages.length === 0;
    for (const [index, image] of attachedImages.entries()) {
      const source = imageDataURL(image);
      if (!source) {
        continue;
      }
      const card = document.createElement("div");
      card.className = "conversation-attachment";
      const preview = document.createElement("img");
      preview.src = source;
      preview.alt = "";
      const text = document.createElement("div");
      const name = document.createElement("strong");
      name.textContent = image.name || `Image ${index + 1}`;
      const size = document.createElement("span");
      size.textContent = formatImageSize(Math.floor(image.data.length * 3 / 4));
      text.append(name, size);
      const remove = document.createElement("button");
      remove.type = "button";
      remove.setAttribute("aria-label", `Remove ${image.name || `image ${index + 1}`}`);
      remove.textContent = "×";
      remove.addEventListener("click", () => {
        attachedImages.splice(index, 1);
        attachmentFeedback = "";
        renderAttachmentTray();
        configureImageControls();
        debug?.add("info", "ui.image.removed", {
          remaining_images: attachedImages.length
        });
      });
      card.append(preview, text, remove);
      attachmentTray.append(card);
    }
  };

  const configureImageControls = (): void => {
    const status = statuses.find((candidate) => candidate.id === provider.value);
    const ready = Boolean(status?.available && status.authenticated);
    submit.disabled = busy || !ready;
    attachButton.disabled = busy || !ready || !status?.image_input;
    if (attachmentFeedback) {
      imageSupportDetail.textContent = attachmentFeedback;
      imageSupportDetail.classList.add("conversation-image-feedback");
    } else if (status?.image_input) {
      const output = status.image_output ? " Returned images render in the conversation." : "";
      imageSupportDetail.textContent = `PNG, JPEG, WebP, or GIF · ${maximumImagesPerTurn} images · ${maximumImageBytes / (1024 * 1024)} MB each.${output}`;
      imageSupportDetail.classList.remove("conversation-image-feedback");
    } else if (status) {
      imageSupportDetail.textContent = "This provider harness does not accept image attachments.";
      imageSupportDetail.classList.remove("conversation-image-feedback");
    } else {
      imageSupportDetail.textContent = "";
      imageSupportDetail.classList.remove("conversation-image-feedback");
    }
  };

  const addImageFiles = async (files: File[]): Promise<void> => {
    const status = statuses.find((candidate) => candidate.id === provider.value);
    if (!status?.image_input) {
      attachmentFeedback = "Choose a provider harness that supports image input.";
      configureImageControls();
      return;
    }
    attachmentFeedback = "";
    const remaining = maximumImagesPerTurn - attachedImages.length;
    if (remaining <= 0) {
      attachmentFeedback = `A turn can include at most ${maximumImagesPerTurn} images.`;
      configureImageControls();
      return;
    }
    for (const file of files.slice(0, remaining)) {
      try {
        attachedImages.push(await readConversationImage(file));
      } catch (error: unknown) {
        attachmentFeedback = error instanceof Error ? error.message : "An image could not be attached.";
      }
    }
    if (files.length > remaining) {
      attachmentFeedback = `Only the first ${remaining} image${remaining === 1 ? "" : "s"} fit the ${maximumImagesPerTurn}-image limit.`;
    }
    renderAttachmentTray();
    configureImageControls();
    debug?.add("info", "ui.images.attached", {
      image_count: attachedImages.length,
      media_types: attachedImages.map((image) => image.media_type),
      encoded_bytes: attachedImages.reduce((total, image) => total + image.data.length, 0)
    });
  };

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
    model.disabled = !ready || Boolean(conversationID);
    effort.disabled = !ready || Boolean(conversationID) || !status?.efforts?.length;
    timeout.disabled = busy || !ready;
    tokenBudget.disabled = busy || !ready || !status?.token_budget;
    detail.textContent = status?.detail ?? "Choose an authenticated local provider.";
    configureImageControls();
    if (!conversationID) {
      renderContextUsage();
      renderTokenUsage();
    }
    debug?.add("info", "ui.provider.configured", {
      provider: status?.id || null,
      ready,
      model: model.value || "provider-default",
      effort: effort.value || "provider-default",
      image_input: status?.image_input ?? false,
      image_output: status?.image_output ?? false,
      interrupt: status?.interrupt ?? false,
      context_usage: status?.context_usage ?? false,
      token_usage: status?.token_usage ?? false,
      token_budget: status?.token_budget ?? false
    });
    syncConversationChrome();
  };

  const appendSources = (
    message: HTMLElement,
    sources: Array<{ label: string; url: string }> = []
  ): void => {
    if (sources.length === 0) {
      return;
    }
    addEvidenceSources(sources);
    const container = document.createElement("div");
    container.className = "conversation-sources";
    for (const source of sources) {
      const link = document.createElement("a");
      link.href = source.url;
      link.textContent = source.label;
      link.target = "_blank";
      link.rel = "noreferrer";
      container.append(link);
    }
    message.append(container);
  };

  const renderStoredTranscript = (conversation: ConversationRecord): void => {
    messages.replaceChildren();
    clearEvidenceSources();
    for (const stored of conversation.messages ?? []) {
      const message = conversationMessage(stored.role);
      const content = message.querySelector<HTMLElement>(".conversation-content");
      if (content && stored.text) {
        if (stored.role === "assistant") {
          renderAssistantMarkdown(content, stored.text, debug, true);
        } else {
          content.textContent = stored.text;
        }
      }
      appendConversationImages(message, stored.images ?? [], stored.role === "user" ? "input" : "output");
      appendSources(message, stored.sources);
      if (stored.status === "interrupted") {
        const notice = document.createElement("p");
        notice.className = "conversation-turn-status conversation-turn-status-interrupted";
        notice.textContent = "Interrupted by you.";
        message.append(notice);
      } else if (stored.error) {
        const notice = document.createElement("p");
        notice.className = "conversation-turn-status conversation-turn-status-error";
        notice.textContent = conversationErrorMessage(stored.error);
        message.append(notice);
      }
      messages.append(message);
    }
    if (!messages.childElementCount) {
      const replacement = document.createElement("div");
      replacement.className = "conversation-empty";
      replacement.dataset.conversationEmpty = "";
      replacement.textContent = "This saved conversation has no messages yet.";
      messages.append(replacement);
    }
    scheduleMessageScroll();
  };

  const renderConversationHistory = (): void => {
    history.replaceChildren();
    for (const summary of conversationSummaries) {
      const item = document.createElement("li");
      item.className = "conversation-history-item";
      item.dataset.conversationId = summary.id;
      item.dataset.searchText = `${summary.title} ${summary.provider}`.toLocaleLowerCase();
      if (summary.id === conversationID) {
        item.dataset.active = "true";
      }
      const open = document.createElement("button");
      open.type = "button";
      open.className = "conversation-history-open";
      if (summary.id === conversationID) {
        open.setAttribute("aria-current", "page");
      }
      const title = document.createElement("strong");
      title.textContent = summary.title;
      const metadata = document.createElement("span");
      const updated = new Date(summary.updated_at);
      const updatedLabel = Number.isNaN(updated.valueOf())
        ? "Saved"
        : new Intl.DateTimeFormat(undefined, { dateStyle: "medium" }).format(updated);
      metadata.textContent = `${summary.provider} · ${summary.message_count} messages · ${updatedLabel}`;
      open.append(title, metadata);
      open.addEventListener("click", () => void openConversation(summary.id));

      const actions = document.createElement("div");
      actions.className = "conversation-history-actions";
      const rename = document.createElement("button");
      rename.type = "button";
      rename.textContent = "Rename";
      rename.setAttribute("aria-label", `Rename ${summary.title}`);
      rename.addEventListener("click", () => {
        item.dataset.editing = "true";
        const editor = document.createElement("form");
        editor.className = "conversation-history-editor";
        const titleInput = document.createElement("input");
        titleInput.value = summary.title;
        titleInput.maxLength = 120;
        titleInput.setAttribute("aria-label", "Conversation title");
        const save = document.createElement("button");
        save.type = "submit";
        save.textContent = "Save";
        const cancel = document.createElement("button");
        cancel.type = "button";
        cancel.textContent = "Cancel";
        cancel.addEventListener("click", () => renderConversationHistory());
        editor.addEventListener("submit", (event) => {
          event.preventDefault();
          void renameSavedConversation(summary, titleInput.value);
        });
        editor.append(titleInput, save, cancel);
        item.append(editor);
        titleInput.focus();
        titleInput.select();
      });
      const remove = document.createElement("button");
      remove.type = "button";
      remove.textContent = "Delete";
      remove.setAttribute("aria-label", `Delete ${summary.title}`);
      remove.addEventListener("click", () => {
        if (remove.dataset.confirmDelete === "true") {
          void deleteSavedConversation(summary);
          return;
        }
        remove.dataset.confirmDelete = "true";
        remove.textContent = "Confirm delete";
        remove.setAttribute("aria-label", `Confirm delete ${summary.title}`);
      });
      actions.append(rename, remove);
      item.append(open, actions);
      history.append(item);
    }
    const query = historyFilter.value.trim().toLocaleLowerCase();
    let visibleConversations = 0;
    for (const item of history.querySelectorAll<HTMLElement>(".conversation-history-item")) {
      const filtered = Boolean(query) && !(item.dataset.searchText ?? "").includes(query);
      item.dataset.filtered = String(filtered);
      if (!filtered) {
        visibleConversations++;
      }
    }
    historyEmpty.hidden = visibleConversations > 0;
    historyEmpty.textContent = conversationSummaries.length === 0
      ? "No saved chats yet."
      : visibleConversations === 0
        ? `No conversations match “${historyFilter.value.trim()}”.`
        : "";
    syncConversationChrome();
  };

  const refreshConversationHistory = async (): Promise<void> => {
    try {
      const response = await fetch("/api/conversations", { headers: { Accept: "application/json" } });
      if (!response.ok) {
        throw new Error(await response.text() || `Saved chats failed (${response.status})`);
      }
      const result = await response.json() as { conversations: ConversationRecord[] };
      conversationSummaries = result.conversations ?? [];
      renderConversationHistory();
    } catch (error: unknown) {
      historyEmpty.hidden = false;
      historyEmpty.textContent = error instanceof Error ? error.message : "Could not load saved chats.";
      debug?.add("error", "conversations.list.failed", describeError(error));
    }
  };

  const openConversation = async (id: string): Promise<void> => {
    if (!id || busy) {
      return;
    }
    try {
      const response = await fetch(`/api/conversations/${encodeURIComponent(id)}`, {
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        throw new Error(await response.text() || `Saved chat failed (${response.status})`);
      }
      const stored = await response.json() as ConversationRecord;
      conversationID = stored.id;
      if (!Array.from(provider.options).some((option) => option.value === stored.provider)) {
        const unavailableProvider = document.createElement("option");
        unavailableProvider.value = stored.provider;
        unavailableProvider.textContent = `${stored.provider} — unavailable`;
        unavailableProvider.disabled = true;
        provider.append(unavailableProvider);
      }
      provider.value = stored.provider;
      configureProvider();
      model.value = stored.model ?? "";
      effort.value = stored.effort ?? "";
      provider.disabled = true;
      model.disabled = true;
      effort.disabled = true;
      const status = statuses.find((candidate) => candidate.id === stored.provider);
      submit.disabled = !status?.available || !status.authenticated;
      renderStoredTranscript(stored);
      renderContextUsage();
      renderTokenUsage({
        input_tokens: stored.input_tokens,
        output_tokens: stored.output_tokens,
        total_tokens: stored.input_tokens + stored.output_tokens
      });
      runtime.textContent = "Restored";
      headerStatus.textContent = `${stored.message_count} messages · restored locally`;
      setConversationURL(stored.id);
      renderConversationHistory();
      setSessionPanelOpen(false);
      input.focus();
      debug?.add("info", "conversation.restored", {
        conversation_id: stored.id,
        message_count: stored.message_count,
        provider: stored.provider
      });
    } catch (error: unknown) {
      debug?.add("error", "conversation.restore.failed", describeError(error));
      historyEmpty.hidden = false;
      historyEmpty.textContent = error instanceof Error ? error.message : "Could not open saved chat.";
    }
  };

  const renameSavedConversation = async (
    conversation: ConversationRecord,
    requestedTitle: string
  ): Promise<void> => {
    if (busy) {
      return;
    }
    const title = requestedTitle.trim();
    if (!title || title === conversation.title) {
      renderConversationHistory();
      return;
    }
    try {
      const response = await fetch(`/api/conversations/${encodeURIComponent(conversation.id)}`, {
        method: "PATCH",
        headers: { Accept: "application/json", "Content-Type": "application/json" },
        body: JSON.stringify({ title })
      });
      if (!response.ok) {
        throw new Error(await response.text() || `Rename failed (${response.status})`);
      }
      await refreshConversationHistory();
    } catch (error: unknown) {
      debug?.add("error", "conversation.rename.failed", describeError(error));
      historyEmpty.hidden = false;
      historyEmpty.textContent = error instanceof Error ? error.message : "Could not rename chat.";
    }
  };

  const deleteSavedConversation = async (conversation: ConversationRecord): Promise<void> => {
    if (busy) {
      return;
    }
    try {
      const response = await fetch(`/api/conversations/${encodeURIComponent(conversation.id)}`, {
        method: "DELETE",
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        throw new Error(await response.text() || `Delete failed (${response.status})`);
      }
      if (conversation.id === conversationID) {
        resetConversation();
      }
      await refreshConversationHistory();
    } catch (error: unknown) {
      debug?.add("error", "conversation.delete.failed", describeError(error));
      historyEmpty.hidden = false;
      historyEmpty.textContent = error instanceof Error ? error.message : "Could not delete chat.";
    }
  };

  const resetConversation = (): void => {
    conversationID = "";
    setConversationURL("");
    provider.disabled = statuses.every((status) => !status.available || !status.authenticated);
    const status = statuses.find((candidate) => candidate.id === provider.value);
    model.disabled = !status?.available || !status.authenticated;
    effort.disabled = !status?.available || !status.authenticated || !status.efforts?.length;
    attachedImages = [];
    attachmentFeedback = "";
    runtime.textContent = "Ready";
    headerStatus.textContent = "Ready for a grounded question";
    runtime.classList.remove("conversation-telemetry-active");
    clearEvidenceSources();
    renderContextUsage();
    renderTokenUsage();
    renderAttachmentTray();
    configureImageControls();
    messages.replaceChildren();
    if (emptyStateTemplate) {
      messages.append(emptyStateTemplate.cloneNode(true));
    }
    renderConversationHistory();
    setSessionPanelOpen(false);
    input.focus();
  };

  renderEvidenceSources();
  setSessionPanelOpen(false);
  setInspectorOpen(false);

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
      providerLabel.textContent = "Provider unavailable";
      headerStatus.textContent = "A provider could not be loaded";
    })
    .finally(() => {
      void refreshConversationHistory().then(() => {
        const requestedConversation = new URL(window.location.href).searchParams.get("conversation");
        if (requestedConversation) {
          void openConversation(requestedConversation);
        }
      });
    });

  provider.addEventListener("change", configureProvider);
  model.addEventListener("input", syncConversationChrome);
  attachButton.addEventListener("click", () => imageInput.click());
  imageInput.addEventListener("change", () => {
    void addImageFiles(Array.from(imageInput.files ?? []));
    imageInput.value = "";
  });
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
  historyFilter.addEventListener("input", renderConversationHistory);
  sessionPanelOpen.addEventListener("click", () => setSessionPanelOpen(true));
  sessionPanelClose.addEventListener("click", () => setSessionPanelOpen(false));
  sessionPanelScrim.addEventListener("click", () => setSessionPanelOpen(false));
  inspectorToggle.addEventListener("click", () => setInspectorOpen(inspector.dataset.open !== "true"));
  inspectorClose.addEventListener("click", () => setInspectorOpen(false));
  inspectorScrim.addEventListener("click", () => setInspectorOpen(false));
  titleEdit.addEventListener("click", () => {
    if (historyFilter.value) {
      historyFilter.value = "";
      renderConversationHistory();
    }
    const active = history.querySelector<HTMLElement>(".conversation-history-item[data-active='true']");
    const rename = active?.querySelector<HTMLButtonElement>(".conversation-history-actions button:first-child");
    if (!rename) {
      return;
    }
    if (window.matchMedia("(max-width: 1023px)").matches) {
      setSessionPanelOpen(true);
    }
    rename.click();
  });
  document.addEventListener("pointerdown", (event) => {
    if (settings.open && !settings.contains(event.target as Node)) {
      settings.open = false;
    }
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      setSessionPanelOpen(false);
      setInspectorOpen(false);
      settings.open = false;
      return;
    }
    if (event.ctrlKey && event.key.toLocaleLowerCase() === "n" && !busy) {
      event.preventDefault();
      resetConversation();
    }
  });
  window.addEventListener("resize", () => {
    inspectorScrim.hidden = inspector.dataset.open !== "true" || window.matchMedia("(min-width: 1280px)").matches;
  });
  messages.addEventListener("click", (event) => {
    const button = (event.target as Element | null)?.closest<HTMLButtonElement>("[data-chat-prompt]");
    if (button) {
      input.value = button.dataset.chatPrompt ?? "";
      input.focus();
      debug?.add("info", "ui.starter.selected", {
        message_length: input.value.length
      });
    }
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
  input.addEventListener("paste", (event) => {
    const images = Array.from(event.clipboardData?.files ?? []).filter((file) => file.type.startsWith("image/"));
    if (images.length > 0) {
      void addImageFiles(images);
    }
  });
  form.addEventListener("dragover", (event) => {
    if (Array.from(event.dataTransfer?.items ?? []).some((item) => item.kind === "file")) {
      event.preventDefault();
      form.classList.add("conversation-form-drop-target");
    }
  });
  form.addEventListener("dragleave", () => form.classList.remove("conversation-form-drop-target"));
  form.addEventListener("drop", (event) => {
    form.classList.remove("conversation-form-drop-target");
    const images = Array.from(event.dataTransfer?.files ?? []).filter((file) => file.type.startsWith("image/"));
    if (images.length > 0) {
      event.preventDefault();
      void addImageFiles(images);
    }
  });
  interrupt.addEventListener("click", async () => {
    if (!busy || !conversationID || interrupt.disabled) {
      return;
    }
    interrupt.disabled = true;
    interrupt.textContent = "Interrupting…";
    const endpoint = `/api/chat/${encodeURIComponent(conversationID)}/interrupt`;
    debug?.add("info", "chat.interrupt.started", {
      endpoint,
      conversation_id: conversationID
    });
    try {
      const response = await fetch(endpoint, {
        method: "POST",
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        const body = await response.text();
        throw new Error(body || `Interrupt failed (${response.status})`);
      }
      debug?.add("info", "chat.interrupt.accepted", {
        status: response.status,
        conversation_id: conversationID
      });
    } catch (error: unknown) {
      interrupt.disabled = false;
      interrupt.textContent = "Interrupt";
      debug?.add("error", "chat.interrupt.failed", describeError(error));
    }
  });
  for (const button of newConversationButtons) {
    button.addEventListener("click", () => {
      if (busy) {
        return;
      }
      debug?.add("info", "ui.conversation.new", {
        previous_conversation: conversationID || null,
        provider: provider.value
      });
      resetConversation();
    });
  }

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const question = input.value.trim();
    if ((!question && attachedImages.length === 0) || !provider.value || busy) {
      debug?.add("warn", "chat.submit.ignored", {
        has_message: Boolean(question),
        image_count: attachedImages.length,
        has_provider: Boolean(provider.value),
        busy
      });
      return;
    }
    const requestImages = attachedImages.slice();
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
    submit.setAttribute("aria-label", "RepoKarta is working");
    setNewConversationDisabled(true);
    timeout.disabled = true;
    tokenBudget.disabled = true;
    const providerStatus = statuses.find((candidate) => candidate.id === provider.value);
    interrupt.hidden = !providerStatus?.interrupt;
    interrupt.disabled = true;
    interrupt.textContent = "Interrupt";
    startRuntimeTimer(requestStarted);
    configureImageControls();
    empty?.remove();
    messages.querySelector("[data-conversation-empty]")?.remove();
    const userMessage = conversationMessage("user", question);
    appendConversationImages(userMessage, requestImages, "input");
    messages.append(userMessage);
    const assistant = conversationMessage("assistant");
    const answer = assistant.querySelector<HTMLElement>(".conversation-content");
    const timelineRenderer = answer
      ? createAssistantTimelineRenderer(answer, composerActivity, debug, scheduleMessageScroll)
      : undefined;
    let renderMetrics: StreamingRenderMetrics | undefined;
    messages.append(assistant);
    timelineRenderer?.thinking();
    input.value = "";
    attachedImages = [];
    attachmentFeedback = "";
    renderAttachmentTray();
    scheduleMessageScroll();
    debug?.add("info", "chat.request.started", {
      endpoint: "/api/chat",
      provider: provider.value,
      model: model.value.trim() || "provider-default",
      effort: effort.value || "provider-default",
      conversation: conversationID ? "continuation" : "new",
      message_length: question.length,
      image_count: requestImages.length,
      image_bytes: requestImages.reduce((total, image) => total + Math.floor(image.data.length * 3 / 4), 0)
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
          message: question,
          images: requestImages,
          timeout_seconds: Number.parseInt(timeout.value, 10),
          token_budget: Number.parseInt(tokenBudget.value, 10)
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
            setConversationURL(conversationID);
            provider.disabled = true;
            model.disabled = true;
            effort.disabled = true;
            interrupt.disabled = !providerStatus?.interrupt;
            if (message.title) {
              title.textContent = message.title;
              titleEdit.hidden = false;
              conversationSummaries = conversationSummaries.map((summary) =>
                summary.id === conversationID ? { ...summary, title: message.title ?? summary.title } : summary
              );
            }
            debug?.add("info", "chat.stream.started", {
              conversation_id: conversationID,
              title: message.title || null
            });
          } else if (message.type === "activity" && message.activity === "thinking") {
            timelineRenderer?.thinking();
          } else if (message.type === "delta" && message.text && answer) {
            deltaEvents++;
            answerCharacters += message.text.length;
            timelineRenderer?.append(message.text, message.segment_id);
          } else if (message.type === "sources" && message.sources?.length) {
            debug?.add("info", "chat.stream.sources", {
              count: message.sources.length
            });
            addEvidenceSources(message.sources);
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
          } else if (message.type === "images" && message.images?.length) {
            const renderedImages = appendConversationImages(assistant, message.images, "output");
            debug?.add(renderedImages === message.images.length ? "info" : "warn", "chat.stream.images", {
              received: message.images.length,
              rendered: renderedImages,
              media_types: message.images.map((image) => image.media_type)
            });
          } else if (message.type === "context" && message.context) {
            renderContextUsage(message.context);
            debug?.add("info", "chat.stream.context", {
              used_tokens: message.context.used_tokens,
              max_tokens: message.context.max_tokens,
              percentage: message.context.percentage,
              model: message.context.model || null
            });
          } else if (message.type === "usage" && message.usage) {
            renderTokenUsage(message.usage);
            debug?.add("info", "chat.stream.usage", message.usage);
          } else if (message.type === "interrupted") {
            streamCompleted = true;
            renderMetrics = timelineRenderer?.finish();
            const notice = document.createElement("p");
            notice.className = "conversation-turn-status conversation-turn-status-interrupted";
            notice.textContent = "Interrupted by you.";
            assistant.append(notice);
            debug?.add("info", "chat.stream.interrupted", {
              delta_events: deltaEvents,
              answer_characters: answerCharacters,
              rendering: renderMetrics,
              duration_ms: Math.round(performance.now() - requestStarted)
            });
          } else if (message.type === "done") {
            streamCompleted = true;
            renderMetrics = timelineRenderer?.finish();
            void refreshConversationHistory();
            debug?.add("info", "chat.stream.completed", {
              delta_events: deltaEvents,
              answer_characters: answerCharacters,
              rendering: renderMetrics,
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
        scheduleMessageScroll();
        if (done) {
          if (!streamCompleted) {
            renderMetrics = timelineRenderer?.finish();
            debug?.add("warn", "chat.stream.closed-without-done", {
              delta_events: deltaEvents,
              answer_characters: answerCharacters,
              rendering: renderMetrics
            });
          }
          break;
        }
      }
    } catch (error: unknown) {
      timelineRenderer?.cancel();
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
      messages.append(conversationMessage("error", conversationErrorMessage(error)));
    } finally {
      busy = false;
      finishRuntimeTimer(requestStarted);
      submit.disabled = false;
      submit.setAttribute("aria-label", "Ask RepoKarta");
      interrupt.hidden = true;
      interrupt.disabled = true;
      interrupt.textContent = "Interrupt";
      setNewConversationDisabled(false);
      configureImageControls();
      configureProvider();
      if (conversationID) {
        void refreshConversationHistory();
      }
      input.focus();
      debug?.add("info", "chat.request.settled", {
        duration_ms: Math.round(performance.now() - requestStarted),
        conversation_id: conversationID || null
      });
    }
  });
}

connectIndexEvents();
enableRepositoryDrawer();
enableQueryChips();
enableCopyButtons();
highlightSource();
focusSourceLine();
highlightSearchResults();
const debugLogger = enableDebugLogger();
enableMermaidViewer(debugLogger);
enableConversations(debugLogger);

document.body.addEventListener("htmx:afterSwap", (event) => {
  const target = (event as CustomEvent<{ target?: ParentNode }>).detail?.target;
  highlightSearchResults(target ?? document);
});
