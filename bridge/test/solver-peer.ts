// Plays the Queue page's side of a direct Session: offers a WebRTC data
// channel to the Bridge through the fake backend, presents a peer token once
// the channel opens and collects what the Bridge sends over it.

import { RTCPeerConnection } from "werift";

import type { Bridge } from "./fake-unstuck.ts";

export type SolverPeer = {
  send(msg: object): void;
  /** Resolves with the next text message of type, or rejects after ms. */
  next(type: string, ms: number): Promise<Record<string, unknown>>;
  /** Frames fully received so far. */
  frames: { metadata: unknown; bytes: Buffer }[];
  /** Resolves once the channel closes, from either side. */
  closed: Promise<void>;
  close(): void;
};

export async function connectPeer(bridge: Bridge, token: string, answerMs = 5_000): Promise<SolverPeer> {
  const pc = new RTCPeerConnection({ iceServers: [] });
  const channel = pc.createDataChannel("unstuck");
  // As the Queue page does: the channel's close reaches the Bridge, the
  // connection's does not.
  const close = () => {
    channel.close();
    setTimeout(() => void pc.close().catch(() => {}), 200);
  };

  const received: Record<string, unknown>[] = [];
  const waiting: { type: string; resolve: (m: Record<string, unknown>) => void }[] = [];
  const frames: SolverPeer["frames"] = [];
  let assembling: { metadata: unknown; size: number; parts: Buffer[]; got: number } | undefined;
  // Sends are best effort: the Bridge may drop this peer at any time.
  const send = (msg: object) => {
    try {
      channel.send(JSON.stringify(msg));
    } catch {}
  };
  let closed!: () => void;
  const closedP = new Promise<void>((resolve) => (closed = resolve));
  const opened = new Promise<void>((resolve) => {
    channel.stateChanged.subscribe((state) => {
      if (state === "open") {
        send({ type: "hello", peer_token: token });
        resolve();
      }
      if (state === "closed") closed();
    });
  });
  channel.onMessage.subscribe((data) => {
    if (typeof data !== "string") {
      if (!assembling) throw new Error("frame bytes without a frame header");
      assembling.parts.push(data);
      assembling.got += data.length;
      if (assembling.got >= assembling.size) {
        frames.push({ metadata: assembling.metadata, bytes: Buffer.concat(assembling.parts) });
        assembling = undefined;
      }
      return;
    }
    const m = JSON.parse(data) as Record<string, unknown>;
    if (m.type === "frame") assembling = { metadata: m.metadata, size: Number(m.size), parts: [], got: 0 };
    const i = waiting.findIndex((w) => w.type === m.type);
    if (i >= 0) waiting.splice(i, 1)[0].resolve(m);
    else received.push(m);
  });

  await pc.setLocalDescription(await pc.createOffer());
  bridge.send({ type: "rtc_offer", sdp: pc.localDescription!.sdp });
  const answer = await bridge.next("rtc_answer", answerMs).catch((err: unknown) => {
    close();
    throw err;
  });
  await pc.setRemoteDescription({ type: "answer", sdp: String(answer.sdp) });
  await opened;

  return {
    send,
    frames,
    closed: closedP,
    close,
    next: (type, ms) => {
      const i = received.findIndex((m) => m.type === type);
      if (i >= 0) return Promise.resolve(received.splice(i, 1)[0]);
      return new Promise((resolve, reject) => {
        const w = {
          type,
          resolve: (m: Record<string, unknown>) => {
            clearTimeout(timer);
            resolve(m);
          },
        };
        const timer = setTimeout(() => {
          waiting.splice(waiting.indexOf(w), 1);
          reject(new Error(`peer: timed out waiting for ${type}`));
        }, ms);
        waiting.push(w);
      });
    },
  };
}
