// marketplaceEdit.ts: the marketplace sheet's form model - the draft, how it
// is seeded from a MarketplaceEntry, and how a dirty draft becomes the
// smallest evener/marketplace/edit request. Pure, so the rules (the kind's
// own field is the only one that counts, a git-subdir entry is left alone
// unless its URL is edited) are pinned without rendering anything.
import type { MarketplaceEditParams, MarketplaceEntry, MarketplaceSourceInput } from "../../../../protocol/types.gen";

export type MarketplaceSourceKind = "url" | "github" | "directory";

/** The same three kinds, in the same order, the add form offers. git-subdir
 * is not offered: the picker would need a second field, and the add form
 * never had one. */
export const MARKETPLACE_SOURCE_OPTIONS: { value: MarketplaceSourceKind; label: string }[] = [
  { value: "url", label: "Git URL" },
  { value: "github", label: "owner/repo" },
  { value: "directory", label: "Local path" },
];

export interface MarketplaceDraft {
  name: string;
  kind: MarketplaceSourceKind;
  url: string;
  repo: string;
  path: string;
}

export function marketplaceDraftFor(entry: MarketplaceEntry): MarketplaceDraft {
  const { source } = entry;
  const kind: MarketplaceSourceKind =
    source.kind === "github" ? "github" : source.kind === "directory" ? "directory" : "url";
  return {
    name: entry.name,
    kind,
    url: kind === "url" ? (source.url ?? "") : "",
    repo: kind === "github" ? (source.repo ?? "") : "",
    path: kind === "directory" ? (source.path ?? "") : "",
  };
}

function sourceFromDraft(draft: MarketplaceDraft): MarketplaceSourceInput {
  if (draft.kind === "github") return { kind: "github", repo: draft.repo.trim() };
  if (draft.kind === "directory") return { kind: "directory", path: draft.path.trim() };
  return { kind: "url", url: draft.url.trim() };
}

/** Whether the draft still describes the entry's own source. A git-subdir
 * entry renders as its URL under the url kind, so it is unchanged exactly
 * when that URL is untouched. */
function sourceUnchanged(entry: MarketplaceEntry, draft: MarketplaceDraft): boolean {
  const { source } = entry;
  if (source.kind === "git-subdir") return draft.kind === "url" && draft.url.trim() === (source.url ?? "");
  const next = sourceFromDraft(draft);
  if (next.kind !== source.kind) return false;
  if (next.kind === "github") return next.repo === (source.repo ?? "");
  if (next.kind === "directory") return next.path === (source.path ?? "");
  return next.url === (source.url ?? "");
}

/** The request carrying exactly what changed, or null when nothing did. */
export function marketplaceEditParams(entry: MarketplaceEntry, draft: MarketplaceDraft): MarketplaceEditParams | null {
  const params: MarketplaceEditParams = { name: entry.name };
  let changed = false;
  const name = draft.name.trim();
  if (name !== entry.name) {
    params.newName = name;
    changed = true;
  }
  if (!sourceUnchanged(entry, draft)) {
    params.source = sourceFromDraft(draft);
    changed = true;
  }
  return changed ? params : null;
}
