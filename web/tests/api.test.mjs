import assert from "node:assert/strict";
import { test } from "node:test";
import { api } from "../src/lib/api.ts";

test("YAML configuration is sent verbatim, never inside a JSON wrapper", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  const yaml = "server:\n  port: 8317\n";
  let captured;
  globalThis.fetch = async (url, options) => {
    captured = { url, options };
    return new Response(JSON.stringify({ status: "ok" }), {
      headers: { "Content-Type": "application/json" },
    });
  };
  await api.config.putConfigYAML(yaml);
  assert.equal(captured.options.method, "PUT");
  assert.equal(captured.options.body, yaml);
  assert.equal(captured.options.headers["Content-Type"], "application/yaml");
});
