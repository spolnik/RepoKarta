export function replaceRenderedRegions(
  current: ParentNode,
  fresh: ParentNode,
  selectors: string[]
): { replaced: string[]; missing: string[] };
