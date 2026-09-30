// A fake Overpass backend that accepts one Task and hands the test its
// Bridge socket, so a test can play the backend's and the Solver's side.

import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { WebSocketServer, type WebSocket } from "ws";

export type Bridge = {
  send(msg: object): void;
  /** Resolves with the next message of type, or rejects after ms. */
  next(type: string, ms: number): Promise<Record<string, unknown>>;
  /** The newest message of type received so far and not taken by next. */
  latest(type: string): Record<string, unknown> | undefined;
  /** How many messages of type have arrived in all. */
  count(type: string): number;
};

// fakeOverpass accepts one Task and hands the test its Bridge socket.
export async function fakeOverpass() {
  const server = createServer((req, res) => {
    if (req.method === "POST" && req.url === "/v1/tasks") {
      res.writeHead(201, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ task_id: "task-1", session_token: "token-1" }));
      return;
    }
    res.writeHead(404).end();
  });
  const wss = new WebSocketServer({ server });
  const bridge = new Promise<Bridge>((resolve) => {
    wss.on("connection", (ws: WebSocket) => {
      // Messages are kept until asked for, so none is lost to a race.
      const received: Record<string, unknown>[] = [];
      const waiting: { type: string; resolve: (m: Record<string, unknown>) => void }[] = [];
      const counts = new Map<unknown, number>();
      ws.on("message", (data) => {
        const m = JSON.parse(String(data)) as Record<string, unknown>;
        counts.set(m.type, (counts.get(m.type) ?? 0) + 1);
        const i = waiting.findIndex((w) => w.type === m.type);
        if (i >= 0) waiting.splice(i, 1)[0].resolve(m);
        else received.push(m);
      });
      resolve({
        send: (msg) => ws.send(JSON.stringify(msg)),
        latest: (type) => received.findLast((m) => m.type === type),
        count: (type) => counts.get(type) ?? 0,
        next: (type, ms) => {
          const i = received.findIndex((m) => m.type === type);
          if (i >= 0) return Promise.resolve(received.splice(i, 1)[0]);
          return new Promise((resolve, reject) => {
            const timer = setTimeout(() => {
              waiting.splice(waiting.indexOf(w), 1);
              reject(new Error(`timed out waiting for ${type}`));
            }, ms);
            const w = {
              type,
              resolve: (m: Record<string, unknown>) => {
                clearTimeout(timer);
                resolve(m);
              },
            };
            waiting.push(w);
          });
        },
      });
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    bridge,
    close: async () => {
      for (const ws of wss.clients) ws.terminate();
      wss.close();
      await new Promise((resolve) => server.close(resolve));
    },
  };
}
