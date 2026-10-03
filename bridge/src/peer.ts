// A direct WebRTC connection between the Bridge and the claiming Solver's
// Queue page. The backend relays the offer and answer; frames and input then
// skip it. The Solver's peer must first present the Session's peer token,
// which only the backend and the claimant have.
//
// WebRTC comes from werift, an optional dependency in plain TypeScript: no
// native threads, so it cannot keep the Agent's process alive once closed.
// Without it the Bridge never answers, and the Session stays on the relay.

import { timingSafeEqual } from "node:crypto";

import { type Input, isInput } from "./input.ts";

/** A screencast frame as CDP delivers it. */
export type ScreencastFrame = { data: string; metadata: object };

/** An ICE server as the backend sends it, in RTCPeerConnection's shape. */
export type IceServer = { urls: string | string[]; username?: string; credential?: string };

/** Frames go in pieces no larger than this, well under any browser's SCTP message limit. */
const CHUNK_BYTES = 16 << 10;
const HELLO_TIMEOUT_MS = 5_000;
const CLOSE_GRACE_MS = 200;

export interface PeerEvents {
  /** The peer presented the token; frames may go to it from now on. */
  onReady(): void;
  onInput(m: Input): void;
  /** The connection closed, from either side. Called at most once. */
  onClose(): void;
}

type Werift = typeof import("werift");
type PeerConnection = InstanceType<Werift["RTCPeerConnection"]>;
type DataChannel = InstanceType<Werift["RTCDataChannel"]>;

let werift: Promise<Werift | undefined> | undefined;

function loadWebRTC() {
  werift ??= import("werift").catch((err: Error) => {
    console.warn(`Unstuck: WebRTC unavailable (${err.message}); Sessions stay relayed.`);
    return undefined;
  });
  return werift;
}

export class Peer {
  #pc: PeerConnection | undefined;
  #channel: DataChannel | undefined;
  #ready = false;
  #closed = false;
  #helloTimer: ReturnType<typeof setTimeout> | undefined;

  constructor(
    private readonly token: string,
    private readonly iceServers: IceServer[],
    private readonly events: PeerEvents,
  ) {}

  /**
   * Answers the Solver's offer. Resolves with the answer SDP, candidates
   * included, so signaling needs no trickle messages; or undefined if it
   * cannot answer.
   */
  async answer(offer: string): Promise<string | undefined> {
    const lib = await loadWebRTC();
    if (!lib || this.#closed) return undefined;
    const pc = new lib.RTCPeerConnection({ iceServers: this.iceServers });
    this.#pc = pc;
    pc.onDataChannel.subscribe((channel) => this.#accept(channel));
    pc.connectionStateChange.subscribe((state) => {
      // A Solver whose network drops goes back to the relay at once rather
      // than after ICE gives up.
      if (state === "disconnected" || state === "failed" || state === "closed") this.close();
    });
    try {
      await pc.setRemoteDescription({ type: "offer", sdp: offer });
      await pc.setLocalDescription(await pc.createAnswer());
      return this.#closed ? undefined : pc.localDescription?.sdp;
    } catch (err) {
      console.warn("Unstuck: answering the Solver's offer:", err);
      this.close();
      return undefined;
    }
  }

  get ready() {
    return this.#ready && !this.#closed;
  }

  /** Reports whether the channel is too far behind to take another frame. */
  busy(maxBuffered: number) {
    return (this.#channel?.bufferedAmount ?? 0) >= maxBuffered;
  }

  /** Sends a frame as a JSON header followed by its JPEG bytes in pieces. */
  sendFrame(frame: ScreencastFrame) {
    const bytes = Buffer.from(frame.data, "base64");
    this.#send(JSON.stringify({ type: "frame", size: bytes.length, metadata: frame.metadata }));
    for (let i = 0; i < bytes.length; i += CHUNK_BYTES) this.#send(bytes.subarray(i, i + CHUNK_BYTES));
  }

  sendURL(url: string) {
    this.#send(JSON.stringify({ type: "url", url }));
  }

  close() {
    if (this.#closed) return;
    this.#closed = true;
    clearTimeout(this.#helloTimer);
    closeGracefully(this.#pc, this.#channel);
    this.events.onClose();
  }

  #send(data: string | Buffer) {
    if (this.#closed || this.#channel?.readyState !== "open") return;
    try {
      this.#channel.send(data);
    } catch {
      this.close(); // closed under us
    }
  }

  // The first message on the channel must be the peer token; anything else
  // ends the connection before a frame is sent or an input applied.
  #accept(channel: DataChannel) {
    if (this.#channel || this.#closed) {
      channel.close(); // one channel per Session
      return;
    }
    this.#channel = channel;
    this.#helloTimer = setTimeout(() => this.close(), HELLO_TIMEOUT_MS);
    channel.stateChanged.subscribe((state) => {
      if (state === "closing" || state === "closed") this.close();
    });
    channel.onMessage.subscribe((data) => {
      let m: Record<string, unknown>;
      try {
        m = JSON.parse(typeof data === "string" ? data : "");
      } catch {
        return this.close();
      }
      if (!this.#ready) {
        clearTimeout(this.#helloTimer);
        if (m?.type !== "hello" || !sameToken(m.peer_token, this.token)) return this.close();
        this.#ready = true;
        this.#send(JSON.stringify({ type: "welcome" }));
        this.events.onReady();
        return;
      }
      if (isInput(m)) this.events.onInput(m);
    });
  }
}

// The channel's close reaches the other side, the connection's does not:
// the connection closes only once the channel's reset has gone out.
function closeGracefully(pc: PeerConnection | undefined, channel: DataChannel | undefined) {
  if (channel && channel.readyState !== "closed") {
    channel.close();
    setTimeout(() => void pc?.close().catch(() => {}), CLOSE_GRACE_MS);
    return;
  }
  void pc?.close().catch(() => {});
}

function sameToken(given: unknown, want: string) {
  if (typeof given !== "string" || want === "") return false;
  const a = Buffer.from(given);
  const b = Buffer.from(want);
  return a.length === b.length && timingSafeEqual(a, b);
}
