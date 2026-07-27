export type ContextualChatLaunch = {
  mode?: string;
  prompt: string;
  contextURL?: string;
};

export declare function normaliseContextMode(value?: string | null): string;
export declare function buildContextualChatURL(baseURL: string, launch: ContextualChatLaunch): string;
