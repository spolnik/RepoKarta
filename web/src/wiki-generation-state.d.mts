export type WikiRunStageID = "survey" | "plan" | "pages";
export type WikiRunStageState = "pending" | "active" | "done" | "reused" | "failed";
export type WikiRunTargetState = "queued" | "active" | "done" | "failed";

export interface WikiRunTarget {
  slug: string;
  number: string;
  title: string;
  state: WikiRunTargetState;
}

export interface WikiGenerationState {
  status: "idle" | "running" | "completed" | "failed" | "cancelled";
  stages: Record<WikiRunStageID, WikiRunStageState>;
  stageNotes: Record<WikiRunStageID, string>;
  targets: WikiRunTarget[];
}

export type WikiGenerationEvent =
  | { type: "reset" }
  | { type: "stage"; stage: WikiRunStageID; state: WikiRunStageState; note?: string }
  | { type: "targets"; targets: Array<Omit<WikiRunTarget, "state">> }
  | { type: "target"; index: number; state: WikiRunTargetState }
  | { type: "complete" }
  | { type: "fail" }
  | { type: "cancel" };

export function createWikiGenerationState(): WikiGenerationState;
export function reduceWikiGeneration(
  state: WikiGenerationState,
  event: WikiGenerationEvent
): WikiGenerationState;
export function wikiGenerationProgress(state: WikiGenerationState): {
  kind: "pages" | "stages";
  done: number;
  total: number;
  percentage: number;
};
