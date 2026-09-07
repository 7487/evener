// hubUpdate.ts drives Settings -> Hub -> Updates: pick a release channel,
// ask the hub whether that channel is ahead of the running build
// (evener/update/check), and apply it (evener/update/apply). Apply is the
// interesting half: the hub answers, then execs the new binary in place, so
// the WebSocket drops and the page has to notice the NEW hub on its own.
// It does that by polling /api/health (auth-exempt, always registered - see
// shell/chrome/webNotBuilt.ts's note) until `version` differs from the
// value the check recorded, then reloads so the SPA bundle matches the
// backend. Fetch failures while polling are the expected mid-restart state.
//
// Like settingsOverview.ts, this store reads connectionStore's current
// client at call time and holds no subscription of its own.

import { useStore } from "zustand";
import { createStore } from "zustand/vanilla";
import { errorText } from "../protocol/errors";
import type { AppwireClientLike } from "../protocol/testing/fakeClient";
import type { UpdateCheckResponse } from "../protocol/types.gen";
import { connectionStore } from "./connection";

export type UpdateChannel = "release" | "snapshot";

export const RESTART_POLL_MS = 1000;
export const RESTART_TIMEOUT_MS = 30_000;

export interface HubUpdateStoreState {
  // null until the section seeds it from the overview's buildChannel; the
  // hub then falls back to its own upgrade channel for an empty request.
  channel: UpdateChannel | null;
  check: UpdateCheckResponse | null;
  checking: boolean;
  checkError: string | null;
  applying: boolean;
  applyError: string | null;
  restarting: boolean;
  restartTimedOut: boolean;
  setChannel(channel: UpdateChannel): void;
  runCheck(): Promise<void>;
  apply(): Promise<void>;
}

interface Deps {
  fetchImpl: typeof fetch;
  reload: () => void;
}

let deps: Deps = { fetchImpl: (...args) => fetch(...args), reload: () => window.location.reload() };

function requireClient(): AppwireClientLike {
  const client = connectionStore.getState().client;
  if (!client) {
    throw new Error("hubUpdate store: no client connected; call useConnectionStore.getState().connect(client) first");
  }
  return client;
}

async function healthVersion(): Promise<string | null> {
  try {
    const response = await deps.fetchImpl("/api/health", { credentials: "same-origin" });
    if (!response.ok) return null;
    const body = (await response.json()) as { version?: string };
    return typeof body.version === "string" ? body.version : null;
  } catch {
    return null; // the hub is mid-restart; keep polling
  }
}

// waitForNewHub polls until /api/health reports a version other than
// previous, then reloads. Resolves after reload() or the timeout.
async function waitForNewHub(previous: string | null): Promise<void> {
  const deadline = Date.now() + RESTART_TIMEOUT_MS;
  while (Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, RESTART_POLL_MS));
    const version = await healthVersion();
    if (version !== null && version !== previous) {
      hubUpdateStore.setState({ restarting: false });
      deps.reload();
      return;
    }
  }
  hubUpdateStore.setState({ restarting: false, restartTimedOut: true });
}

const INITIAL = {
  channel: null,
  check: null,
  checking: false,
  checkError: null,
  applying: false,
  applyError: null,
  restarting: false,
  restartTimedOut: false,
} as const;

export const hubUpdateStore = createStore<HubUpdateStoreState>((set, get) => ({
  ...INITIAL,

  setChannel(channel) {
    set({ channel, check: null, checking: false, checkError: null, applyError: null });
  },

  async runCheck() {
    set({ checking: true, checkError: null });
    const channel = get().channel;
    try {
      const check = await requireClient().request("evener/update/check", { channel: channel ?? "" });
      if (get().channel === channel) {
        set({ check, checking: false });
      }
    } catch (err) {
      if (get().channel === channel) {
        set({ check: null, checking: false, checkError: errorText(err) });
      }
    }
  },

  async apply() {
    if (get().applying || get().restarting) {
      return;
    }
    const check = get().check;
    if (!check) {
      set({ applyError: "Check for updates first" });
      return;
    }
    set({ applying: true, applyError: null, restartTimedOut: false });
    const previous = check.currentVersion;
    try {
      await requireClient().request("evener/update/apply", { channel: get().channel ?? "" });
    } catch (err) {
      set({ applying: false, applyError: errorText(err) });
      return;
    }
    set({ applying: false, restarting: true });
    await waitForNewHub(previous);
  },
}));

export function useHubUpdateStore(): HubUpdateStoreState;
export function useHubUpdateStore<T>(selector: (state: HubUpdateStoreState) => T): T;
export function useHubUpdateStore<T>(selector?: (state: HubUpdateStoreState) => T): T | HubUpdateStoreState {
  // Not a real conditional hook call - see stores/connection.ts's own
  // useConnectionStore for the full explanation (zustand's useStore has a
  // `selector = identity` JS default param, so both arms run identically).
  // biome-ignore lint/correctness/useHookAtTopLevel: same hook both arms, JS default param not a real conditional - see stores/connection.ts
  return selector ? useStore(hubUpdateStore, selector) : useStore(hubUpdateStore);
}

// resetHubUpdateStoreForTests resets state and lets tests inject the
// health fetch and reload so the restart poll runs under fake timers with
// no real network and no real navigation. Production never calls this.
export function resetHubUpdateStoreForTests(overrides: Partial<Deps> = {}): void {
  deps = {
    fetchImpl: overrides.fetchImpl ?? ((...args) => fetch(...args)),
    reload: overrides.reload ?? (() => window.location.reload()),
  };
  hubUpdateStore.setState({ ...INITIAL });
}
