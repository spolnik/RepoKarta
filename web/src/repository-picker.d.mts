export interface RepositoryPickerConfig {
  select: HTMLSelectElement;
  picker: HTMLElement;
  trigger: HTMLButtonElement;
  current: HTMLElement;
  meta: HTMLElement;
  backdrop: HTMLElement;
  popover: HTMLElement;
  search: HTMLInputElement;
  options: HTMLButtonElement[];
  fallbackToFirst?: boolean;
  syncDisabled?: boolean;
  describe: (
    selected: HTMLButtonElement | undefined,
    nativeOption: HTMLOptionElement | undefined
  ) => { label: string; meta: string };
  matches?: (option: HTMLButtonElement, query: string) => boolean;
}

export interface RepositoryPicker {
  close(restoreFocus?: boolean): void;
  filter(): void;
  open(focusSelected?: boolean): void;
  sync(): void;
  visibleOptions(): HTMLButtonElement[];
}

export function createRepositoryPicker(config: RepositoryPickerConfig): RepositoryPicker;
