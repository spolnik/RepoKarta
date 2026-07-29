/**
 * @typedef {{
 *   select: HTMLSelectElement;
 *   picker: HTMLElement;
 *   trigger: HTMLButtonElement;
 *   current: HTMLElement;
 *   meta: HTMLElement;
 *   backdrop: HTMLElement;
 *   popover: HTMLElement;
 *   search: HTMLInputElement;
 *   options: HTMLButtonElement[];
 *   fallbackToFirst?: boolean;
 *   syncDisabled?: boolean;
 *   describe: (
 *     selected: HTMLButtonElement | undefined,
 *     nativeOption: HTMLOptionElement | undefined
 *   ) => { label: string; meta: string };
 *   matches?: (option: HTMLButtonElement, query: string) => boolean;
 * }} RepositoryPickerConfig
 */

/**
 * Owns the shared accessible repository-picker behavior used by map and Wiki
 * workspaces.
 *
 * @param {RepositoryPickerConfig} config
 */
export function createRepositoryPicker(config) {
  const visibleOptions = () => config.options.filter((option) => !option.hidden);

  const filter = () => {
    const query = config.search.value.trim().toLocaleLowerCase();
    for (const option of config.options) {
      const matches = config.matches
        ? config.matches(option, query)
        : (option.dataset.label?.toLocaleLowerCase() || "").includes(query);
      option.hidden = query !== "" && !matches;
    }
  };

  const sync = () => {
    const exact = config.options.find((option) => option.dataset.value === config.select.value);
    const selected = exact ?? (config.fallbackToFirst ? config.options[0] : undefined);
    const display = config.describe(selected, config.select.selectedOptions[0]);
    config.current.textContent = display.label;
    config.meta.textContent = display.meta;
    if (config.syncDisabled) {
      config.trigger.disabled = config.select.disabled;
    }
    for (const option of config.options) {
      option.setAttribute("aria-selected", String(option === selected));
    }
  };

  const close = (restoreFocus = false) => {
    config.popover.hidden = true;
    config.picker.dataset.open = "false";
    config.trigger.setAttribute("aria-expanded", "false");
    config.search.value = "";
    filter();
    if (restoreFocus) {
      config.trigger.focus();
    }
  };

  const open = (focusSelected = false) => {
    if (config.trigger.disabled) {
      return;
    }
    config.popover.hidden = false;
    config.picker.dataset.open = "true";
    config.trigger.setAttribute("aria-expanded", "true");
    if (focusSelected) {
      (config.options.find((option) => option.getAttribute("aria-selected") === "true")
        ?? visibleOptions()[0])?.focus();
      return;
    }
    config.search.focus();
  };

  const focusOption = (current, offset) => {
    const options = visibleOptions();
    if (options.length === 0) {
      return;
    }
    const currentIndex = options.indexOf(current);
    options[(currentIndex + offset + options.length) % options.length]?.focus();
  };

  const choose = (option) => {
    config.select.value = option.dataset.value || "";
    sync();
    const change = config.select.ownerDocument.createEvent("Event");
    change.initEvent("change", true, false);
    config.select.dispatchEvent(change);
    close(true);
  };

  config.trigger.addEventListener("click", () => {
    if (config.popover.hidden) {
      open();
    } else {
      close();
    }
  });
  config.trigger.addEventListener("keydown", (event) => {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      open(true);
    }
  });
  config.backdrop.addEventListener("click", () => close(true));
  config.search.addEventListener("input", filter);
  config.search.addEventListener("keydown", (event) => {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      visibleOptions()[0]?.focus();
    } else if (event.key === "Enter") {
      const options = visibleOptions();
      if (options.length === 1) {
        event.preventDefault();
        choose(options[0]);
      }
    } else if (event.key === "Escape") {
      event.preventDefault();
      close(true);
    }
  });
  for (const option of config.options) {
    option.addEventListener("click", () => choose(option));
    option.addEventListener("keydown", (event) => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        choose(option);
      } else if (event.key === "ArrowDown") {
        event.preventDefault();
        focusOption(option, 1);
      } else if (event.key === "ArrowUp") {
        event.preventDefault();
        focusOption(option, -1);
      } else if (event.key === "Home") {
        event.preventDefault();
        visibleOptions()[0]?.focus();
      } else if (event.key === "End") {
        event.preventDefault();
        visibleOptions().at(-1)?.focus();
      } else if (event.key === "Escape") {
        event.preventDefault();
        close(true);
      }
    });
  }
  config.picker.ownerDocument.addEventListener("pointerdown", (event) => {
    const target = /** @type {Node | null} */ (event.target);
    if (
      !config.popover.hidden &&
      target !== null &&
      !config.picker.contains(target)
    ) {
      close();
    }
  });

  return { close, filter, open, sync, visibleOptions };
}
