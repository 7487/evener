// marketplaceEdit.ts: the marketplace sheet's form model - the draft, how it
// is seeded from a MarketplaceEntry, and how a dirty draft becomes the
// smallest evener/marketplace/edit request. Pure, so the rules (the kind's
// own field is the only one that counts; a source whose kind the picker does
// not offer shows its URL under Git URL and keeps that kind, and the fields
// it carries, until the user picks a different kind; a blanked field is an
// unfinished edit, not a change) are pinned without rendering anything.
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

function isOfferedKind(kind: string): kind is MarketplaceSourceKind {
  return MARKETPLACE_SOURCE_OPTIONS.some((option) => option.value === kind);
}

export function marketplaceDraftFor(entry: MarketplaceEntry): MarketplaceDraft {
  const { source } = entry;
  const kind: MarketplaceSourceKind = isOfferedKind(source.kind) ? source.kind : "url";
  return {
    name: entry.name,
    kind,
    url: kind === "url" ? (source.url ?? "") : "",
    repo: kind === "github" ? (source.repo ?? "") : "",
    path: kind === "directory" ? (source.path ?? "") : "",
  };
}

/** The one field the draft's kind reads. The others hold whatever the user
 * last typed under another kind and are ignored. */
function draftValue(draft: MarketplaceDraft): string {
  if (draft.kind === "github") return draft.repo.trim();
  if (draft.kind === "directory") return draft.path.trim();
  return draft.url.trim();
}

function sourceFromDraft(entry: MarketplaceEntry, draft: MarketplaceDraft): MarketplaceSourceInput {
  const value = draftValue(draft);
  if (draft.kind === "github") return { kind: "github", repo: value };
  if (draft.kind === "directory") return { kind: "directory", path: value };
  // A kind the picker cannot offer shows its URL here, so editing that URL
  // re-points the source it already is - dropping to a plain url would
  // silently change which catalog the marketplace reads.
  if (!isOfferedKind(entry.source.kind)) return { ...entry.source, url: value };
  return { kind: "url", url: value };
}

/** Whether the draft still describes the entry's own source. */
function sourceUnchanged(entry: MarketplaceEntry, draft: MarketplaceDraft): boolean {
  const { source } = entry;
  const next = sourceFromDraft(entry, draft);
  if (next.kind !== source.kind) return false;
  if (next.kind === "github") return next.repo === (source.repo ?? "");
  if (next.kind === "directory") return next.path === (source.path ?? "");
  return next.url === (source.url ?? "");
}

/** The request carrying exactly what changed, or null when nothing did. An
 * emptied field is nothing changed rather than a change to nothing: the
 * server ignores an empty newName and cannot fetch an empty source. */
export function marketplaceEditParams(entry: MarketplaceEntry, draft: MarketplaceDraft): MarketplaceEditParams | null {
  const params: MarketplaceEditParams = { name: entry.name };
  let changed = false;
  const name = draft.name.trim();
  if (name && name !== entry.name) {
    params.newName = name;
    changed = true;
  }
  if (draftValue(draft) && !sourceUnchanged(entry, draft)) {
    params.source = sourceFromDraft(entry, draft);
    changed = true;
  }
  return changed ? params : null;
}
