import { act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { FakeClient } from "../protocol/testing/fakeClient";
import type { UpdateCheckResponse } from "../protocol/types.gen";
import { connectionStore } from "./connection";
import { hubUpdateStore, RESTART_POLL_MS, RESTART_TIMEOUT_MS, resetHubUpdateStoreForTests } from "./hubUpdate";
import { resetThreadsStoreForTests } from "./threads";

function connectFakeClient(): FakeClient {
  const fake = new FakeClient("ready");
  connectionStore.getState().connect(fake);
  return fake;
}

const UP_TO_DATE: UpdateCheckResponse = {
  channel: "snapshot",
  buildChannel: "snapshot",
  currentVersion: "be70029",
  currentCommit: "be70029",
  latestTag: "snapshot",
  latestCommit: "be7002918fdc60dbdeab71d9dd17e00d3d006c56",
  updateAvailable: false,
  applicable: true,
};

function healthFetch(versions: string[]): typeof fetch {
  let i = 0;
  return vi.fn(async () => {
    const version = versions[Math.min(i, versions.length - 1)];
    i += 1;
    if (version === "DOWN") throw new TypeError("Failed to fetch");
    return new Response(JSON.stringify({ version }), { status: 200 });
  }) as unknown as typeof fetch;
}

beforeEach(() => {
  connectionStore.setState({ state: "idle", serverInfo: undefined, client: null });
  resetThreadsStoreForTests();
  resetHubUpdateStoreForTests();
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("runCheck", () => {
  test("requests evener/update/check with the selected channel and stores the result", async () => {
    const fake = connectFakeClient();
    fake.on("evener/update/check", () => UP_TO_DATE);
    hubUpdateStore.getState().setChannel("snapshot");

    await act(() => hubUpdateStore.getState().runCheck());

    expect(fake.calls).toEqual([{ method: "evener/update/check", params: { channel: "snapshot" } }]);
    const state = hubUpdateStore.getState();
    expect(state.check).toEqual(UP_TO_DATE);
    expect(state.checking).toBe(false);
    expect(state.checkError).toBeNull();
  });

  test("sends an empty channel when none is selected yet", async () => {
    const fake = connectFakeClient();
    fake.on("evener/update/check", () => UP_TO_DATE);

    await act(() => hubUpdateStore.getState().runCheck());

    expect(fake.calls).toEqual([{ method: "evener/update/check", params: { channel: "" } }]);
  });

  test("stores the error text and clears the previous result on failure", async () => {
    const fake = connectFakeClient();
    fake.on("evener/update/check", () => {
      throw new Error("GET x: 403 Forbidden: API rate limit exceeded");
    });

    await act(() => hubUpdateStore.getState().runCheck());

    const state = hubUpdateStore.getState();
    expect(state.check).toBeNull();
    expect(state.checkError).toContain("rate limit");
  });

  test("setChannel clears the stale check result", async () => {
    const fake = connectFakeClient();
    fake.on("evener/update/check", () => UP_TO_DATE);
    await act(() => hubUpdateStore.getState().runCheck());

    act(() => hubUpdateStore.getState().setChannel("release"));

    expect(hubUpdateStore.getState().channel).toBe("release");
    expect(hubUpdateStore.getState().check).toBeNull();
  });
});

describe("apply", () => {
  test("requests evener/update/apply, then polls /api/health until the version changes and reloads", async () => {
    vi.useFakeTimers();
    const reload = vi.fn();
    const fetchImpl = healthFetch(["be70029", "DOWN", "DOWN", "3b1c5f8"]);
    resetHubUpdateStoreForTests({ fetchImpl, reload });
    const fake = connectFakeClient();
    fake.on("evener/update/check", () => ({ ...UP_TO_DATE, updateAvailable: true, latestCommit: "3b1c5f8aaaa" }));
    fake.on("evener/update/apply", () => ({
      release: "snapshot",
      channel: "snapshot",
      installed: ["/x/evener", "/x/evener-dev"],
      restarting: true,
    }));
    hubUpdateStore.getState().setChannel("snapshot");
    await act(() => hubUpdateStore.getState().runCheck());

    act(() => {
      void hubUpdateStore.getState().apply();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(fake.calls[1]).toEqual({ method: "evener/update/apply", params: { channel: "snapshot" } });
    expect(hubUpdateStore.getState().restarting).toBe(true);
    expect(hubUpdateStore.getState().applying).toBe(false);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(RESTART_POLL_MS * 4);
    });

    expect(reload).toHaveBeenCalledTimes(1);
    expect(hubUpdateStore.getState().restartTimedOut).toBe(false);
  });

  test("gives up after RESTART_TIMEOUT_MS when the version never changes", async () => {
    vi.useFakeTimers();
    const reload = vi.fn();
    resetHubUpdateStoreForTests({ fetchImpl: healthFetch(["be70029"]), reload });
    const fake = connectFakeClient();
    fake.on("evener/update/check", () => ({ ...UP_TO_DATE, updateAvailable: true }));
    fake.on("evener/update/apply", () => ({
      release: "snapshot",
      channel: "snapshot",
      installed: ["/x/evener"],
      restarting: true,
    }));
    await act(() => hubUpdateStore.getState().runCheck());
    act(() => {
      void hubUpdateStore.getState().apply();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(RESTART_TIMEOUT_MS + RESTART_POLL_MS);
    });

    expect(reload).not.toHaveBeenCalled();
    expect(hubUpdateStore.getState().restarting).toBe(false);
    expect(hubUpdateStore.getState().restartTimedOut).toBe(true);
  });

  test("stores applyError and does not poll when the hub refuses", async () => {
    vi.useFakeTimers();
    const fetchImpl = healthFetch(["be70029"]);
    resetHubUpdateStoreForTests({ fetchImpl, reload: vi.fn() });
    const fake = connectFakeClient();
    fake.on("evener/update/apply", () => {
      throw new Error("this hub is a dev build");
    });

    await act(() => hubUpdateStore.getState().apply());
    await act(async () => {
      await vi.advanceTimersByTimeAsync(RESTART_POLL_MS * 2);
    });

    expect(hubUpdateStore.getState().applyError).toContain("dev build");
    expect(hubUpdateStore.getState().restarting).toBe(false);
    expect(fetchImpl).not.toHaveBeenCalled();
  });
});
