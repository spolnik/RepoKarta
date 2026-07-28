import htmx from "htmx.org";
import hljs from "highlight.js/lib/core";
import bash from "highlight.js/lib/languages/bash";
import c from "highlight.js/lib/languages/c";
import cpp from "highlight.js/lib/languages/cpp";
import csharp from "highlight.js/lib/languages/csharp";
import css from "highlight.js/lib/languages/css";
import diff from "highlight.js/lib/languages/diff";
import go from "highlight.js/lib/languages/go";
import groovy from "highlight.js/lib/languages/groovy";
import ini from "highlight.js/lib/languages/ini";
import java from "highlight.js/lib/languages/java";
import javascript from "highlight.js/lib/languages/javascript";
import json from "highlight.js/lib/languages/json";
import kotlin from "highlight.js/lib/languages/kotlin";
import markdown from "highlight.js/lib/languages/markdown";
import php from "highlight.js/lib/languages/php";
import plaintext from "highlight.js/lib/languages/plaintext";
import python from "highlight.js/lib/languages/python";
import ruby from "highlight.js/lib/languages/ruby";
import rust from "highlight.js/lib/languages/rust";
import sql from "highlight.js/lib/languages/sql";
import typescript from "highlight.js/lib/languages/typescript";
import xml from "highlight.js/lib/languages/xml";
import yaml from "highlight.js/lib/languages/yaml";
import DOMPurify from "dompurify";
import { marked } from "marked";
import { createFrameBatcher } from "./frame-batcher.mjs";
import {
  recommendedProviderEffort,
  recommendedProviderModel
} from "./provider-defaults.mjs";
import { activeContextMention } from "./context-mention.mjs";
import { parseRepoKartaContextURL } from "./context-url.mjs";
import { buildContextualChatURL, normaliseContextMode } from "./contextual-chat.mjs";
import { applyQueryCompletion, type QueryCompletionEdit } from "./query-completion.mjs";
import {
  sourceHighlightLanguage,
  sourceSearchSummary,
  sourceSearchURL,
  sourceSelection
} from "./source-intelligence.mjs";
import { filterTopologyConnections } from "./topology-integrity.mjs";
import { wikiPrimaryAction } from "./wiki-run-state.mjs";
import "./styles.css";

const highlightedLanguages: Array<[string, typeof bash, string[]]> = [
  ["bash", bash, ["sh", "shell", "zsh"]],
  ["c", c, []],
  ["cpp", cpp, ["cc", "cxx", "hpp"]],
  ["csharp", csharp, ["cs"]],
  ["css", css, []],
  ["diff", diff, ["patch"]],
  ["go", go, ["golang"]],
  ["groovy", groovy, ["gradle"]],
  ["ini", ini, ["toml", "properties"]],
  ["java", java, []],
  ["javascript", javascript, ["js", "jsx", "mjs", "cjs"]],
  ["json", json, []],
  ["kotlin", kotlin, ["kt", "kts"]],
  ["markdown", markdown, ["md"]],
  ["php", php, []],
  ["plaintext", plaintext, ["text", "txt"]],
  ["python", python, ["py"]],
  ["ruby", ruby, ["rb"]],
  ["rust", rust, ["rs"]],
  ["sql", sql, []],
  ["typescript", typescript, ["ts", "tsx"]],
  ["xml", xml, ["html", "htm", "svg"]],
  ["yaml", yaml, ["yml"]]
];
highlightedLanguages.forEach(([name, language, aliases]) => {
  hljs.registerLanguage(name, language);
  hljs.registerAliases(aliases, { languageName: name });
});

htmx.config.allowEval = false;

function enableContextualChatLauncher(): void {
  const dialog = document.querySelector<HTMLDialogElement>("#contextual-chat-dialog");
  const form = dialog?.querySelector<HTMLFormElement>("[data-contextual-chat-form]");
  const input = dialog?.querySelector<HTMLTextAreaElement>("[data-contextual-chat-question]");
  const close = dialog?.querySelector<HTMLButtonElement>("[data-contextual-chat-close]");
  const modeLabel = dialog?.querySelector<HTMLElement>("[data-contextual-chat-mode]");
  const scopeLabel = dialog?.querySelector<HTMLElement>("[data-contextual-chat-scope]");
  const triggers = document.querySelectorAll<HTMLButtonElement>("[data-contextual-chat-open]");
  if (!dialog || !form || !input || !close || !modeLabel || !scopeLabel || triggers.length === 0) {
    return;
  }

  let mode = "context";
  let contextURL = "";
  const open = (trigger: HTMLButtonElement): void => {
    mode = normaliseContextMode(trigger.dataset.contextualChatMode);
    const parsedContext = parseRepoKartaContextURL(window.location.href, window.location.href);
    contextURL = parsedContext ? window.location.href : "";
    modeLabel.textContent = `${mode.charAt(0).toUpperCase()}${mode.slice(1)} context`;
    scopeLabel.textContent = contextURL
      ? "The current repository, revision, file, or line will be attached."
      : "No single repository is selected, so this chat will search every repository you can access.";
    dialog.showModal();
    window.requestAnimationFrame(() => input.focus());
  };

  for (const trigger of triggers) {
    trigger.addEventListener("click", () => open(trigger));
  }
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog) {
      dialog.close();
    }
  });
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    const prompt = input.value.trim();
    if (!prompt) {
      input.focus();
      return;
    }
    window.location.assign(buildContextualChatURL(window.location.href, {
      mode,
      prompt,
      ...(contextURL ? { contextURL } : {})
    }));
  });
}

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
  // On the search page the health chip opens the catalogue rather than
  // navigating; on every other surface it stays an ordinary link home.
  document.addEventListener("click", (event) => {
    const chip = (event.target as HTMLElement | null)?.closest("[data-index-health]");
    if (!chip) {
      return;
    }
    event.preventDefault();
    setExpanded(true);
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

interface EvidenceDrawer {
  open: (open: boolean) => void;
  isOverlay: () => boolean;
}

/**
 * Shared behaviour for evidence rails that dock at wide widths and become
 * overlays below a breakpoint. Docked panels stay in the tab order; overlays
 * are inert until opened, dismiss on Escape or scrim, and return focus to the
 * trigger. Used by the Wiki provenance rail and the map inspector so no
 * evidence surface is ever removed from the page.
 */
function enableEvidenceDrawer(options: {
  panel: string;
  toggle: string;
  close: string;
  scrim: string;
  dockedFrom: string;
}): EvidenceDrawer | undefined {
  const panel = document.querySelector<HTMLElement>(options.panel);
  const toggle = document.querySelector<HTMLButtonElement>(options.toggle);
  const close = document.querySelector<HTMLButtonElement>(options.close);
  const scrim = document.querySelector<HTMLButtonElement>(options.scrim);
  if (!panel || !toggle || !close || !scrim) {
    return undefined;
  }

  const docked = window.matchMedia(options.dockedFrom);
  const isOverlay = (): boolean => !docked.matches;

  const apply = (open: boolean): void => {
    const overlay = isOverlay();
    panel.dataset.open = String(open);
    // A docked rail is always available; only an overlay is ever inert.
    panel.inert = overlay && !open;
    panel.setAttribute("aria-hidden", String(overlay && !open));
    toggle.setAttribute("aria-expanded", String(overlay && open));
    scrim.hidden = !overlay || !open;
  };

  toggle.addEventListener("click", () => apply(panel.dataset.open !== "true"));
  close.addEventListener("click", () => {
    apply(false);
    toggle.focus();
  });
  scrim.addEventListener("click", () => {
    apply(false);
    toggle.focus();
  });
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape" || !isOverlay() || panel.dataset.open !== "true") {
      return;
    }
    apply(false);
    toggle.focus();
  });
  // Re-evaluate inert and scrim when the layout crosses the breakpoint.
  docked.addEventListener("change", () => apply(panel.dataset.open === "true"));

  apply(false);
  return { open: apply, isOverlay };
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

type QueryCompletion = QueryCompletionEdit & {
  label: string;
  detail: string;
};

type QueryCompletionResponse = {
  completions: QueryCompletion[];
};

function enableSearchQueryCompletion(): void {
  const input = document.querySelector<HTMLInputElement>("#search-query");
  const list = document.querySelector<HTMLElement>("#search-query-completions");
  if (!input || !list) {
    return;
  }

  let completions: QueryCompletion[] = [];
  let activeIndex = -1;
  let requestSequence = 0;
  let controller: AbortController | undefined;

  const close = (): void => {
    completions = [];
    activeIndex = -1;
    list.replaceChildren();
    list.hidden = true;
    input.setAttribute("aria-expanded", "false");
    input.removeAttribute("aria-activedescendant");
  };

  const setActive = (index: number): void => {
    if (completions.length === 0) {
      activeIndex = -1;
      input.removeAttribute("aria-activedescendant");
      return;
    }
    activeIndex = (index + completions.length) % completions.length;
    list.querySelectorAll<HTMLElement>("[role=option]").forEach((option, optionIndex) => {
      const active = optionIndex === activeIndex;
      option.dataset.active = String(active);
      option.setAttribute("aria-selected", String(active));
      if (active) {
        input.setAttribute("aria-activedescendant", option.id);
        option.scrollIntoView({ block: "nearest" });
      }
    });
  };

  const apply = (completion: QueryCompletion): void => {
    const edit = applyQueryCompletion(input.value, completion);
    input.value = edit.value;
    input.focus();
    input.setSelectionRange(edit.cursor, edit.cursor);
    close();
  };

  const render = (next: QueryCompletion[]): void => {
    completions = next;
    activeIndex = -1;
    list.replaceChildren();
    if (completions.length === 0 || document.activeElement !== input) {
      close();
      return;
    }
    completions.forEach((completion, index) => {
      const option = document.createElement("button");
      option.type = "button";
      option.id = `search-query-completion-${index}`;
      option.className = "search-query-completion";
      option.setAttribute("role", "option");
      option.setAttribute("aria-selected", "false");
      option.dataset.active = "false";

      const label = document.createElement("strong");
      label.textContent = completion.label;
      option.append(label);
      if (completion.detail) {
        const detail = document.createElement("span");
        detail.textContent = completion.detail;
        option.append(detail);
      }
      option.addEventListener("mouseenter", () => setActive(index));
      option.addEventListener("mousedown", (event) => event.preventDefault());
      option.addEventListener("click", () => apply(completion));
      list.append(option);
    });
    list.hidden = false;
    input.setAttribute("aria-expanded", "true");
  };

  const refresh = async (): Promise<void> => {
    const sequence = ++requestSequence;
    controller?.abort();
    controller = new AbortController();
    const cursor = input.selectionStart ?? input.value.length;
    const parameters = new URLSearchParams({ q: input.value, cursor: String(cursor) });
    try {
      const response = await fetch(`/api/search/query-completions?${parameters}`, {
        headers: { Accept: "application/json" },
        signal: controller.signal
      });
      if (!response.ok) {
        close();
        return;
      }
      const payload = await response.json() as QueryCompletionResponse;
      if (sequence === requestSequence) {
        render(payload.completions ?? []);
      }
    } catch (error) {
      if (!(error instanceof DOMException && error.name === "AbortError")) {
        close();
      }
    }
  };

  input.addEventListener("input", () => void refresh());
  input.addEventListener("click", () => void refresh());
  input.addEventListener("focus", () => void refresh());
  input.addEventListener("keydown", (event) => {
    if (event.key === "ArrowDown" && completions.length > 0) {
      event.preventDefault();
      setActive(activeIndex + 1);
    } else if (event.key === "ArrowUp" && completions.length > 0) {
      event.preventDefault();
      setActive(activeIndex - 1);
    } else if ((event.key === "Tab" || event.key === "Enter") && activeIndex >= 0) {
      event.preventDefault();
      apply(completions[activeIndex]);
    } else if (event.key === "Escape") {
      event.preventDefault();
      close();
    }
  });
  input.addEventListener("blur", () => window.setTimeout(close, 0));
}

/**
 * Search feedback. Previously the only in-flight signal was the submit button
 * relabelling itself, so a slow cross-repository search left stale results
 * looking current, and aria-busy was hardcoded false in the template.
 */
function enableSearchFeedback(): void {
  const results = document.querySelector<HTMLElement>("[data-search-results]");
  const form = document.querySelector<HTMLFormElement>('form[action="/search"]');
  if (!results || !form) {
    return;
  }

  const setBusy = (busy: boolean): void => {
    results.setAttribute("aria-busy", String(busy));
  };
  document.body.addEventListener("htmx:beforeRequest", (event) => {
    if ((event as CustomEvent<{ elt: HTMLElement }>).detail?.elt === form) {
      setBusy(true);
    }
  });
  /*
   * htmx:afterRequest is the only event guaranteed to fire once a request
   * settles, whatever the outcome. Listening for swap/load events alone left
   * the busy flag stuck whenever a response produced no swap, and a stuck flag
   * is indistinguishable from a broken search.
   */
  for (const name of ["htmx:afterRequest", "htmx:responseError", "htmx:sendError", "htmx:abort"]) {
    document.body.addEventListener(name, () => setBusy(false));
  }
  // Belt and braces: never leave the results inert if an event is missed.
  window.setInterval(() => {
    if (results.getAttribute("aria-busy") === "true" && !document.querySelector(".htmx-request")) {
      setBusy(false);
    }
  }, 1000);

  // "Show more" re-runs the current query against a higher file limit instead
  // of making the reader scroll back up and edit the numeric limit field.
  results.addEventListener("click", (event) => {
    const refinement = (event.target as HTMLElement | null)?.closest<HTMLButtonElement>(
      "[data-search-refinement-field][data-search-refinement-value]"
    );
    if (refinement) {
      const query = form.querySelector<HTMLInputElement>('input[name="q"]');
      const field = refinement.dataset.searchRefinementField;
      const value = refinement.dataset.searchRefinementValue;
      if (!query || !field || !value) {
        return;
      }
      const encoded = /[\s"\\]/.test(value)
        ? `"${value.replaceAll("\\", "\\\\").replaceAll('"', '\\"')}"`
        : value;
      const token = `${field}:${encoded}`;
      query.value = `${query.value.trim()} ${token}`.trim();
      query.focus();
      form.requestSubmit();
      return;
    }
    const button = (event.target as HTMLElement | null)?.closest<HTMLButtonElement>("[data-search-more]");
    if (!button) {
      return;
    }
    const limit = form.querySelector<HTMLInputElement>('input[name="limit"]');
    if (!limit) {
      return;
    }
    limit.value = button.dataset.searchMore ?? limit.value;
    form.requestSubmit();
  });
}

/**
 * Mirrors live catalogue counts into the first-run panel. The catalogue list is
 * swapped by SSE; this keeps the indexing headline and bar in step without
 * replacing the results region, which would clobber an active search.
 */
function enableFirstRunProgress(): void {
  const panel = document.querySelector<HTMLElement>("[data-first-run]");
  const metrics = document.querySelector<HTMLElement>("#repository-metrics");
  if (!panel || !metrics) {
    return;
  }
  const heading = panel.querySelector<HTMLElement>("[data-first-run-heading]");
  const detail = panel.querySelector<HTMLElement>("[data-first-run-detail]");
  const bar = panel.querySelector<HTMLElement>("[data-first-run-bar]");
  if (!heading || !detail || !bar) {
    return;
  }

  const sync = (): void => {
    const values = Array.from(metrics.querySelectorAll("strong")).map((node) => Number(node.textContent ?? "0"));
    const [ready = 0, pending = 0, failed = 0, empty = 0] = values;
    const total = ready + pending + failed;
    if (total <= 0) {
      heading.textContent = "No repositories need indexing";
      detail.textContent = empty
        ? `${empty} empty ${empty === 1 ? "repository has" : "repositories have"} nothing to index.`
        : "No Git repositories were found.";
      bar.style.width = "0%";
      return;
    }
    if (pending === 0) {
      heading.textContent = `Indexed ${ready} of ${total} indexable repositories`;
      detail.textContent = failed
        ? `${failed} ${failed === 1 ? "repository needs" : "repositories need"} attention` +
          (empty ? ` · ${empty} empty with nothing to index` : "") + "."
        : "Search now covers every indexable repository" +
          (empty ? ` · ${empty} empty with nothing to index` : "") + ".";
    } else {
      heading.textContent = `Indexing ${pending} of ${total} indexable repositories`;
      detail.textContent =
        `Search works now, but results stay partial until every repository is indexed. ${ready} ready` +
        (failed ? ` · ${failed} need attention` : "") +
        (empty ? ` · ${empty} empty with nothing to index` : "") + ".";
    }
    bar.style.width = `${Math.min(100, Math.round((ready / total) * 100))}%`;
  };

  new MutationObserver(sync).observe(metrics, { childList: true, subtree: true, characterData: true });
  sync();
}

/**
 * Focus the search field from anywhere. A cross-repository code search tool
 * with no keyboard entry point is the highest-cost omission for its audience.
 */
function enableSearchShortcut(): void {
  const input = document.querySelector<HTMLInputElement>("#search-query");
  document.addEventListener("keydown", (event) => {
    const target = event.target as HTMLElement | null;
    const typing =
      target instanceof HTMLInputElement ||
      target instanceof HTMLTextAreaElement ||
      target instanceof HTMLSelectElement ||
      target?.isContentEditable === true;

    const palette = (event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k";
    const slash = event.key === "/" && !typing && !event.metaKey && !event.ctrlKey && !event.altKey;
    if (!palette && !slash) {
      return;
    }
    event.preventDefault();
    if (input) {
      input.focus();
      input.select();
      return;
    }
    // Every other surface routes back to search, which is the shared entry point.
    window.location.assign("/");
  });
}

/** Shows the platform's own modifier rather than always naming Ctrl. */
function localiseShortcutHints(): void {
  const apple = /Mac|iPhone|iPad/.test(navigator.platform) || /Mac/.test(navigator.userAgent);
  if (!apple) {
    return;
  }
  document.querySelectorAll<HTMLElement>("[data-platform-shortcut]").forEach((node) => {
    node.textContent = `⌘ ${node.dataset.platformShortcut}`;
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

function enableMCPSecretControls(): void {
  const secret = document.querySelector<HTMLElement>("[data-mcp-secret]");
  const toggle = document.querySelector<HTMLButtonElement>("[data-mcp-secret-toggle]");
  if (!secret || !toggle) {
    return;
  }
  const masked = secret.textContent || "";
  const value = secret.dataset.secret || "";
  let revealed = false;
  toggle.setAttribute("aria-pressed", "false");
  toggle.addEventListener("click", () => {
    revealed = !revealed;
    secret.textContent = revealed ? value : masked;
    toggle.textContent = revealed ? "Hide" : "Reveal";
    toggle.setAttribute("aria-pressed", String(revealed));
  });
}

function enableTokenBudgetHelp(): void {
  const dialog = document.querySelector<HTMLDialogElement>("#token-budget-help");
  const close = dialog?.querySelector<HTMLButtonElement>("[data-token-budget-help-close]");
  const triggers = Array.from(document.querySelectorAll<HTMLButtonElement>("[data-token-budget-help]"));
  if (!dialog || !close || triggers.length === 0) {
    return;
  }
  for (const trigger of triggers) {
    trigger.addEventListener("click", () => dialog.showModal());
  }
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog) {
      dialog.close();
    }
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

type SourceIntelligenceLine = {
  number: number;
  text?: string;
  reference_kind?: string;
  reference_target?: string;
  reference_receiver?: string;
  reference_confidence?: string;
};

type SourceIntelligenceMatch = {
  repository_id?: number;
  repository: string;
  revision: string;
  path: string;
  language?: string;
  source_url?: string;
  lines?: SourceIntelligenceLine[];
};

type SourceIntelligencePayload = {
  match_count: number;
  returned_files: number;
  truncated: boolean;
  matches?: SourceIntelligenceMatch[];
  warnings?: Array<{ code: string; message: string }>;
  reference_index?: {
    provider: string;
    state: string;
    requested_repositories: number;
    ready_repositories: number;
    pending_repositories: number;
  };
};

function enableSourceIntelligence(): void {
  const workspace = document.querySelector<HTMLElement>("[data-source-intelligence]");
  const viewer = document.querySelector<HTMLElement>(".source-viewer");
  const form = workspace?.querySelector<HTMLFormElement>("[data-source-intelligence-form]");
  const query = workspace?.querySelector<HTMLInputElement>("[data-source-intelligence-query]");
  const mode = workspace?.querySelector<HTMLSelectElement>("[data-source-intelligence-mode]");
  const toggle = workspace?.querySelector<HTMLButtonElement>("[data-source-intelligence-toggle]");
  const status = workspace?.querySelector<HTMLElement>("[data-source-intelligence-status]");
  const results = workspace?.querySelector<HTMLElement>("[data-source-intelligence-results]");
  const repositoryID = Number(workspace?.dataset.repositoryId);
  if (!workspace || !viewer || !form || !query || !mode || !toggle || !status || !results ||
    !Number.isInteger(repositoryID) || repositoryID <= 0) {
    return;
  }

  let requestController: AbortController | undefined;
  const setResultsExpanded = (expanded: boolean): void => {
    results.hidden = !expanded;
    toggle.textContent = expanded ? "Collapse findings" : "Show findings";
    toggle.setAttribute("aria-expanded", String(expanded));
  };
  const resultSourceURL = (match: SourceIntelligenceMatch, line: SourceIntelligenceLine): string => {
    if (match.source_url) {
      try {
        const target = new URL(match.source_url, location.origin);
        target.searchParams.set("focus", `${line.number}-${line.number}`);
        target.hash = `L${line.number}`;
        return target.pathname + target.search + target.hash;
      } catch {
        return match.source_url;
      }
    }
    const parameters = new URLSearchParams({
      rev: match.revision,
      path: match.path,
      focus: `${line.number}-${line.number}`
    });
    return `/source/${match.repository_id ?? repositoryID}?${parameters.toString()}#L${line.number}`;
  };
  const render = (payload: SourceIntelligencePayload): void => {
    results.replaceChildren();
    const matches = payload.matches ?? [];
    if (matches.length === 0) {
      const empty = document.createElement("p");
      empty.className = "source-intelligence-empty";
      empty.textContent = mode.value === "references"
        ? "No indexed usages matched this symbol in the current repository."
        : "No code matched this query in the current repository.";
      results.append(empty);
    }
    for (const match of matches) {
      const article = document.createElement("details");
      article.className = "source-intelligence-result";
      article.open = true;
      const heading = document.createElement("summary");
      heading.className = "source-intelligence-result-heading";
      const title = document.createElement("strong");
      title.textContent = match.path;
      const repository = document.createElement("span");
      const lines = match.lines ?? [];
      repository.textContent = `${lines.length} ${lines.length === 1 ? "finding" : "findings"} · ${match.repository} · ${match.revision.slice(0, 8)}`;
      heading.append(title, repository);
      article.append(heading);

      for (const line of lines) {
        const link = document.createElement("a");
        link.className = "source-intelligence-line";
        link.href = resultSourceURL(match, line);
        const number = document.createElement("span");
        number.textContent = `L${line.number}`;
        const code = document.createElement("code");
        code.textContent = line.text || line.reference_target || query.value;
        code.dataset.searchHighlightLanguage = sourceHighlightLanguage(match.language, match.path);
        const metadata = document.createElement("small");
        metadata.textContent = [
          line.reference_kind,
          line.reference_receiver ? `receiver ${line.reference_receiver}` : "",
          line.reference_confidence
        ].filter(Boolean).join(" · ");
        link.append(number, code, metadata);
        article.append(link);
      }
      results.append(article);
    }
    for (const warning of payload.warnings ?? []) {
      const item = document.createElement("p");
      item.className = "source-intelligence-warning";
      item.textContent = warning.message;
      results.append(item);
    }
    results.hidden = false;
    toggle.hidden = false;
    setResultsExpanded(true);
    status.textContent = sourceSearchSummary(payload);
    highlightSearchResults(results);
  };

  const prepareSelection = (): void => {
    const selection = document.getSelection();
    if (!selection || selection.rangeCount === 0 || selection.isCollapsed) {
      return;
    }
    const range = selection.getRangeAt(0);
    if (!viewer.contains(range.commonAncestorContainer)) {
      return;
    }
    const selected = sourceSelection(selection.toString());
    if (!selected) {
      return;
    }
    query.value = selected.query;
    mode.value = selected.mode;
    status.textContent = selected.mode === "references"
      ? `Ready to find usages of ${selected.query}.`
      : "Ready to search the selected code in this repository.";
  };

  viewer.addEventListener("mouseup", prepareSelection);
  viewer.addEventListener("keyup", prepareSelection);
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const searchQuery = query.value.trim();
    if (!searchQuery) {
      query.focus();
      status.textContent = "Select a symbol or enter code to search.";
      return;
    }
    requestController?.abort();
    requestController = new AbortController();
    status.textContent = mode.value === "references"
      ? `Finding usages of ${searchQuery}…`
      : `Searching ${workspace.dataset.repositoryName ?? "this repository"}…`;
    results.hidden = true;
    toggle.hidden = true;
    try {
      const response = await fetch(sourceSearchURL(repositoryID, searchQuery, mode.value), {
        cache: "no-store",
        headers: { Accept: "application/json" },
        signal: requestController.signal
      });
      if (!response.ok && response.status !== 202) {
        throw new Error(await responseErrorMessage(response, `Search failed (${response.status})`));
      }
      render(await response.json() as SourceIntelligencePayload);
    } catch (error) {
      if (error instanceof DOMException && error.name === "AbortError") {
        return;
      }
      status.textContent = error instanceof Error ? error.message : "The repository search failed.";
      results.hidden = true;
      toggle.hidden = true;
    }
  });
  toggle.addEventListener("click", () => {
    setResultsExpanded(results.hasAttribute("hidden"));
  });
  document.addEventListener("keydown", (event) => {
    if ((event.ctrlKey || event.metaKey) && event.shiftKey && event.key.toLowerCase() === "f") {
      event.preventDefault();
      mode.value = "zoekt";
      query.focus();
      query.select();
    }
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

type ContextKind = "repository" | "file" | "directory" | "symbol";

type ContextSelector = {
  kind: ContextKind;
  repository_id: number;
  revision?: string;
  path?: string;
  symbol?: string;
  symbol_kind?: string;
  line?: number;
};

type ContextSource = {
  kind: "explicit" | "named" | "personal_default" | "administrator_default";
  id?: string;
  title?: string;
};

type ResolvedContext = ContextSelector & {
  repository: string;
  revision: string;
  start_line?: number;
  end_line?: number;
  label: string;
  url: string;
  sources?: ContextSource[];
};

type ContextSuggestion = {
  context: ContextSelector;
  label: string;
  detail?: string;
  url?: string;
  sources?: ContextSource[];
};

type NamedContext = {
  id: string;
  title: string;
  description?: string;
  category: "team" | "product" | "service_fleet" | "release" | "personal_task";
  visibility: "personal" | "shared";
  default_scope: "none" | "personal" | "administrator";
  owner_id: string;
  managed: boolean;
  editable: boolean;
  state: "ready" | "invalid";
  url: string;
  contexts: ResolvedContext[];
  issues?: Array<{ code: string; message: string }>;
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
  author: ConversationAuthor;
  created_at: string;
  updated_at: string;
  message_count: number;
  input_tokens: number;
  output_tokens: number;
  messages?: ConversationRecordMessage[];
};

type ConversationAuthor = {
  id: string;
  name?: string;
  email?: string;
  provider: string;
};

type ConversationHistoryResponse = {
  conversations: ConversationRecord[];
  viewer: ConversationAuthor;
  can_view_all: boolean;
  scope: "own" | "all";
};

type ConversationRecordMessage = {
  id: number;
  role: "user" | "assistant";
  text?: string;
  images?: ConversationImage[];
  sources?: Array<{ label: string; url: string }>;
  contexts?: ResolvedContext[];
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
  models?: Array<{ id: string; label: string; efforts?: string[] | null }>;
  efforts?: string[];
  image_input: boolean;
  image_output: boolean;
  interrupt: boolean;
  context_usage: boolean;
  token_usage: boolean;
  token_budget: boolean;
};

const providerModelEfforts = (
  status: ProviderStatus | undefined,
  modelID: string
): string[] => {
  const selected = status?.models?.find((candidate) => candidate.id === modelID);
  return selected && selected.efforts !== undefined && selected.efforts !== null
    ? selected.efforts
    : status?.efforts ?? [];
};

const supportedImageTypes = new Set(["image/gif", "image/jpeg", "image/png", "image/webp"]);
const maximumImagesPerTurn = 4;
const maximumImageBytes = 8 * 1024 * 1024;
const maximumStructuredContexts = 32;

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

async function responseErrorMessage(response: Response, fallback: string): Promise<string> {
  const body = await response.text();
  try {
    const parsed = JSON.parse(body) as { error?: { message?: string } };
    return parsed.error?.message?.trim() || body.trim() || fallback;
  } catch {
    return body.trim() || fallback;
  }
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
    count.dataset.empty = String(entries.length === 0);
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
    count.dataset.empty = "true";
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
    const download = canvas.parentElement?.querySelector<HTMLButtonElement>("[data-mermaid-download]");
    if (download) {
      download.disabled = false;
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
      const download = document.createElement("button");
      download.type = "button";
      download.className = "mermaid-diagram-action";
      download.disabled = true;
      download.setAttribute("data-mermaid-download", "");
      download.setAttribute("aria-label", "Download diagram as SVG");
      download.innerHTML = `
        <svg viewBox="0 0 20 20" aria-hidden="true">
          <path d="M10 3.5v8M6.75 8.5 10 11.75l3.25-3.25M4 14.5v2h12v-2"></path>
        </svg>
        <span>SVG</span>
      `;
      const expand = document.createElement("button");
      expand.type = "button";
      expand.className = "mermaid-diagram-action";
      expand.disabled = true;
      expand.setAttribute("data-mermaid-expand", "");
      expand.setAttribute("aria-label", "Expand diagram");
      expand.innerHTML = `
        <svg viewBox="0 0 20 20" aria-hidden="true">
          <path d="M7.5 3.5h-4v4M12.5 3.5h4v4M16.5 12.5v4h-4M7.5 16.5h-4v-4"></path>
        </svg>
        <span>Expand</span>
      `;
      toolbar.append(download, expand);
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

function mermaidDownloadName(label: string): string {
  const diagramType = label
    .toLowerCase()
    .replace(/\bdiagram\b/g, "")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "") || "diagram";
  const timestamp = new Date().toISOString().replace(/[-:]/g, "").replace(/\.\d{3}Z$/, "Z");
  return `repokarta-${diagramType}-${timestamp}.svg`;
}

function downloadMermaidSVG(svg: SVGSVGElement, label: string, debug?: DebugLogger): void {
  const clone = svg.cloneNode(true) as SVGSVGElement;
  clone.setAttribute("xmlns", "http://www.w3.org/2000/svg");
  const viewBox = clone.viewBox.baseVal;
  if (viewBox.width > 0 && viewBox.height > 0) {
    clone.setAttribute("width", String(Math.ceil(viewBox.width)));
    clone.setAttribute("height", String(Math.ceil(viewBox.height)));
  }
  clone.style.removeProperty("max-width");
  const serialized = new XMLSerializer().serializeToString(clone);
  const url = URL.createObjectURL(new Blob(
    [`<?xml version="1.0" encoding="UTF-8"?>\n${serialized}`],
    { type: "image/svg+xml;charset=utf-8" }
  ));
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = mermaidDownloadName(label);
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
  debug?.add("info", "chat.diagram.downloaded", {
    format: "svg",
    file_name: anchor.download
  });
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
  const download = dialog?.querySelector<HTMLButtonElement>("[data-mermaid-viewer-download]");
  const close = dialog?.querySelector<HTMLButtonElement>("[data-mermaid-viewer-close]");
  if (!dialog || !canvas || !stage || !title || !status || !zoomSelect || !zoomOut || !zoomIn || !download || !close) {
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
    download.disabled = true;
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
    download.disabled = false;
    applyZoom("fit", false);
    debug?.add("info", "chat.diagram.viewer.opened", {
      type: result.diagramType,
      intrinsic_width: Math.round(intrinsicWidth),
      duration_ms: Math.round(performance.now() - started)
    });
  };

  document.addEventListener("click", (event) => {
    const downloadButton = (event.target as Element | null)?.closest<HTMLButtonElement>("[data-mermaid-download]");
    if (downloadButton && !downloadButton.disabled) {
      const figure = downloadButton.closest<HTMLElement>(".mermaid-diagram");
      const svg = figure?.querySelector<SVGSVGElement>(".mermaid-diagram-canvas svg");
      if (svg) {
        downloadMermaidSVG(svg, figure?.querySelector(".mermaid-diagram-canvas")?.getAttribute("aria-label") ?? "Mermaid diagram", debug);
      }
      return;
    }
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
  download.addEventListener("click", () => {
    const svg = canvas.querySelector<SVGSVGElement>("svg");
    if (svg) {
      downloadMermaidSVG(svg, title.textContent ?? "Mermaid diagram", debug);
    }
  });
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
    download.disabled = true;
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

function conversationImageDownloadName(
  image: ConversationImage,
  kind: "input" | "output",
  index: number
): string {
  const suppliedName = image.name.trim().replaceAll("\\", "/").split("/").pop();
  if (suppliedName) {
    return suppliedName;
  }
  const extension = image.media_type.toLowerCase() === "image/jpeg"
    ? ".jpg"
    : image.media_type.toLowerCase() === "image/gif"
      ? ".gif"
      : image.media_type.toLowerCase() === "image/webp"
        ? ".webp"
        : ".png";
  return `repokarta-${kind === "input" ? "attachment" : "generated-image"}-${index + 1}${extension}`;
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
  for (const [index, image] of images.entries()) {
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
    const download = document.createElement("a");
    download.className = "conversation-image-download";
    download.href = source;
    download.download = conversationImageDownloadName(image, kind, index);
    download.title = `Download ${download.download}`;
    download.setAttribute("aria-label", `Download ${download.download}`);
    download.innerHTML = `
      <svg viewBox="0 0 20 20" aria-hidden="true">
        <path d="M10 3.5v8M6.75 8.5 10 11.75l3.25-3.25M4 14.5v2h12v-2"></path>
      </svg>
      <span>Download</span>
    `;
    caption.append(download);
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
  if (location.pathname !== "/chat" && !document.querySelector("[data-chat-workspace]")) {
    return;
  }
  const form = document.querySelector<HTMLFormElement>("#conversation-form");
  const messages = document.querySelector<HTMLElement>("#conversation-messages");
  const empty = document.querySelector<HTMLElement>("[data-conversation-empty]");
  const provider = document.querySelector<HTMLSelectElement>("#conversation-provider");
  const model = document.querySelector<HTMLSelectElement>("#conversation-model");
  const effort = document.querySelector<HTMLSelectElement>("#conversation-effort");
  const timeout = document.querySelector<HTMLSelectElement>("#conversation-timeout");
  const tokenBudget = document.querySelector<HTMLSelectElement>("#conversation-token-budget");
  const tokenBudgetField = document.querySelector<HTMLElement>("[data-token-budget-field]");
  const input = document.querySelector<HTMLTextAreaElement>("#conversation-message");
  const imageInput = document.querySelector<HTMLInputElement>("#conversation-image-input");
  const attachButton = document.querySelector<HTMLButtonElement>("[data-image-attach]");
  const attachmentTray = document.querySelector<HTMLElement>("#conversation-attachments");
  const contextTray = document.querySelector<HTMLElement>("#conversation-contexts");
  const contextError = document.querySelector<HTMLElement>("#conversation-context-error");
  const contextSuggestions = document.querySelector<HTMLElement>("#conversation-context-suggestions");
  const contextAdd = document.querySelector<HTMLButtonElement>("[data-context-add]");
  const namedContextButton = document.querySelector<HTMLButtonElement>("[data-named-contexts]");
  const namedContextDialog = document.querySelector<HTMLDialogElement>("#named-context-dialog");
  const namedContextClose = document.querySelector<HTMLButtonElement>("[data-named-context-close]");
  const namedContextNew = document.querySelector<HTMLButtonElement>("[data-named-context-new]");
  const namedContextCancel = document.querySelector<HTMLButtonElement>("[data-named-context-cancel]");
  const namedContextList = document.querySelector<HTMLElement>("[data-named-context-list]");
  const namedContextFeedback = document.querySelector<HTMLElement>("[data-named-context-feedback]");
  const namedContextForm = document.querySelector<HTMLFormElement>("[data-named-context-form]");
  const namedContextEditorTitle = document.querySelector<HTMLElement>("[data-named-context-editor-title]");
  const namedContextEditorError = document.querySelector<HTMLElement>("[data-named-context-editor-error]");
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
  const historyScopeButtons = Array.from(
    document.querySelectorAll<HTMLButtonElement>("[data-conversation-scope]")
  );
  const authorFilterField = document.querySelector<HTMLElement>(".conversation-author-filter");
  const authorFilter = document.querySelector<HTMLSelectElement>("[data-conversation-author-filter]");
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
  const initializationChecks = [
    { selector: "#conversation-form", expected: "1", actual: Number(Boolean(form)) },
    { selector: "#conversation-messages", expected: "1", actual: Number(Boolean(messages)) },
    { selector: "#conversation-provider", expected: "1", actual: Number(Boolean(provider)) },
    { selector: "#conversation-model", expected: "1", actual: Number(Boolean(model)) },
    { selector: "#conversation-effort", expected: "1", actual: Number(Boolean(effort)) },
    { selector: "#conversation-timeout", expected: "1", actual: Number(Boolean(timeout)) },
    { selector: "#conversation-token-budget", expected: "1", actual: Number(Boolean(tokenBudget)) },
    { selector: "[data-token-budget-field]", expected: "1", actual: Number(Boolean(tokenBudgetField)) },
    { selector: "#conversation-message", expected: "1", actual: Number(Boolean(input)) },
    { selector: "#conversation-image-input", expected: "1", actual: Number(Boolean(imageInput)) },
    { selector: "[data-image-attach]", expected: "1", actual: Number(Boolean(attachButton)) },
    { selector: "#conversation-attachments", expected: "1", actual: Number(Boolean(attachmentTray)) },
    { selector: "#conversation-contexts", expected: "1", actual: Number(Boolean(contextTray)) },
    { selector: "#conversation-context-error", expected: "1", actual: Number(Boolean(contextError)) },
    { selector: "#conversation-context-suggestions", expected: "1", actual: Number(Boolean(contextSuggestions)) },
    { selector: "[data-context-add]", expected: "1", actual: Number(Boolean(contextAdd)) },
    { selector: "[data-named-contexts]", expected: "1", actual: Number(Boolean(namedContextButton)) },
    { selector: "#named-context-dialog", expected: "1", actual: Number(Boolean(namedContextDialog)) },
    { selector: "[data-named-context-close]", expected: "1", actual: Number(Boolean(namedContextClose)) },
    { selector: "[data-named-context-new]", expected: "1", actual: Number(Boolean(namedContextNew)) },
    { selector: "[data-named-context-cancel]", expected: "1", actual: Number(Boolean(namedContextCancel)) },
    { selector: "[data-named-context-list]", expected: "1", actual: Number(Boolean(namedContextList)) },
    { selector: "[data-named-context-feedback]", expected: "1", actual: Number(Boolean(namedContextFeedback)) },
    { selector: "[data-named-context-form]", expected: "1", actual: Number(Boolean(namedContextForm)) },
    { selector: "[data-named-context-editor-title]", expected: "1", actual: Number(Boolean(namedContextEditorTitle)) },
    { selector: "[data-named-context-editor-error]", expected: "1", actual: Number(Boolean(namedContextEditorError)) },
    { selector: "#conversation-activity", expected: "1", actual: Number(Boolean(composerActivity)) },
    { selector: "#image-support-detail", expected: "1", actual: Number(Boolean(imageSupportDetail)) },
    { selector: "#conversation-submit", expected: "1", actual: Number(Boolean(submit)) },
    { selector: "#conversation-interrupt", expected: "1", actual: Number(Boolean(interrupt)) },
    { selector: "#conversation-runtime", expected: "1", actual: Number(Boolean(runtime)) },
    { selector: "#conversation-context-value", expected: "1", actual: Number(Boolean(contextValue)) },
    { selector: "#conversation-context-meter", expected: "1", actual: Number(Boolean(contextMeter)) },
    { selector: "#conversation-usage-value", expected: "1", actual: Number(Boolean(usageValue)) },
    { selector: "#provider-detail", expected: "1", actual: Number(Boolean(detail)) },
    { selector: "[data-new-conversation]", expected: "at least 1", actual: newConversationButtons.length },
    { selector: "#conversation-history", expected: "1", actual: Number(Boolean(history)) },
    { selector: "[data-conversation-history-empty]", expected: "1", actual: Number(Boolean(historyEmpty)) },
    { selector: "[data-conversation-filter]", expected: "1", actual: Number(Boolean(historyFilter)) },
    { selector: "[data-conversation-scope]", expected: "1..2", actual: historyScopeButtons.length },
    {
      selector: '[data-conversation-scope="own"]',
      expected: "1",
      actual: historyScopeButtons.filter((button) => button.dataset.conversationScope === "own").length
    },
    {
      selector: '[data-conversation-scope="all"]',
      expected: historyScopeButtons.length === 2 ? "1" : "0",
      actual: historyScopeButtons.filter((button) => button.dataset.conversationScope === "all").length
    },
    { selector: ".conversation-author-filter", expected: "1", actual: Number(Boolean(authorFilterField)) },
    { selector: "[data-conversation-author-filter]", expected: "1", actual: Number(Boolean(authorFilter)) },
    { selector: "[data-chat-workspace]", expected: "1", actual: Number(Boolean(workspace)) },
    { selector: "[data-session-panel]", expected: "1", actual: Number(Boolean(sessionPanel)) },
    { selector: "[data-session-panel-open]", expected: "1", actual: Number(Boolean(sessionPanelOpen)) },
    { selector: "[data-session-panel-close]", expected: "1", actual: Number(Boolean(sessionPanelClose)) },
    { selector: "[data-session-panel-scrim]", expected: "1", actual: Number(Boolean(sessionPanelScrim)) },
    { selector: "[data-inspector]", expected: "1", actual: Number(Boolean(inspector)) },
    { selector: "[data-inspector-toggle]", expected: "1", actual: Number(Boolean(inspectorToggle)) },
    { selector: "[data-inspector-close]", expected: "1", actual: Number(Boolean(inspectorClose)) },
    { selector: "[data-inspector-scrim]", expected: "1", actual: Number(Boolean(inspectorScrim)) },
    { selector: "#conversation-title", expected: "1", actual: Number(Boolean(title)) },
    { selector: "[data-conversation-title-edit]", expected: "1", actual: Number(Boolean(titleEdit)) },
    { selector: "#conversation-header-status", expected: "1", actual: Number(Boolean(headerStatus)) },
    { selector: "#conversation-provider-label", expected: "1", actual: Number(Boolean(providerLabel)) },
    { selector: ".conversation-settings", expected: "1", actual: Number(Boolean(settings)) },
    { selector: "#conversation-evidence-list", expected: "1", actual: Number(Boolean(evidenceList)) },
    { selector: "[data-evidence-empty]", expected: "1", actual: Number(Boolean(evidenceEmpty)) },
    { selector: "[data-evidence-count]", expected: "at least 1", actual: evidenceCounts.length }
  ];
  const initializationMismatches = initializationChecks.filter((check) => {
    if (check.expected === "at least 1") {
      return check.actual < 1;
    }
    if (check.expected === "1..2") {
      return check.actual < 1 || check.actual > 2;
    }
    return check.actual !== Number.parseInt(check.expected, 10);
  });
  if (
    !form ||
    !messages ||
    !provider ||
    !model ||
    !effort ||
    !timeout ||
    !tokenBudget ||
    !tokenBudgetField ||
    !input ||
    !imageInput ||
    !attachButton ||
    !attachmentTray ||
    !contextTray ||
    !contextError ||
    !contextSuggestions ||
    !contextAdd ||
    !namedContextButton ||
    !namedContextDialog ||
    !namedContextClose ||
    !namedContextNew ||
    !namedContextCancel ||
    !namedContextList ||
    !namedContextFeedback ||
    !namedContextForm ||
    !namedContextEditorTitle ||
    !namedContextEditorError ||
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
    historyScopeButtons.length < 1 ||
    historyScopeButtons.length > 2 ||
    historyScopeButtons.filter((button) => button.dataset.conversationScope === "own").length !== 1 ||
    historyScopeButtons.filter((button) => button.dataset.conversationScope === "all").length !==
      (historyScopeButtons.length === 2 ? 1 : 0) ||
    !authorFilterField ||
    !authorFilter ||
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
    debug?.add("error", "chat.initialization.failed", {
      mismatches: initializationMismatches
    });
    debug?.open();
    const failureHeading = "Chat failed to initialise";
    const failureDetail = "The page is incomplete. Open Debug for the missing interface details, then reload after updating RepoKarta.";
    const visibleHistoryError = historyEmpty ??
      document.querySelector<HTMLElement>("[data-conversation-history-empty]");
    if (visibleHistoryError) {
      visibleHistoryError.hidden = false;
      visibleHistoryError.textContent = `${failureHeading}. Open Debug for details.`;
    }
    const visibleHeaderStatus = headerStatus ??
      document.querySelector<HTMLElement>("#conversation-header-status");
    if (visibleHeaderStatus) {
      visibleHeaderStatus.textContent = failureHeading;
    }
    const visibleMessages = messages ??
      document.querySelector<HTMLElement>("#conversation-messages");
    const visibleFailure = document.createElement("section");
    visibleFailure.className = "conversation-initialization-error";
    visibleFailure.dataset.conversationInitializationError = "";
    visibleFailure.setAttribute("role", "alert");
    const visibleFailureHeading = document.createElement("h3");
    visibleFailureHeading.textContent = failureHeading;
    const visibleFailureDetail = document.createElement("p");
    visibleFailureDetail.textContent = failureDetail;
    visibleFailure.append(visibleFailureHeading, visibleFailureDetail);
    if (visibleMessages) {
      visibleMessages.replaceChildren(visibleFailure);
    } else {
      document.querySelector<HTMLElement>("[data-conversation]")?.prepend(visibleFailure);
    }
    const visibleInput = input ??
      document.querySelector<HTMLTextAreaElement>("#conversation-message");
    if (visibleInput) {
      visibleInput.disabled = true;
      visibleInput.placeholder = "Chat unavailable";
    }
    const visibleSubmit = submit ??
      document.querySelector<HTMLButtonElement>("#conversation-submit");
    if (visibleSubmit) {
      visibleSubmit.disabled = true;
    }
    return;
  }

  const singleScope = historyScopeButtons.length === 1;
  const historyScope = historyScopeButtons[0].closest<HTMLElement>(".conversation-history-scope");
  if (historyScope) {
    historyScope.dataset.singleScope = String(singleScope);
  }
  for (const button of historyScopeButtons) {
    button.disabled = singleScope;
    button.setAttribute("aria-disabled", String(singleScope));
  }

  const runSettingsStorageKey = "repokarta:conversation-run-settings:v1";
  const activeConversationStorageKey = "repokarta:active-conversation:v1";
  const restoreRunSettings = (): void => {
    try {
      const stored = window.localStorage.getItem(runSettingsStorageKey);
      if (!stored) {
        return;
      }
      const parsed = JSON.parse(stored) as { timeout?: unknown; token_budget?: unknown };
      const timeoutValue = typeof parsed.timeout === "string" ? parsed.timeout : "";
      const tokenBudgetValue = typeof parsed.token_budget === "string" ? parsed.token_budget : "";
      if (Array.from(timeout.options).some((option) => option.value === timeoutValue)) {
        timeout.value = timeoutValue;
      }
      if (Array.from(tokenBudget.options).some((option) => option.value === tokenBudgetValue)) {
        tokenBudget.value = tokenBudgetValue;
      }
    } catch {
      // Browsers can deny local storage. Run controls still retain safe defaults.
    }
  };
  const persistRunSettings = (): void => {
    try {
      window.localStorage.setItem(runSettingsStorageKey, JSON.stringify({
        timeout: timeout.value,
        token_budget: tokenBudget.value
      }));
    } catch {
      // Run controls remain usable when local storage is unavailable.
    }
  };
  restoreRunSettings();
  timeout.addEventListener("change", persistRunSettings);
  tokenBudget.addEventListener("change", persistRunSettings);

  let conversationID = "";
  let busy = false;
  let attachedImages: ConversationImage[] = [];
  let explicitContexts: ContextSuggestion[] = [];
  let selectedContexts: ContextSuggestion[] = [];
  let namedContexts: NamedContext[] = [];
  const activeNamedContextIDs = new Set<string>();
  let defaultContextsEnabled = true;
  let namedContextViewerIsAdmin = false;
  let suggestedContexts: ContextSuggestion[] = [];
  let activeContextSuggestion = -1;
  let contextRequestSequence = 0;
  let contextMention: { kind: ContextKind; start: number; end: number } | undefined;
  let attachmentFeedback = "";
  let statuses: ProviderStatus[] = [];
  let conversationSummaries: ConversationRecord[] = [];
  let conversationScope: "own" | "all" = "own";
  let conversationViewer: ConversationAuthor = {
    id: "local:admin",
    name: "Local administrator",
    provider: "local"
  };
  let canViewAllConversations = false;
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
    const modelName = status?.models?.find((candidate) => candidate.id === model.value)?.label
      || model.value.trim()
      || "Provider default model";
    const effortName = effort.selectedOptions[0]?.textContent || "Provider default";
    providerLabel.textContent = `${providerName} · ${modelName} · ${effortName} effort`;
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
    try {
      if (id) {
        window.localStorage.setItem(activeConversationStorageKey, id);
      } else {
        window.localStorage.removeItem(activeConversationStorageKey);
      }
    } catch {
      // Deep links still work when the browser denies local storage.
    }
    const url = new URL(window.location.href);
    if (id) {
      url.searchParams.set("conversation", id);
    } else {
      url.searchParams.delete("conversation");
    }
    // Switching conversations is navigation, so it earns a history entry;
    // re-stating the current one does not.
    const entry = { conversation: id };
    if (window.location.href === url.href) {
      window.history.replaceState(entry, "", url);
    } else {
      window.history.pushState(entry, "", url);
    }
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

  const contextKey = (selector: ContextSelector): string => {
    return [
      selector.kind,
      selector.repository_id,
      selector.revision ?? "",
      selector.path ?? "",
      selector.symbol ?? "",
      selector.symbol_kind ?? "",
      selector.line ?? ""
    ].join(":");
  };

  const contextSourceText = (sources: ContextSource[] = []): string => {
    if (sources.length === 0) {
      return "explicit";
    }
    return sources.map((source) => {
      const kind = source.kind.replaceAll("_", " ");
      return source.title ? `${source.title} (${kind})` : kind;
    }).join(", ");
  };

  const contextSuggestionFromResolved = (context: ResolvedContext): ContextSuggestion => ({
    context: {
      kind: context.kind,
      repository_id: context.repository_id,
      revision: context.revision,
      ...(context.path ? { path: context.path } : {}),
      ...(context.symbol ? { symbol: context.symbol } : {}),
      ...(context.symbol_kind ? { symbol_kind: context.symbol_kind } : {}),
      ...(context.line ? { line: context.line } : {})
    },
    label: context.label,
    detail: `${context.repository} @ ${context.revision.slice(0, 8)}`,
    url: context.url,
    sources: context.sources ?? []
  });

  const copyContextURL = async (value: string, button: HTMLButtonElement): Promise<void> => {
    if (!value) {
      return;
    }
    const original = button.textContent;
    try {
      await navigator.clipboard.writeText(value);
      button.textContent = "✓";
    } catch {
      showContextError("The context URL could not be copied. Open the context and copy it from the address bar.");
    } finally {
      window.setTimeout(() => {
        button.textContent = original;
      }, 1200);
    }
  };

  const refreshEffectiveContexts = async (): Promise<void> => {
    try {
      const response = await fetch("/api/contexts/resolve", {
        method: "POST",
        headers: {
          Accept: "application/json",
          "Content-Type": "application/json"
        },
        body: JSON.stringify({
          contexts: explicitContexts.map((context) => context.context),
          named_context_ids: Array.from(activeNamedContextIDs),
          use_default_contexts: defaultContextsEnabled
        })
      });
      if (!response.ok) {
        throw new Error(await responseErrorMessage(response, "Effective contexts could not be resolved."));
      }
      const result = await response.json() as {
        contexts?: ResolvedContext[];
        named_contexts?: NamedContext[];
      };
      selectedContexts = (result.contexts ?? []).map(contextSuggestionFromResolved);
      renderContextTray();
      renderNamedContextList();
      showContextError();
    } catch (error: unknown) {
      selectedContexts = [];
      renderContextTray();
      showContextError(error instanceof Error ? error.message : "Effective contexts could not be resolved.");
    }
  };

  const removeEffectiveContext = (context: ContextSuggestion): void => {
    const key = contextKey(context.context);
    explicitContexts = explicitContexts.filter((candidate) => contextKey(candidate.context) !== key);
    for (const source of context.sources ?? []) {
      if (source.kind === "named" && source.id) {
        activeNamedContextIDs.delete(source.id);
      }
      if (source.kind === "personal_default" || source.kind === "administrator_default") {
        defaultContextsEnabled = false;
      }
    }
    void refreshEffectiveContexts();
  };

  const appendMessageContexts = (
    message: HTMLElement,
    contexts: Array<ResolvedContext | ContextSuggestion> = []
  ): void => {
    if (contexts.length === 0) {
      return;
    }
    const container = document.createElement("div");
    container.className = "conversation-message-contexts";
    for (const context of contexts) {
      const chip = document.createElement("span");
      chip.className = "conversation-context-chip conversation-context-chip-readonly";
      const selector = "context" in context ? context.context : context;
      const contextURL = context.url;
      const label = document.createElement(contextURL ? "a" : "span");
      label.textContent = context.label;
      if (label instanceof HTMLAnchorElement && contextURL) {
        label.href = contextURL;
        label.target = "_blank";
        label.rel = "noreferrer";
      }
      chip.title = `${selector.kind} context at ${(selector.revision ?? "").slice(0, 8)} · ${contextSourceText(context.sources)}`;
      chip.append(label);
      if (contextURL) {
        const copy = document.createElement("button");
        copy.type = "button";
        copy.textContent = "⧉";
        copy.setAttribute("aria-label", `Copy ${context.label} context URL`);
        copy.addEventListener("click", () => void copyContextURL(contextURL, copy));
        chip.append(copy);
      }
      container.append(chip);
    }
    message.append(container);
  };

  const showContextError = (message = ""): void => {
    contextError.textContent = message;
    contextError.hidden = !message;
  };

  const closeContextSuggestions = (): void => {
    contextRequestSequence++;
    suggestedContexts = [];
    activeContextSuggestion = -1;
    contextMention = undefined;
    contextSuggestions.replaceChildren();
    contextSuggestions.hidden = true;
    input.removeAttribute("aria-activedescendant");
  };

  const renderContextTray = (): void => {
    contextTray.replaceChildren();
    contextTray.hidden = selectedContexts.length === 0;
    for (const context of selectedContexts) {
      const chip = document.createElement("span");
      chip.className = "conversation-context-chip";
      const label = document.createElement(context.url ? "a" : "span");
      label.textContent = context.label;
      if (label instanceof HTMLAnchorElement && context.url) {
        label.href = context.url;
        label.target = "_blank";
        label.rel = "noreferrer";
      }
      chip.title = `${context.context.kind} context · ${contextSourceText(context.sources)}`;
      if (context.url) {
        const copy = document.createElement("button");
        copy.type = "button";
        copy.setAttribute("aria-label", `Copy ${context.label} context URL`);
        copy.textContent = "⧉";
        copy.addEventListener("click", () => void copyContextURL(context.url ?? "", copy));
        chip.append(label, copy);
      } else {
        chip.append(label);
      }
      const remove = document.createElement("button");
      remove.type = "button";
      remove.setAttribute("aria-label", `Remove ${context.label}`);
      remove.textContent = "\u00d7";
      remove.addEventListener("click", () => {
        removeEffectiveContext(context);
        input.focus();
      });
      chip.append(remove);
      contextTray.append(chip);
    }
  };

  const renderContextSuggestions = (): void => {
    contextSuggestions.replaceChildren();
    contextSuggestions.hidden = suggestedContexts.length === 0;
    for (const [index, suggestion] of suggestedContexts.entries()) {
      const option = document.createElement("button");
      option.type = "button";
      option.id = `conversation-context-option-${index}`;
      option.className = "conversation-context-suggestion";
      option.setAttribute("role", "option");
      option.setAttribute("aria-selected", String(index === activeContextSuggestion));
      const label = document.createElement("strong");
      label.textContent = suggestion.label;
      const detail = document.createElement("span");
      detail.textContent = `${suggestion.context.kind} \u00b7 ${suggestion.detail ?? "indexed"}`;
      option.append(label, detail);
      option.addEventListener("pointerdown", (event) => event.preventDefault());
      option.addEventListener("click", () => selectContextSuggestion(index));
      contextSuggestions.append(option);
    }
    if (activeContextSuggestion >= 0) {
      input.setAttribute("aria-activedescendant", `conversation-context-option-${activeContextSuggestion}`);
    } else {
      input.removeAttribute("aria-activedescendant");
    }
  };

  const selectContextSuggestion = (index: number): void => {
    const suggestion = suggestedContexts[index];
    if (!suggestion || !contextMention) {
      return;
    }
    if (!explicitContexts.some((context) => contextKey(context.context) === contextKey(suggestion.context))) {
      explicitContexts.push(suggestion);
    }
    const before = input.value.slice(0, contextMention.start);
    const after = input.value.slice(contextMention.end);
    const separator = before && !before.endsWith(" ") && after && !after.startsWith(" ") ? " " : "";
    input.value = before + separator + after;
    const caret = before.length + separator.length;
    input.setSelectionRange(caret, caret);
    void refreshEffectiveContexts();
    closeContextSuggestions();
    showContextError();
    input.focus();
  };

  const activeMention = (): { kind: ContextKind; query: string; start: number; end: number } | undefined => {
    const caret = input.selectionStart ?? input.value.length;
    return activeContextMention(input.value, caret);
  };

  const updateContextSuggestions = async (): Promise<void> => {
    const mention = activeMention();
    if (!mention || busy) {
      closeContextSuggestions();
      return;
    }
    const params = new URLSearchParams({
      kind: mention.kind,
      q: mention.query,
      limit: "12"
    });
    if (mention.kind !== "repository") {
      const repositoryIDs = Array.from(new Set(selectedContexts.map((context) => context.context.repository_id)));
      if (repositoryIDs.length !== 1) {
        closeContextSuggestions();
        showContextError(`Add context from exactly one repository before choosing @${mention.kind}.`);
        return;
      }
      params.set("repository_id", String(repositoryIDs[0]));
    }
    const sequence = ++contextRequestSequence;
    try {
      const response = await fetch(`/api/contexts/suggest?${params.toString()}`, {
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        throw new Error(await responseErrorMessage(response, "Context suggestions could not be loaded."));
      }
      const result = await response.json() as { suggestions?: ContextSuggestion[] };
      if (sequence !== contextRequestSequence) {
        return;
      }
      suggestedContexts = result.suggestions ?? [];
      activeContextSuggestion = suggestedContexts.length > 0 ? 0 : -1;
      contextMention = { kind: mention.kind, start: mention.start, end: mention.end };
      showContextError(suggestedContexts.length === 0 ? `No visible ${mention.kind} contexts match.` : "");
      renderContextSuggestions();
    } catch (error: unknown) {
      if (sequence !== contextRequestSequence) {
        return;
      }
      closeContextSuggestions();
      showContextError(error instanceof Error ? error.message : "Context suggestions could not be loaded.");
    }
  };

  const addPastedContext = async (
    selector: ContextSelector | { named_context_id: string }
  ): Promise<void> => {
    if ("named_context_id" in selector) {
      activeNamedContextIDs.add(selector.named_context_id);
      await refreshEffectiveContexts();
      return;
    }
    if (selectedContexts.length >= maximumStructuredContexts) {
      showContextError(`A turn can include at most ${maximumStructuredContexts} structured contexts.`);
      return;
    }
    showContextError();
    try {
      const response = await fetch("/api/contexts/resolve", {
        method: "POST",
        headers: {
          Accept: "application/json",
          "Content-Type": "application/json"
        },
        body: JSON.stringify({ contexts: [selector], use_default_contexts: false })
      });
      if (!response.ok) {
        throw new Error(await responseErrorMessage(response, "The pasted RepoKarta URL could not be resolved."));
      }
      const result = await response.json() as { contexts?: ResolvedContext[] };
      const resolved = result.contexts?.[0];
      if (!resolved) {
        throw new Error("The pasted RepoKarta URL did not resolve to a structured context.");
      }
      const suggestion = contextSuggestionFromResolved(resolved);
      if (!explicitContexts.some((context) => contextKey(context.context) === contextKey(suggestion.context))) {
        explicitContexts.push(suggestion);
      }
      await refreshEffectiveContexts();
      closeContextSuggestions();
      debug?.add("info", "ui.context.url-resolved", {
        kind: resolved.kind,
        repository_id: resolved.repository_id,
        revision: resolved.revision,
        path: resolved.path || null,
        symbol: resolved.symbol || null,
        line: resolved.line || null
      });
    } catch (error: unknown) {
      showContextError(error instanceof Error ? error.message : "The pasted RepoKarta URL could not be resolved.");
      debug?.add("warn", "ui.context.url-resolution-failed", describeError(error));
    }
  };

  const namedField = <ElementType extends HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>(
    name: string
  ): ElementType => namedContextForm.elements.namedItem(name) as ElementType;

  const resetNamedContextEditor = (): void => {
    namedContextForm.reset();
    namedField<HTMLInputElement>("id").value = "";
    namedField<HTMLSelectElement>("category").value = "personal_task";
    namedField<HTMLSelectElement>("visibility").value = "personal";
    namedField<HTMLSelectElement>("default_scope").value = "none";
    namedContextEditorTitle.textContent = "New named context";
    namedContextEditorError.hidden = true;
    namedContextEditorError.textContent = "";
    for (const checkbox of namedContextForm.querySelectorAll<HTMLInputElement>('input[name="repository"]')) {
      checkbox.checked = false;
    }
  };

  const editNamedContext = (context: NamedContext): void => {
    resetNamedContextEditor();
    namedField<HTMLInputElement>("id").value = context.id;
    namedField<HTMLInputElement>("title").value = context.title;
    namedField<HTMLTextAreaElement>("description").value = context.description ?? "";
    namedField<HTMLSelectElement>("category").value = context.category;
    namedField<HTMLSelectElement>("visibility").value = context.visibility;
    namedField<HTMLSelectElement>("default_scope").value = context.default_scope;
    namedContextEditorTitle.textContent = `Edit ${context.title}`;
    const repositoryIDs = new Set(context.contexts.map((item) => item.repository_id));
    for (const checkbox of namedContextForm.querySelectorAll<HTMLInputElement>('input[name="repository"]')) {
      checkbox.checked = repositoryIDs.has(Number.parseInt(checkbox.value, 10));
    }
    namedField<HTMLInputElement>("title").focus();
  };

  const loadNamedContexts = async (): Promise<void> => {
    namedContextFeedback.textContent = "Loading named contexts…";
    try {
      const response = await fetch("/api/contexts/named", {
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        throw new Error(await responseErrorMessage(response, "Named contexts could not be loaded."));
      }
      const result = await response.json() as { named_contexts?: NamedContext[] };
      namedContexts = result.named_contexts ?? [];
      namedContextFeedback.textContent = namedContexts.length === 0
        ? "No named contexts yet. Create a revision-pinned reusable scope."
        : `${namedContexts.length} visible named context${namedContexts.length === 1 ? "" : "s"}.`;
      renderNamedContextList();
    } catch (error: unknown) {
      namedContexts = [];
      namedContextFeedback.textContent = error instanceof Error ? error.message : "Named contexts could not be loaded.";
      renderNamedContextList();
    }
  };

  const applyNamedContext = async (context: NamedContext): Promise<void> => {
    if (context.state !== "ready") {
      namedContextFeedback.textContent = context.issues?.[0]?.message ?? "This named context is invalid.";
      return;
    }
    const activeAsDefault = defaultContextsEnabled && context.default_scope !== "none";
    if (activeNamedContextIDs.has(context.id)) {
      activeNamedContextIDs.delete(context.id);
    } else if (activeAsDefault) {
      defaultContextsEnabled = false;
    } else if (context.default_scope !== "none" && !defaultContextsEnabled) {
      defaultContextsEnabled = true;
    } else {
      activeNamedContextIDs.add(context.id);
    }
    await refreshEffectiveContexts();
  };

  const deleteNamedContext = async (context: NamedContext): Promise<void> => {
    if (!context.editable || !window.confirm(`Delete named context "${context.title}"?`)) {
      return;
    }
    try {
      const response = await fetch(`/api/contexts/named/${encodeURIComponent(context.id)}`, {
        method: "DELETE",
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        throw new Error(await responseErrorMessage(response, "Named context could not be deleted."));
      }
      activeNamedContextIDs.delete(context.id);
      await loadNamedContexts();
      await refreshEffectiveContexts();
      resetNamedContextEditor();
    } catch (error: unknown) {
      namedContextFeedback.textContent = error instanceof Error ? error.message : "Named context could not be deleted.";
    }
  };

  function renderNamedContextList(): void {
    namedContextList!.replaceChildren();
    for (const context of namedContexts) {
      const active = activeNamedContextIDs.has(context.id) ||
        (defaultContextsEnabled && context.default_scope !== "none");
      const card = document.createElement("article");
      card.className = "named-context-card";
      card.dataset.active = String(active);
      const header = document.createElement("div");
      header.className = "named-context-card-header";
      const heading = document.createElement("h4");
      heading.textContent = context.title;
      const state = document.createElement("span");
      state.className = context.state === "ready" ? "status-ready" : "status-error";
      state.textContent = context.state;
      header.append(heading, state);
      card.append(header);
      if (context.description) {
        const description = document.createElement("p");
        description.textContent = context.description;
        card.append(description);
      }
      const meta = document.createElement("div");
      meta.className = "named-context-card-meta";
      meta.textContent = [
        context.category.replaceAll("_", " "),
        `${context.contexts.length} repositor${context.contexts.length === 1 ? "y" : "ies"}`,
        context.default_scope === "none" ? context.visibility : `${context.default_scope} default`
      ].join(" · ");
      card.append(meta);
      if (context.state !== "ready" && context.issues?.length) {
        const issue = document.createElement("p");
        issue.textContent = context.issues[0].message;
        card.append(issue);
      }
      const actions = document.createElement("div");
      actions.className = "named-context-card-actions";
      const apply = document.createElement("button");
      apply.type = "button";
      apply.textContent = active ? "Remove" : "Apply";
      apply.disabled = context.state !== "ready";
      apply.addEventListener("click", () => void applyNamedContext(context));
      const open = document.createElement("a");
      open.href = context.url;
      open.target = "_blank";
      open.rel = "noreferrer";
      open.textContent = "Open";
      const copy = document.createElement("button");
      copy.type = "button";
      copy.textContent = "Copy URL";
      copy.addEventListener("click", () => void copyContextURL(context.url, copy));
      actions.append(apply, open, copy);
      if (context.editable) {
        const edit = document.createElement("button");
        edit.type = "button";
        edit.textContent = "Edit";
        edit.addEventListener("click", () => editNamedContext(context));
        const remove = document.createElement("button");
        remove.type = "button";
        remove.textContent = "Delete";
        remove.addEventListener("click", () => void deleteNamedContext(context));
        actions.append(edit, remove);
      }
      card.append(actions);
      namedContextList!.append(card);
    }
  }

  const configureNamedContextAdministration = async (): Promise<void> => {
    try {
      const response = await fetch("/api/whoami", { headers: { Accept: "application/json" } });
      if (response.ok) {
        const result = await response.json() as { admin?: boolean };
        namedContextViewerIsAdmin = Boolean(result.admin);
      }
    } catch {
      namedContextViewerIsAdmin = false;
    }
    for (const option of namedContextForm.querySelectorAll<HTMLOptionElement>("[data-admin-context-option]")) {
      option.disabled = !namedContextViewerIsAdmin;
    }
  };

  namedContextForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const id = namedField<HTMLInputElement>("id").value.trim();
    const visibility = namedField<HTMLSelectElement>("visibility").value;
    const defaultScope = namedField<HTMLSelectElement>("default_scope").value;
    if (defaultScope === "administrator") {
      namedField<HTMLSelectElement>("visibility").value = "shared";
    } else if (defaultScope === "personal") {
      namedField<HTMLSelectElement>("visibility").value = "personal";
    }
    const selectors = Array.from(
      namedContextForm.querySelectorAll<HTMLInputElement>('input[name="repository"]:checked')
    ).map((checkbox) => ({
      kind: "repository" as const,
      repository_id: Number.parseInt(checkbox.value, 10),
      revision: checkbox.dataset.revision ?? ""
    }));
    if (selectors.length === 0) {
      namedContextEditorError.textContent = "Choose at least one indexed repository.";
      namedContextEditorError.hidden = false;
      return;
    }
    namedContextEditorError.hidden = true;
    try {
      const response = await fetch(
        id ? `/api/contexts/named/${encodeURIComponent(id)}` : "/api/contexts/named",
        {
          method: id ? "PUT" : "POST",
          headers: {
            Accept: "application/json",
            "Content-Type": "application/json"
          },
          body: JSON.stringify({
            title: namedField<HTMLInputElement>("title").value,
            description: namedField<HTMLTextAreaElement>("description").value,
            category: namedField<HTMLSelectElement>("category").value,
            visibility: defaultScope === "administrator" ? "shared" : defaultScope === "personal" ? "personal" : visibility,
            default_scope: defaultScope,
            selectors
          })
        }
      );
      if (!response.ok) {
        throw new Error(await responseErrorMessage(response, "Named context could not be saved."));
      }
      resetNamedContextEditor();
      await loadNamedContexts();
      await refreshEffectiveContexts();
    } catch (error: unknown) {
      namedContextEditorError.textContent = error instanceof Error ? error.message : "Named context could not be saved.";
      namedContextEditorError.hidden = false;
    }
  });

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

    model.replaceChildren();
    for (const modelOption of status?.models ?? []) {
      const option = document.createElement("option");
      option.value = modelOption.id;
      option.textContent = modelOption.label;
      model.append(option);
    }
    const modelIDs = (status?.models ?? []).map((candidate) => candidate.id);
    const preferredModel = preferences
      ? preferences.model
      : recommendedProviderModel(status?.id);
    model.value = modelIDs.includes(preferredModel)
      ? preferredModel
      : modelIDs[0] ?? "";

    const supportedEfforts = providerModelEfforts(status, model.value);
    effort.replaceChildren();
    const defaultEffort = document.createElement("option");
    defaultEffort.value = "";
    defaultEffort.textContent = "Provider default";
    effort.append(defaultEffort);
    for (const effortID of supportedEfforts) {
      const option = document.createElement("option");
      option.value = effortID;
      option.textContent = effortID.charAt(0).toUpperCase() + effortID.slice(1);
      effort.append(option);
    }
    const preferredEffort = preferences
      ? preferences.effort
      : recommendedProviderEffort(status?.id);
    effort.value = supportedEfforts.includes(preferredEffort)
      ? preferredEffort
      : "";

    const ready = Boolean(status?.available && status.authenticated);
    model.disabled = !ready || Boolean(conversationID);
    effort.disabled = !ready || Boolean(conversationID) || supportedEfforts.length === 0;
    const settingsLock = document.querySelector<HTMLElement>("[data-settings-lock]");
    if (settingsLock) {
      settingsLock.hidden = !conversationID;
    }
    timeout.disabled = busy || !ready;
    tokenBudget.disabled = busy || !ready || !status?.token_budget;
    tokenBudgetField.hidden = !status?.token_budget;
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

  const conversationAuthorLabel = (author: ConversationAuthor | undefined): string => {
    return author?.name?.trim() || author?.email?.trim() || author?.id || "Unknown author";
  };

  const renderStoredTranscript = (conversation: ConversationRecord): void => {
    messages.replaceChildren();
    clearEvidenceSources();
    for (const stored of conversation.messages ?? []) {
      const message = conversationMessage(stored.role);
      if (stored.role === "user") {
        const role = message.querySelector<HTMLElement>(".conversation-role");
        if (role) {
          role.textContent = conversation.author.id === conversationViewer.id
            ? "You"
            : conversationAuthorLabel(conversation.author);
        }
      }
      const content = message.querySelector<HTMLElement>(".conversation-content");
      if (content && stored.text) {
        if (stored.role === "assistant") {
          renderAssistantMarkdown(content, stored.text, debug, true);
        } else {
          content.textContent = stored.text;
        }
      }
      appendMessageContexts(message, stored.contexts ?? []);
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
    for (const button of historyScopeButtons) {
      const scope = button.dataset.conversationScope;
      button.hidden = scope === "all" && !canViewAllConversations;
      button.setAttribute("aria-pressed", String(scope === conversationScope));
    }
    authorFilterField.hidden = conversationScope !== "all" || !canViewAllConversations;
    const selectedAuthor = authorFilter.value;
    const authors = new Map<string, string>();
    for (const summary of conversationSummaries) {
      authors.set(summary.author.id, conversationAuthorLabel(summary.author));
    }
    const authorOptions = Array.from(authors, ([id, label]) => ({ id, label }))
      .sort((left, right) => left.label.localeCompare(right.label));
    authorFilter.replaceChildren(new Option("All authors", ""));
    for (const author of authorOptions) {
      authorFilter.append(new Option(author.label, author.id));
    }
    authorFilter.value = authorOptions.some((author) => author.id === selectedAuthor)
      ? selectedAuthor
      : "";

    history.replaceChildren();
    for (const summary of conversationSummaries) {
      const item = document.createElement("li");
      item.className = "conversation-history-item";
      item.dataset.conversationId = summary.id;
      item.dataset.authorId = summary.author.id;
      item.dataset.searchText = `${summary.title} ${summary.provider} ${conversationAuthorLabel(summary.author)} ${summary.author.email ?? ""}`.toLocaleLowerCase();
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
      const details = document.createElement("span");
      details.className = "conversation-history-details";
      const author = document.createElement("span");
      author.className = "conversation-history-author";
      author.textContent = summary.author.id === conversationViewer.id
        ? "You"
        : conversationAuthorLabel(summary.author);
      const metadata = document.createElement("span");
      metadata.className = "conversation-history-meta";
      const updated = new Date(summary.updated_at);
      const updatedLabel = Number.isNaN(updated.valueOf())
        ? "Saved"
        : new Intl.DateTimeFormat(undefined, { dateStyle: "medium" }).format(updated);
      metadata.textContent = `${summary.provider} · ${summary.message_count} messages · ${updatedLabel}`;
      details.append(author, metadata);
      open.append(title, details);
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
    const authorID = conversationScope === "all" ? authorFilter.value : "";
    let visibleConversations = 0;
    for (const item of history.querySelectorAll<HTMLElement>(".conversation-history-item")) {
      const queryMismatch = Boolean(query) && !(item.dataset.searchText ?? "").includes(query);
      const authorMismatch = Boolean(authorID) && item.dataset.authorId !== authorID;
      const filtered = queryMismatch || authorMismatch;
      item.dataset.filtered = String(filtered);
      if (!filtered) {
        visibleConversations++;
      }
    }
    historyEmpty.hidden = visibleConversations > 0;
    historyEmpty.textContent = conversationSummaries.length === 0
      ? conversationScope === "own" ? "You have no saved chats yet." : "No saved chats yet."
      : visibleConversations === 0
        ? "No conversations match the selected filters."
        : "";
    syncConversationChrome();
  };

  const refreshConversationHistory = async (): Promise<void> => {
    try {
      const response = await fetch(`/api/conversations?scope=${conversationScope}`, {
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        throw new Error(await response.text() || `Saved chats failed (${response.status})`);
      }
      const result = await response.json() as ConversationHistoryResponse;
      conversationViewer = result.viewer;
      canViewAllConversations = !singleScope && result.can_view_all;
      conversationScope = canViewAllConversations && result.scope === "all" ? "all" : "own";
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
      const lastContextMessage = [...(stored.messages ?? [])].reverse().find(
        (message) => message.role === "user" && (message.contexts?.length ?? 0) > 0
      );
      explicitContexts = [];
      activeNamedContextIDs.clear();
      defaultContextsEnabled = true;
      for (const context of lastContextMessage?.contexts ?? []) {
        const suggestion = contextSuggestionFromResolved(context);
        const sources = context.sources ?? [];
        if (sources.length === 0 || sources.some((source) => source.kind === "explicit")) {
          explicitContexts.push(suggestion);
        }
        for (const source of sources) {
          if (source.kind === "named" && source.id) {
            activeNamedContextIDs.add(source.id);
          }
        }
      }
      void refreshEffectiveContexts();
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
    explicitContexts = [];
    selectedContexts = [];
    activeNamedContextIDs.clear();
    defaultContextsEnabled = true;
    attachmentFeedback = "";
    runtime.textContent = "Ready";
    headerStatus.textContent = "Ready for a grounded question";
    runtime.classList.remove("conversation-telemetry-active");
    clearEvidenceSources();
    renderContextUsage();
    renderTokenUsage();
    renderAttachmentTray();
    renderContextTray();
    void refreshEffectiveContexts();
    closeContextSuggestions();
    showContextError();
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
  void configureNamedContextAdministration();
  const requestedParameters = new URL(window.location.href).searchParams;
  const requestedNamedContext = requestedParameters.get("context")?.trim();
  const requestedContextURL = requestedParameters.get("context_url")?.trim();
  const requestedPrompt = requestedParameters.get("prompt")?.trim() || "";
  const requestedMode = normaliseContextMode(requestedParameters.get("mode"));
  const requestedAutostart = requestedParameters.get("autostart") === "true";
  const contextsReady = loadNamedContexts();

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
      void Promise.all([refreshConversationHistory(), contextsReady]).then(async () => {
        let requestedConversation = requestedParameters.get("conversation")?.trim() || "";
        if (!requestedConversation && requestedParameters.get("reuse") === "current") {
          try {
            requestedConversation = window.localStorage.getItem(activeConversationStorageKey)?.trim() || "";
          } catch {
            requestedConversation = "";
          }
        }
        if (requestedConversation) {
          await openConversation(requestedConversation);
        }
        if (requestedNamedContext) {
          activeNamedContextIDs.add(requestedNamedContext);
          await refreshEffectiveContexts();
        }
        if (requestedContextURL) {
          const parsed = parseRepoKartaContextURL(requestedContextURL, window.location.href);
          if (parsed) {
            await addPastedContext(parsed);
          } else {
            showContextError("The requested context URL is not a supported RepoKarta context.");
          }
        } else if (!requestedNamedContext && !requestedConversation) {
          await refreshEffectiveContexts();
        }
        if (requestedPrompt && !requestedConversation) {
          input.value = requestedPrompt;
          headerStatus.textContent = `${requestedMode.charAt(0).toUpperCase()}${requestedMode.slice(1)} context ready`;
          input.focus();
          if (requestedAutostart && provider.value && !busy) {
            form.requestSubmit();
          }
        }
      });
    });

  // Back and Forward move between saved conversations.
  window.addEventListener("popstate", (event) => {
    const state = event.state as { conversation?: string } | null;
    if (!state || typeof state.conversation !== "string") {
      return;
    }
    if (state.conversation && state.conversation !== conversationID) {
      void openConversation(state.conversation);
    } else if (!state.conversation && conversationID) {
      resetConversation();
    }
  });

  provider.addEventListener("change", configureProvider);
  model.addEventListener("change", configureProvider);
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
    syncConversationChrome();
    debug?.add("info", "ui.effort.changed", {
      provider: provider.value,
      effort: effort.value || "provider-default"
    });
  });
  historyFilter.addEventListener("input", renderConversationHistory);
  authorFilter.addEventListener("change", renderConversationHistory);
  for (const button of historyScopeButtons) {
    button.addEventListener("click", () => {
      const requested = button.dataset.conversationScope === "all" ? "all" : "own";
      if (requested === conversationScope || (requested === "all" && !canViewAllConversations)) {
        return;
      }
      conversationScope = requested;
      authorFilter.value = "";
      void refreshConversationHistory();
    });
  }
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
      closeContextSuggestions();
      showContextError();
      setSessionPanelOpen(false);
      setInspectorOpen(false);
      settings.open = false;
      return;
    }
    // macOS is a first-class target, so accept the platform modifier too.
    if ((event.ctrlKey || event.metaKey) && event.key.toLocaleLowerCase() === "n" && !busy) {
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
    if (!contextSuggestions.hidden && suggestedContexts.length > 0) {
      if (event.key === "ArrowDown" || event.key === "ArrowUp") {
        event.preventDefault();
        const direction = event.key === "ArrowDown" ? 1 : -1;
        activeContextSuggestion = (
          activeContextSuggestion + direction + suggestedContexts.length
        ) % suggestedContexts.length;
        renderContextSuggestions();
        return;
      }
      if (event.key === "Enter" && !event.shiftKey) {
        event.preventDefault();
        selectContextSuggestion(Math.max(0, activeContextSuggestion));
        return;
      }
      if (event.key === "Escape") {
        event.preventDefault();
        closeContextSuggestions();
        return;
      }
    }
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
  let contextSuggestionTimer = 0;
  input.addEventListener("input", () => {
    window.clearTimeout(contextSuggestionTimer);
    contextSuggestionTimer = window.setTimeout(() => void updateContextSuggestions(), 120);
  });
  input.addEventListener("click", () => void updateContextSuggestions());
  contextAdd.addEventListener("click", () => {
    const prefix = selectedContexts.length > 0 ? "@file:" : "@repository:";
    const caret = input.selectionStart ?? input.value.length;
    const before = input.value.slice(0, caret);
    const after = input.value.slice(caret);
    const leadingSpace = before && !/\s$/.test(before) ? " " : "";
    input.value = before + leadingSpace + prefix + after;
    const nextCaret = before.length + leadingSpace.length + prefix.length;
    input.setSelectionRange(nextCaret, nextCaret);
    input.focus();
    void updateContextSuggestions();
  });
  namedContextButton.addEventListener("click", () => {
    resetNamedContextEditor();
    void loadNamedContexts();
    namedContextDialog.showModal();
  });
  namedContextClose.addEventListener("click", () => namedContextDialog.close());
  namedContextCancel.addEventListener("click", resetNamedContextEditor);
  namedContextNew.addEventListener("click", resetNamedContextEditor);
  namedContextDialog.addEventListener("click", (event) => {
    if (event.target === namedContextDialog) {
      namedContextDialog.close();
    }
  });
  input.addEventListener("paste", (event) => {
    const pastedContext = parseRepoKartaContextURL(
      event.clipboardData?.getData("text/plain") ?? "",
      window.location.href
    );
    if (pastedContext) {
      event.preventDefault();
      void addPastedContext(pastedContext);
    }
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
    const requestContexts = explicitContexts.map((context) => context.context);
    const requestNamedContextIDs = Array.from(activeNamedContextIDs);
    const requestContextViews = selectedContexts.slice();
    const requestStarted = performance.now();
    let deltaEvents = 0;
    let answerCharacters = 0;
    let streamCompleted = false;
    busy = true;
    providerPreferences.set(provider.value, {
      model: model.value,
      effort: effort.value
    });
    persistRunSettings();
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
    appendMessageContexts(userMessage, requestContextViews);
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
          contexts: requestContexts,
          named_context_ids: requestNamedContextIDs,
          use_default_contexts: defaultContextsEnabled,
          timeout_seconds: Number.parseInt(timeout.value, 10),
          token_budget: providerStatus?.token_budget ? Number.parseInt(tokenBudget.value, 10) : 0
        })
      });
      debug?.add(response.ok ? "info" : "warn", "chat.response.received", {
        status: response.status,
        status_text: response.statusText,
        content_type: response.headers.get("content-type"),
        duration_ms: Math.round(performance.now() - requestStarted)
      });
      if (!response.ok) {
        throw new Error(await responseErrorMessage(response, `Conversation failed (${response.status})`));
      }
      if (!response.body) {
        throw new Error("Conversation failed because the streaming response was empty.");
      }
      closeContextSuggestions();
      showContextError();
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
      renderMetrics = timelineRenderer?.finish();
      debug?.add("error", "chat.request.failed", {
        endpoint: "/api/chat",
        online: navigator.onLine,
        duration_ms: Math.round(performance.now() - requestStarted),
        rendering: renderMetrics,
        ...describeError(error)
      });
      if (debug && (error instanceof TypeError || (error instanceof Error && /fetch|network/i.test(error.message)))) {
        await probeServerHealth(debug);
      }
      const notice = document.createElement("p");
      notice.className = "conversation-turn-status conversation-turn-status-error";
      notice.textContent = conversationErrorMessage(error);
      assistant.append(notice);
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

type MapEvidence = {
  repository_id: number;
  repository: string;
  revision: string;
  path: string;
  line: number;
  label: string;
  url: string;
};

type MapNode = {
  id: string;
  kind: string;
  label: string;
  subtitle?: string;
  layer: string;
  repository_id?: number;
  repository?: string;
  path?: string;
  evidence: MapEvidence[];
};

type MapEdge = {
  id: string;
  source: string;
  target: string;
  kind: string;
  label: string;
  confidence?: "high" | "low";
  evidence: MapEvidence[];
};

type MapSnapshot = {
  id: string;
  generated_at: string;
  repositories: Array<{ id: number; name: string; revision: string; file_count: number }>;
  languages: Array<{ name: string; files: number; percentage: number }>;
  manifests: Array<{ kind: string; path: string }>;
  nodes: MapNode[];
  edges: MapEdge[];
  file_count: number;
  truncated: boolean;
  scope: {
    kind: "collection" | "repository";
    complete: boolean;
    total_repositories: number;
    analyzed_repositories: number;
    omitted_repositories: number;
    repository_limit?: number;
    requested_repository_id?: number;
  };
};

function enableRepositoryMaps(debug?: DebugLogger): void {
  const workspace = document.querySelector<HTMLElement>("[data-map-workspace]");
  const canvas = document.querySelector<HTMLElement>("[data-map-canvas]");
  const loading = document.querySelector<HTMLElement>("[data-map-loading]");
  const repository = document.querySelector<HTMLSelectElement>("[data-map-repository]");
  const repositoryPicker = document.querySelector<HTMLElement>("[data-map-repository-picker]");
  const repositoryTrigger = document.querySelector<HTMLButtonElement>("[data-map-repository-trigger]");
  const repositoryCurrent = document.querySelector<HTMLElement>("[data-map-repository-current]");
  const repositoryMeta = document.querySelector<HTMLElement>("[data-map-repository-meta]");
  const repositoryBackdrop = document.querySelector<HTMLButtonElement>("[data-map-repository-backdrop]");
  const repositoryPopover = document.querySelector<HTMLElement>("[data-map-repository-popover]");
  const repositorySearch = document.querySelector<HTMLInputElement>("[data-map-repository-search]");
  const repositoryOptions = Array.from(
    document.querySelectorAll<HTMLButtonElement>("[data-map-repository-option]")
  );
  const search = document.querySelector<HTMLInputElement>("[data-map-search]");
  const searchResults = document.querySelector<HTMLElement>("[data-map-search-results]");
  const refresh = document.querySelector<HTMLButtonElement>("[data-map-refresh]");
  const exportLink = document.querySelector<HTMLAnchorElement>("[data-map-export]");
  const status = document.querySelector<HTMLElement>("[data-map-status]");
  const summaryHeading = document.querySelector<HTMLElement>("#map-summary-heading");
  const snapshotID = document.querySelector<HTMLElement>("[data-map-snapshot-id]");
  const repositoryCount = document.querySelector<HTMLElement>("[data-map-repositories]");
  const scopeNote = document.querySelector<HTMLElement>("[data-map-scope]");
  const fileCount = document.querySelector<HTMLElement>("[data-map-files]");
  const nodeCount = document.querySelector<HTMLElement>("[data-map-nodes]");
  const edgeCount = document.querySelector<HTMLElement>("[data-map-edges]");
  const languages = document.querySelector<HTMLElement>("[data-map-languages]");
  const focus = document.querySelector<HTMLButtonElement>("[data-map-focus]");
  const reset = document.querySelector<HTMLButtonElement>("[data-map-reset]");
  const inspector = document.querySelector<HTMLElement>(".map-inspector");
  const inspectorEmpty = document.querySelector<HTMLElement>("[data-map-inspector-empty]");
  const inspectorContent = document.querySelector<HTMLElement>("[data-map-inspector-content]");
  const inspectorKind = document.querySelector<HTMLElement>("[data-map-inspector-kind]");
  const inspectorTitle = document.querySelector<HTMLElement>("[data-map-inspector-title]");
  const inspectorSubtitle = document.querySelector<HTMLElement>("[data-map-inspector-subtitle]");
  const inspectorLayer = document.querySelector<HTMLElement>("[data-map-inspector-layer]");
  const inspectorRepository = document.querySelector<HTMLElement>("[data-map-inspector-repository]");
  const inspectorPath = document.querySelector<HTMLElement>("[data-map-inspector-path]");
  const inspectorEvidence = document.querySelector<HTMLOListElement>("[data-map-evidence]");
  const inspectorEvidenceCount = document.querySelector<HTMLElement>("[data-map-evidence-count]");
  const inspectorTotal = document.querySelector<HTMLElement>("[data-map-evidence-total]");
  const nodeList = document.querySelector<HTMLElement>("[data-map-node-list]");
  const nodeListItems = document.querySelector<HTMLOListElement>("[data-map-node-list-items]");
  const listToggle = document.querySelector<HTMLButtonElement>("[data-map-list-toggle]");
  const inspectorDrawer = enableEvidenceDrawer({
    panel: "#map-inspector",
    toggle: "[data-map-inspector-toggle]",
    close: "[data-map-inspector-close]",
    scrim: "[data-map-inspector-scrim]",
    dockedFrom: "(min-width: 1101px)"
  });
  const viewButtons = Array.from(document.querySelectorAll<HTMLButtonElement>("[data-map-view]"));
  if (
    !workspace || !canvas || !loading || !repository || !repositoryPicker || !repositoryTrigger ||
    !repositoryCurrent || !repositoryMeta || !repositoryBackdrop || !repositoryPopover || !repositorySearch ||
    repositoryOptions.length === 0 || !search || !searchResults || !refresh || !exportLink || !status ||
    !summaryHeading || !snapshotID || !repositoryCount || !scopeNote || !fileCount || !nodeCount || !edgeCount ||
    !languages || !focus || !reset || !inspector || !inspectorEmpty || !inspectorContent ||
    !inspectorKind || !inspectorTitle || !inspectorSubtitle || !inspectorLayer ||
    !inspectorRepository || !inspectorPath || !inspectorEvidence || !inspectorEvidenceCount ||
    viewButtons.length === 0
  ) {
    return;
  }

  type MapCore = import("cytoscape").Core;
  type MapEvent = import("cytoscape").EventObject;
  type MapSingular = import("cytoscape").NodeSingular | import("cytoscape").EdgeSingular;

  let graph: MapCore | undefined;
  let selected: MapSingular | undefined;
  let activeView = "all";
  let loadRevision = 0;
  let sharedDependencyIDs = new Set<string>();
  let currentRepositoryCount = 0;
  const initialParameters = new URL(window.location.href).searchParams;
  let requestedMapFocus = initialParameters.get("focus")?.trim() || "";

  const selectedRepositoryQuery = (): string => repository.value ? `repository=${encodeURIComponent(repository.value)}` : "";

  const visibleRepositoryOptions = (): HTMLButtonElement[] =>
    repositoryOptions.filter((option) => !option.hidden);

  const syncRepositoryPicker = (): void => {
    const selectedOption = repositoryOptions.find((option) => option.dataset.value === repository.value)
      ?? repositoryOptions[0];
    repositoryCurrent.textContent = selectedOption.dataset.label || selectedOption.textContent?.trim() || "Repository";
    repositoryMeta.textContent = selectedOption.dataset.meta || "Single repository";
    for (const option of repositoryOptions) {
      option.setAttribute("aria-selected", String(option === selectedOption));
    }
  };

  const filterRepositoryOptions = (): void => {
    const query = repositorySearch.value.trim().toLocaleLowerCase();
    for (const option of repositoryOptions) {
      const label = option.dataset.label?.toLocaleLowerCase() || "";
      const isFleet = option.dataset.value === "";
      option.hidden = query !== "" && (isFleet || !label.includes(query));
    }
  };

  const closeRepositoryPicker = (restoreFocus = false): void => {
    repositoryPopover.hidden = true;
    repositoryPicker.dataset.open = "false";
    repositoryTrigger.setAttribute("aria-expanded", "false");
    repositorySearch.value = "";
    filterRepositoryOptions();
    if (restoreFocus) {
      repositoryTrigger.focus();
    }
  };

  const openRepositoryPicker = (focusSelected = false): void => {
    repositoryPopover.hidden = false;
    repositoryPicker.dataset.open = "true";
    repositoryTrigger.setAttribute("aria-expanded", "true");
    if (focusSelected) {
      (repositoryOptions.find((option) => option.getAttribute("aria-selected") === "true")
        ?? visibleRepositoryOptions()[0])?.focus();
      return;
    }
    repositorySearch.focus();
  };

  const focusRepositoryOption = (current: HTMLButtonElement, offset: number): void => {
    const options = visibleRepositoryOptions();
    const currentIndex = options.indexOf(current);
    options[(currentIndex + offset + options.length) % options.length]?.focus();
  };

  const chooseRepository = (option: HTMLButtonElement): void => {
    repository.value = option.dataset.value || "";
    syncRepositoryPicker();
    repository.dispatchEvent(new Event("change", { bubbles: true }));
    closeRepositoryPicker(true);
  };

  const updateExportLink = (): void => {
    const query = selectedRepositoryQuery();
    exportLink.href = `/api/maps/export${query ? `?${query}` : ""}`;
  };

  const clearInspector = (): void => {
    selected = undefined;
    focus.disabled = true;
    inspectorEmpty.hidden = false;
    inspectorContent.hidden = true;
    if (inspectorDrawer) {
      inspectorDrawer.open(false);
    } else {
      inspector.dataset.open = "false";
    }
    if (inspectorTotal) {
      inspectorTotal.textContent = "0";
    }
  };

  const showEvidence = (evidence: MapEvidence[]): void => {
    inspectorEvidence.replaceChildren();
    inspectorEvidenceCount.textContent = String(evidence.length);
    if (inspectorTotal) {
      inspectorTotal.textContent = String(evidence.length);
    }
    for (const item of evidence) {
      const listItem = document.createElement("li");
      const link = document.createElement("a");
      link.href = item.url;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      const label = document.createElement("strong");
      label.textContent = `${item.repository} · ${item.path}:${item.line}`;
      const detail = document.createElement("span");
      detail.textContent = `${item.revision.slice(0, 8)} · ${item.label}`;
      link.append(label, detail);
      listItem.append(link);
      inspectorEvidence.append(listItem);
    }
  };

  const inspectElement = (element: MapSingular): void => {
    selected = element;
    focus.disabled = false;
    inspectorEmpty.hidden = true;
    inspectorContent.hidden = false;
    // Selecting a fact reveals the rail on narrow layouts too, so evidence is
    // never produced into a panel the reader cannot see.
    if (inspectorDrawer) {
      inspectorDrawer.open(true);
    } else {
      inspector.dataset.open = "true";
    }
    if (element.isNode()) {
      const fact = element.data("fact") as MapNode;
      inspectorKind.textContent = fact.kind;
      inspectorTitle.textContent = fact.label;
      inspectorSubtitle.textContent = fact.subtitle || "Deterministic structural fact";
      inspectorLayer.textContent = fact.layer;
      inspectorRepository.textContent = fact.repository || "Shared across repositories";
      inspectorPath.textContent = fact.path || "—";
      showEvidence(fact.evidence);
    } else {
      const fact = element.data("fact") as MapEdge;
      inspectorKind.textContent = "relationship";
      inspectorTitle.textContent = `${element.source().data("label")} → ${element.target().data("label")}`;
      inspectorSubtitle.textContent = fact.confidence === "low"
        ? `${fact.label} · low confidence`
        : fact.label;
      inspectorLayer.textContent = fact.confidence
        ? `${fact.kind} · ${fact.confidence} confidence`
        : fact.kind;
      inspectorRepository.textContent = fact.evidence[0]?.repository || "—";
      inspectorPath.textContent = fact.evidence[0]?.path || "—";
      showEvidence(fact.evidence);
    }
  };

  const focusMapTarget = (): void => {
    const query = requestedMapFocus.trim().toLocaleLowerCase();
    if (!graph || !query) {
      return;
    }
    const ranked = graph.nodes().map((node) => {
      const fact = node.data("fact") as MapNode;
      const id = fact.id.toLocaleLowerCase();
      const path = (fact.path || "").toLocaleLowerCase();
      const label = fact.label.toLocaleLowerCase();
      const evidencePath = fact.evidence.some(
        (item) => item.path.toLocaleLowerCase() === query
      );
      let score = 0;
      if (id === query) score = 6;
      else if (path === query) score = 5;
      else if (label === query) score = 4;
      else if (evidencePath) score = 3;
      else if (path.includes(query) || query.includes(path) && path !== "") score = 2;
      else if (label.includes(query)) score = 1;
      return { node, score };
    }).filter((candidate) => candidate.score > 0)
      .sort((left, right) => right.score - left.score);
    const target = ranked[0]?.node;
    requestedMapFocus = "";
    if (!target) {
      status.textContent = "Map ready · no structural element matched the requested result";
      return;
    }
    target.removeClass("map-hidden map-search-muted");
    graph.elements().unselect();
    target.select();
    inspectElement(target);
    graph.animate({
      center: { eles: target },
      zoom: Math.max(graph.zoom(), 1.05),
      duration: 180
    });
  };

  const renderSearchResults = (): void => {
    const query = search.value.trim().toLocaleLowerCase();
    searchResults.replaceChildren();
    if (!graph || query.length < 2) {
      searchResults.hidden = true;
      return;
    }
    const matches = graph.nodes()
      .not(".map-hidden")
      .filter((node) =>
        String(node.data("label")).toLocaleLowerCase().includes(query) ||
        String(node.data("subtitle") || "").toLocaleLowerCase().includes(query)
      )
      .slice(0, 8);
    if (matches.length === 0) {
      const empty = document.createElement("p");
      empty.className = "map-search-empty";
      empty.textContent = "No visible nodes match this view.";
      searchResults.append(empty);
    } else {
      matches.forEach((node) => {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "map-search-result";
        const label = document.createElement("strong");
        label.textContent = String(node.data("label"));
        const kind = document.createElement("span");
        kind.textContent = String(node.data("kind"));
        button.append(label, kind);
        button.addEventListener("click", () => {
          graph?.elements().unselect();
          node.select();
          inspectElement(node);
          graph?.animate({
            center: { eles: node },
            zoom: Math.max(graph.zoom(), 1.05),
            duration: 180
          });
          searchResults.hidden = true;
        });
        searchResults.append(button);
      });
    }
    searchResults.hidden = false;
  };

  const applyFilters = (): void => {
    if (!graph) {
      return;
    }
    const allowedKinds: Record<string, Set<string>> = {
      packages: new Set(["repository", "package", "manifest", "entrypoint", "component"]),
      dependencies: new Set(["repository", "package", "manifest", "dependency"]),
      routes: new Set(["repository", "package", "entrypoint", "component", "route"])
    };
    const allowed = allowedKinds[activeView];
    const query = search.value.trim().toLocaleLowerCase();
    graph.batch(() => {
      graph?.nodes().forEach((node) => {
        const kind = String(node.data("kind"));
        const kindVisible = activeView === "all"
          ? kind === "repository" ||
            (kind === "dependency" && sharedDependencyIDs.has(node.id())) ||
            (currentRepositoryCount === 1 && ["entrypoint", "component", "route", "manifest"].includes(kind))
          : !allowed || allowed.has(kind);
        node.toggleClass("map-hidden", !kindVisible);
        const matches = !query ||
          String(node.data("label")).toLocaleLowerCase().includes(query) ||
          String(node.data("subtitle") || "").toLocaleLowerCase().includes(query);
        node.toggleClass("map-search-muted", kindVisible && !matches);
        node.toggleClass("map-search-match", kindVisible && Boolean(query) && matches);
      });
      graph?.edges().forEach((edge) => {
        const endpointsVisible =
          !edge.source().hasClass("map-hidden") &&
          !edge.target().hasClass("map-hidden") &&
          (activeView === "all"
            ? Boolean(edge.data("system")) ||
              edge.data("kind") === "service_call" ||
              currentRepositoryCount === 1
            : !edge.data("system"));
        edge.toggleClass("map-hidden", !endpointsVisible);
        edge.toggleClass(
          "map-search-muted",
          endpointsVisible && Boolean(query) &&
            edge.source().hasClass("map-search-muted") &&
            edge.target().hasClass("map-search-muted")
        );
      });
    });
    renderSearchResults();
    renderNodeList();
  };

  /**
   * DOM mirror of the visible graph nodes. Cytoscape draws to a canvas, so the
   * graph itself carries no keyboard focus and no accessible names; this list
   * gives the same facts and the same inspector to keyboard and screen-reader
   * users without touching the renderer.
   */
  const renderNodeList = (): void => {
    if (!nodeList || !nodeListItems || nodeList.hidden || !graph) {
      return;
    }
    nodeListItems.replaceChildren();
    const visible = graph.nodes().filter((node) => !node.hasClass("map-hidden"));
    visible.forEach((node) => {
      const fact = node.data("fact") as MapNode | undefined;
      const item = document.createElement("li");
      const button = document.createElement("button");
      button.type = "button";
      button.className = "map-node-list-item";
      const label = document.createElement("strong");
      label.textContent = String(node.data("label"));
      const meta = document.createElement("span");
      meta.textContent = [fact?.kind, fact?.repository, fact?.path].filter(Boolean).join(" · ") ||
        "Deterministic structural fact";
      button.append(label, meta);
      button.addEventListener("click", () => {
        graph?.center(node);
        inspectElement(node);
      });
      item.append(button);
      nodeListItems.append(item);
    });
    if (visible.length === 0) {
      const empty = document.createElement("li");
      empty.className = "map-node-list-empty";
      empty.textContent = "No nodes match the current scope, view, and filter.";
      nodeListItems.append(empty);
    }
  };

  const fitGraph = (): void => {
    if (!graph) {
      return;
    }
    graph.elements().removeClass("map-focus-hidden");
    applyFilters();
    graph.fit(graph.elements().not(".map-hidden"), 72);
    graph.center();
  };

  const layoutGraph = (): void => {
    if (!graph) {
      return;
    }
    const visible = graph.elements().not(".map-hidden");
    if (visible.nodes().length === 0) {
      return;
    }
    visible.layout({
      name: activeView === "all" ? "circle" : "cose",
      animate: false,
      padding: 90,
      fit: true,
      nodeDimensionsIncludeLabels: true,
      nodeRepulsion: 7200,
      idealEdgeLength: activeView === "all" ? 120 : 82,
      edgeElasticity: 90,
      gravity: 0.3,
      numIter: 900,
      randomize: true
    }).run();
    window.setTimeout(() => fitGraph(), 20);
  };

  const renderSummary = (value: MapSnapshot): void => {
    summaryHeading.textContent = value.scope.complete && !value.truncated
      ? "Complete snapshot"
      : "Bounded snapshot";
    snapshotID.textContent = value.id.slice(0, 8);
    snapshotID.title = `Generated ${new Date(value.generated_at).toLocaleString()}`;
    repositoryCount.textContent = value.scope.kind === "collection"
      ? `${value.scope.analyzed_repositories} / ${value.scope.total_repositories}`
      : String(value.scope.analyzed_repositories);
    scopeNote.hidden = value.scope.complete;
    scopeNote.textContent = value.scope.complete
      ? ""
      : `Partial collection: analyzed ${value.scope.analyzed_repositories} of ${value.scope.total_repositories} repositories; ${value.scope.omitted_repositories} omitted. Choose a repository for a complete map.`;
    fileCount.textContent = value.file_count.toLocaleString();
    nodeCount.textContent = value.nodes.length.toLocaleString();
    edgeCount.textContent = value.edges.length.toLocaleString();
    languages.replaceChildren();
    for (const language of value.languages.slice(0, 8)) {
      const item = document.createElement("span");
      item.textContent = `${language.name} ${language.percentage.toFixed(language.percentage >= 10 ? 0 : 1)}%`;
      item.title = `${language.files.toLocaleString()} files`;
      languages.append(item);
    }
  };

  const renderGraph = async (value: MapSnapshot): Promise<void> => {
    const { default: cytoscape } = await import("cytoscape");
    const nodeByID = new Map(value.nodes.map((node) => [node.id, node]));
    currentRepositoryCount = value.repositories.length;
    const initialColumns = Math.max(1, Math.ceil(Math.sqrt(value.nodes.length)));
    const dependencyRepositories = new Map<string, Map<number, MapEvidence[]>>();
    for (const edge of value.edges) {
      const target = nodeByID.get(edge.target);
      const source = nodeByID.get(edge.source);
      if (target?.kind !== "dependency" || !source?.repository_id) {
        continue;
      }
      let repositories = dependencyRepositories.get(target.id);
      if (!repositories) {
        repositories = new Map();
        dependencyRepositories.set(target.id, repositories);
      }
      repositories.set(
        source.repository_id,
        [...(repositories.get(source.repository_id) ?? []), ...edge.evidence]
      );
    }
    const sharedDependencies = Array.from(dependencyRepositories.entries())
      .filter(([, repositories]) => repositories.size >= (value.repositories.length === 1 ? 1 : 2))
      .sort((left, right) => right[1].size - left[1].size || left[0].localeCompare(right[0]))
      .slice(0, value.repositories.length === 1 ? 18 : 12);
    sharedDependencyIDs = new Set(sharedDependencies.map(([dependencyID]) => dependencyID));
    const systemEdges = sharedDependencies.flatMap(([dependencyID, repositories]) =>
      Array.from(repositories.entries()).map(([repositoryID, evidence]) => ({
        data: {
          id: `system:repository:${repositoryID}:${dependencyID}`,
          source: `repository:${repositoryID}`,
          target: dependencyID,
          label: "shares",
          kind: "dependency",
          system: true,
          fact: {
            id: `system:repository:${repositoryID}:${dependencyID}`,
            source: `repository:${repositoryID}`,
            target: dependencyID,
            kind: "dependency",
            label: "uses shared dependency",
            evidence
          } satisfies MapEdge
        },
        classes: "system-edge"
      }))
    );
    graph?.destroy();
    graph = cytoscape({
      container: canvas,
      elements: [
        ...value.nodes.map((node, index) => ({
          data: {
            id: node.id,
            label: node.label,
            subtitle: node.subtitle || "",
            kind: node.kind,
            layer: node.layer,
            fact: node
          },
          position: {
            x: (index % initialColumns) * 96,
            y: Math.floor(index / initialColumns) * 96
          },
          classes: node.kind
        })),
        ...value.edges.map((edge) => ({
          data: {
            id: edge.id,
            source: edge.source,
            target: edge.target,
            label: edge.label,
            kind: edge.kind,
            fact: edge
          },
          classes: edge.confidence === "low" ? "low-confidence" : ""
        })),
        ...systemEdges
      ],
      layout: {
        name: "preset"
      },
      minZoom: 0.12,
      maxZoom: 2.4,
      style: [
        {
          selector: "node",
          style: {
            "background-color": "#111827",
            "border-color": "#64748b",
            "border-width": 1.25,
            color: "#cbd5e1",
            label: "data(label)",
            "font-family": "Inter, ui-sans-serif, system-ui, sans-serif",
            "font-size": 10,
            "font-weight": 600,
            "text-margin-y": 4,
            "text-valign": "bottom",
            "text-wrap": "ellipsis",
            "text-max-width": "116px",
            width: 42,
            height: 42,
            "overlay-opacity": 0,
            "transition-property": "opacity, border-width, border-color, background-color",
            "transition-duration": 120
          }
        },
        {
          selector: "node.repository",
          style: {
            "background-color": "#064e3b",
            "border-color": "#34d399",
            "border-width": 2,
            shape: "round-rectangle",
            width: 54,
            height: 54,
            color: "#d1fae5"
          }
        },
        {
          selector: "node.package",
          style: {
            "background-color": "#312e81",
            "border-color": "#a78bfa",
            shape: "round-rectangle",
            color: "#ede9fe"
          }
        },
        {
          selector: "node.entrypoint",
          style: {
            "background-color": "#172554",
            "border-color": "#60a5fa",
            shape: "diamond",
            color: "#dbeafe"
          }
        },
        {
          selector: "node.component",
          style: {
            "background-color": "#164e63",
            "border-color": "#22d3ee",
            shape: "round-rectangle",
            color: "#cffafe"
          }
        },
        {
          selector: "node.route",
          style: {
            "background-color": "#451a03",
            "border-color": "#fbbf24",
            shape: "tag",
            color: "#fef3c7"
          }
        },
        {
          selector: "node.dependency",
          style: {
            "background-color": "#1e293b",
            "border-color": "#64748b",
            width: 32,
            height: 32,
            "font-size": 8,
            color: "#94a3b8"
          }
        },
        {
          selector: "node.manifest",
          style: {
            "background-color": "#082f49",
            "border-color": "#38bdf8",
            shape: "round-tag",
            color: "#bae6fd"
          }
        },
        {
          selector: "edge",
          style: {
            width: 1,
            "line-color": "#475569",
            "target-arrow-color": "#64748b",
            "target-arrow-shape": "triangle",
            "arrow-scale": 0.72,
            "curve-style": "bezier",
            opacity: 0.58,
            "overlay-opacity": 0
          }
        },
        {
          selector: "edge[kind = 'import']",
          style: {
            "line-color": "#8b5cf6",
            "target-arrow-color": "#a78bfa"
          }
        },
        {
          selector: "edge[kind = 'route']",
          style: {
            "line-color": "#d97706",
            "target-arrow-color": "#fbbf24"
          }
        },
        {
          selector: "edge[kind = 'service_call']",
          style: {
            width: 2,
            "line-color": "#ec4899",
            "target-arrow-color": "#f472b6",
            "line-style": "dashed"
          }
        },
        {
          selector: "edge.low-confidence",
          style: {
            opacity: 0.24,
            "line-style": "dotted"
          }
        },
        {
          selector: "edge.system-edge",
          style: {
            width: 1.4,
            "line-color": "#10b981",
            "target-arrow-color": "#34d399",
            opacity: 0.28
          }
        },
        {
          selector: ":selected",
          style: {
            "border-width": 3,
            "border-color": "#f8fafc",
            "line-color": "#34d399",
            "target-arrow-color": "#34d399",
            opacity: 1
          }
        },
        {
          selector: ".map-hidden, .map-focus-hidden",
          style: { display: "none" }
        },
        {
          selector: ".map-search-muted",
          style: { opacity: 0.12 }
        },
        {
          selector: ".map-search-match",
          style: {
            "border-width": 3,
            "border-color": "#34d399",
            opacity: 1
          }
        }
      ]
    });
    graph.on("tap", "node, edge", (event: MapEvent) => inspectElement(event.target));
    graph.on("tap", (event: MapEvent) => {
      if (event.target === graph) {
        graph?.elements().unselect();
        clearInspector();
      }
    });
    applyFilters();
    layoutGraph();
    window.setTimeout(focusMapTarget, 40);
  };

  const loadMap = async (force = false): Promise<void> => {
    const revision = ++loadRevision;
    loading.hidden = false;
    loading.querySelector("strong")!.textContent = force ? "Rebuilding structural facts" : "Reading committed structure";
    status.textContent = "Building the structural snapshot…";
    refresh.disabled = true;
    clearInspector();
    updateExportLink();
    try {
      const parameters = new URLSearchParams();
      if (repository.value) {
        parameters.set("repository", repository.value);
      }
      if (force) {
        parameters.set("refresh", "true");
      }
      const response = await fetch(`/api/maps?${parameters.toString()}`, {
        cache: "no-store",
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        throw new Error(await response.text() || `Map request failed (${response.status})`);
      }
      const result = await response.json() as MapSnapshot;
      if (revision !== loadRevision) {
        return;
      }
      renderSummary(result);
      await renderGraph(result);
      status.textContent = result.truncated
        ? "Bounded map ready · source limits reached"
        : `${result.nodes.length} facts · ${result.edges.length} relationships · commit-pinned`;
      loading.hidden = true;
      debug?.add("info", "map.loaded", {
        snapshot_id: result.id,
        repositories: result.repositories.length,
        nodes: result.nodes.length,
        edges: result.edges.length,
        truncated: result.truncated
      });
    } catch (error: unknown) {
      if (revision !== loadRevision) {
        return;
      }
      loading.querySelector("strong")!.textContent = "Map could not be built";
      loading.querySelector("p")!.textContent = error instanceof Error ? error.message : String(error);
      status.textContent = "Structural analysis failed";
      debug?.add("error", "map.load.failed", describeError(error));
    } finally {
      if (revision === loadRevision) {
        refresh.disabled = false;
      }
    }
  };

  /*
   * Map scope lives in the URL. Without it a map view could not be linked,
   * bookmarked, or survive a reload, and Back did nothing on this surface.
   */
  const setMapURL = (repositoryID: string, view: string): void => {
    const url = new URL(window.location.href);
    if (repositoryID) {
      url.searchParams.set("repository", repositoryID);
    } else {
      url.searchParams.delete("repository");
    }
    if (view && view !== "all") {
      url.searchParams.set("view", view);
    } else {
      url.searchParams.delete("view");
    }
    const entry = { map: { repository: repositoryID, view } };
    if (window.location.href === url.href) {
      window.history.replaceState(entry, "", url);
    } else {
      window.history.pushState(entry, "", url);
    }
  };

  const selectView = (view: string): void => {
    activeView = view;
    for (const candidate of viewButtons) {
      candidate.setAttribute("aria-pressed", String(candidate.dataset.mapView === view));
    }
  };

  repositoryTrigger.addEventListener("click", () => {
    if (repositoryPopover.hidden) {
      openRepositoryPicker();
    } else {
      closeRepositoryPicker();
    }
  });
  repositoryTrigger.addEventListener("keydown", (event) => {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      openRepositoryPicker(true);
    }
  });
  repositoryBackdrop.addEventListener("click", () => closeRepositoryPicker(true));
  repositorySearch.addEventListener("input", filterRepositoryOptions);
  repositorySearch.addEventListener("keydown", (event) => {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      visibleRepositoryOptions()[0]?.focus();
    } else if (event.key === "Enter") {
      const options = visibleRepositoryOptions();
      if (options.length === 1) {
        event.preventDefault();
        chooseRepository(options[0]);
      }
    } else if (event.key === "Escape") {
      event.preventDefault();
      closeRepositoryPicker(true);
    }
  });
  for (const option of repositoryOptions) {
    option.addEventListener("click", () => chooseRepository(option));
    option.addEventListener("keydown", (event) => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        chooseRepository(option);
      } else if (event.key === "ArrowDown") {
        event.preventDefault();
        focusRepositoryOption(option, 1);
      } else if (event.key === "ArrowUp") {
        event.preventDefault();
        focusRepositoryOption(option, -1);
      } else if (event.key === "Home") {
        event.preventDefault();
        visibleRepositoryOptions()[0]?.focus();
      } else if (event.key === "End") {
        event.preventDefault();
        visibleRepositoryOptions().at(-1)?.focus();
      } else if (event.key === "Escape") {
        event.preventDefault();
        closeRepositoryPicker(true);
      }
    });
  }
  document.addEventListener("pointerdown", (event) => {
    if (!repositoryPopover.hidden && !repositoryPicker.contains(event.target as Node)) {
      closeRepositoryPicker();
    }
  });

  repository.addEventListener("change", () => {
    syncRepositoryPicker();
    setMapURL(repository.value, activeView);
    void loadMap(false);
  });
  refresh.addEventListener("click", () => void loadMap(true));
  search.addEventListener("input", applyFilters);

  listToggle?.addEventListener("click", () => {
    if (!nodeList) {
      return;
    }
    const showing = nodeList.hidden !== false;
    nodeList.hidden = !showing;
    listToggle.setAttribute("aria-pressed", String(showing));
    canvas.classList.toggle("map-canvas-behind", showing);
    renderNodeList();
  });

  window.addEventListener("popstate", (event) => {
    const state = (event.state as { map?: { repository: string; view: string } } | null)?.map;
    if (!state) {
      return;
    }
    selectView(state.view || "all");
    if (state.repository !== repository.value) {
      repository.value = state.repository;
      syncRepositoryPicker();
      void loadMap(false);
    } else {
      applyFilters();
    }
  });
  for (const button of viewButtons) {
    button.addEventListener("click", () => {
      selectView(button.dataset.mapView || "all");
      setMapURL(repository.value, activeView);
      applyFilters();
      layoutGraph();
    });
  }
  focus.addEventListener("click", () => {
    if (!graph || !selected) {
      return;
    }
    const neighborhood = selected.closedNeighborhood();
    graph.elements().toggleClass("map-focus-hidden", true);
    neighborhood.toggleClass("map-focus-hidden", false);
    graph.fit(neighborhood, 90);
  });
  reset.addEventListener("click", () => {
    search.value = "";
    applyFilters();
    searchResults.hidden = true;
    layoutGraph();
    clearInspector();
  });
  window.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      clearInspector();
      graph?.elements().unselect();
      searchResults.hidden = true;
    }
  });
  window.addEventListener("resize", () => graph?.resize());

  // Restore scope and view from the URL so a shared map link opens as sent.
  const initialRepository = initialParameters.get("repository");
  if (initialRepository && Array.from(repository.options).some((option) => option.value === initialRepository)) {
    repository.value = initialRepository;
  }
  syncRepositoryPicker();
  const initialView = initialParameters.get("view");
  if (initialView && viewButtons.some((button) => button.dataset.mapView === initialView)) {
    selectView(initialView);
  }
  setMapURL(repository.value, activeView);

  updateExportLink();
  void loadMap(false);
}

type WikiPage = {
  repository_id: number;
  slug: string;
  title: string;
  summary: string;
  order: number;
  number: string;
  parent_slug?: string;
  depth: number;
  plan_revision?: string;
  plan_version: number;
  plan_provider?: string;
  plan_model?: string;
  status: "planned" | "generating" | "ready" | "stale" | "error";
  revision?: string;
  provider?: string;
  model?: string;
  input_tokens: number;
  output_tokens: number;
  started_at?: string;
  generated_at?: string;
  error?: string;
  supporting_files: string[];
  citations: MapEvidence[];
  markdown?: string;
};

type WikiSite = {
  version: number;
  repository_id: number;
  repository: string;
  revision: string;
  updated_at: string;
  /** How the standard pipeline scaled itself: "standard" or "compact". */
  profile?: string;
  profile_pages?: string;
  steering: {
    title?: string;
    include?: string[];
    exclude?: string[];
    notes?: Record<string, string>;
  };
  pages: WikiPage[];
  survey_ready: boolean;
  survey_stale: boolean;
  survey_status?: string;
  survey_error?: string;
  /** Survey checkpoint, which carries its own provider token usage. */
  survey?: { profile?: string; input_tokens?: number; output_tokens?: number };
  plan_ready: boolean;
  plan_stale: boolean;
  plan_revision?: string;
  plan_provider?: string;
  plan_model?: string;
  ready: number;
  stale: number;
  pending: number;
  failed: number;
};

function enableRepositoryWiki(debug?: DebugLogger): void {
  const workspace = document.querySelector<HTMLElement>("[data-wiki-workspace]");
  const repository = document.querySelector<HTMLSelectElement>("[data-wiki-repository]");
  const repositoryPicker = document.querySelector<HTMLElement>("[data-wiki-repository-picker]");
  const repositoryTrigger = document.querySelector<HTMLButtonElement>("[data-wiki-repository-trigger]");
  const repositoryCurrent = document.querySelector<HTMLElement>("[data-wiki-repository-current]");
  const repositoryMeta = document.querySelector<HTMLElement>("[data-wiki-repository-meta]");
  const repositoryBackdrop = document.querySelector<HTMLButtonElement>("[data-wiki-repository-backdrop]");
  const repositoryPopover = document.querySelector<HTMLElement>("[data-wiki-repository-popover]");
  const repositorySearch = document.querySelector<HTMLInputElement>("[data-wiki-repository-search]");
  const repositoryOptions = Array.from(
    document.querySelectorAll<HTMLButtonElement>("[data-wiki-repository-option]")
  );
  const provider = document.querySelector<HTMLSelectElement>("[data-wiki-provider]");
  const providerState = document.querySelector<HTMLElement>("[data-wiki-provider-state]");
  const providerDetail = document.querySelector<HTMLElement>("[data-wiki-provider-detail]");
  const model = document.querySelector<HTMLSelectElement>("[data-wiki-model]");
  const effort = document.querySelector<HTMLSelectElement>("[data-wiki-effort]");
  const timeout = document.querySelector<HTMLSelectElement>("[data-wiki-timeout]");
  const tokenBudget = document.querySelector<HTMLSelectElement>("[data-wiki-token-budget]");
  const tokenBudgetField = document.querySelector<HTMLElement>("[data-wiki-token-budget-field]");
  const planHeading = document.querySelector<HTMLElement>("[data-wiki-plan-heading]");
  const commit = document.querySelector<HTMLElement>("[data-wiki-commit]");
  const ready = document.querySelector<HTMLElement>("[data-wiki-ready]");
  const stale = document.querySelector<HTMLElement>("[data-wiki-stale]");
  const pending = document.querySelector<HTMLElement>("[data-wiki-pending]");
  const failed = document.querySelector<HTMLElement>("[data-wiki-failed]");
  const generateAll = document.querySelector<HTMLButtonElement>("[data-wiki-generate]");
  const exportLink = document.querySelector<HTMLAnchorElement>("[data-wiki-export]");
  const steering = document.querySelector<HTMLElement>("[data-wiki-steering]");
  const pages = document.querySelector<HTMLElement>("[data-wiki-pages]");
  const pageCount = document.querySelector<HTMLElement>("[data-wiki-page-count]");
  const pageSearch = document.querySelector<HTMLInputElement>("[data-wiki-page-search]");
  const repositoryName = document.querySelector<HTMLElement>("[data-wiki-repository-name]");
  const pageTitle = document.querySelector<HTMLElement>("[data-wiki-page-title]");
  const pageStatus = document.querySelector<HTMLElement>("[data-wiki-page-status]");
  const refreshPage = document.querySelector<HTMLButtonElement>("[data-wiki-refresh-page]");
  const content = document.querySelector<HTMLElement>("[data-wiki-content]");
  const empty = document.querySelector<HTMLElement>("[data-wiki-empty]");
  const loading = document.querySelector<HTMLElement>("[data-wiki-loading]");
  const runPanel = document.querySelector<HTMLElement>("[data-wiki-run]");
  const loadingQuiet = document.querySelector<HTMLElement>("[data-wiki-loading-quiet]");
  const runTitle = document.querySelector<HTMLElement>("[data-wiki-run-title]");
  const runEngine = document.querySelector<HTMLElement>("[data-wiki-run-engine]");
  const runProgress = document.querySelector<HTMLElement>("[data-wiki-run-progress]");
  const runBar = document.querySelector<HTMLElement>("[data-wiki-run-bar]");
  const runCurrent = document.querySelector<HTMLElement>("[data-wiki-run-current]");
  const runPages = document.querySelector<HTMLOListElement>("[data-wiki-run-pages]");
  const runDone = document.querySelector<HTMLElement>("[data-wiki-run-done]");
  const runStepElapsed = document.querySelector<HTMLElement>("[data-wiki-run-step-elapsed]");
  const runTotalElapsed = document.querySelector<HTMLElement>("[data-wiki-run-total-elapsed]");
  const runTokens = document.querySelector<HTMLElement>("[data-wiki-run-tokens]");
  const runNote = document.querySelector<HTMLElement>("[data-wiki-run-note]");
  const cancelGeneration = document.querySelector<HTMLButtonElement>("[data-wiki-cancel-generation]");
  const error = document.querySelector<HTMLElement>("[data-wiki-error]");
  const errorMessage = document.querySelector<HTMLElement>("[data-wiki-error-message]");
  const pageRevision = document.querySelector<HTMLElement>("[data-wiki-page-revision]");
  const pageGenerator = document.querySelector<HTMLElement>("[data-wiki-page-generator]");
  const pageGenerated = document.querySelector<HTMLElement>("[data-wiki-page-generated]");
  const pageTokens = document.querySelector<HTMLElement>("[data-wiki-page-tokens]");
  const supportCount = document.querySelector<HTMLElement>("[data-wiki-support-count]");
  const supportingFiles = document.querySelector<HTMLUListElement>("[data-wiki-supporting-files]");
  const citationCount = document.querySelector<HTMLElement>("[data-wiki-citation-count]");
  const evidenceCount = document.querySelector<HTMLElement>("[data-wiki-evidence-count]");
  const provenanceDrawer = enableEvidenceDrawer({
    panel: "[data-wiki-provenance]",
    toggle: "[data-wiki-provenance-toggle]",
    close: "[data-wiki-provenance-close]",
    scrim: "[data-wiki-provenance-scrim]",
    dockedFrom: "(min-width: 1181px)"
  });
  const citations = document.querySelector<HTMLOListElement>("[data-wiki-citations]");
  const outline = document.querySelector<HTMLElement>("[data-wiki-outline]");
  if (
    !workspace || !repository || !repositoryPicker || !repositoryTrigger || !repositoryCurrent ||
    !repositoryMeta || !repositoryBackdrop || !repositoryPopover || !repositorySearch ||
    repositoryOptions.length === 0 || !provider || !providerState || !providerDetail || !model ||
    !effort || !timeout || !tokenBudget || !tokenBudgetField || !planHeading || !commit || !ready || !stale || !pending || !failed ||
    !generateAll || !exportLink || !steering || !pages || !pageCount || !repositoryName || !pageTitle || !pageStatus ||
    !refreshPage || !content || !empty || !loading || !error || !errorMessage || !pageRevision ||
    !pageGenerator || !pageGenerated || !pageTokens || !supportCount || !supportingFiles ||
    !citationCount || !citations || !outline || !pageSearch || !runPanel ||
    !loadingQuiet || !runTitle || !runEngine || !runProgress || !runBar || !runCurrent ||
    !runPages || !runDone || !runStepElapsed || !runTotalElapsed || !runTokens || !runNote ||
    !cancelGeneration
  ) {
    return;
  }

  let site: WikiSite | undefined;
  let activeSlug = "";
  let requestRevision = 0;
  let generating = false;
  let providerStatuses: ProviderStatus[] = [];
  let configuredProvider = "";
  let generationTimer: number | undefined;
  let runStartedAt: number | undefined;
  let generationAbort: AbortController | undefined;
  let generationRepository = "";
  let repositoryLoadFailed = false;

  const visibleRepositoryOptions = (): HTMLButtonElement[] =>
    repositoryOptions.filter((option) => !option.hidden);

  const syncRepositoryPicker = (): void => {
    const selectedOption = repositoryOptions.find((option) => option.dataset.value === repository.value);
    const nativeOption = repository.selectedOptions[0];
    repositoryCurrent.textContent = selectedOption?.dataset.label
      || nativeOption?.textContent?.trim()
      || "Choose a repository";
    repositoryMeta.textContent = selectedOption ? "Deep Wiki workspace" : "Select indexed code";
    repositoryTrigger.disabled = repository.disabled;
    for (const option of repositoryOptions) {
      option.setAttribute("aria-selected", String(option === selectedOption));
    }
  };

  const filterRepositoryOptions = (): void => {
    const query = repositorySearch.value.trim().toLocaleLowerCase();
    for (const option of repositoryOptions) {
      const label = option.dataset.label?.toLocaleLowerCase() || "";
      option.hidden = query !== "" && !label.includes(query);
    }
  };

  const closeRepositoryPicker = (restoreFocus = false): void => {
    repositoryPopover.hidden = true;
    repositoryPicker.dataset.open = "false";
    repositoryTrigger.setAttribute("aria-expanded", "false");
    repositorySearch.value = "";
    filterRepositoryOptions();
    if (restoreFocus) {
      repositoryTrigger.focus();
    }
  };

  const openRepositoryPicker = (focusSelected = false): void => {
    if (repositoryTrigger.disabled) {
      return;
    }
    repositoryPopover.hidden = false;
    repositoryPicker.dataset.open = "true";
    repositoryTrigger.setAttribute("aria-expanded", "true");
    if (focusSelected) {
      (repositoryOptions.find((option) => option.getAttribute("aria-selected") === "true")
        ?? visibleRepositoryOptions()[0])?.focus();
      return;
    }
    repositorySearch.focus();
  };

  const focusRepositoryOption = (current: HTMLButtonElement, offset: number): void => {
    const options = visibleRepositoryOptions();
    if (options.length === 0) {
      return;
    }
    const currentIndex = options.indexOf(current);
    options[(currentIndex + offset + options.length) % options.length]?.focus();
  };

  const chooseRepository = (option: HTMLButtonElement): void => {
    repository.value = option.dataset.value || "";
    syncRepositoryPicker();
    repository.dispatchEvent(new Event("change", { bubbles: true }));
    closeRepositoryPicker(true);
  };

  const selectedProvider = (): ProviderStatus | undefined =>
    providerStatuses.find((status) => status.id === provider.value);

  const selectedModelLabel = (): string =>
    selectedProvider()?.models?.find((candidate) => candidate.id === model.value)?.label
      ?? model.value;

  const wikiModelEfforts = (): string[] => {
    const selected = selectedProvider();
    const supported = providerModelEfforts(selected, model.value);
    return selected?.id === "claude" || selected?.id === "anthropic-api"
      ? supported
      : supported.filter((level) => ["high", "xhigh", "max", "ultra"].includes(level));
  };

  const selectedEffortLabel = (): string =>
    effort.value ? `${effort.value} effort` : "Provider default effort";

  const providerReady = (): boolean => {
    const selected = selectedProvider();
    const supported = wikiModelEfforts();
    return Boolean(
      selected?.available &&
      selected.authenticated &&
      model.value &&
      (supported.length === 0 ? !effort.value : supported.includes(effort.value))
    );
  };

  // Wiki runs are long, so the chosen engine and checkpoint limits are kept
  // between visits. Only non-secret select values are stored.
  const runSettingsStorageKey = "repokarta:wiki-run-settings:v2";
  type WikiRunSettings = {
    provider?: string;
    model?: string;
    effort?: string;
    timeout?: string;
    token_budget?: string;
  };
  const readRunSettings = (): WikiRunSettings => {
    try {
      const stored = window.localStorage.getItem(runSettingsStorageKey);
      return stored ? JSON.parse(stored) as WikiRunSettings : {};
    } catch {
      return {};
    }
  };
  const applyStoredValue = (element: HTMLSelectElement, value: unknown): boolean => {
    if (typeof value !== "string" || !value) {
      return false;
    }
    if (!Array.from(element.options).some((option) => option.value === value)) {
      return false;
    }
    element.value = value;
    return true;
  };
  const persistRunSettings = (): void => {
    try {
      window.localStorage.setItem(runSettingsStorageKey, JSON.stringify({
        provider: provider.value,
        model: model.value,
        effort: effort.value,
        timeout: timeout.value,
        token_budget: tokenBudget.value
      }));
    } catch {
      // Wiki controls keep their defaults when local storage is unavailable.
    }
  };

  const configureProvider = (): void => {
    const selected = selectedProvider();
    const previousModel = model.value;
    const previousEffort = effort.value;
    const sameProvider = configuredProvider === selected?.id;
    model.disabled = !selected?.authenticated || generating;
    model.replaceChildren();
    for (const modelOption of selected?.models ?? []) {
      const option = document.createElement("option");
      option.value = modelOption.id;
      option.textContent = modelOption.label;
      model.append(option);
    }
    const modelIDs = (selected?.models ?? []).map((candidate) => candidate.id);
    const stored = readRunSettings();
    if (
      sameProvider &&
      previousModel &&
      modelIDs.includes(previousModel)
    ) {
      model.value = previousModel;
    } else if (!(stored.provider === selected?.id && applyStoredValue(model, stored.model))) {
      const recommended = recommendedProviderModel(selected?.id);
      model.value = recommended && modelIDs.includes(recommended)
        ? recommended
        : modelIDs[0] ?? "";
    }
    configuredProvider = selected?.id ?? "";
    effort.replaceChildren();
    const wikiEfforts = wikiModelEfforts();
    for (const level of wikiEfforts) {
      const option = document.createElement("option");
      option.value = level;
      option.textContent = level[0].toUpperCase() + level.slice(1);
      effort.append(option);
    }
    if (sameProvider && previousEffort && Array.from(effort.options).some((option) => option.value === previousEffort)) {
      effort.value = previousEffort;
    } else if (!(stored.provider === selected?.id && applyStoredValue(effort, stored.effort))) {
      const recommended = recommendedProviderEffort(selected?.id);
      effort.value = wikiEfforts.includes(recommended)
        ? recommended
        : wikiEfforts.includes("high") ? "high" : "";
    }
    effort.disabled = !selected?.authenticated || generating || wikiEfforts.length === 0;
    provider.disabled = generating || providerStatuses.every((status) => !status.authenticated);
    timeout.disabled = !selected?.authenticated || generating;
    tokenBudgetField.hidden = !selected?.token_budget;
    tokenBudget.disabled = !selected?.authenticated || generating || !selected?.token_budget;
    providerState.textContent = selected?.authenticated ? "Ready" : "Unavailable";
    providerState.dataset.state = selected?.authenticated ? "ready" : "error";
    providerDetail.textContent = selected?.authenticated
      ? `${selectedModelLabel()} · ${selectedEffortLabel()} · isolated read-only checkpoints; Wiki work never appears in saved chats.`
      : selected?.detail || "Choose an authenticated local provider to build this Wiki.";
    if (site) {
      renderSite(site);
      refreshPage.disabled = generating || !activeSlug || !providerReady();
    }
  };

  const loadProviders = async (): Promise<void> => {
    try {
      const response = await fetch("/api/providers", {
        cache: "no-store",
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        throw new Error(await response.text() || `Provider request failed (${response.status})`);
      }
      providerStatuses = (await response.json() as { providers: ProviderStatus[] }).providers ?? [];
      provider.replaceChildren();
      for (const status of providerStatuses) {
        const option = document.createElement("option");
        option.value = status.id;
        option.textContent = `${status.name}${status.authenticated ? " — ready" : " — login required"}`;
        option.disabled = !status.available || !status.authenticated;
        provider.append(option);
      }
      const preferred = providerStatuses.find((status) => status.id === "codex" && status.authenticated)
        ?? providerStatuses.find((status) => status.authenticated);
      if (preferred) {
        provider.value = preferred.id;
      }
      provider.disabled = !preferred;
      configureProvider();
      persistRunSettings();
    } catch (providerError: unknown) {
      providerState.textContent = "Unavailable";
      providerState.dataset.state = "error";
      providerDetail.textContent = providerError instanceof Error ? providerError.message : String(providerError);
      debug?.add("error", "wiki.providers.failed", describeError(providerError));
    }
  };

  const setStage = (state: "content" | "empty" | "loading" | "error"): void => {
    content.hidden = state !== "content";
    empty.hidden = state !== "empty";
    loading.hidden = state !== "loading";
    error.hidden = state !== "error";
    const running = state === "loading" && generating;
    runPanel.hidden = !running;
    loadingQuiet.hidden = running;
  };

  const formatElapsed = (milliseconds: number): string => {
    const totalSeconds = Math.max(0, Math.floor(milliseconds / 1000));
    const minutes = Math.floor(totalSeconds / 60);
    const seconds = String(totalSeconds % 60).padStart(2, "0");
    return `${minutes}:${seconds}`;
  };

  /*
   * Run model for the generation pipeline. Survey and knowledge map each run at
   * most once and are frequently reused from a saved checkpoint; the page stage
   * is a loop of one provider call per page. Modelling that explicitly lets the
   * UI show which stage is live, which were skipped because a checkpoint already
   * existed, and exactly which page is being written.
   */
  type RunStageID = "survey" | "plan" | "pages";
  type RunStageState = "pending" | "active" | "done" | "reused" | "failed";
  interface RunTarget {
    slug: string;
    number: string;
    title: string;
    state: "queued" | "active" | "done" | "failed";
  }

  const runTargets: RunTarget[] = [];
  let runStepStartedAt: number | undefined;

  const stageStates: Record<RunStageID, RunStageState> = {
    survey: "pending",
    plan: "pending",
    pages: "pending"
  };

  const setStageState = (id: RunStageID, state: RunStageState, note = ""): void => {
    stageStates[id] = state;
    renderRunCounters();
    const row = runPanel.querySelector<HTMLElement>(`[data-stage="${id}"]`);
    if (!row) {
      return;
    }
    row.dataset.state = state;
    const label = row.querySelector<HTMLElement>("[data-stage-state]");
    if (label) {
      label.textContent = note || {
        pending: "Waiting",
        active: "Running",
        done: "Done",
        reused: "Reused",
        failed: "Failed"
      }[state];
    }
  };

  const setStageDetail = (id: RunStageID, detail: string): void => {
    runPanel.querySelector<HTMLElement>(`[data-stage="${id}"] [data-stage-detail]`)
      ?.replaceChildren(document.createTextNode(detail));
  };

  /*
   * The counter reports whichever unit is meaningful right now. Before the plan
   * exists there are no pages to count, so a page counter reads "0 / 0" and
   * looks broken; stage progress is the honest measure at that point, and a
   * reused survey legitimately counts as one of three stages already done.
   */
  const renderRunCounters = (): void => {
    const done = runTargets.filter((target) => target.state === "done").length;
    if (runTargets.length > 0) {
      runDone.textContent = `${done} / ${runTargets.length} pages`;
      runBar.style.width = `${Math.round((done / runTargets.length) * 100)}%`;
      return;
    }
    const settled = (["survey", "plan", "pages"] as RunStageID[])
      .filter((id) => stageStates[id] === "done" || stageStates[id] === "reused").length;
    runDone.textContent = `${settled} / 3 stages`;
    runBar.style.width = `${Math.round((settled / 3) * 100)}%`;
  };

  const renderRunTargets = (): void => {
    runPages.replaceChildren();
    for (const target of runTargets) {
      const item = document.createElement("li");
      item.dataset.state = target.state;
      item.textContent = target.number || target.slug;
      item.title = `${target.number} ${target.title} · ${target.state}`;
      runPages.append(item);
    }
    renderRunCounters();
  };

  const renderRunTokens = (): void => {
    if (!site) {
      return;
    }
    /*
     * The survey checkpoint carries its own usage, so counting pages alone
     * under-reported a run and showed a flat zero for the whole of stages one
     * and two.
     */
    const totals = site.pages.reduce(
      (sum, page) => ({
        input: sum.input + (page.input_tokens || 0),
        output: sum.output + (page.output_tokens || 0)
      }),
      {
        input: site.survey?.input_tokens || 0,
        output: site.survey?.output_tokens || 0
      }
    );
    /*
     * Not every harness reports usage. Codex in particular returns no usage
     * event for an ephemeral turn, so a literal "0 in · 0 out" would claim the
     * run was free rather than admitting the number is unknown.
     */
    if (totals.input === 0 && totals.output === 0) {
      const finished = site.pages.some((page) => page.status === "ready" || page.status === "stale");
      runTokens.textContent = finished || site.survey_ready ? "not reported" : "—";
      return;
    }
    runTokens.textContent = `${formatTokenCount(totals.input)} in · ${formatTokenCount(totals.output)} out`;
  };

  const startRunStep = (id: RunStageID, current: string, note?: string): void => {
    if (generationTimer !== undefined) {
      window.clearInterval(generationTimer);
    }
    runStepStartedAt = Date.now();
    if (runStartedAt === undefined) {
      runStartedAt = runStepStartedAt;
    }
    setStageState(id, "active");
    runCurrent.textContent = current;
    if (note) {
      runNote.textContent = note;
    }
    cancelGeneration.disabled = false;
    cancelGeneration.textContent = "Cancel";

    const tick = (): void => {
      runStepElapsed.textContent = formatElapsed(Date.now() - (runStepStartedAt ?? Date.now()));
      runTotalElapsed.textContent = formatElapsed(Date.now() - (runStartedAt ?? Date.now()));
    };
    tick();
    generationTimer = window.setInterval(tick, 1000);
  };

  const beginRun = (): void => {
    runTargets.length = 0;
    runStartedAt = undefined;
    runStepStartedAt = undefined;
    for (const id of ["survey", "plan", "pages"] as RunStageID[]) {
      setStageState(id, "pending");
    }
    runProgress.hidden = true;
    runPages.replaceChildren();
    runCurrent.textContent = "";
    renderRunCounters();
    runStepElapsed.textContent = "0:00";
    runTotalElapsed.textContent = "0:00";
    runNote.textContent = "Completed work is saved as it finishes and survives cancellation.";
    runTitle.textContent = site ? `Building ${site.repository}` : "Building the knowledge map";
    const engine = [provider.options[provider.selectedIndex]?.textContent?.trim(), model.value.trim()]
      .filter(Boolean)
      .join(" · ");
    runEngine.textContent = engine || "Provider default";
    // Say out loud that a small repository runs a lighter pipeline, so a short
    // wiki reads as intentional rather than as a truncated one.
    if (site?.profile === "compact") {
      runNote.textContent =
        `Compact repository: no service entry point or routes were found, so this run uses the ` +
        `lighter survey and a ${site.profile_pages || "shorter"} plan.`;
    }
    renderRunTokens();
  };

  const endGenerationProgress = (): void => {
    if (generationTimer !== undefined) {
      window.clearInterval(generationTimer);
      generationTimer = undefined;
    }
    generationAbort = undefined;
    runPanel.hidden = true;
    loadingQuiet.hidden = false;
    runStartedAt = undefined;
    runStepStartedAt = undefined;
  };

  /*
   * Reading another Wiki page deliberately replaces the run panel in the
   * article stage. The primary action remains the stable way back to the live
   * run: it never starts a second generation while one is active.
   */
  const showGenerationProgress = (): void => {
    if (!generating) {
      return;
    }
    // Ignore a page response that may still be in flight; it must not replace
    // the run panel after the reader has explicitly returned to it.
    requestRevision++;
    setStage("loading");
    runPanel.scrollIntoView({ block: "start" });
  };

  const resetProvenance = (): void => {
    pageRevision.textContent = "—";
    pageGenerator.textContent = "—";
    pageGenerated.textContent = "—";
    pageTokens.textContent = "0 in · 0 out";
    supportCount.textContent = "0";
    citationCount.textContent = "0";
    if (evidenceCount) {
      evidenceCount.textContent = "0";
    }
    supportingFiles.replaceChildren();
    citations.replaceChildren();
    const outlineEmpty = document.createElement("p");
    outlineEmpty.textContent = "Headings appear after a page is generated.";
    outline.replaceChildren(outlineEmpty);
  };

  const renderOutline = (): void => {
    const headings = Array.from(content.querySelectorAll<HTMLHeadingElement>("h2, h3"));
    outline.replaceChildren();
    if (headings.length === 0) {
      const message = document.createElement("p");
      message.textContent = "This page has no navigable sections.";
      outline.append(message);
      return;
    }
    const used = new Set<string>();
    for (const heading of headings) {
      let id = heading.textContent
        ?.toLowerCase()
        .normalize("NFKD")
        .replace(/[\u0300-\u036f]/g, "")
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-|-$/g, "") || "section";
      const base = id;
      let suffix = 2;
      while (used.has(id)) {
        id = `${base}-${suffix++}`;
      }
      used.add(id);
      heading.id = id;
      const link = document.createElement("a");
      link.href = `#${id}`;
      link.textContent = heading.textContent || "Section";
      link.dataset.depth = heading.tagName === "H3" ? "1" : "0";
      outline.append(link);
    }
  };

  const bindWikiLinks = (): void => {
    content.querySelectorAll<HTMLAnchorElement>('a[href^="./"]').forEach((link) => {
      const target = link.getAttribute("href")?.replace(/^\.\//, "").replace(/\.md(?:#.*)?$/, "") ?? "";
      if (!site?.pages.some((page) => page.slug === target)) {
        return;
      }
      link.addEventListener("click", (event) => {
        event.preventDefault();
        void loadPage(target);
      });
    });
  };

  const renderProvenance = (page: WikiPage): void => {
    const pageFiles = page.supporting_files ?? [];
    const pageCitations = page.citations ?? [];
    pageRevision.textContent = page.revision ? page.revision.slice(0, 12) : "—";
    pageRevision.title = page.revision || "";
    pageGenerator.textContent = page.provider
      ? `${page.provider} · ${page.model || "default"}`
      : "Not generated";
    pageGenerated.textContent = page.generated_at && !page.generated_at.startsWith("0001-")
      ? new Date(page.generated_at).toLocaleString()
      : "—";
    pageTokens.textContent = `${page.input_tokens.toLocaleString()} in · ${page.output_tokens.toLocaleString()} out`;
    supportCount.textContent = String(pageFiles.length);
    supportingFiles.replaceChildren();
    for (const file of pageFiles) {
      const item = document.createElement("li");
      item.textContent = file;
      item.title = file;
      supportingFiles.append(item);
    }
    citationCount.textContent = String(pageCitations.length);
    if (evidenceCount) {
      evidenceCount.textContent = String(pageCitations.length);
    }
    citations.replaceChildren();
    for (const citation of pageCitations) {
      const item = document.createElement("li");
      const link = document.createElement("a");
      link.href = citation.url;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      const label = document.createElement("strong");
      label.textContent = `${citation.path}:${citation.line}`;
      const detail = document.createElement("span");
      detail.textContent = `${citation.revision.slice(0, 8)} · ${citation.label}`;
      link.append(label, detail);
      item.append(link);
      citations.append(item);
    }
  };

  const updatePageSelection = (): void => {
    pages.querySelectorAll<HTMLButtonElement>("[data-wiki-page]").forEach((button) => {
      if (button.dataset.wikiPage !== activeSlug) {
        button.removeAttribute("aria-current");
        return;
      }
      button.setAttribute("aria-current", "page");
    });
  };

  const showPlannedPage = (page: WikiPage): void => {
    setStage(page.status === "error" ? "error" : "empty");
    const emptyHeading = empty.querySelector<HTMLElement>("h2");
    const emptyCopy = empty.querySelector<HTMLElement>("p");
    if (emptyHeading && emptyCopy) {
      emptyHeading.textContent = page.status === "stale" ? "This page needs a refresh" : "This page is ready to generate";
      emptyCopy.textContent = "Generation runs independently, so completed pages stay available if another page needs to be retried.";
    }
    if (page.status === "error") {
      errorMessage.textContent = page.error || "Generation failed. Retry only this page.";
    }
    renderProvenance(page);
  };

  const loadPage = async (slug: string): Promise<void> => {
    if (!site) {
      return;
    }
    const page = site.pages.find((candidate) => candidate.slug === slug);
    if (!page) {
      return;
    }
    activeSlug = slug;
    updatePageSelection();
    // Opening a different page returns the reader to the article; a docked rail
    // is unaffected because only overlays are hidden.
    provenanceDrawer?.open(false);
    repositoryName.textContent = site.repository;
    pageTitle.textContent = page.title;
    pageStatus.textContent = page.status === "planned" ? "Not generated" : page.status;
    pageStatus.dataset.state = page.status;
    refreshPage.textContent = page.status === "planned" || page.status === "error"
      ? "Generate page"
      : "Refresh page";
    refreshPage.disabled = generating || !providerReady();
    // pushState, not replaceState: reading pages 1 -> 2 -> 3 and pressing Back
    // used to leave the Wiki entirely instead of stepping back a page. The
    // state object is what the popstate handler restores from.
    const location = new URL(window.location.href);
    location.searchParams.set("repository", String(site.repository_id));
    location.searchParams.set("page", page.slug);
    const entry = { wiki: { repository: String(site.repository_id), page: page.slug } };
    if (window.location.href === location.href) {
      window.history.replaceState(entry, "", location);
    } else {
      window.history.pushState(entry, "", location);
    }
    if (page.status !== "ready" && page.status !== "stale") {
      resetProvenance();
      showPlannedPage(page);
      return;
    }
    const revision = ++requestRevision;
    setStage("loading");
    loading.querySelector("strong")!.textContent = page.status === "stale"
      ? "Opening the last trusted revision"
      : "Opening generated Markdown";
    loading.querySelector("p")!.textContent = page.status === "stale"
      ? "Changed supporting files are flagged; refresh this page when ready."
      : "Rendering citations and validated diagrams locally.";
    try {
      const response = await fetch(`/api/wiki/${site.repository_id}/${encodeURIComponent(slug)}`, {
        cache: "no-store",
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        throw new Error(await response.text() || `Page request failed (${response.status})`);
      }
      const loaded = await response.json() as WikiPage;
      if (revision !== requestRevision) {
        return;
      }
      renderAssistantMarkdown(content, loaded.markdown || "", debug, true);
      bindWikiLinks();
      renderOutline();
      renderProvenance(loaded);
      setStage("content");
      debug?.add("info", "wiki.page.loaded", {
        repository_id: site.repository_id,
        page: loaded.slug,
        status: loaded.status,
        citations: loaded.citations.length
      });
    } catch (loadError: unknown) {
      if (revision !== requestRevision) {
        return;
      }
      errorMessage.textContent = loadError instanceof Error ? loadError.message : String(loadError);
      setStage("error");
      debug?.add("error", "wiki.page.failed", describeError(loadError));
    }
  };

  const generate = async (slug = ""): Promise<void> => {
    if (!site || generating) {
      return;
    }
    generating = true;
    generationRepository = repository.value;
    generationAbort = new AbortController();
    repository.disabled = true;
    syncRepositoryPicker();
    generateAll.disabled = true;
    refreshPage.disabled = true;
    // Engine choices are locked in for the life of a run so resumed
    // checkpoints keep the model, effort, and limits they started with.
    configureProvider();
    pages.querySelectorAll<HTMLButtonElement>("[data-wiki-generate-page]").forEach((button) => {
      button.disabled = true;
    });
    setStage("loading");
    const profileChanged = !slug && site.survey?.profile === "fast";
    const refreshAll = !slug && (
      profileChanged ||
      site.survey_stale ||
      site.plan_stale ||
      site.pages.every((page) => page.status === "ready")
    );
    const requestGeneration = async (
      page = "",
      planOnly = false,
      surveyOnly = false,
      refresh = false
    ): Promise<WikiSite> => {
      const response = await fetch("/api/wiki/generate", {
        method: "POST",
        signal: generationAbort?.signal,
        headers: {
          Accept: "application/json",
          "Content-Type": "application/json"
        },
        body: JSON.stringify({
          repository_id: site!.repository_id,
          page,
          refresh,
          survey_only: surveyOnly,
          plan_only: planOnly,
          preset: "quality",
          provider: provider.value,
          model: model.value,
          effort: effort.value,
          timeout_seconds: Number.parseInt(timeout.value, 10),
          token_budget: selectedProvider()?.token_budget ? Number.parseInt(tokenBudget.value, 10) : 0
        })
      });
      if (!response.ok) {
        throw new Error(await responseErrorMessage(response, `Generation failed (${response.status})`));
      }
      return await response.json() as WikiSite;
    };
    let cancelled = false;
    beginRun();
    try {
      if (!site.survey_ready || profileChanged || (site.survey_stale && !slug)) {
        setStageDetail("survey", "Bounded code discovery, persisted as survey.md");
        startRunStep(
          "survey",
          "Discovering architecture, flows, tests, and domain concepts across the committed tree.",
          "The survey is saved before planning begins, so this work is never repeated."
        );
        site = await requestGeneration("", false, true, site.survey_stale || profileChanged);
        setStageState("survey", "done");
        renderSite(site);
      } else {
        setStageState("survey", "reused");
        setStageDetail("survey", "Existing survey.md checkpoint reused");
      }
      renderRunTokens();

      if (!site.plan_ready || profileChanged || (site.plan_stale && !slug)) {
        setStageDetail("plan", "Survey organised into a page hierarchy");
        startRunStep(
          "plan",
          "Turning the saved survey into a repository-specific page hierarchy.",
          "Planning reuses the survey rather than repeating discovery."
        );
        site = await requestGeneration("", true, false, site.plan_stale || profileChanged);
        setStageState("plan", "done");
        renderSite(site);
        activeSlug = site.pages.some((page) => page.slug === activeSlug) ? activeSlug : "";
      } else {
        setStageState("plan", "reused");
        setStageDetail("plan", `Existing plan reused · ${site.pages.length} pages`);
      }
      renderRunTokens();

      const targets = slug
        ? site.pages.filter((page) => page.slug === slug)
        : site.pages.filter((page) =>
            refreshAll ||
            page.status === "planned" ||
            page.status === "stale" ||
            page.status === "error"
          );
      runTargets.push(...targets.map((page) => ({
        slug: page.slug,
        number: page.number || String(page.order),
        title: page.title,
        state: "queued" as const
      })));
      runProgress.hidden = runTargets.length === 0;
      setStageDetail("pages", `${runTargets.length} page${runTargets.length === 1 ? "" : "s"} to write`);
      renderRunTargets();

      for (const [index, target] of targets.entries()) {
        const tracked = runTargets[index];
        if (tracked) {
          tracked.state = "active";
        }
        renderRunTargets();
        startRunStep(
          "pages",
          `${target.number} ${target.title}`,
          "Implementation, diagrams, cross-links, and exact citations are assembled into one Markdown file."
        );
        setStageState("pages", "active", `${index + 1} / ${targets.length}`);
        try {
          site = await requestGeneration(
            target.slug,
            false,
            false,
            slug ? true : refreshAll || target.status !== "planned"
          );
        } catch (pageError: unknown) {
          if (tracked) {
            tracked.state = "failed";
          }
          renderRunTargets();
          throw pageError;
        }
        if (tracked) {
          tracked.state = "done";
        }
        renderSite(site);
        renderRunTargets();
        renderRunTokens();
      }
      setStageState("pages", runTargets.length ? "done" : "reused");

      endGenerationProgress();
      const selected = site.pages.some((page) => page.slug === (slug || activeSlug))
        ? slug || activeSlug
        : site.pages.find((page) => page.status === "ready" || page.status === "stale")?.slug
          ?? site.pages[0]?.slug;
      if (selected) {
        await loadPage(selected);
      }
      debug?.add("info", "wiki.generated", {
        repository_id: site.repository_id,
        page: slug || "pending",
        ready: site.ready,
        stale: site.stale,
        failed: site.failed
      });
    } catch (generationError: unknown) {
      cancelled = generationError instanceof DOMException && generationError.name === "AbortError";
      if (!cancelled) {
        errorMessage.textContent = generationError instanceof Error ? generationError.message : String(generationError);
        setStage("error");
        debug?.add("error", "wiki.generation.failed", describeError(generationError));
      } else {
        debug?.add("info", "wiki.generation.cancelled", { repository_id: site.repository_id });
      }
    } finally {
      generating = false;
      generationRepository = "";
      repository.disabled = false;
      syncRepositoryPicker();
      endGenerationProgress();
      configureProvider();
      if (cancelled) {
        await loadSite();
      } else if (site) {
        renderSite(site);
        refreshPage.disabled = !activeSlug || !providerReady();
      }
    }
  };

  const renderSite = (value: WikiSite): void => {
    site = value;
    repositoryLoadFailed = false;
    planHeading.textContent = value.plan_ready
      ? `${value.pages.length} code knowledge pages`
      : value.survey_ready
        ? "Repository survey saved"
        : value.survey_status === "error"
          ? "Survey interrupted · ready to resume"
          : "Knowledge map not built";
    commit.textContent = value.revision.slice(0, 8);
    commit.title = value.revision;
    ready.textContent = String(value.ready);
    stale.textContent = String(value.stale);
    pending.textContent = String(value.pending);
    failed.textContent = String(value.failed);
    repositoryName.textContent = value.repository;
    exportLink.href = `/api/wiki/export?repository=${value.repository_id}`;
    const exportable = value.ready + value.stale > 0;
    exportLink.setAttribute("aria-disabled", String(!exportable));
    const primaryAction = wikiPrimaryAction(generating, providerReady(), {
      planReady: value.plan_ready,
      surveyReady: value.survey_ready,
      planStale: value.plan_stale,
      stale: value.stale,
      pending: value.pending,
      failed: value.failed
    });
    generateAll.disabled = primaryAction.disabled;
    generateAll.dataset.state = primaryAction.mode;
    const generateLabel = generateAll.querySelector<HTMLElement>("span");
    if (generateLabel) {
      generateLabel.textContent = primaryAction.label;
    }

    const configured = Boolean(
      value.steering.title ||
      value.steering.include?.length ||
      value.steering.exclude?.length ||
      Object.keys(value.steering.notes || {}).length
    );
    steering.querySelector("strong")!.textContent = configured
      ? "Plan steered by .repokarta.yml"
      : value.plan_ready
        ? "Repository-specific knowledge map"
        : value.survey_ready
          ? "Repository survey checkpoint saved"
        : "Knowledge map will be discovered from code";
    steering.querySelector("p")!.textContent = configured
      ? "Reviewed repository guidance is active and revision-pinned."
      : value.plan_ready
        ? `${value.plan_provider || "Provider"} identified the real subsystem hierarchy at this revision.`
        : value.survey_ready
          ? "survey.md is safely on disk; the next step turns it into the Wiki hierarchy."
        : "The selected provider will inspect architecture, flows, tests, and domain concepts before planning pages.";

    pages.replaceChildren();
    pageCount.textContent = String(value.pages.length);
    for (const page of value.pages) {
      const row = document.createElement("div");
      row.className = "wiki-page-row";
      row.dataset.depth = String(page.depth || 0);
      row.dataset.wikiSearch = `${page.number} ${page.title} ${page.summary}`.toLowerCase();
      const select = document.createElement("button");
      select.type = "button";
      select.className = "wiki-page-select";
      select.dataset.wikiPage = page.slug;
      const statusLabel = page.status === "planned"
        ? "Not generated"
        : page.status.charAt(0).toUpperCase() + page.status.slice(1);
      select.setAttribute("aria-label", `${page.number || page.order}. ${page.title} · ${statusLabel}`);
      const index = document.createElement("span");
      index.className = "wiki-page-index";
      index.textContent = page.number || String(page.order);
      const copy = document.createElement("span");
      copy.className = "wiki-page-copy";
      const title = document.createElement("strong");
      title.textContent = page.title;
      const status = document.createElement("span");
      status.dataset.state = page.status;
      status.textContent = statusLabel;
      copy.append(title, status);
      select.append(index, copy);
      select.addEventListener("click", () => void loadPage(page.slug));

      const regenerate = document.createElement("button");
      regenerate.type = "button";
      regenerate.className = "wiki-page-generate";
      regenerate.dataset.wikiGeneratePage = page.slug;
      regenerate.title = page.status === "planned" ? `Generate ${page.title}` : `Refresh ${page.title}`;
      regenerate.setAttribute("aria-label", regenerate.title);
      regenerate.innerHTML = page.status === "planned"
        ? '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M10 4v12M4 10h12"></path></svg>'
        : '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M15.5 7A6 6 0 1 0 16 11M15.5 3.5V7H12"></path></svg>';
      regenerate.disabled = generating || !providerReady();
      regenerate.addEventListener("click", () => void generate(page.slug));

      // Page numbering already carries hierarchy (for example 2 and 2.1).
      // A single visual alignment keeps the navigation scannable and prevents
      // controls from wrapping into misleading indentation levels.
      row.append(select, regenerate);
      pages.append(row);

      row.hidden = Boolean(
        pageSearch.value.trim() &&
        !row.dataset.wikiSearch.includes(pageSearch.value.trim().toLowerCase())
      );
    }
    updatePageSelection();
  };

  const beginRepositoryLoad = (): void => {
    site = undefined;
    repositoryLoadFailed = false;
    activeSlug = "";
    const selectedRepository = repository.selectedOptions[0]?.textContent?.trim() || "repository";
    repositoryName.textContent = selectedRepository;
    pageTitle.textContent = "Loading Deep Wiki";
    pageStatus.textContent = "Loading";
    pageStatus.dataset.state = "pending";
    planHeading.textContent = `Loading ${selectedRepository}`;
    commit.textContent = "—";
    commit.removeAttribute("title");
    ready.textContent = "0";
    stale.textContent = "0";
    pending.textContent = "0";
    failed.textContent = "0";
    pageCount.textContent = "0";
    pages.replaceChildren();
    generateAll.disabled = true;
    generateAll.dataset.state = "generate";
    generateAll.querySelector<HTMLElement>("span")!.textContent = "Loading repository…";
    refreshPage.textContent = "Refresh page";
    refreshPage.disabled = true;
    exportLink.href = "#";
    exportLink.setAttribute("aria-disabled", "true");
    steering.querySelector("strong")!.textContent = "Loading repository knowledge";
    steering.querySelector("p")!.textContent =
      "Checking the selected revision and its saved documentation state.";
    resetProvenance();
    setStage("loading");
  };

  const showRepositoryLoadFailure = (message: string): void => {
    repositoryLoadFailed = true;
    planHeading.textContent = "Repository could not be loaded";
    steering.querySelector("strong")!.textContent = "Wiki state unavailable";
    steering.querySelector("p")!.textContent =
      "Retry the repository load. Previously opened Wiki pages were cleared to avoid showing stale content.";
    errorMessage.textContent = message;
    generateAll.disabled = false;
    generateAll.dataset.state = "retry";
    generateAll.querySelector<HTMLElement>("span")!.textContent = "Retry repository";
    setStage("error");
  };

  const loadSite = async (): Promise<void> => {
    if (generating) {
      if (generationRepository) {
        repository.value = generationRepository;
        syncRepositoryPicker();
      }
      showGenerationProgress();
      return;
    }
    const repositoryID = Number.parseInt(repository.value, 10);
    if (!Number.isFinite(repositoryID) || repositoryID <= 0) {
      return;
    }
    const revision = ++requestRevision;
    beginRepositoryLoad();
    loading.querySelector("strong")!.textContent = "Planning documentation";
    loading.querySelector("p")!.textContent = "Reading the current structural snapshot and repository steering.";
    try {
      const response = await fetch(`/api/wiki?repository=${repositoryID}`, {
        cache: "no-store",
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        throw new Error(await responseErrorMessage(response, `Documentation plan failed (${response.status})`));
      }
      const loaded = await response.json() as WikiSite;
      if (revision !== requestRevision) {
        return;
      }
      renderSite(loaded);
      const requestedPage = new URL(window.location.href).searchParams.get("page");
      const initial = loaded.pages.find((page) => page.slug === requestedPage)
        ?? loaded.pages.find((page) => page.status === "ready" || page.status === "stale")
        ?? loaded.pages[0];
      if (initial) {
        await loadPage(initial.slug);
      }
      debug?.add("info", "wiki.plan.loaded", {
        repository_id: loaded.repository_id,
        revision: loaded.revision,
        pages: loaded.pages.length
      });
    } catch (planError: unknown) {
      if (revision !== requestRevision) {
        return;
      }
      showRepositoryLoadFailure(planError instanceof Error ? planError.message : String(planError));
      debug?.add("error", "wiki.plan.failed", describeError(planError));
    }
  };

  const storedRunSettings = readRunSettings();
  applyStoredValue(timeout, storedRunSettings.timeout);
  applyStoredValue(tokenBudget, storedRunSettings.token_budget);

  repositoryTrigger.addEventListener("click", () => {
    if (repositoryPopover.hidden) {
      openRepositoryPicker();
    } else {
      closeRepositoryPicker();
    }
  });
  repositoryTrigger.addEventListener("keydown", (event) => {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      openRepositoryPicker(true);
    }
  });
  repositoryBackdrop.addEventListener("click", () => closeRepositoryPicker(true));
  repositorySearch.addEventListener("input", filterRepositoryOptions);
  repositorySearch.addEventListener("keydown", (event) => {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      visibleRepositoryOptions()[0]?.focus();
    } else if (event.key === "Enter") {
      const options = visibleRepositoryOptions();
      if (options.length === 1) {
        event.preventDefault();
        chooseRepository(options[0]);
      }
    } else if (event.key === "Escape") {
      event.preventDefault();
      closeRepositoryPicker(true);
    }
  });
  for (const option of repositoryOptions) {
    option.addEventListener("click", () => chooseRepository(option));
    option.addEventListener("keydown", (event) => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        chooseRepository(option);
      } else if (event.key === "ArrowDown") {
        event.preventDefault();
        focusRepositoryOption(option, 1);
      } else if (event.key === "ArrowUp") {
        event.preventDefault();
        focusRepositoryOption(option, -1);
      } else if (event.key === "Home") {
        event.preventDefault();
        visibleRepositoryOptions()[0]?.focus();
      } else if (event.key === "End") {
        event.preventDefault();
        visibleRepositoryOptions().at(-1)?.focus();
      } else if (event.key === "Escape") {
        event.preventDefault();
        closeRepositoryPicker(true);
      }
    });
  }
  document.addEventListener("pointerdown", (event) => {
    if (!repositoryPopover.hidden && !repositoryPicker.contains(event.target as Node)) {
      closeRepositoryPicker();
    }
  });

  repository.addEventListener("change", () => {
    syncRepositoryPicker();
    void loadSite();
  });
  provider.addEventListener("change", () => {
    configureProvider();
    persistRunSettings();
  });
  model.addEventListener("change", () => {
    configureProvider();
    persistRunSettings();
    if (site) {
      renderSite(site);
    }
  });
  effort.addEventListener("change", () => {
    const selected = selectedProvider();
    if (selected?.authenticated) {
      providerDetail.textContent =
        `${selectedModelLabel()} · ${selectedEffortLabel()} · isolated read-only checkpoints; Wiki work never appears in saved chats.`;
    }
    persistRunSettings();
    if (site) {
      renderSite(site);
    }
  });
  timeout.addEventListener("change", persistRunSettings);
  tokenBudget.addEventListener("change", persistRunSettings);
  cancelGeneration.addEventListener("click", () => {
    if (!generationAbort || generationAbort.signal.aborted) {
      return;
    }
    cancelGeneration.disabled = true;
    cancelGeneration.textContent = "Cancelling…";
    runNote.textContent =
      "Interrupting the isolated read-only session. Completed checkpoints and pages remain on disk.";
    generationAbort.abort();
  });
  pageSearch.addEventListener("input", () => {
    const query = pageSearch.value.trim().toLowerCase();
    pages.querySelectorAll<HTMLElement>(".wiki-page-row").forEach((row) => {
      row.hidden = Boolean(query && !row.dataset.wikiSearch?.includes(query));
    });
  });
  generateAll.addEventListener("click", () => {
    if (generating) {
      showGenerationProgress();
      return;
    }
    if (!site && repositoryLoadFailed) {
      void loadSite();
      return;
    }
    void generate();
  });
  refreshPage.addEventListener("click", () => {
    if (activeSlug) {
      void generate(activeSlug);
    }
  });
  exportLink.addEventListener("click", (event) => {
    if (exportLink.getAttribute("aria-disabled") === "true") {
      event.preventDefault();
    }
  });

  // Back and Forward move between Wiki pages instead of leaving the surface.
  window.addEventListener("popstate", (event) => {
    const state = (event.state as { wiki?: { repository: string; page: string } } | null)?.wiki;
    if (!state) {
      return;
    }
    if (state.repository !== repository.value) {
      repository.value = state.repository;
      syncRepositoryPicker();
      void loadSite().then(() => loadPage(state.page));
      return;
    }
    if (state.page !== activeSlug) {
      void loadPage(state.page);
    }
  });

  void loadProviders();
  if (repository.options.length > 1) {
    const requestedRepository = new URL(window.location.href).searchParams.get("repository");
    if (requestedRepository && Array.from(repository.options).some((option) => option.value === requestedRepository)) {
      repository.value = requestedRepository;
    } else {
      repository.selectedIndex = 1;
    }
    syncRepositoryPicker();
    void loadSite();
  }
}

function enableArtifactProgress(): void {
  const health = document.querySelector<HTMLElement>("[data-index-health]");
  const dependencyPending = document.querySelector<HTMLElement>("[data-dependency-build-pending]");
  const dependencyValue = dependencyPending?.querySelector<HTMLElement>("[data-dependency-build-progress]");
  if (!health) {
    return;
  }
  let previousPending = -1;
  const poll = async (): Promise<void> => {
    try {
      const response = await fetch("/api/artifacts/progress", {
        cache: "no-store",
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        return;
      }
      const progress = await response.json() as {
        state: string;
        requested_repositories: number;
        ready_repositories: number;
        pending_repositories: number;
      };
      const value = health.querySelector<HTMLElement>("[data-artifact-progress]");
      if (value) {
        value.textContent = progress.pending_repositories > 0
          ? `References ${progress.ready_repositories}/${progress.requested_repositories}`
          : `${progress.ready_repositories} ready`;
      }
      if (dependencyValue) {
        dependencyValue.textContent = `${progress.ready_repositories} of ${progress.requested_repositories}`;
      }
      if (dependencyPending && previousPending > 0 && progress.pending_repositories === 0) {
        window.location.reload();
        return;
      }
      previousPending = progress.pending_repositories;
      if (progress.pending_repositories > 0) {
        window.setTimeout(() => void poll(), 2000);
      }
    } catch {
      // The normal index status remains visible if the progress endpoint is
      // briefly unavailable during startup or shutdown.
    }
  };
  void poll();
}

function enableDependencyRefresh(): void {
  const button = document.querySelector<HTMLButtonElement>("[data-dependency-refresh]");
  const status = document.querySelector<HTMLElement>("[data-dependency-refresh-status]");
  if (!button || !status) {
    return;
  }
  let polling = false;
  let watching = false;
  const poll = async (): Promise<void> => {
    if (polling) {
      return;
    }
    polling = true;
    try {
      const response = await fetch(button.dataset.dependencyProgressUrl || "/api/dependencies/progress", {
        cache: "no-store",
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        throw new Error(`Progress request failed (${response.status})`);
      }
      const progress = await response.json() as {
        state: string;
        total: number;
        completed: number;
        failed: number;
        skipped: number;
        error?: string;
      };
      if (progress.state === "running") {
        watching = true;
        button.disabled = true;
        status.textContent = `Checking ${progress.completed} of ${progress.total} items` +
          (progress.failed > 0 ? ` · ${progress.failed} failed` : "");
        window.setTimeout(() => {
          polling = false;
          void poll();
        }, 750);
        return;
      }
      button.disabled = false;
      if (progress.state === "complete" && watching) {
        status.textContent = `Checked ${progress.completed} packages` +
          (progress.failed > 0 ? ` · ${progress.failed} failed` : "") +
          ` · ${progress.skipped} cached or unsupported`;
        window.location.reload();
      } else if (progress.state === "error") {
        status.textContent = progress.error || "Dependency refresh failed";
      }
    } catch (error) {
      button.disabled = false;
      status.textContent = error instanceof Error ? error.message : "Dependency refresh failed";
    } finally {
      polling = false;
    }
  };
  button.addEventListener("click", async () => {
    watching = true;
    button.disabled = true;
    status.textContent = "Preparing dependency checks…";
    try {
      const endpoint = button.dataset.dependencyRefreshUrl;
      if (!endpoint) {
        throw new Error("Dependency refresh URL is missing");
      }
      const response = await fetch(endpoint, {
        method: "POST",
        headers: { Accept: "application/json" }
      });
      if (!response.ok) {
        throw new Error(await responseErrorMessage(response, `Refresh failed (${response.status})`));
      }
      status.textContent = "Dependency checks started";
      void poll();
    } catch (error) {
      button.disabled = false;
      status.textContent = error instanceof Error ? error.message : "Dependency refresh failed";
    }
  });
  void poll();
}

type TopologyEvidence = {
  repository: string;
  revision: string;
  path: string;
  line: number;
  label: string;
  url: string;
};

type TopologyComponentPayload = {
  id: string;
  name: string;
  kind: string;
  technology?: string;
  repository?: string;
  path?: string;
  capabilities?: string[];
  external: boolean;
  candidate?: boolean;
  evidence?: TopologyEvidence[];
  origins: string[];
  neighborhood_role?: "selected" | "inbound" | "outbound" | "bidirectional" | "context";
};

type TopologyRuntimePayload = {
  provider: string;
  environment?: string;
  observed_from: string;
  observed_to: string;
  request_count: number;
  error_count: number;
  error_rate: number;
  latency_p95_ms?: number;
};

type TopologyConnectionPayload = {
  id: string;
  source: string;
  source_name: string;
  target: string;
  target_name: string;
  protocol: string;
  interaction: string;
  transport?: string;
  confidence: string;
  state: "confirmed" | "static_only" | "runtime_only";
  origins: string[];
  target_resolved: boolean;
  environment_variable?: string;
  resolution_tier?: string;
  environment?: string;
  resolution_divergent?: boolean;
  unresolved_reason?: string;
  evidence?: TopologyEvidence[];
  runtime?: TopologyRuntimePayload;
  neighborhood_direction?: "inbound" | "outbound" | "context";
};

type TopologyNeighborhoodGroupPayload = {
  id: string;
  direction: "inbound" | "outbound" | "context";
  label: string;
  omitted_connection_count: number;
  omitted_component_count: number;
};

type TopologyNeighborhoodPayload = {
  repository_id: number;
  direction: "both" | "inbound" | "outbound";
  depth: number;
  display_cap: number;
  selected_component_ids: string[];
  inbound_connection_count: number;
  outbound_connection_count: number;
  context_connection_count?: number;
  groups: TopologyNeighborhoodGroupPayload[];
};

type TopologyPayload = {
  components: TopologyComponentPayload[];
  connections: TopologyConnectionPayload[];
  build_progress: { state: string; pending_repositories: number };
  warnings?: Array<{ code: string; message: string; count: number }>;
  neighborhood?: TopologyNeighborhoodPayload;
};

function enableDependencyTopology(debug?: DebugLogger): void {
  const workspace = document.querySelector<HTMLElement>("[data-topology-workspace]");
  const canvas = workspace?.querySelector<HTMLElement>("[data-topology-canvas]");
  const empty = workspace?.querySelector<HTMLElement>("[data-topology-empty]");
  const focus = workspace?.querySelector<HTMLButtonElement>("[data-topology-focus]");
  const reset = workspace?.querySelector<HTMLButtonElement>("[data-topology-reset]");
  const listToggle = workspace?.querySelector<HTMLButtonElement>("[data-topology-list]");
  const table = workspace?.querySelector<HTMLElement>("[data-topology-table]");
  const warning = workspace?.querySelector<HTMLElement>("[data-topology-warning]");
  const warningText = workspace?.querySelector<HTMLElement>("[data-topology-warning-text]");
  const warningDismiss = workspace?.querySelector<HTMLButtonElement>("[data-topology-warning-dismiss]");
  const inspectorEmpty = workspace?.querySelector<HTMLElement>("[data-topology-inspector-empty]");
  const inspector = workspace?.querySelector<HTMLElement>("[data-topology-inspector]");
  const inspectorKind = workspace?.querySelector<HTMLElement>("[data-topology-inspector-kind]");
  const inspectorTitle = workspace?.querySelector<HTMLElement>("[data-topology-inspector-title]");
  const inspectorSubtitle = workspace?.querySelector<HTMLElement>("[data-topology-inspector-subtitle]");
  const inspectorMeta = workspace?.querySelector<HTMLElement>("[data-topology-inspector-meta]");
  const inspectorEvidence = workspace?.querySelector<HTMLOListElement>("[data-topology-evidence]");
  if (!workspace || !canvas || !empty || !focus || !reset || !listToggle || !table ||
    !warning || !warningText || !warningDismiss ||
    !inspectorEmpty || !inspector || !inspectorKind || !inspectorTitle ||
    !inspectorSubtitle || !inspectorMeta || !inspectorEvidence) {
    return;
  }
  const apiURL = workspace.dataset.topologyApi;
  if (!apiURL) {
    return;
  }

  type TopologyCore = import("cytoscape").Core;
  type TopologySingular = import("cytoscape").NodeSingular | import("cytoscape").EdgeSingular;
  let graph: TopologyCore | undefined;
  let selected: TopologySingular | undefined;
  const scopedNeighborhood = workspace.dataset.topologyScoped === "true";
  let currentDepth = Number(workspace.dataset.topologyDepth || "1");
  const showIntegrityWarning = (count: number): void => {
    if (count <= 0) {
      warning.hidden = true;
      return;
    }
    warningText.textContent = count === 1
      ? "1 connection referenced missing components and was hidden"
      : `${count} connections referenced missing components and were hidden`;
    warning.hidden = false;
  };
  warningDismiss.addEventListener("click", () => {
    warning.hidden = true;
  });

  const metadata = (entries: Array<[string, string | number | undefined]>): void => {
    inspectorMeta.replaceChildren();
    entries.forEach(([label, value]) => {
      if (value === undefined || value === "") {
        return;
      }
      const row = document.createElement("div");
      const term = document.createElement("dt");
      const detail = document.createElement("dd");
      term.textContent = label;
      detail.textContent = String(value);
      row.append(term, detail);
      inspectorMeta.append(row);
    });
  };
  const evidence = (items: TopologyEvidence[] = []): void => {
    inspectorEvidence.replaceChildren();
    if (items.length === 0) {
      const item = document.createElement("li");
      item.className = "text-xs leading-5 text-ink-faint";
      item.textContent = "No commit-pinned source evidence. Runtime observations are timestamped telemetry, not source facts.";
      inspectorEvidence.append(item);
      return;
    }
    items.forEach((fact) => {
      const item = document.createElement("li");
      const link = document.createElement("a");
      link.className = "topology-evidence-link";
      link.href = fact.url;
      link.textContent = `${fact.repository} · ${fact.path}:${fact.line}`;
      link.title = `${fact.label} at ${fact.revision}`;
      item.append(link);
      inspectorEvidence.append(item);
    });
  };
  const inspect = (element: TopologySingular): void => {
    selected = element;
    focus.disabled = scopedNeighborhood ? currentDepth >= 2 : false;
    inspectorEmpty.hidden = true;
    inspector.hidden = false;
    graph?.elements().removeClass("topology-dim topology-selected");
    element.addClass("topology-selected");
    if (element.isNode()) {
      const node = element.data("payload") as TopologyComponentPayload;
      inspectorKind.textContent = node.candidate
        ? `Internal ${node.kind} candidate`
        : node.external ? `External ${node.kind}` : node.kind;
      inspectorTitle.textContent = node.name;
      inspectorSubtitle.textContent = [
        node.technology,
        node.capabilities?.length ? `Capabilities: ${node.capabilities.join(", ")}` : undefined
      ].filter(Boolean).join(" · ") || "Deployable component";
      metadata([
        ["Repository", node.repository],
        ["Path", node.path],
        ["Evidence", node.origins.join(" + ")],
        ["Identity", node.id]
      ]);
      evidence(node.evidence);
      return;
    }
    const edge = element.data("payload") as TopologyConnectionPayload;
    inspectorKind.textContent = `${edge.protocol} · ${edge.state.replaceAll("_", " ")}`;
    inspectorTitle.textContent = `${edge.source_name || edge.source} → ${edge.target_name || edge.target}`;
    inspectorSubtitle.textContent = `${edge.interaction}${edge.transport ? ` over ${edge.transport}` : ""}`;
    const runtime = edge.runtime;
    metadata([
      ["Confidence", edge.confidence],
      ["Peer identity", edge.target_resolved ? "Resolved" : "Unresolved"],
      ["Placeholder", edge.environment_variable],
      ["Resolution tier", edge.resolution_tier?.replaceAll("_", " ")],
      ["Environment", edge.environment || runtime?.environment],
      ["Divergent assignments", edge.resolution_divergent ? "Yes" : undefined],
      ["Unresolved reason", edge.unresolved_reason?.replaceAll("_", " ")],
      ["Provider", runtime?.provider],
      ["Requests", runtime?.request_count],
      ["Errors", runtime?.error_count],
      ["Error rate", runtime ? `${(runtime.error_rate * 100).toFixed(2)}%` : undefined],
      ["p95 latency", runtime?.latency_p95_ms === undefined ? undefined : `${runtime.latency_p95_ms.toFixed(1)} ms`],
      ["Observed", runtime ? `${new Date(runtime.observed_from).toLocaleString()} – ${new Date(runtime.observed_to).toLocaleString()}` : undefined]
    ]);
    evidence(edge.evidence);
  };
  const fit = (): void => {
    if (!graph || graph.elements().length === 0) {
      return;
    }
    graph.elements().removeClass("topology-dim");
    graph.fit(graph.elements(), 48);
  };
  focus.addEventListener("click", () => {
    if (scopedNeighborhood) {
      if (currentDepth >= 2) {
        return;
      }
      const endpoint = new URL(apiURL, window.location.href);
      endpoint.searchParams.set("depth", "2");
      focus.disabled = true;
      focus.textContent = "Loading depth 2...";
      void loadTopology(endpoint.pathname + endpoint.search);
      return;
    }
    if (!graph || !selected) {
      return;
    }
    const neighborhood = selected.isNode()
      ? selected.closedNeighborhood()
      : selected.connectedNodes().union(selected);
    graph.elements().forEach((element) => {
      element.toggleClass("topology-dim", !neighborhood.contains(element));
    });
    graph.fit(neighborhood, 70);
  });
  reset.addEventListener("click", () => {
    selected = undefined;
    focus.disabled = scopedNeighborhood ? currentDepth >= 2 : true;
    graph?.elements().removeClass("topology-dim topology-selected");
    inspector.hidden = true;
    inspectorEmpty.hidden = false;
    fit();
  });
  listToggle.addEventListener("click", () => {
    table.hidden = !table.hidden;
    listToggle.setAttribute("aria-pressed", String(!table.hidden));
    listToggle.textContent = table.hidden ? "Connection table" : "Hide table";
  });

  const render = async (payload: TopologyPayload): Promise<void> => {
    const { default: cytoscape } = await import("cytoscape");
    graph?.destroy();
    empty.hidden = payload.components.length > 0;
    canvas.hidden = payload.components.length === 0;
    if (payload.components.length === 0) {
      return;
    }
    const laneX: Record<string, number> = {
      inbound: 120,
      selected: 440,
      outbound: 760,
      bidirectional: 440,
      context: 980
    };
    const roleCounts = new Map<string, number>();
    const roleTotals = new Map<string, number>();
    payload.components.forEach((component) => {
      const role = component.neighborhood_role ?? "context";
      roleTotals.set(role, (roleTotals.get(role) ?? 0) + 1);
    });
    payload.neighborhood?.groups.forEach((group) => {
      roleTotals.set(group.direction, (roleTotals.get(group.direction) ?? 0) + 1);
    });
    const lanePosition = (role: string): { x: number; y: number } | undefined => {
      if (!payload.neighborhood) {
        return undefined;
      }
      const index = roleCounts.get(role) ?? 0;
      roleCounts.set(role, index + 1);
      const total = roleTotals.get(role) ?? 1;
      return {
        x: laneX[role] ?? laneX.context,
        y: 280 + (index - (total - 1) / 2) * 82
      };
    };
    const selectedID = payload.neighborhood?.selected_component_ids[0];
    const componentElements = payload.components.map((component) => ({
        data: {
          id: component.id,
          label: component.name,
          kind: component.kind,
          external: component.external ? "yes" : "no",
          candidate: component.candidate ? "yes" : "no",
          origin: component.origins.join("-"),
          neighborhoodRole: component.neighborhood_role ?? "",
          payload: component
        },
        position: lanePosition(component.neighborhood_role ?? "context")
      }));
    const connectionElements = payload.connections.map((connection) => ({
        data: {
          id: connection.id,
          source: connection.source,
          target: connection.target,
          label: connection.protocol === "unknown"
            ? connection.interaction
            : `${connection.protocol} · ${connection.interaction}`,
          protocol: connection.protocol,
          state: connection.state,
          neighborhoodDirection: connection.neighborhood_direction ?? "",
          width: Math.min(7, 2 + Math.log10((connection.runtime?.request_count ?? 0) + 1)),
          payload: connection
        }
      }));
    const groupElements = (payload.neighborhood?.groups ?? []).flatMap((group) => {
      if (!selectedID) {
        return [];
      }
      const inbound = group.direction === "inbound";
      return [
        {
          data: {
            id: group.id,
            label: group.label,
            group: "yes",
            neighborhoodRole: group.direction
          },
          position: lanePosition(group.direction)
        },
        {
          data: {
            id: `${group.id}:edge`,
            source: inbound ? group.id : selectedID,
            target: inbound ? selectedID : group.id,
            label: "grouped",
            group: "yes",
            neighborhoodDirection: group.direction,
            width: 2
          }
        }
      ];
    });
    const elements = [...componentElements, ...connectionElements, ...groupElements];
    graph = cytoscape({
      container: canvas,
      elements,
      minZoom: 0.15,
      maxZoom: 2.4,
      wheelSensitivity: 0.18,
      style: [
        {
          selector: "node",
          style: {
            "background-color": "#111827",
            "border-color": "#64748b",
            "border-width": 2,
            color: "#e2e8f0",
            content: "data(label)",
            "font-family": "Inter, ui-sans-serif, system-ui, sans-serif",
            "font-size": 11,
            "font-weight": 600,
            height: 48,
            "text-valign": "center",
            "text-wrap": "ellipsis",
            "text-max-width": "112px",
            width: 132,
            shape: "round-rectangle"
          }
        },
        { selector: 'node[kind = "database"]', style: { shape: "barrel", "border-color": "#38bdf8" } },
        { selector: 'node[kind = "queue"], node[kind = "broker"]', style: { shape: "diamond", "border-color": "#fbbf24", height: 62, width: 92 } },
        { selector: 'node[kind = "mcp_server"]', style: { shape: "hexagon", "border-color": "#f472b6" } },
        { selector: 'node[external = "yes"]', style: { "border-style": "dashed", "background-color": "#15121c" } },
        { selector: 'node[candidate = "yes"]', style: { "border-color": "#fbbf24", "background-color": "#1c1810" } },
        { selector: 'node[neighborhoodRole = "selected"]', style: { "border-color": "#f8fafc", "border-width": 4, "background-color": "#172033", width: 152 } },
        { selector: 'node[neighborhoodRole = "inbound"]', style: { "border-color": "#7dd3fc" } },
        { selector: 'node[neighborhoodRole = "outbound"]', style: { "border-color": "#6ee7b7" } },
        { selector: 'node[neighborhoodRole = "bidirectional"]', style: { "border-color": "#c4b5fd", "border-style": "double" } },
        { selector: 'node[group = "yes"]', style: { shape: "round-rectangle", "border-style": "dashed", "background-color": "#18181b", color: "#f8fafc", width: 156, height: 42 } },
        {
          selector: "edge",
          style: {
            "curve-style": "bezier",
            "line-color": "#64748b",
            "target-arrow-color": "#64748b",
            "target-arrow-shape": "triangle",
            width: "data(width)",
            opacity: 0.72,
            label: "data(label)",
            color: "#94a3b8",
            "font-size": 8,
            "text-background-color": "#090b0f",
            "text-background-opacity": 0.88,
            "text-background-padding": "2px",
            "text-rotation": "autorotate"
          }
        },
        { selector: 'edge[protocol = "http"]', style: { "line-color": "#34d399", "target-arrow-color": "#34d399" } },
        { selector: 'edge[protocol = "grpc"]', style: { "line-color": "#a78bfa", "target-arrow-color": "#a78bfa" } },
        { selector: 'edge[protocol = "kafka"]', style: { "line-color": "#fbbf24", "target-arrow-color": "#fbbf24" } },
        { selector: 'edge[protocol = "database"]', style: { "line-color": "#38bdf8", "target-arrow-color": "#38bdf8" } },
        { selector: 'edge[protocol = "mcp"]', style: { "line-color": "#f472b6", "target-arrow-color": "#f472b6" } },
        { selector: 'edge[state = "runtime_only"]', style: { "line-style": "dashed", opacity: 0.9 } },
        { selector: 'edge[state = "static_only"]', style: { opacity: 0.48 } },
        { selector: 'edge[neighborhoodDirection = "inbound"]', style: { "target-arrow-color": "#7dd3fc", "target-arrow-shape": "triangle" } },
        { selector: 'edge[neighborhoodDirection = "outbound"]', style: { "target-arrow-color": "#6ee7b7", "target-arrow-shape": "triangle" } },
        { selector: 'edge[group = "yes"]', style: { "line-style": "dashed", "line-color": "#94a3b8", "target-arrow-color": "#94a3b8", label: "" } },
        { selector: ".topology-selected", style: { "overlay-color": "#ffffff", "overlay-opacity": 0.08, "overlay-padding": 7 } },
        { selector: ".topology-dim", style: { opacity: 0.08, "text-opacity": 0.08 } }
      ],
      layout: payload.neighborhood
        ? { name: "preset", padding: 48, fit: true }
        : {
            name: "breadthfirst",
            directed: true,
            padding: 48,
            spacingFactor: 1.25,
            avoidOverlap: true,
            circle: false
          }
    });
    graph.on("tap", "node, edge", (event) => {
      const element = event.target as TopologySingular;
      if (element.data("group") === "yes") {
        return;
      }
      inspect(element);
    });
    graph.on("tap", (event) => {
      if (event.target === graph) {
        reset.click();
      }
    });
    graph.on("mouseover", "node, edge", () => {
      canvas.style.cursor = "pointer";
    });
    graph.on("mouseout", "node, edge", () => {
      canvas.style.cursor = "default";
    });
    if (payload.neighborhood) {
      currentDepth = payload.neighborhood.depth;
      focus.disabled = currentDepth >= 2;
      focus.textContent = currentDepth >= 2 ? "Depth 2 shown" : "Show depth 2";
    }
    fit();
  };

  const loadTopology = async (endpoint: string): Promise<void> => {
    const started = performance.now();
    try {
      const response = await fetch(endpoint, {
        cache: "no-store",
        headers: { Accept: "application/json" }
      });
      if (!response.ok && response.status !== 202) {
        throw new Error(await responseErrorMessage(response, `Topology request failed (${response.status})`));
      }
      const rawPayload = await response.json() as TopologyPayload;
      const integrity = filterTopologyConnections(
        rawPayload.components ?? [],
        rawPayload.connections ?? []
      );
      const serverHiddenCount = (rawPayload.warnings ?? [])
        .filter((warning) => warning.code === "missing_component_reference")
        .reduce((count, warning) => count + warning.count, 0);
      const payload: TopologyPayload = {
        ...rawPayload,
        components: rawPayload.components ?? [],
        connections: integrity.connections
      };
      const hiddenConnections = Math.max(integrity.hiddenCount, serverHiddenCount);
      showIntegrityWarning(hiddenConnections);
      await render(payload);
      debug?.add("info", "dependency.topology.rendered", {
        components: payload.components.length,
        connections: payload.connections.length,
        hidden_connections: hiddenConnections,
        partial: payload.build_progress.pending_repositories > 0,
        duration_ms: Math.round(performance.now() - started)
      });
    } catch (error) {
      canvas.hidden = true;
      empty.hidden = false;
      empty.querySelector("strong")!.textContent = "The topology could not be rendered.";
      empty.querySelector("p")!.textContent = error instanceof Error ? error.message : "Unknown topology error";
      debug?.add("error", "dependency.topology.failed", describeError(error));
    }
  };

  void loadTopology(apiURL);
}

connectIndexEvents();
enableContextualChatLauncher();
enableArtifactProgress();
enableDependencyRefresh();
enableRepositoryDrawer();
enableQueryChips();
enableSearchQueryCompletion();
enableSearchFeedback();
enableFirstRunProgress();
enableSearchShortcut();
localiseShortcutHints();
enableCopyButtons();
enableMCPSecretControls();
enableTokenBudgetHelp();
highlightSource();
focusSourceLine();
enableSourceIntelligence();
highlightSearchResults();
const debugLogger = enableDebugLogger();
enableMermaidViewer(debugLogger);
enableConversations(debugLogger);
enableRepositoryMaps(debugLogger);
enableDependencyTopology(debugLogger);
enableRepositoryWiki(debugLogger);

document.body.addEventListener("htmx:afterSwap", (event) => {
  const target = (event as CustomEvent<{ target?: ParentNode }>).detail?.target;
  highlightSearchResults(target ?? document);
});
