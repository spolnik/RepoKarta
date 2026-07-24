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
};

function renderAssistantMarkdown(target: HTMLElement, markdown: string): void {
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
  target.querySelectorAll<HTMLElement>("pre code").forEach((code) => {
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

function enableConversations(): void {
  const form = document.querySelector<HTMLFormElement>("#conversation-form");
  const messages = document.querySelector<HTMLElement>("#conversation-messages");
  const empty = document.querySelector<HTMLElement>("[data-conversation-empty]");
  const provider = document.querySelector<HTMLSelectElement>("#conversation-provider");
  const model = document.querySelector<HTMLInputElement>("#conversation-model");
  const input = document.querySelector<HTMLTextAreaElement>("#conversation-message");
  const submit = document.querySelector<HTMLButtonElement>("#conversation-submit");
  const detail = document.querySelector<HTMLElement>("#provider-detail");
  const newConversation = document.querySelector<HTMLButtonElement>("[data-new-conversation]");
  if (!form || !messages || !provider || !model || !input || !submit || !detail || !newConversation) {
    return;
  }

  let conversationID = "";
  let busy = false;
  let statuses: ProviderStatus[] = [];

  const updateProviderDetail = (): void => {
    const status = statuses.find((candidate) => candidate.id === provider.value);
    detail.textContent = status?.detail ?? "Choose an authenticated local provider.";
  };

  void fetch("/api/providers", { headers: { Accept: "application/json" } })
    .then(async (response) => {
      if (!response.ok) {
        throw new Error(`Provider check failed (${response.status})`);
      }
      return response.json() as Promise<{ providers: ProviderStatus[] }>;
    })
    .then((result) => {
      statuses = result.providers;
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
      updateProviderDetail();
    })
    .catch((error: unknown) => {
      detail.textContent = error instanceof Error ? error.message : "Could not check providers.";
    });

  provider.addEventListener("change", updateProviderDetail);
  document.querySelectorAll<HTMLButtonElement>("[data-chat-prompt]").forEach((button) => {
    button.addEventListener("click", () => {
      input.value = button.dataset.chatPrompt ?? "";
      input.focus();
    });
  });
  newConversation.addEventListener("click", () => {
    conversationID = "";
    provider.disabled = false;
    model.disabled = false;
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
      return;
    }
    busy = true;
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
          message: question
        })
      });
      if (!response.ok || !response.body) {
        throw new Error((await response.text()) || `Conversation failed (${response.status})`);
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
          const message = JSON.parse(line) as ConversationEvent;
          if (message.type === "meta" && message.conversation_id) {
            conversationID = message.conversation_id;
            provider.disabled = true;
            model.disabled = true;
          } else if (message.type === "delta" && message.text && answer) {
            answerText += message.text;
            renderAssistantMarkdown(answer, answerText);
          } else if (message.type === "sources" && message.sources?.length) {
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
          } else if (message.type === "error") {
            throw new Error(message.text || "The provider could not complete this turn.");
          }
        }
        messages.scrollTop = messages.scrollHeight;
        if (done) {
          break;
        }
      }
    } catch (error: unknown) {
      assistant.remove();
      messages.append(conversationMessage("error", error instanceof Error ? error.message : "Conversation failed."));
    } finally {
      busy = false;
      submit.disabled = false;
      submit.textContent = "Ask RepoKarta";
      input.focus();
    }
  });
}

connectIndexEvents();
enableQueryChips();
enableCopyButtons();
highlightSource();
focusSourceLine();
highlightSearchResults();
enableConversations();

document.body.addEventListener("htmx:afterSwap", (event) => {
  const target = (event as CustomEvent<{ target?: ParentNode }>).detail?.target;
  highlightSearchResults(target ?? document);
});
