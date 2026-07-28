/**
 * Keeps one malformed connection from invalidating an otherwise useful graph.
 *
 * @template {{ id: string }} Component
 * @template {{ source: string, target: string }} Connection
 * @param {Component[]} components
 * @param {Connection[]} connections
 * @returns {{ connections: Connection[], hiddenCount: number }}
 */
export function filterTopologyConnections(components, connections) {
  const componentIDs = new Set(components.map((component) => component.id));
  const filtered = connections.filter((connection) =>
    componentIDs.has(connection.source) && componentIDs.has(connection.target)
  );
  return {
    connections: filtered,
    hiddenCount: connections.length - filtered.length
  };
}
