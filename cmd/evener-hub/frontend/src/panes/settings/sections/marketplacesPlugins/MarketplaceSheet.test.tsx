import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { FakeClient } from "../../../../protocol/testing/fakeClient";
import type { MarketplaceEntry } from "../../../../protocol/types.gen";
import { connectionStore } from "../../../../stores/connection";
import { extensionsStore, resetExtensionsStoreForTests } from "../../../../stores/extensions";
import { Toast } from "../../../../widgets";
import { getToasts, resetToastStoreForTests } from "../../../../widgets/toast/store";
import { MarketplaceSheet } from "./MarketplaceSheet";

const ACME: MarketplaceEntry = {
  name: "acme",
  source: { kind: "github", repo: "acme/plugins" },
  installLocation: "/home/u/.config/evener/plugins/marketplaces/acme",
  lastUpdated: 1_700_000_000,
};

function connectFakeClient(): FakeClient {
  const fake = new FakeClient("ready");
  connectionStore.getState().connect(fake);
  return fake;
}

function renderSheet(entry: MarketplaceEntry | null, expanded: Set<string> = new Set()) {
  const onClose = vi.fn();
  const onRenamed = vi.fn();
  extensionsStore.setState({ marketplaces: entry === null ? [] : [entry] });
  render(
    <>
      <Toast />
      <MarketplaceSheet
        name={entry?.name ?? null}
        onClose={onClose}
        onRenamed={onRenamed}
        expandedMarketplaces={expanded}
      />
    </>,
  );
  return { onClose, onRenamed };
}

function field(label: string): HTMLInputElement {
  return screen.getByLabelText(label) as HTMLInputElement;
}
function saveButton(): HTMLButtonElement {
  return screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
}

beforeEach(() => {
  connectionStore.setState({ state: "idle", serverInfo: undefined, client: null });
  resetExtensionsStoreForTests();
  resetToastStoreForTests();
  connectFakeClient();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

test("renders nothing when name is null", () => {
  renderSheet(null);
  expect(screen.queryByRole("dialog")).toBeNull();
});

test("prefills the name, the source kind, and its field; shows install location and last updated", () => {
  renderSheet(ACME);
  expect(screen.getByRole("dialog", { name: "acme" })).toBeTruthy();
  expect(field("Name").value).toBe("acme");
  // RadioGroup renders <button role="radio" aria-checked>, not an <input>, so
  // the checked state reads off the attribute (the idiom in index.test.tsx).
  expect(screen.getByRole("radio", { name: "owner/repo" }).getAttribute("aria-checked")).toBe("true");
  // The kind's input shares its label text with the radio, so query it by
  // placeholder - the same idiom MarketplacesSection.test.tsx uses.
  expect((screen.getByPlaceholderText("owner/repo") as HTMLInputElement).value).toBe("acme/plugins");
  expect(screen.queryByPlaceholderText("https://github.com/owner/repo.git")).toBeNull();
  expect(screen.getByText("/home/u/.config/evener/plugins/marketplaces/acme")).toBeTruthy();
  expect(screen.getByText("Last updated")).toBeTruthy();
});

test("Save is disabled until something changes", async () => {
  renderSheet(ACME);
  expect(saveButton().disabled).toBe(true);
  const user = userEvent.setup();
  await user.type(field("Name"), "2");
  expect(saveButton().disabled).toBe(false);
});

test("switching the kind shows only that kind's field and the re-fetch note", async () => {
  renderSheet(ACME);
  const user = userEvent.setup();
  await user.click(screen.getByRole("radio", { name: "Git URL" }));
  expect(screen.getByPlaceholderText("https://github.com/owner/repo.git")).toBeTruthy();
  expect(screen.queryByPlaceholderText("owner/repo")).toBeNull();
  expect(screen.getByText(/Saving re-fetches the marketplace/)).toBeTruthy();
});

test("a kind picked but not filled in keeps Save disabled, even with the name edited", async () => {
  renderSheet(ACME);
  const user = userEvent.setup();
  await user.click(screen.getByRole("radio", { name: "Git URL" }));
  await user.type(field("Name"), "2");
  // The name alone would be a rename, but saving now would silently keep the
  // old source while the picker says otherwise.
  expect(saveButton().disabled).toBe(true);
  await user.type(screen.getByPlaceholderText("https://github.com/owner/repo.git"), "https://x/y.git");
  expect(saveButton().disabled).toBe(false);
});

test("Save sends a rename and the section re-selects the new name without the sheet closing", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  fake.on("evener/marketplace/edit", (params) => {
    expect(params).toEqual({ name: "acme", newName: "acme2" });
    return { marketplaces: [{ ...ACME, name: "acme2" }] };
  });
  const { onClose, onRenamed } = renderSheet(ACME);
  const user = userEvent.setup();
  await user.type(field("Name"), "2");
  await user.click(saveButton());
  await waitFor(() => expect(onRenamed).toHaveBeenCalledWith("acme2"));
  expect(getToasts().some((t) => t.text === "Saved acme2")).toBe(true);
  expect(onClose).not.toHaveBeenCalled();
});

test("Save sends a changed source and reseeds the form from the refreshed entry", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  fake.on("evener/marketplace/edit", (params) => {
    expect(params).toEqual({ name: "acme", source: { kind: "url", url: "https://x/y.git" } });
    return { marketplaces: [{ ...ACME, source: { kind: "url", url: "https://x/y.git" } }] };
  });
  renderSheet(ACME);
  const user = userEvent.setup();
  await user.click(screen.getByRole("radio", { name: "Git URL" }));
  await user.type(screen.getByPlaceholderText("https://github.com/owner/repo.git"), "https://x/y.git");
  await user.click(saveButton());
  await waitFor(() => expect(getToasts().some((t) => t.text === "Saved acme")).toBe(true));
  expect(saveButton().disabled).toBe(true);
  expect((screen.getByPlaceholderText("https://github.com/owner/repo.git") as HTMLInputElement).value).toBe(
    "https://x/y.git",
  );
});

test("a failed save shows the error inline and toasts", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  fake.on("evener/marketplace/edit", () => {
    throw new Error("clone failed");
  });
  renderSheet(ACME);
  const user = userEvent.setup();
  await user.type(field("Name"), "2");
  await user.click(saveButton());
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("clone failed"));
  expect(getToasts().some((t) => t.text.startsWith("Save failed"))).toBe(true);
  expect(field("Name").value).toBe("acme2");
});

test("Refresh calls refreshMarketplace, is busy in flight, toasts, and re-browses an expanded marketplace", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  let release: (() => void) | undefined;
  fake.on(
    "evener/marketplace/refresh",
    (params) =>
      new Promise((resolve) => {
        expect(params).toEqual({ name: "acme" });
        release = () => resolve({ marketplaces: [ACME] });
      }),
  );
  fake.on("evener/marketplace/browse", () => ({ name: "acme", description: "", plugins: [] }));
  renderSheet(ACME, new Set(["acme"]));
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Refresh" }));
  expect((screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement).disabled).toBe(true);
  act(() => release?.());
  await waitFor(() => expect(getToasts().some((t) => t.text === "Refreshed acme")).toBe(true));
  expect((screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement).disabled).toBe(false);
  expect(fake.calls.some((c) => c.method === "evener/marketplace/browse")).toBe(true);
});

test("Refresh leaves a marketplace nothing has expanded un-browsed", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  fake.on("evener/marketplace/refresh", () => ({ marketplaces: [ACME] }));
  renderSheet(ACME);
  await userEvent.setup().click(screen.getByRole("button", { name: "Refresh" }));
  await waitFor(() => expect(getToasts().some((t) => t.text === "Refreshed acme")).toBe(true));
  expect(fake.calls.some((c) => c.method === "evener/marketplace/browse")).toBe(false);
});

test("a failed Refresh toasts and re-enables the button", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  fake.on("evener/marketplace/refresh", () => {
    throw new Error("fetch failed");
  });
  renderSheet(ACME);
  await userEvent.setup().click(screen.getByRole("button", { name: "Refresh" }));
  await waitFor(() => expect(getToasts().some((t) => t.text === "Refresh failed: fetch failed")).toBe(true));
  expect((screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement).disabled).toBe(false);
});

test("Remove opens a confirm; confirming removes, toasts, and the sheet closes when the entry vanishes", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  fake.on("evener/marketplace/remove", (params) => {
    expect(params).toEqual({ name: "acme" });
    return { marketplaces: [] };
  });
  const { onClose } = renderSheet(ACME);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Remove" }));
  const confirm = screen.getByRole("dialog", { name: "Remove marketplace" });
  expect(
    within(confirm).getByText('Remove marketplace "acme"? Installed plugins from it are unaffected.'),
  ).toBeTruthy();
  await user.click(within(confirm).getByRole("button", { name: "Remove" }));
  await waitFor(() => expect(getToasts().some((t) => t.text === "Removed marketplace acme")).toBe(true));
  await waitFor(() => expect(onClose).toHaveBeenCalled());
});

test("cancelling the remove confirm calls nothing", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  renderSheet(ACME);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Remove" }));
  await user.click(
    within(screen.getByRole("dialog", { name: "Remove marketplace" })).getByRole("button", { name: "Cancel" }),
  );
  expect(fake.calls.some((c) => c.method === "evener/marketplace/remove")).toBe(false);
});

test("the confirm dialog's buttons disable while removal is in flight, and it stays open until it resolves", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  let release: (() => void) | undefined;
  fake.on(
    "evener/marketplace/remove",
    () =>
      new Promise((resolve) => {
        release = () => resolve({ marketplaces: [] });
      }),
  );
  renderSheet(ACME);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Remove" }));
  const confirm = screen.getByRole("dialog", { name: "Remove marketplace" });
  await user.click(within(confirm).getByRole("button", { name: "Remove" }));
  await waitFor(() =>
    expect((within(confirm).getByRole("button", { name: "Remove" }) as HTMLButtonElement).disabled).toBe(true),
  );
  expect((within(confirm).getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByRole("dialog", { name: "Remove marketplace" })).toBeTruthy();
  act(() => release?.());
  await waitFor(() => expect(getToasts().some((t) => t.text === "Removed marketplace acme")).toBe(true));
  expect(screen.queryByRole("dialog", { name: "Remove marketplace" })).toBeNull();
});

test("closes itself when the entry disappears from the store", async () => {
  const { onClose } = renderSheet(ACME);
  act(() => extensionsStore.setState({ marketplaces: [] }));
  await waitFor(() => expect(onClose).toHaveBeenCalled());
});
