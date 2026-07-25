export interface WikiActionSite {
  planReady: boolean;
  surveyReady: boolean;
  planStale: boolean;
  stale: number;
  pending: number;
  failed: number;
}

export interface WikiPrimaryAction {
  disabled: boolean;
  mode: "view" | "generate";
  label: string;
}

export function wikiPrimaryAction(
  generating: boolean,
  providerReady: boolean,
  site: WikiActionSite
): WikiPrimaryAction;
