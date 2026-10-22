import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

import { api } from "../src/lib/api.ts";

const json = (body, status = 200, headers = {}) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });

/** Installs a fetch mock that records every call. */
function mockFetch(t, handler) {
  const originalFetch = globalThis.fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });
  const calls = [];
  globalThis.fetch = async (url, options = {}) => {
    const call = { url: String(url), options };
    calls.push(call);
    return handler(call, calls.length);
  };
  return calls;
}

test("legacy v0 management paths are never emitted", async () => {
  const source = await readFile(
    new URL("../src/lib/api.ts", import.meta.url),
    "utf8",
  );
  // The prefix must never appear as a string literal; it may only be documented.
  assert.equal(/["'`]\/v0\//.test(source), false);
});

test("boolean settings write to their v8 YAML tree leaf", async (t) => {
  const calls = mockFetch(t, () => json({ status: "ok" }));
  await api.boolean.putDebug(true);

  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, "/v8/management/config/observability/logs/debug");
  assert.equal(calls[0].options.method, "PUT");
  assert.equal(calls[0].options.body, "true");
  assert.equal(calls[0].options.headers["Content-Type"], "application/json");
});

test("conditional config writes send If-Match and retry once on 412", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });

  const calls = [];
  let getCount = 0;
  let putCount = 0;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url: String(url), options });
    if (options.method === "GET") {
      getCount += 1;
      return getCount === 1
        ? json(true, 200, { ETag: '"v1"' })
        : json(false, 200, { ETag: '"v2"' });
    }
    putCount += 1;
    return putCount === 1
      ? json({ error: "precondition_failed" }, 412)
      : json({ status: "ok" });
  };

  await api.config.updateConfigValue("debug", (value) => !value);

  assert.equal(calls.length, 4);
  assert.equal(calls[1].options.method, "PUT");
  assert.equal(calls[1].options.headers["If-Match"], '"v1"');
  assert.equal(calls[1].options.body, "false");
  assert.equal(calls[3].options.method, "PUT");
  assert.equal(calls[3].options.headers["If-Match"], '"v2"');
  assert.equal(calls[3].options.body, "true");
});

test("credentials status and delete use v8 payload shapes", async (t) => {
  const calls = mockFetch(t, () => json({ status: "ok" }));

  await api.authFiles.deleteAuthFile({ name: "alpha.json" });
  await api.authFiles.patchAuthFileStatus({
    name: "beta.json",
    disabled: true,
  });

  assert.equal(calls[0].url, "/v8/management/credentials");
  assert.equal(calls[0].options.method, "DELETE");
  assert.deepEqual(JSON.parse(calls[0].options.body), { name: "alpha.json" });

  assert.equal(calls[1].url, "/v8/management/credentials/status");
  assert.equal(calls[1].options.method, "PATCH");
  assert.deepEqual(JSON.parse(calls[1].options.body), {
    name: "beta.json",
    disabled: true,
  });
});

test("oauth auth-url goes through the single v8 route with provider params", async (t) => {
  const calls = mockFetch(t, () =>
    json({ status: "ok", url: "https://gitlab.example/oauth" }),
  );

  await api.oauthFlows.requestGitLabToken({
    client_id: "cid",
    base_url: "https://gitlab.example",
  });

  assert.equal(calls.length, 1);
  const url = new URL(calls[0].url, "http://localhost");
  assert.equal(url.pathname, "/v8/management/oauth/auth-url");
  assert.equal(url.searchParams.get("provider"), "gitlab");
  assert.equal(url.searchParams.get("client_id"), "cid");
  assert.equal(url.searchParams.get("base_url"), "https://gitlab.example");
  assert.equal(calls[0].options.method, "GET");
});

test("usage and logs target v8 observability routes", async (t) => {
  const calls = mockFetch(t, () =>
    json({ usage: {}, failed_requests: 0, lines: [] }),
  );

  await api.usage.getUsageStatistics();
  await api.logs.getLogs(7);

  assert.equal(calls[0].url, "/v8/management/observability/usage");
  assert.equal(calls[1].url, "/v8/management/observability/logs?after=7");
});
