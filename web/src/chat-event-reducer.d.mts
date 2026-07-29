import type { ConversationEvent } from "./generated/api-contract";

export interface ChatEventReducerState {
  buffer: string;
  malformed: number;
}

export interface MalformedChatEvent {
  line: string;
  reason: string;
}

export function createChatEventReducerState(): ChatEventReducerState;
export function reduceChatEventChunk(
  state: ChatEventReducerState,
  chunk: string,
  done?: boolean
): {
  state: ChatEventReducerState;
  events: ConversationEvent[];
  malformed: MalformedChatEvent[];
};
