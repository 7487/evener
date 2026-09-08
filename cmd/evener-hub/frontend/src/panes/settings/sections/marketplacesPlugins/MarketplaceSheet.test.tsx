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

// A local-path marketplace that has never been fetched: its source field is
// the DirectoryPicker trigger rather than a text input, and its meta rows have
// nothing to show.
const LOCAL: MarketplaceEntry = {
  name: "local",
  source: { kind: "directory", path: "/srv/mkt" },
  installLocation: "",
  lastUpdated: 0,
};

// A source kind this frontend cannot represent, carrying no url either, so the
// only draft it can seed is an empty Git URL field.
const FUTURE: MarketplaceEntry = {
  name: "acme",
  source: { kind: "gitlab", ref: "main" },
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
  const tree = (name: string | null) => (
    <>
      <Toast />
      <MarketplaceSheet name={name} onClose={onClose} onRenamed={onRenamed} expandedMarketplaces={expanded} />
    </>
  );
  const view = render(tree(entry?.name ?? null));
  // What the page's own selection does: re-renders the sheet under a different
  // name, or none at all.
  return { onClose, onRenamed, select: (name: string | null) => view.rerender(tree(name)) };
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
  // Nothing has been touched, so nothing is about to be re-fetched.
  expect(screen.queryByText(/Saving re-fetches/)).toBeNull();
});

test("Save is disabled until something changes, and a rename alone promises no re-fetch", async () => {
  renderSheet(ACME);
  expect(saveButton().disabled).toBe(true);
  const user = userEvent.setup();
  await user.type(field("Name"), "2");
  expect(saveButton().disabled).toBe(false);
  expect(screen.queryByText(/Saving re-fetches/)).toBeNull();
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

test("emptying the source field disables Save and keeps the re-fetch note up", async () => {
  renderSheet(ACME);
  const user = userEvent.setup();
  await user.clear(screen.getByPlaceholderText("owner/repo"));
  await user.type(field("Name"), "2");
  // Mid-edit of the source, so the note is guidance rather than a promise
  // about a save that cannot happen yet.
  expect(saveButton().disabled).toBe(true);
  expect(screen.getByText(/Saving re-fetches/)).toBeTruthy();
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
  expect(getToasts().some((t) => t.kind === "success" && t.text === "Saved acme2")).toBe(true);
  expect(onClose).not.toHaveBeenCalled();
});

test("a rename that resolves after the sheet has moved on does not re-select the new name", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  let release: (() => void) | undefined;
  fake.on(
    "evener/marketplace/edit",
    () =>
      new Promise((resolve) => {
        release = () => resolve({ marketplaces: [{ ...ACME, name: "acme2" }] });
      }),
  );
  const { onClose, onRenamed, select } = renderSheet(ACME);
  const user = userEvent.setup();
  await user.type(field("Name"), "2");
  await user.click(saveButton());
  // An edit re-clones the repo, so there is a real window in which the user
  // dismisses the sheet or switches segments before the response lands.
  select(null);
  act(() => release?.());
  await waitFor(() => expect(getToasts().some((t) => t.kind === "success" && t.text === "Saved acme2")).toBe(true));
  expect(onRenamed).not.toHaveBeenCalled();
  // Nor may the abandoned rename leave the close-on-vanish path suppressed:
  // re-selecting a name the store no longer has still closes the sheet.
  select("acme");
  await waitFor(() => expect(onClose).toHaveBeenCalled());
});

test("an entry whose source kind this frontend cannot represent can still be renamed", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  fake.on("evener/marketplace/edit", (params) => {
    expect(params).toEqual({ name: "acme", newName: "acme2" });
    return { marketplaces: [{ ...FUTURE, name: "acme2" }] };
  });
  const { onRenamed } = renderSheet(FUTURE);
  const user = userEvent.setup();
  await user.type(field("Name"), "2");
  // The Git URL field is empty because the source carries no url, not because
  // the user emptied it - so this is a rename, with no source to re-fetch.
  expect((screen.getByPlaceholderText("https://github.com/owner/repo.git") as HTMLInputElement).value).toBe("");
  expect(screen.queryByText(/Saving re-fetches/)).toBeNull();
  expect(saveButton().disabled).toBe(false);
  await user.click(saveButton());
  await waitFor(() => expect(onRenamed).toHaveBeenCalledWith("acme2"));
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
  await waitFor(() => expect(getToasts().some((t) => t.kind === "success" && t.text === "Saved acme")).toBe(true));
  expect(saveButton().disabled).toBe(true);
  expect((screen.getByPlaceholderText("https://github.com/owner/repo.git") as HTMLInputElement).value).toBe(
    "https://x/y.git",
  );
});

test("a local path browses for a new directory and locks the field while saving", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  let release: (() => void) | undefined;
  fake.on("evener/paths/complete", (params) => {
    // Directories only, and the prefix goes over the wire verbatim.
    expect(params.includeFiles).toBe(false);
    return { data: params.prefix === "/srv/mkt/" ? ["/srv/mkt/inner"] : [] };
  });
  fake.on("evener/path/validate", ({ path }) => ({ valid: true, path }));
  fake.on("evener/marketplace/edit", (params) => {
    expect(params).toEqual({ name: "local", source: { kind: "directory", path: "/srv/mkt/inner" } });
    return new Promise((resolve) => {
      release = () => resolve({ marketplaces: [{ ...LOCAL, source: { kind: "directory", path: "/srv/mkt/inner" } }] });
    });
  });
  renderSheet(LOCAL);
  expect(screen.getByText("not fetched yet")).toBeTruthy();
  expect(screen.getByText("never")).toBeTruthy();
  const user = userEvent.setup();
  await user.click(screen.getByLabelText("Local path"));
  await user.click(await screen.findByRole("button", { name: "Open /srv/mkt/inner" }));
  await user.click(screen.getByRole("button", { name: "Use this folder" }));
  expect(screen.getByText(/Saving re-fetches the marketplace/)).toBeTruthy();
  await user.click(saveButton());
  await waitFor(() => expect((screen.getByLabelText("Local path") as HTMLButtonElement).disabled).toBe(true));
  act(() => release?.());
  await waitFor(() => expect(getToasts().some((t) => t.kind === "success" && t.text === "Saved local")).toBe(true));
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
  expect(getToasts().some((t) => t.kind === "error" && t.text.startsWith("Save failed"))).toBe(true);
  expect(field("Name").value).toBe("acme2");
});

test("Enter in the form never saves - the footer's Save is the only path", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  renderSheet(ACME);
  const user = userEvent.setup();
  await user.type(field("Name"), "2{Enter}");
  expect(fake.calls.some((c) => c.method === "evener/marketplace/edit")).toBe(false);
  expect(field("Name").value).toBe("acme2");
});

test("Enter does not save on a source kind whose field is a button either", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  // The local-path form's only text field is Name, which is exactly the shape
  // HTML lets submit a form implicitly.
  renderSheet(LOCAL);
  const user = userEvent.setup();
  await user.type(field("Name"), "2{Enter}");
  expect(fake.calls.some((c) => c.method === "evener/marketplace/edit")).toBe(false);
  expect(field("Name").value).toBe("local2");
});

test("Refresh calls refreshMarketplace, is busy in flight, toasts, and re-browses an expanded marketplace", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  let release: (() => void) | undefined;
  fake.on("evener/marketplace/refresh", (params) => {
    // Asserted out here, not inside the executor: a mismatch in there rejects
    // the promise instead of failing, and the test would time out with no diff.
    expect(params).toEqual({ name: "acme" });
    return new Promise((resolve) => {
      release = () => resolve({ marketplaces: [ACME] });
    });
  });
  fake.on("evener/marketplace/browse", () => ({ name: "acme", description: "", plugins: [] }));
  renderSheet(ACME, new Set(["acme"]));
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Refresh" }));
  expect((screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement).disabled).toBe(true);
  act(() => release?.());
  await waitFor(() => expect(getToasts().some((t) => t.kind === "success" && t.text === "Refreshed acme")).toBe(true));
  expect((screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement).disabled).toBe(false);
  expect(fake.calls.some((c) => c.method === "evener/marketplace/browse")).toBe(true);
});

test("Refresh leaves a marketplace nothing has expanded un-browsed", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  fake.on("evener/marketplace/refresh", () => ({ marketplaces: [ACME] }));
  renderSheet(ACME);
  await userEvent.setup().click(screen.getByRole("button", { name: "Refresh" }));
  await waitFor(() => expect(getToasts().some((t) => t.kind === "success" && t.text === "Refreshed acme")).toBe(true));
  expect(fake.calls.some((c) => c.method === "evener/marketplace/browse")).toBe(false);
});

test("a failed Refresh toasts and re-enables the button", async () => {
  const fake = connectionStore.getState().client as FakeClient;
  fake.on("evener/marketplace/refresh", () => {
    throw new Error("fetch failed");
  });
  renderSheet(ACME);
  await userEvent.setup().click(screen.getByRole("button", { name: "Refresh" }));
  await waitFor(() =>
    expect(getToasts().some((t) => t.kind === "error" && t.text === "Refresh failed: fetch failed")).toBe(true),
  );
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
  await waitFor(() =>
    expect(getToasts().some((t) => t.kind === "success" && t.text === "Removed marketplace acme")).toBe(true),
  );
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
  await waitFor(() =>
    expect(getToasts().some((t) => t.kind === "success" && t.text === "Removed marketplace acme")).toBe(true),
  );
  expect(screen.queryByRole("dialog", { name: "Remove marketplace" })).toBeNull();
});

test("closes itself when the entry disappears from the store", async () => {
  const { onClose } = renderSheet(ACME);
  act(() => extensionsStore.setState({ marketplaces: [] }));
  await waitFor(() => expect(onClose).toHaveBeenCalled());
});
