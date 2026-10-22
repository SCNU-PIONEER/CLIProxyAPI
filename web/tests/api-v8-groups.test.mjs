import assert from "node:assert/strict";
import { test } from "node:test";

import { api } from "../src/lib/api.ts";

const json = (body, status = 200, headers = {}) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });

const FAMILY_PATH = "/v8/management/config/api-keys/gemini";

/**
 * Minimal grouped v8 config backend: one api-keys.<family> subtree with ETag
 * preconditions, exactly like the real /v8/management/config handler.
 */
function groupedBackend(t, initialGroups) {
  const state = { groups: structuredClone(initialGroups), version: 1 };
  const writes = [];

  const originalFetch = globalThis.fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });

  globalThis.fetch = async (url, options = {}) => {
    const target = String(url);
    if (target !== FAMILY_PATH) {
      return json({ error: "not_found" }, 404);
    }
    if ((options.method ?? "GET") === "GET") {
      return json(state.groups, 200, { ETag: `"v${state.version}"` });
    }
    const ifMatch = (options.headers ?? {})["If-Match"];
    if (ifMatch !== `"v${state.version}"`) {
      return json({ error: "precondition_failed" }, 412, {
        ETag: `"v${state.version}"`,
      });
    }
    const next = JSON.parse(String(options.body));
    writes.push(structuredClone(next));
    state.groups = structuredClone(next);
    state.version += 1;
    return json({ status: "ok" }, 200, { ETag: `"v${state.version}"` });
  };

  return { state, writes };
}

const fixture = () => [
  {
    name: "primary",
    "base-url": "https://primary.example",
    priority: 3,
    "unknown-group-field": "preserved",
    keys: [
      { "api-key": "key-one", weight: 2 },
      { "api-key": "key-two" },
    ],
  },
  {
    name: "secondary",
    "base-url": "https://secondary.example",
    keys: [{ "api-key": "key-three" }],
  },
];

test("grouped v8 api-keys are flattened with group context", async (t) => {
  groupedBackend(t, fixture());

  const keys = await api.geminiKeys.getGeminiKeys();

  assert.equal(keys.length, 3);
  assert.deepEqual(
    keys.map((key) => [key["api-key"], key.__group]),
    [
      ["key-one", "primary"],
      ["key-two", "primary"],
      ["key-three", "secondary"],
    ],
  );
  // Group-level policy is inherited by the flattened rows.
  assert.equal(keys[0].priority, 3);
  assert.equal(keys[0].__key, "key-one");
  assert.equal(keys[1].weight, undefined);
});

test("editing one credential preserves group fields and sibling keys", async (t) => {
  const backend = groupedBackend(t, fixture());

  await api.geminiKeys.patchGeminiKey({
    group: "primary",
    match: "key-one",
    value: { "api-key": "key-one", weight: 9 },
  });

  assert.equal(backend.writes.length, 1);
  const written = backend.writes[0];
  assert.equal(written[0]["unknown-group-field"], "preserved");
  assert.equal(written[0].priority, 3);
  assert.deepEqual(
    written[0].keys.map((key) => key["api-key"]),
    ["key-one", "key-two"],
  );
  assert.equal(written[0].keys[0].weight, 9);
});

test("concurrent insert does not shift an update onto another credential", async (t) => {
  const backend = groupedBackend(t, fixture());

  // Between the client read and its conditional write, another writer inserts a
  // new group at the front and bumps the ETag: the client must see 412 and retry
  // by stable group name + api-key, never by array index.
  const originalFetch = globalThis.fetch;
  let injected = false;
  globalThis.fetch = async (url, options = {}) => {
    if (!injected && options.method === "PUT") {
      injected = true;
      backend.state.groups.unshift({
        name: "concurrent",
        "base-url": "https://concurrent.example",
        keys: [{ "api-key": "concurrent-key" }],
      });
      backend.state.version += 1;
      return json({ error: "precondition_failed" }, 412);
    }
    return originalFetch(url, options);
  };

  await api.geminiKeys.patchGeminiKey({
    group: "secondary",
    match: "key-three",
    value: { "api-key": "key-three", weight: 7 },
  });

  const written = backend.state.groups;
  assert.equal(written[0].name, "concurrent");
  assert.deepEqual(written[0].keys, [{ "api-key": "concurrent-key" }]);
  const secondary = written.find((group) => group.name === "secondary");
  assert.equal(secondary.keys[0].weight, 7);
  const primary = written.find((group) => group.name === "primary");
  assert.deepEqual(
    primary.keys.map((key) => key["api-key"]),
    ["key-one", "key-two"],
  );
});

test("deleting the last key drops the empty group", async (t) => {
  const backend = groupedBackend(t, fixture());

  await api.geminiKeys.deleteGeminiKey({
    group: "secondary",
    match: "key-three",
  });

  assert.deepEqual(
    backend.state.groups.map((group) => group.name),
    ["primary"],
  );
});

test("access api-keys mutations target stable key values under retry", async (t) => {
  const state = { keys: ["sk-existing"], version: 1 };
  const writes = [];
  const originalFetch = globalThis.fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });
  let injected = false;
  globalThis.fetch = async (url, options = {}) => {
    assert.equal(String(url), "/v8/management/config/access/api-keys");
    if ((options.method ?? "GET") === "GET") {
      return json(state.keys, 200, { ETag: `"v${state.version}"` });
    }
    if (!injected) {
      injected = true;
      state.keys = ["sk-concurrent", ...state.keys];
      state.version += 1;
      return json({ error: "precondition_failed" }, 412);
    }
    const next = JSON.parse(String(options.body));
    writes.push(structuredClone(next));
    state.keys = structuredClone(next);
    state.version += 1;
    return json({ status: "ok" }, 200, { ETag: `"v${state.version}"` });
  };

  await api.apiKeys.patchAPIKeys({ value: "sk-added" });

  assert.equal(writes.length, 1);
  assert.deepEqual(writes[0], ["sk-concurrent", "sk-existing", "sk-added"]);
});

test("deleting an access api-key uses its value, not its position", async (t) => {
  const state = { keys: ["sk-a", "sk-b", "sk-c"], version: 1 };
  const writes = [];
  const originalFetch = globalThis.fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });
  let injected = false;
  globalThis.fetch = async (url, options = {}) => {
    if ((options.method ?? "GET") === "GET") {
      return json(state.keys, 200, { ETag: `"v${state.version}"` });
    }
    if (!injected) {
      injected = true;
      state.keys = ["sk-new", ...state.keys];
      state.version += 1;
      return json({ error: "precondition_failed" }, 412);
    }
    const next = JSON.parse(String(options.body));
    writes.push(structuredClone(next));
    state.keys = structuredClone(next);
    state.version += 1;
    return json({ status: "ok" }, 200, { ETag: `"v${state.version}"` });
  };

  await api.apiKeys.deleteAPIKeys({ value: "sk-b" });

  assert.equal(writes.length, 1);
  assert.deepEqual(writes[0], ["sk-new", "sk-a", "sk-c"]);
});
