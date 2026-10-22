import assert from "node:assert/strict";
import { test } from "node:test";

import { api } from "../src/lib/api.ts";

const json = (body, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

function mockFetch(t, handler) {
  const originalFetch = globalThis.fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });
  const calls = [];
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url: String(url), options });
    return handler(String(url), options);
  };
  return calls;
}

test("the OAuth provider catalog comes from the v8 route, not a local list", async (t) => {
  const calls = mockFetch(t, () =>
    json({
      providers: [
        {
          key: "kiro",
          display_name: "Kiro",
          flow_type: "aws_builder_id",
          auth_url_endpoint: "/v8/management/oauth/auth-url?provider=kiro",
          flow: "device",
          available: true,
        },
        {
          key: "codearts",
          display_name: "CodeArts",
          flow_type: "web_oauth",
          available: false,
          unavailable_reason: "backend disabled",
        },
      ],
    }),
  );

  const providers = await api.oauthFlows.getOAuthProviders();

  assert.equal(calls[0].url, "/v8/management/oauth/providers");
  assert.equal(providers.length, 2);
  assert.equal(providers[0].key, "kiro");
  assert.equal(providers[1].available, false);
});

test("starting a flow forwards provider parameters on the single auth-url route", async (t) => {
  const calls = mockFetch(t, () =>
    json({ status: "wait", state: "s1", provider: "kiro", flow: "device" }),
  );

  await api.oauthFlows.requestToken("kiro", {
    method: "idc",
    flow: "device",
    start_url: "https://example.awsapps.com/start",
    region: "us-east-1",
    empty: "",
  });

  const url = new URL(calls[0].url, "http://localhost");
  assert.equal(url.pathname, "/v8/management/oauth/auth-url");
  assert.equal(url.searchParams.get("provider"), "kiro");
  assert.equal(url.searchParams.get("method"), "idc");
  assert.equal(url.searchParams.get("flow"), "device");
  assert.equal(
    url.searchParams.get("start_url"),
    "https://example.awsapps.com/start",
  );
  assert.equal(url.searchParams.get("region"), "us-east-1");
});

test("provider prompts are answered through the input endpoint", async (t) => {
  const calls = mockFetch(t, () => json({ status: "ok" }));

  await api.oauthFlows.submitAuthInput({
    state: "s1",
    prompt_id: "p1",
    input: "http://127.0.0.1:1234/callback?code=abc",
  });

  assert.equal(calls[0].url, "/v8/management/oauth/session/input");
  assert.equal(calls[0].options.method, "POST");
  assert.deepEqual(JSON.parse(calls[0].options.body), {
    state: "s1",
    prompt_id: "p1",
    input: "http://127.0.0.1:1234/callback?code=abc",
  });
});

test("challenge/input states are carried by the status shape", async (t) => {
  const calls = mockFetch(t, () =>
    json({
      status: "input",
      state: "s1",
      provider: "codearts",
      prompt: "Paste the redirect URL",
      prompt_id: "p1",
      url: "https://example/oauth",
    }),
  );

  const status = await api.oauthFlows.getAuthStatus("s1");

  assert.equal(calls[0].url, "/v8/management/oauth/status?state=s1");
  assert.equal(status.status, "input");
  assert.equal(status.prompt_id, "p1");
  assert.equal(status.prompt, "Paste the redirect URL");
});

test("cancelling a session reports the backend verdict", async (t) => {
  const calls = mockFetch(t, () =>
    json({ status: "ok", cancelled: false }, 409),
  );

  await assert.rejects(
    () => api.oauthFlows.cancelAuthSession("s1"),
    (error) => {
      assert.equal(error.status, 409);
      assert.equal(error.body?.cancelled, false);
      return true;
    },
  );
  assert.equal(calls[0].url, "/v8/management/oauth/session?state=s1");
  assert.equal(calls[0].options.method, "DELETE");
});
