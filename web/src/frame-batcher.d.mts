export type FrameBatcher = {
  append: (text: string) => void;
  finish: () => number;
  cancel: () => void;
};

export function createFrameBatcher(options: {
  schedule: (callback: () => void) => unknown;
  cancel: (handle: unknown) => void;
  write: (text: string) => void;
}): FrameBatcher;
