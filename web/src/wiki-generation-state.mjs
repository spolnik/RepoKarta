/**
 * @typedef {"survey" | "plan" | "pages"} WikiRunStageID
 * @typedef {"pending" | "active" | "done" | "reused" | "failed"} WikiRunStageState
 * @typedef {"queued" | "active" | "done" | "failed"} WikiRunTargetState
 * @typedef {{
 *   slug: string,
 *   number: string,
 *   title: string,
 *   state: WikiRunTargetState
 * }} WikiRunTarget
 * @typedef {{
 *   status: "idle" | "running" | "completed" | "failed" | "cancelled",
 *   stages: Record<WikiRunStageID, WikiRunStageState>,
 *   stageNotes: Record<WikiRunStageID, string>,
 *   targets: WikiRunTarget[]
 * }} WikiGenerationState
 * @typedef {
 *   {type: "reset"} |
 *   {type: "stage", stage: WikiRunStageID, state: WikiRunStageState, note?: string} |
 *   {type: "targets", targets: Array<Omit<WikiRunTarget, "state">>} |
 *   {type: "target", index: number, state: WikiRunTargetState} |
 *   {type: "complete"} |
 *   {type: "fail"} |
 *   {type: "cancel"}
 * } WikiGenerationEvent
 */

/** @returns {WikiGenerationState} */
export function createWikiGenerationState() {
  return {
    status: "idle",
    stages: {
      survey: "pending",
      plan: "pending",
      pages: "pending",
    },
    stageNotes: {
      survey: "",
      plan: "",
      pages: "",
    },
    targets: [],
  };
}

/**
 * @param {WikiGenerationState} state
 * @param {WikiGenerationEvent} event
 * @returns {WikiGenerationState}
 */
export function reduceWikiGeneration(state, event) {
  if (event.type === "reset") {
    return { ...createWikiGenerationState(), status: "running" };
  }
  if (event.type === "stage") {
    return {
      ...state,
      status: event.state === "failed" ? "failed" : "running",
      stages: { ...state.stages, [event.stage]: event.state },
      stageNotes: {
        ...state.stageNotes,
        [event.stage]: event.note ?? state.stageNotes[event.stage],
      },
    };
  }
  if (event.type === "targets") {
    return {
      ...state,
      targets: event.targets.map((target) => ({ ...target, state: "queued" })),
    };
  }
  if (event.type === "target") {
    return {
      ...state,
      targets: state.targets.map((target, index) =>
        index === event.index ? { ...target, state: event.state } : target
      ),
    };
  }
  return {
    ...state,
    status: event.type === "complete"
      ? "completed"
      : event.type === "cancel"
        ? "cancelled"
        : "failed",
  };
}

/** @param {WikiGenerationState} state */
export function wikiGenerationProgress(state) {
  if (state.targets.length > 0) {
    const done = state.targets.filter((target) => target.state === "done").length;
    return {
      kind: "pages",
      done,
      total: state.targets.length,
      percentage: Math.round((done / state.targets.length) * 100),
    };
  }
  const done = /** @type {WikiRunStageID[]} */ (["survey", "plan", "pages"])
    .filter((stage) =>
      state.stages[stage] === "done" || state.stages[stage] === "reused"
    ).length;
  return {
    kind: "stages",
    done,
    total: 3,
    percentage: Math.round((done / 3) * 100),
  };
}
