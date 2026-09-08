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

  test("a git-subdir source shows its URL under Git URL", () => {
    expect(marketplaceDraftFor(SUBDIR)).toEqual({
      name: "acme",
      kind: "url",
      url: "https://x/mono.git",
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
    expect(marketplaceEditParams(GITHUB, { ...marketplaceDraftFor(GITHUB), name: "beta" })).toEqual({
      name: "acme",
      newName: "beta",
    });
  });

  test("a changed field or kind sends the whole new source", () => {
    expect(marketplaceEditParams(GITHUB, { ...marketplaceDraftFor(GITHUB), repo: "acme/other" })).toEqual({
      name: "acme",
      source: { kind: "github", repo: "acme/other" },
    });
    expect(
      marketplaceEditParams(GITHUB, { ...marketplaceDraftFor(GITHUB), kind: "directory", path: "/srv/x" }),
    ).toEqual({
      name: "acme",
      source: { kind: "directory", path: "/srv/x" },
    });
  });

  test("an untouched git-subdir source is not re-sent; an edited URL becomes a plain url source", () => {
    expect(marketplaceEditParams(SUBDIR, marketplaceDraftFor(SUBDIR))).toBeNull();
    expect(marketplaceEditParams(SUBDIR, { ...marketplaceDraftFor(SUBDIR), url: "https://x/other.git" })).toEqual({
      name: "acme",
      source: { kind: "url", url: "https://x/other.git" },
    });
  });

  test("rename and re-source ride one request", () => {
    expect(marketplaceEditParams(URL, { ...marketplaceDraftFor(URL), name: "beta", url: "https://x/z.git" })).toEqual({
      name: "acme",
      newName: "beta",
      source: { kind: "url", url: "https://x/z.git" },
    });
  });
});

test("the source options are the add form's three kinds", () => {
  expect(MARKETPLACE_SOURCE_OPTIONS.map((o) => o.value)).toEqual(["url", "github", "directory"]);
});
