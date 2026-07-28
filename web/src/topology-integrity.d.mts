export function filterTopologyConnections<
  Component extends { id: string },
  Connection extends { source: string; target: string }
>(
  components: Component[],
  connections: Connection[]
): { connections: Connection[]; hiddenCount: number };
