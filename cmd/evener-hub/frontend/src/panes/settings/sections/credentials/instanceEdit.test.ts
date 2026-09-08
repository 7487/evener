// @vitest-environment node
import { describe, expect, test } from "vitest";
import type { InstanceEntry, ProviderDescriptor } from "../../../../protocol/types.gen";
import { draftFor, instanceEditParams, PROTOCOL_OPTIONS, SURFACE_OPTIONS, varRows } from "./instanceEdit";

function instance(overrides: Partial<InstanceEntry> & Pick<InstanceEntry, "name" | "providerId">): InstanceEntry {
  return {
    protocol: "openai-chat",
    auth: "bearer",
    implicit: false,
    isDefault: false,
    activeSource: "none",
    hasStoredOAuth: false,
    credentialRequired: true,
    ...overrides,
  };
}

const VERTEX: ProviderDescriptor = {
  id: "google-vertex-anthropic",
  protocol: "anthropic",
  auth: "gcp-adc",
  implicit: true,
  vars: { GOOGLE_VERTEX_PROJECT: "GOOGLE_VERTEX_PROJECT", GOOGLE_VERTEX_LOCATION: "GOOGLE_VERTEX_LOCATION" },
};

describe("draftFor", () => {
  test("seeds every field from the entry, blanking what is unset", () => {
    const draft = draftFor(
      instance({ name: "a", providerId: "openai", baseUrl: "https://x", surface: "generic" }),
      undefined,
    );
    expect(draft).toEqual({
      name: "a",
      baseUrl: "https://x",
      protocol: "openai-chat",
      surface: "generic",
      vars: {},
      apiKeyEnv: "",
      credentialHeader: "",
    });
  });

  test("seeds a row for every template var, prefilled from the entry's vars", () => {
    const draft = draftFor(
      instance({ name: "v", providerId: "google-vertex-anthropic", vars: { GOOGLE_VERTEX_PROJECT: "p1", EXTRA: "e" } }),
      VERTEX,
    );
    expect(draft.vars).toEqual({ GOOGLE_VERTEX_PROJECT: "p1", GOOGLE_VERTEX_LOCATION: "", EXTRA: "e" });
  });

  test("carries the authored apiKeyEnv and credentialHeader", () => {
    const draft = draftFor(
      instance({
        name: "a",
        providerId: "openai",
        apiKeyEnv: "PORTKEY_KEY",
        credentialHeader: "Authorization=Bearer $PORTKEY_KEY",
      }),
      undefined,
    );
    expect(draft.apiKeyEnv).toBe("PORTKEY_KEY");
    expect(draft.credentialHeader).toBe("Authorization=Bearer $PORTKEY_KEY");
  });
});

describe("varRows", () => {
  test("template vars first, labelled by env var name and sorted, then authored extras labelled by key", () => {
    const draft = draftFor(
      instance({ name: "v", providerId: "google-vertex-anthropic", vars: { EXTRA: "e" } }),
      VERTEX,
    );
    expect(varRows(draft, VERTEX)).toEqual([
      { key: "GOOGLE_VERTEX_LOCATION", label: "GOOGLE_VERTEX_LOCATION" },
      { key: "GOOGLE_VERTEX_PROJECT", label: "GOOGLE_VERTEX_PROJECT" },
      { key: "EXTRA", label: "EXTRA" },
    ]);
  });
});

describe("instanceEditParams", () => {
  const base = draftFor(
    instance({
      name: "a",
      providerId: "openai",
      baseUrl: "https://x",
      surface: "openai",
      apiKeyEnv: "K",
      credentialHeader: "H=$K",
      vars: { R: "1" },
    }),
    undefined,
  );

  test("returns null when nothing changed", () => {
    expect(instanceEditParams(base, { ...base })).toBeNull();
    expect(instanceEditParams(base, { ...base, name: " a ", baseUrl: "https://x " })).toBeNull();
  });

  test("a changed name is a rename", () => {
    expect(instanceEditParams(base, { ...base, name: "b" })).toEqual({ name: "a", newName: "b" });
  });

  test("a changed base URL is sent; an emptied one is a clear", () => {
    expect(instanceEditParams(base, { ...base, baseUrl: "https://y" })).toEqual({ name: "a", baseUrl: "https://y" });
    expect(instanceEditParams(base, { ...base, baseUrl: "" })).toEqual({ name: "a", clearBaseUrl: true });
  });

  test("protocol and surface: a value is sent, inherit is a clear", () => {
    expect(instanceEditParams(base, { ...base, protocol: "openai-responses" })).toEqual({
      name: "a",
      protocol: "openai-responses",
    });
    expect(instanceEditParams(base, { ...base, protocol: "" })).toEqual({ name: "a", clearProtocol: true });
    expect(instanceEditParams(base, { ...base, surface: "generic" })).toEqual({ name: "a", surface: "generic" });
    expect(instanceEditParams(base, { ...base, surface: "" })).toEqual({ name: "a", clearSurface: true });
  });

  test("only changed vars are sent, trimmed; an emptied var is sent empty so the hub deletes it", () => {
    expect(instanceEditParams(base, { ...base, vars: { R: "1", Z: " 2 " } })).toEqual({ name: "a", vars: { Z: "2" } });
    expect(instanceEditParams(base, { ...base, vars: { R: "" } })).toEqual({ name: "a", vars: { R: "" } });
  });

  test("api key env and credential header: a value is sent, an emptied one is a clear", () => {
    expect(instanceEditParams(base, { ...base, apiKeyEnv: "K2" })).toEqual({ name: "a", apiKeyEnv: "K2" });
    expect(instanceEditParams(base, { ...base, apiKeyEnv: "" })).toEqual({ name: "a", clearApiKeyEnv: true });
    expect(instanceEditParams(base, { ...base, credentialHeader: "X=$K" })).toEqual({
      name: "a",
      credentialHeader: "X=$K",
    });
    expect(instanceEditParams(base, { ...base, credentialHeader: "" })).toEqual({
      name: "a",
      clearCredentialHeader: true,
    });
  });

  test("several changes ride one request", () => {
    expect(instanceEditParams(base, { ...base, name: "b", baseUrl: "", protocol: "google" })).toEqual({
      name: "a",
      newName: "b",
      clearBaseUrl: true,
      protocol: "google",
    });
  });
});

test("the option lists lead with inherit and carry exactly the registry's vocabularies", () => {
  expect(PROTOCOL_OPTIONS.map((o) => o.value)).toEqual(["", "openai-chat", "openai-responses", "anthropic", "google"]);
  expect(SURFACE_OPTIONS.map((o) => o.value)).toEqual(["", "openai", "anthropic", "google", "generic"]);
  expect(PROTOCOL_OPTIONS[0]?.label).toBe("inherit from base");
  expect(SURFACE_OPTIONS[0]?.label).toBe("inherit from base");
});
