// @vitest-environment node
import { describe, expect, test } from "vitest";
import type { MarketplaceEntry } from "../../../../protocol/types.gen";
import { MARKETPLACE_SOURCE_OPTIONS, marketplaceDraftFor, marketplaceEditParams } from "./marketplaceEdit";

const GITHUB: MarketplaceEntry = { name: "acme", source: { kind: "github", repo: "acme/plugins" }, lastUpdated: 1 };
const URL: MarketplaceEntry = { name: "acme", source: { kind: "url", url: "https://x/y.git" }, lastUpdated: 1 };
const DIR: MarketplaceEntry = { name: "acme", source: { kind: "directory", path: "/srv/mkt" }, lastUpdated: 1 };
const SUBDIR: MarketplaceEntry = {
  name: "acme",
  source: { kind: "git-subdir", url: "https://x/mono.git", path: "mkt" },
  lastUpdated: 1,
};
// A kind the picker does not offer and this frontend has never heard of,
// standing in for whatever the wire grows next.
const FUTURE: MarketplaceEntry = {
  name: "acme",
  source: { kind: "gitlab", url: "https://gl/x.git", ref: "main" },
  lastUpdated: 1,
};

describe("marketplaceDraftFor", () => {
  test("seeds the kind and the matching field, blanking the others", () => {
    expect(marketplaceDraftFor(GITHUB)).toEqual({
      name: "acme",
      kind: "github",
      url: "",
      repo: "acme/plugins",
      path: "",
    });
    expect(marketplaceDraftFor(URL)).toEqual({ name: "acme", kind: "url", url: "https://x/y.git", repo: "", path: "" });
    expect(marketplaceDraftFor(DIR)).toEqual({ name: "acme", kind: "directory", url: "", repo: "", path: "/srv/mkt" });
  });

  test("a kind the picker does not offer shows its URL under Git URL", () => {
    expect(marketplaceDraftFor(SUBDIR)).toEqual({
      name: "acme",
      kind: "url",
      url: "https://x/mono.git",
      repo: "",
      path: "",
    });
    expect(marketplaceDraftFor(FUTURE)).toEqual({
      name: "acme",
      kind: "url",
      url: "https://gl/x.git",
      repo: "",
      path: "",
    });
  });
});

describe("marketplaceEditParams", () => {
  test("null when nothing changed, whitespace included", () => {
    expect(marketplaceEditParams(GITHUB, marketplaceDraftFor(GITHUB))).toBeNull();
    expect(
      marketplaceEditParams(GITHUB, { ...marketplaceDraftFor(GITHUB), name: " acme ", repo: "acme/plugins " }),
    ).toBeNull();
  });

  test("a changed name is a rename", () => {
    expect(marketplaceEditParams(GITHUB, { ...marketplaceDraftFor(GITHUB), name: "beta" })).toStrictEqual({
      name: "acme",
      newName: "beta",
    });
  });

  test("a changed field or kind sends the whole new source", () => {
    expect(marketplaceEditParams(GITHUB, { ...marketplaceDraftFor(GITHUB), repo: "acme/other" })).toStrictEqual({
      name: "acme",
      source: { kind: "github", repo: "acme/other" },
    });
    expect(
      marketplaceEditParams(GITHUB, { ...marketplaceDraftFor(GITHUB), kind: "directory", path: "/srv/x" }),
    ).toStrictEqual({
      name: "acme",
      source: { kind: "directory", path: "/srv/x" },
    });
  });

  test("switching the kind away and back is not a change", () => {
    const draft = marketplaceDraftFor(GITHUB);
    expect(marketplaceEditParams(GITHUB, { ...draft, kind: "url", url: "https://x/y.git" })).toStrictEqual({
      name: "acme",
      source: { kind: "url", url: "https://x/y.git" },
    });
    expect(marketplaceEditParams(GITHUB, { ...draft, kind: "github", url: "https://x/y.git" })).toBeNull();
  });

  test("a blanked name is not a rename, and a blanked source field is not a re-source", () => {
    expect(marketplaceEditParams(GITHUB, { ...marketplaceDraftFor(GITHUB), name: "   " })).toBeNull();
    expect(marketplaceEditParams(GITHUB, { ...marketplaceDraftFor(GITHUB), repo: "" })).toBeNull();
    expect(marketplaceEditParams(DIR, { ...marketplaceDraftFor(DIR), path: "  " })).toBeNull();
    expect(marketplaceEditParams(URL, { ...marketplaceDraftFor(URL), name: "", url: "" })).toBeNull();
    expect(marketplaceEditParams(GITHUB, { ...marketplaceDraftFor(GITHUB), name: "beta", repo: "" })).toStrictEqual({
      name: "acme",
      newName: "beta",
    });
  });

  test("an untouched source the picker does not offer is not re-sent", () => {
    expect(marketplaceEditParams(SUBDIR, marketplaceDraftFor(SUBDIR))).toBeNull();
    expect(marketplaceEditParams(FUTURE, marketplaceDraftFor(FUTURE))).toBeNull();
  });

  test("editing such a source's URL keeps its own kind and the fields that kind carries", () => {
    expect(marketplaceEditParams(SUBDIR, { ...marketplaceDraftFor(SUBDIR), url: "https://x/other.git" })).toStrictEqual(
      {
        name: "acme",
        source: { kind: "git-subdir", url: "https://x/other.git", path: "mkt" },
      },
    );
    expect(marketplaceEditParams(FUTURE, { ...marketplaceDraftFor(FUTURE), url: "https://gl/z.git" })).toStrictEqual({
      name: "acme",
      source: { kind: "gitlab", url: "https://gl/z.git", ref: "main" },
    });
  });

  test("picking a different kind for such a source is a wholesale replacement", () => {
    const draft = marketplaceDraftFor(SUBDIR);
    expect(marketplaceEditParams(SUBDIR, { ...draft, kind: "github", repo: "acme/plugins" })).toStrictEqual({
      name: "acme",
      source: { kind: "github", repo: "acme/plugins" },
    });
    expect(marketplaceEditParams(SUBDIR, { ...draft, kind: "url", repo: "acme/plugins" })).toBeNull();
  });

  test("rename and re-source ride one request", () => {
    expect(
      marketplaceEditParams(URL, { ...marketplaceDraftFor(URL), name: "beta", url: "https://x/z.git" }),
    ).toStrictEqual({
      name: "acme",
      newName: "beta",
      source: { kind: "url", url: "https://x/z.git" },
    });
  });
});

test("the source options are the add form's three kinds", () => {
  expect(MARKETPLACE_SOURCE_OPTIONS.map((o) => o.value)).toEqual(["url", "github", "directory"]);
});
