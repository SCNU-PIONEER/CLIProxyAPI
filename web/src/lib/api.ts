const STORAGE_KEY = "mgmt-key";

/**
 * Every call targets the v8 management contract. The legacy v0 tree is
 * deprecated and no longer maintained, so this module must never emit a
 * /v0/management path.
 */
const API_PREFIX = "/v8/management";

class APIError extends Error {
  status: number;
  body: unknown;

  constructor(status: number, body: unknown) {
    const message =
      typeof body === "object" && body !== null && "error" in body
        ? String((body as { error: unknown }).error)
        : `HTTP ${status}`;
    super(message);
    this.name = "APIError";
    this.status = status;
    this.body = body;
  }
}

function getBaseURL(): string {
  if (typeof window !== "undefined") {
    return window.location.origin;
  }
  return "";
}

function getManagementKey(): string {
  if (typeof window === "undefined") return "";
  return localStorage.getItem(STORAGE_KEY) ?? "";
}

export function setManagementKey(key: string): void {
  localStorage.setItem(STORAGE_KEY, key);
}

export function clearManagementKey(): void {
  localStorage.removeItem(STORAGE_KEY);
}

export function hasManagementKey(): boolean {
  if (typeof window === "undefined") return false;
  return localStorage.getItem(STORAGE_KEY) !== null;
}

function buildURL(path: string): string {
  return `${getBaseURL()}${API_PREFIX}${path}`;
}

function authHeaders(): Record<string, string> {
  const key = getManagementKey();
  const headers: Record<string, string> = {};
  if (key) {
    headers["Authorization"] = `Bearer ${key}`;
  }
  return headers;
}

async function throwAPIError(res: Response): Promise<never> {
  let body: unknown;
  try {
    body = await res.json();
  } catch {
    try {
      body = await res.text();
    } catch {
      body = undefined;
    }
  }
  throw new APIError(res.status, body);
}

async function parseBody<T>(res: Response): Promise<T> {
  if (!res.ok) return throwAPIError(res);
  const contentType = res.headers.get("content-type") ?? "";
  if (contentType.includes("application/json")) {
    return res.json() as Promise<T>;
  }
  if (contentType.includes("text/") || contentType.includes("yaml")) {
    return res.text() as unknown as T;
  }
  return res.blob() as unknown as T;
}

/** Performs a v8 request and exposes the raw Response (needed for ETags). */
async function rawRequest(
  path: string,
  options: RequestInit = {},
): Promise<Response> {
  const headers: Record<string, string> = {
    ...authHeaders(),
    ...(options.headers as Record<string, string> | undefined),
  };
  return fetch(buildURL(path), {
    ...options,
    headers,
  });
}

async function request<T>(
  path: string,
  options: RequestInit = {},
): Promise<T> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...authHeaders(),
    ...(options.headers as Record<string, string> | undefined),
  };
  const res = await fetch(buildURL(path), {
    ...options,
    headers,
  });
  return parseBody<T>(res);
}

function get<T>(path: string): Promise<T> {
  return request<T>(path, { method: "GET" });
}

function put<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, {
    method: "PUT",
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
}

function patch<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, {
    method: "PATCH",
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
}

function del<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, {
    method: "DELETE",
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
}

function post<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, {
    method: "POST",
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
}

// ---------------------------------------------------------------------------
// v8 configuration tree
//
// The v8 contract stores configuration as a YAML tree. `GET /config` returns a
// JSON projection of that tree, path reads/writes use YAML keys joined by
// slashes (`/config/observability/logs/debug`), and writes may carry If-Match
// with the strong ETag from the last read so concurrent edits fail with 412
// instead of being silently overwritten.
// ---------------------------------------------------------------------------

/** Legacy settings paths mapped to their v8 YAML tree locations. */
const CONFIG_PATHS: Record<string, string> = {
  debug: "observability/logs/debug",
  "logging-to-file": "observability/logs/logging-to-file",
  "logs-max-total-size-mb": "observability/logs/logs-max-total-size-mb",
  "request-log": "observability/logs/request-log",
  "error-logs-max-files": "observability/logs/error-logs-max-files",
  "usage-statistics-enabled": "observability/usage/usage-statistics-enabled",
  "proxy-url": "requests/proxy-url",
  "request-retry": "routing/retry/request-retry",
  "max-retry-interval": "routing/retry/max-retry-interval",
  "max-retry-credentials": "routing/retry/max-retry-credentials",
  "force-model-prefix": "routing/force-model-prefix",
  "routing/strategy": "routing/strategy",
  "ws-auth": "oauth/providers/aistudio/ws-auth",
  "api-keys": "access/api-keys",
  "gemini-api-key": "api-keys/gemini",
  "claude-api-key": "api-keys/claude",
  "codex-api-key": "api-keys/codex",
  "vertex-api-key": "api-keys/vertex",
  "xai-api-key": "api-keys/xai",
  "meta-api-key": "api-keys/meta",
  "interactions-api-key": "api-keys/interactions",
  "openai-compatibility": "api-keys/openai-compatibility",
  "oauth-excluded-models": "oauth/excluded-models",
  "oauth-model-alias": "oauth/model-alias",
  "oauth-request-scoped-errors": "oauth/request-scoped-errors",
};

function configPath(path: string): string {
  const clean = path.replace(/^\/+/, "").replace(/\/+$/, "");
  return CONFIG_PATHS[clean] ?? clean.replace(/\./g, "/");
}

function configURL(path: string): string {
  return `/config/${configPath(path)}`;
}

async function getConfigValue<T>(path: string): Promise<T> {
  return get<T>(configURL(path));
}

async function putConfigValue<T>(path: string, value: unknown): Promise<T> {
  return put<T>(configURL(path), value);
}

async function deleteConfigValue<T>(path: string): Promise<T> {
  return del<T>(configURL(path));
}

/**
 * Conditional read-modify-write of a config subtree. A concurrent writer bumps
 * the config ETag, so a stale If-Match write returns 412 and we retry once
 * against the fresh value instead of clobbering the other change.
 */
async function updateConfigValue<T>(
  path: string,
  mutate: (current: T) => T,
): Promise<void> {
  for (let attempt = 0; attempt < 2; attempt += 1) {
    const res = await rawRequest(configURL(path), {
      method: "GET",
      headers: { Accept: "application/json" },
    });
    if (!res.ok) return throwAPIError(res);
    const current = (await res.json()) as T;
    const etag = res.headers.get("etag");
    const next = mutate(current);
    const write = await rawRequest(configURL(path), {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
        ...(etag ? { "If-Match": etag } : {}),
      },
      body: JSON.stringify(next),
    });
    if (write.status === 412 && attempt === 0) {
      continue;
    }
    if (!write.ok) return throwAPIError(write);
    return;
  }
}

async function getBool(path: string): Promise<boolean> {
  return (await getConfigValue<boolean>(path)) ?? false;
}

async function getInt(path: string): Promise<number> {
  return (await getConfigValue<number>(path)) ?? 0;
}

async function getStr(path: string): Promise<string> {
  return (await getConfigValue<string>(path)) ?? "";
}

export interface ConfigResponse {
  [key: string]: unknown;
}

export interface BooleanValue {
  value: boolean;
}

export interface IntValue {
  value: number;
}

export interface StringValue {
  value: string;
}

export interface ModelAlias {
  name: string;
  alias: string;
}

export interface GeminiKey {
  __group?: string;
  __key?: string;
  "api-key": string;
  priority?: number;
  prefix?: string;
  "base-url"?: string;
  "proxy-url"?: string;
  models?: ModelAlias[];
  headers?: Record<string, string>;
  "excluded-models"?: string[];
}

export interface ClaudeKey {
  __group?: string;
  __key?: string;
  "api-key": string;
  priority?: number;
  prefix?: string;
  "base-url"?: string;
  "proxy-url"?: string;
  models?: ModelAlias[];
  headers?: Record<string, string>;
  "excluded-models"?: string[];
}

export interface CodexKey {
  __group?: string;
  __key?: string;
  "api-key": string;
  priority?: number;
  prefix?: string;
  "base-url"?: string;
  websockets?: boolean;
  "proxy-url"?: string;
  models?: ModelAlias[];
  headers?: Record<string, string>;
  "excluded-models"?: string[];
}

export interface VertexKey {
  __group?: string;
  __key?: string;
  "api-key": string;
  priority?: number;
  prefix?: string;
  "base-url"?: string;
  "proxy-url"?: string;
  headers?: Record<string, string>;
  models?: ModelAlias[];
  "excluded-models"?: string[];
}

export interface OpenAICompatAPIKeyEntry {
  "api-key": string;
  "proxy-url"?: string;
}

export interface OpenAICompatEntry {
  __group?: string;
  __key?: string;
  name: string;
  priority?: number;
  prefix?: string;
  "base-url": string;
  "api-key-entries"?: OpenAICompatAPIKeyEntry[];
  models?: ModelAlias[];
  headers?: Record<string, string>;
}

export interface OAuthExcludedModel {
  model: string;
  reason?: string;
}

export interface OAuthModelAlias {
  alias: string;
  model: string;
}

export interface OAuthProvider {
  key: string;
  display_name: string;
  flow_type: string;
  auth_url_endpoint?: string;
  aliases?: string[];
  configured?: boolean;
  /** Backend capability flags: unavailable providers are listed but disabled. */
  available?: boolean;
  unavailable_reason?: string;
  flow?: string;
  methods?: string[];
}

export interface AuthFile {
  name: string;
  provider: string;
  status: string;
  models?: string[];
  size?: number;
  modified?: string;
  disabled?: boolean;
  fields?: Record<string, string>;
  // v8 /credentials entries carry additional runtime metadata used by the table.
  id?: string;
  type?: string;
  auth_index?: number;
  label?: string;
  status_message?: string;
  unavailable?: boolean;
  priority?: number;
  cooldowns?: unknown;
}

export interface AuthFileModels {
  models: string[];
}

export interface UsageStatistics {
  [key: string]: unknown;
}

export interface ApplicationLogs {
  lines: string[];
  line_count: number;
  latest_timestamp: number;
}

export interface ErrorLogFile {
  name: string;
  size: number;
  modified: string;
}

export interface ErrorLogFiles {
  files: ErrorLogFile[];
}

export interface RequestLogEntry {
  id: string;
  [key: string]: unknown;
}

export interface ModelMapping {
  source: string;
  target: string;
}

export interface ModelDefinitions {
  [channel: string]: unknown;
}

/**
 * State machine of a v8 OAuth session:
 * wait -> challenge (URL/user code to visit) -> maybe input (paste a code or a
 * redirect URL) -> ok | error. Terminal states are retained briefly by the
 * backend, so the UI may observe them more than once.
 */
export interface AuthStatus {
  status: "ok" | "wait" | "challenge" | "input" | "error";
  state?: string;
  provider?: string;
  error?: string;
  url?: string;
  user_code?: string;
  verification_uri?: string;
  prompt?: string;
  prompt_id?: string;
  message?: string;
}

export interface LatestVersion {
  "latest-version": string;
}

export interface RoutingStrategy {
  value: string;
}

export interface ConfigWithETag {
  config: ConfigResponse;
  etag: string | null;
}

async function readConfigWithETag(): Promise<ConfigWithETag> {
  const res = await rawRequest("/config", {
    method: "GET",
    headers: { Accept: "application/json" },
  });
  if (!res.ok) return throwAPIError(res);
  return {
    config: (await res.json()) as ConfigResponse,
    etag: res.headers.get("etag"),
  };
}

async function readConfigYAMLWithETag(): Promise<{
  yaml: string;
  etag: string | null;
}> {
  const res = await rawRequest("/config.yaml", {
    method: "GET",
    headers: { Accept: "application/yaml" },
  });
  if (!res.ok) return throwAPIError(res);
  return { yaml: await res.text(), etag: res.headers.get("etag") };
}

async function writeConfigYAML(yaml: string, etag?: string | null) {
  const headers: Record<string, string> = {
    "Content-Type": "application/yaml",
  };
  if (etag) {
    headers["If-Match"] = etag;
  }
  const res = await rawRequest("/config.yaml", {
    method: "PUT",
    headers,
    body: yaml,
  });
  const body = await parseBody<{ status: string }>(res);
  return { status: body.status ?? "ok", etag: res.headers.get("etag") };
}

// ---------------------------------------------------------------------------
// Provider credentials are grouped in v8: every entry of api-keys.<family> is a
// named group that owns one base-url and a keys list. Group-level policy fields
// are shared; a key may override them. The table works on a flattened view, but
// every mutation targets the stable group name plus the api-key value so a
// concurrent insert cannot shift an update onto another credential.
// ---------------------------------------------------------------------------

export interface V8KeyGroup {
  name?: string;
  "base-url"?: string;
  keys?: Array<Record<string, unknown>>;
  [field: string]: unknown;
}

export interface KeyIdentity {
  __group?: string;
  __key?: string;
}

const GROUP_SHARED_FIELDS = [
  "priority",
  "prefix",
  "proxy-url",
  "headers",
  "models",
  "excluded-models",
  "disable-cooling",
  "request-retry",
  "request-scoped-errors",
] as const;

interface KeyPatch<T> {
  group?: string;
  match?: string;
  value: Partial<T>;
}

interface KeyDelete {
  group?: string;
  match?: string;
}

function groupName(group: V8KeyGroup, index: number): string {
  const name = group.name;
  return typeof name === "string" && name.trim() !== ""
    ? name.trim()
    : `group-${index + 1}`;
}

function flattenKeyGroups<T>(groups: V8KeyGroup[] | null): Array<T & KeyIdentity> {
  const out: Array<T & KeyIdentity> = [];
  (groups ?? []).forEach((group, index) => {
    const name = groupName(group, index);
    const shared: Record<string, unknown> = {};
    for (const field of GROUP_SHARED_FIELDS) {
      if (group[field] !== undefined) shared[field] = group[field];
    }
    if (group["base-url"] !== undefined) shared["base-url"] = group["base-url"];
    const keys = Array.isArray(group.keys) ? group.keys : [];
    for (const key of keys) {
      out.push({
        ...shared,
        ...key,
        __group: name,
        __key: String(key["api-key"] ?? ""),
      } as T & KeyIdentity);
    }
  });
  return out;
}

function splitKeyValue(value: Record<string, unknown>): {
  groupFields: Record<string, unknown>;
  keyFields: Record<string, unknown>;
} {
  const groupFields: Record<string, unknown> = {};
  const keyFields: Record<string, unknown> = {};
  for (const [field, fieldValue] of Object.entries(value)) {
    if (field === "__group" || field === "__key") continue;
    if (
      field === "base-url" ||
      (GROUP_SHARED_FIELDS as readonly string[]).includes(field)
    ) {
      groupFields[field] = fieldValue;
      continue;
    }
    keyFields[field] = fieldValue;
  }
  return { groupFields, keyFields };
}

function uniqueGroupName(
  groups: V8KeyGroup[],
  family: string,
  apiKey: string,
): string {
  const suffix = apiKey.replace(/[^A-Za-z0-9]+/g, "").slice(-8).toLowerCase();
  const base = `${family}-${suffix || "key"}`;
  const taken = new Set(groups.map((group, index) => groupName(group, index)));
  let name = base;
  let counter = 2;
  while (taken.has(name)) {
    name = `${base}-${counter}`;
    counter += 1;
  }
  return name;
}

function findGroupIndex(groups: V8KeyGroup[], name?: string): number {
  if (!name) return -1;
  return groups.findIndex((group, index) => groupName(group, index) === name);
}

async function readKeyGroup<T>(family: string): Promise<Array<T & KeyIdentity>> {
  const groups = await getConfigValue<V8KeyGroup[]>(`api-keys/${family}`);
  return flattenKeyGroups<T>(groups);
}

function mutateKeyGroup(
  current: V8KeyGroup[] | null,
  family: string,
  body: KeyPatch<Record<string, unknown>>,
): V8KeyGroup[] {
  const groups: V8KeyGroup[] = (current ?? []).map((group) => ({
    ...group,
    keys: Array.isArray(group.keys) ? [...group.keys] : [],
  }));
  const { groupFields, keyFields } = splitKeyValue(body.value);
  const groupIndex = findGroupIndex(groups, body.group);
  if (groupIndex < 0) {
    const apiKey = String(keyFields["api-key"] ?? "");
    groups.push({
      ...groupFields,
      name: uniqueGroupName(groups, family, apiKey),
      keys: [keyFields],
    });
    return groups;
  }

  const group = groups[groupIndex];
  const keys = group.keys as Array<Record<string, unknown>>;
  const match = body.match ?? String(keyFields["api-key"] ?? "");
  const keyIndex = match
    ? keys.findIndex((key) => String(key["api-key"] ?? "") === match)
    : -1;
  const baseUrlChanged =
    typeof groupFields["base-url"] === "string" &&
    groupFields["base-url"] !== (group["base-url"] ?? "");

  if (keyIndex >= 0) {
    if (baseUrlChanged && keys.length > 1) {
      // base-url belongs to the group. Move only this credential so sibling
      // keys keep their existing endpoint and policy.
      const [movedKey] = keys.splice(keyIndex, 1);
      if (keys.length === 0) groups.splice(groupIndex, 1);
      groups.push({
        ...groupFields,
        name: uniqueGroupName(groups, family, String(keyFields["api-key"] ?? "")),
        keys: [{ ...movedKey, ...keyFields }],
      });
      return groups;
    }
    if (keys.length === 1) {
      Object.assign(group, groupFields);
      Object.assign(keys[0], keyFields);
      return groups;
    }
    // Shared fields become explicit per-key overrides; the group and sibling
    // credentials stay untouched. base-url cannot be a per-key field.
    const overrides = { ...groupFields };
    delete overrides["base-url"];
    Object.assign(keys[keyIndex], overrides, keyFields);
    return groups;
  }

  if (keys.length === 1) {
    Object.assign(group, groupFields);
    Object.assign(keys[0], keyFields);
    return groups;
  }
  const overrides = { ...groupFields };
  delete overrides["base-url"];
  keys.push({ ...overrides, ...keyFields });
  return groups;
}

function patchKeyGroup<T>(
  family: string,
  body: KeyPatch<T>,
): Promise<void> {
  return updateConfigValue<V8KeyGroup[]>(`api-keys/${family}`, (current) =>
    mutateKeyGroup(
      current,
      family,
      body as KeyPatch<Record<string, unknown>>,
    ),
  );
}

function deleteKeyGroup(
  family: string,
  params: KeyDelete,
): Promise<{ status: string }> {
  return updateConfigValue<V8KeyGroup[]>(`api-keys/${family}`, (current) => {
    const groups: V8KeyGroup[] = (current ?? []).map((group) => ({
      ...group,
      keys: Array.isArray(group.keys) ? [...group.keys] : [],
    }));
    const groupIndex = findGroupIndex(groups, params.group);
    if (groupIndex < 0) return groups;
    const group = groups[groupIndex];
    const keys = group.keys as Array<Record<string, unknown>>;
    const match = params.match ?? "";
    const keyIndex = keys.findIndex(
      (key) => String(key["api-key"] ?? "") === match,
    );
    if (keyIndex >= 0) keys.splice(keyIndex, 1);
    if (keys.length === 0) groups.splice(groupIndex, 1);
    return groups;
  }).then(() => ({ status: "ok" }));
}

export const api = {
  config: {
    getConfig: () => get<ConfigResponse>("/config"),
    getConfigWithETag: readConfigWithETag,
    putConfig: (
      config: ConfigResponse,
      etag?: string | null,
    ): Promise<{ status: string; etag: string | null }> =>
      rawRequest("/config", {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          ...(etag ? { "If-Match": etag } : {}),
        },
        body: JSON.stringify(config),
      }).then(async (res) => {
        const body = await parseBody<{ status: string }>(res);
        return { status: body.status ?? "ok", etag: res.headers.get("etag") };
      }),
    updateConfigValue: <T,>(path: string, mutate: (value: T) => T) =>
      updateConfigValue<T>(path, mutate),
    getConfigYAML: async () => (await readConfigYAMLWithETag()).yaml,
    getConfigYAMLWithETag: readConfigYAMLWithETag,
    putConfigYAML: (yaml: string, etag?: string | null) =>
      writeConfigYAML(yaml, etag),
    getLatestVersion: () => get<LatestVersion>("/server/latest-version"),
  },

  boolean: {
    getDebug: () => getBool("debug"),
    putDebug: (value: boolean) => putConfigValue("debug", value),
    getLoggingToFile: () => getBool("logging-to-file"),
    putLoggingToFile: (value: boolean) =>
      putConfigValue("logging-to-file", value),
    getUsageStatisticsEnabled: () => getBool("usage-statistics-enabled"),
    putUsageStatisticsEnabled: (value: boolean) =>
      putConfigValue("usage-statistics-enabled", value),
    getRequestLogEnabled: () => getBool("request-log"),
    putRequestLog: (value: boolean) => putConfigValue("request-log", value),
    getWsAuth: () => getBool("ws-auth"),
    putWsAuth: (value: boolean) => putConfigValue("ws-auth", value),
    getForceModelPrefix: () => getBool("force-model-prefix"),
    putForceModelPrefix: (value: boolean) =>
      putConfigValue("force-model-prefix", value),
  },

  string: {
    getProxyURL: () => getStr("proxy-url"),
    putProxyURL: (value: string) => putConfigValue("proxy-url", value),
    deleteProxyURL: () => deleteConfigValue("proxy-url"),
  },

  integer: {
    getLogsMaxTotalSizeMB: () => getInt("logs-max-total-size-mb"),
    putLogsMaxTotalSizeMB: (value: number) =>
      putConfigValue("logs-max-total-size-mb", value),
    getErrorLogsMaxFiles: () => getInt("error-logs-max-files"),
    putErrorLogsMaxFiles: (value: number) =>
      putConfigValue("error-logs-max-files", value),
    getRequestRetry: () => getInt("request-retry"),
    putRequestRetry: (value: number) => putConfigValue("request-retry", value),
    getMaxRetryInterval: () => getInt("max-retry-interval"),
    putMaxRetryInterval: (value: number) =>
      putConfigValue("max-retry-interval", value),
  },

  routing: {
    getRoutingStrategy: async () =>
      (await getConfigValue<string | null>("routing/strategy")) ?? "",
    putRoutingStrategy: (strategy: string) =>
      putConfigValue("routing/strategy", strategy),
  },

  apiKeys: {
    getAPIKeys: async () =>
      (await getConfigValue<string[] | null>("api-keys")) ?? [],
    patchAPIKeys: async ({ value }: { value: string }) => {
      await updateConfigValue<string[]>("api-keys", (current) => {
        const keys = current ?? [];
        return keys.includes(value) ? keys : [...keys, value];
      });
      return { status: "ok" };
    },
    deleteAPIKeys: async ({
      value,
      index,
    }: {
      value?: string;
      index?: number;
    }) => {
      await updateConfigValue<string[]>("api-keys", (current) => {
        const keys = current ?? [];
        if (value !== undefined) {
          return keys.filter((key) => key !== value);
        }
        if (index !== undefined) {
          return keys.filter((_, position) => position !== index);
        }
        return keys;
      });
      return { status: "ok" };
    },
  },

  geminiKeys: {
    getGeminiKeys: () => readKeyGroup<GeminiKey>("gemini"),
    patchGeminiKey: (body: KeyPatch<GeminiKey>) =>
      patchKeyGroup("gemini", body),
    deleteGeminiKey: (params: KeyDelete) =>
      deleteKeyGroup("gemini", params),
  },

  claudeKeys: {
    getClaudeKeys: () => readKeyGroup<ClaudeKey>("claude"),
    patchClaudeKey: (body: KeyPatch<ClaudeKey>) =>
      patchKeyGroup("claude", body),
    deleteClaudeKey: (params: KeyDelete) =>
      deleteKeyGroup("claude", params),
  },

  codexKeys: {
    getCodexKeys: () => readKeyGroup<CodexKey>("codex"),
    patchCodexKey: (body: KeyPatch<CodexKey>) =>
      patchKeyGroup("codex", body),
    deleteCodexKey: (params: KeyDelete) =>
      deleteKeyGroup("codex", params),
  },

  vertexKeys: {
    getVertexKeys: () => readKeyGroup<VertexKey>("vertex"),
    patchVertexKey: (body: KeyPatch<VertexKey>) =>
      patchKeyGroup("vertex", body),
    deleteVertexKey: (params: KeyDelete) =>
      deleteKeyGroup("vertex", params),
  },

  xaiKeys: {
    getXAIKeys: () => readKeyGroup<VertexKey>("xai"),
    patchXAIKey: (body: KeyPatch<VertexKey>) => patchKeyGroup("xai", body),
    deleteXAIKey: (params: KeyDelete) => deleteKeyGroup("xai", params),
  },

  metaKeys: {
    getMetaKeys: () => readKeyGroup<VertexKey>("meta"),
    patchMetaKey: (body: KeyPatch<VertexKey>) => patchKeyGroup("meta", body),
    deleteMetaKey: (params: KeyDelete) => deleteKeyGroup("meta", params),
  },

  openAICompat: {
    getOpenAICompat: () => readKeyGroup<OpenAICompatEntry>("openai-compatibility"),
    patchOpenAICompat: (body: KeyPatch<OpenAICompatEntry>) =>
      patchKeyGroup("openai-compatibility", body),
    deleteOpenAICompat: (params: KeyDelete) =>
      deleteKeyGroup("openai-compatibility", params),
  },

  oauth: {
    getOAuthExcludedModels: async () => {
      const res = await getConfigValue<Record<string, string[]>>(
        "oauth-excluded-models",
      );
      return res ?? {};
    },
    putOAuthExcludedModels: (models: Record<string, string[]>) =>
      putConfigValue("oauth-excluded-models", models),
    patchOAuthExcludedModels: (body: { provider: string; models: string[] }) =>
      updateConfigValue<Record<string, string[]>>(
        "oauth-excluded-models",
        (models) => ({ ...(models ?? {}), [body.provider]: body.models }),
      ),
    deleteOAuthExcludedModels: (params: { provider: string }) =>
      updateConfigValue<Record<string, string[]>>(
        "oauth-excluded-models",
        (models) => {
          const next = { ...(models ?? {}) };
          delete next[params.provider];
          return next;
        },
      ),
    getOAuthModelAlias: async () => {
      const res = await getConfigValue<Record<string, ModelAlias[]>>(
        "oauth-model-alias",
      );
      return res ?? {};
    },
    putOAuthModelAlias: (aliases: Record<string, ModelAlias[]>) =>
      putConfigValue("oauth-model-alias", aliases),
    patchOAuthModelAlias: (body: { provider: string; aliases: ModelAlias[] }) =>
      updateConfigValue<Record<string, ModelAlias[]>>(
        "oauth-model-alias",
        (aliases) => ({ ...(aliases ?? {}), [body.provider]: body.aliases }),
      ),
    deleteOAuthModelAlias: (params: { channel: string }) =>
      updateConfigValue<Record<string, ModelAlias[]>>(
        "oauth-model-alias",
        (aliases) => {
          const next = { ...(aliases ?? {}) };
          delete next[params.channel];
          return next;
        },
      ),
  },

  authFiles: {
    listAuthFiles: async () => {
      const res = await get<{ files: AuthFile[] }>("/credentials");
      return res.files ?? [];
    },
    getAuthFileModels: (name: string) => {
      const query = new URLSearchParams({ name }).toString();
      return get<AuthFileModels>(`/credentials/models?${query}`);
    },
    downloadAuthFile: (params: { name: string; provider?: string }) => {
      const query = new URLSearchParams({ name: params.name }).toString();
      return get<Blob>(`/credentials/download?${query}`);
    },
    uploadAuthFile: (formData: FormData) => {
      return fetch(buildURL("/credentials"), {
        method: "POST",
        headers: authHeaders(),
        body: formData,
      }).then((res) => parseBody<{ status: string }>(res));
    },
    deleteAuthFile: (params: { name: string; provider?: string }) =>
      del<{ status: string }>("/credentials", { name: params.name }),
    patchAuthFileStatus: (params: {
      name: string;
      provider?: string;
      disabled: boolean;
    }) =>
      patch<{ status: string }>("/credentials/status", {
        name: params.name,
        disabled: params.disabled,
      }),
    patchAuthFileFields: (params: {
      name: string;
      provider?: string;
      fields: Record<string, string>;
    }) =>
      patch<{ status: string }>("/credentials/fields", {
        name: params.name,
        ...params.fields,
      }),
    refreshAuthFile: (params: { name?: string; all?: boolean }) =>
      post<{ ok: boolean }>("/credentials/refresh", params),
  },

  oauthFlows: {
    getOAuthProviders: () =>
      get<{ providers: OAuthProvider[] }>("/oauth/providers").then(
        (body) => body.providers ?? [],
      ),
    requestToken: (provider: string, params?: Record<string, unknown>) => {
      const query = new URLSearchParams({ provider });
      if (params) {
        for (const [key, value] of Object.entries(params)) {
          if (value !== undefined && value !== null) {
            query.set(key, String(value));
          }
        }
      }
      return get<{
        status: string;
        url?: string;
        state?: string;
        method?: string;
        provider?: string;
        flow?: string;
        user_code?: string;
        verification_uri?: string;
        prompt?: string;
        prompt_id?: string;
      }>(`/oauth/auth-url?${query.toString()}`);
    },
    requestAnthropicToken: () => api.oauthFlows.requestToken("claude"),
    requestCodexToken: (params?: Record<string, unknown>) =>
      api.oauthFlows.requestToken("codex", params),
    requestAntigravityToken: (params?: Record<string, unknown>) =>
      api.oauthFlows.requestToken("antigravity", params),
    requestKimiToken: () => api.oauthFlows.requestToken("kimi"),
    requestKimiAIToken: () => api.oauthFlows.requestToken("kimi-ai"),
    requestXAIToken: (params?: Record<string, unknown>) =>
      api.oauthFlows.requestToken("xai", params),
    requestDevinToken: () => api.oauthFlows.requestToken("devin"),
    requestMetaToken: () => api.oauthFlows.requestToken("meta"),
    requestKiroToken: (params?: Record<string, unknown>) =>
      api.oauthFlows.requestToken("kiro", params),
    requestCursorToken: (params?: Record<string, unknown>) =>
      api.oauthFlows.requestToken("cursor", params),
    requestGitLabToken: (params?: Record<string, unknown>) =>
      api.oauthFlows.requestToken("gitlab", params),
    requestQoderToken: (params?: Record<string, unknown>) =>
      api.oauthFlows.requestToken("qoder", params),
    requestKiloToken: () => api.oauthFlows.requestToken("kilo"),
    requestCodeArtsToken: (params?: Record<string, unknown>) =>
      api.oauthFlows.requestToken("codearts", params),
    requestJoyCodeToken: (params?: Record<string, unknown>) =>
      api.oauthFlows.requestToken("joycode", params),
    requestIFlowToken: (params?: Record<string, unknown>) =>
      api.oauthFlows.requestToken("iflow", params),
    requestGitHubToken: () => api.oauthFlows.requestToken("github-copilot"),
    postOAuthCallback: (body: Record<string, unknown>) =>
      post<{ status: string }>("/oauth/callback", body),
    getAuthStatus: (state: string) =>
      get<AuthStatus>(`/oauth/status?state=${encodeURIComponent(state)}`),
    /** Answer a provider prompt (device code, redirect URL, region choice...). */
    submitAuthInput: (params: {
      state: string;
      prompt_id: string;
      input: string;
    }) =>
      post<{ status: string; state?: string }>("/oauth/session/input", {
        state: params.state,
        prompt_id: params.prompt_id,
        input: params.input,
      }),
    /**
     * Cancels a pending session. A 409 means the provider is already committing
     * the credential; surface that instead of pretending the cancel worked.
     */
    cancelAuthSession: (state: string) =>
      del<{ status: string; cancelled: boolean }>(
        `/oauth/session?state=${encodeURIComponent(state)}`,
      ),
    importVertexCredential: (file: File | Blob, filename = "vertex-sa.json") => {
      const formData = new FormData();
      formData.append("file", file, filename);
      return fetch(buildURL("/oauth/import?provider=vertex"), {
        method: "POST",
        headers: authHeaders(),
        body: formData,
      }).then((res) => parseBody<{ status: string }>(res));
    },
  },

  usage: {
    getUsageStatistics: () =>
      get<{ usage: UsageStatistics; failed_requests: number }>(
        "/observability/usage",
      ),
    exportUsageStatistics: () =>
      get<{ version: number; exported_at: string; usage: UsageStatistics }>(
        "/observability/usage/export",
      ),
    importUsageStatistics: (data: unknown) =>
      post<{
        added: number;
        skipped: number;
        total_requests: number;
        failed_requests: number;
      }>("/observability/usage/import", data),
    getAPIKeyUsage: () => get<Record<string, unknown>>("/observability/usage/api-keys"),
    getUsageQueue: (count = 1) =>
      get<unknown[]>(`/observability/usage/queue?count=${count}`),
  },

  logs: {
    getLogs: (after?: number) =>
      get<ApplicationLogs>(
        `/observability/logs${after != null ? `?after=${after}` : ""}`,
      ),
    deleteLogs: () => del<{ status: string }>("/observability/logs"),
    getRequestErrorLogs: () =>
      get<ErrorLogFiles>("/observability/logs/errors"),
    downloadRequestErrorLog: (name: string) =>
      get<Blob>(`/observability/logs/errors/${encodeURIComponent(name)}`),
    getRequestLogByID: (id: string) =>
      get<RequestLogEntry>(
        `/observability/logs/requests/${encodeURIComponent(id)}`,
      ),
    getRequestLogEnabled: () => getBool("request-log"),
    putRequestLog: (value: boolean) => putConfigValue("request-log", value),
  },

  modelDefinitions: {
    getModelDefinitions: (channel: string) =>
      get<{ channel: string; models: ModelDefinitions }>(
        `/routing/model-definitions/${encodeURIComponent(channel)}`,
      ),
  },

  apiCall: (body: Record<string, unknown>) =>
    post<Record<string, unknown>>("/requests/api-call", body),

  vertexImport: (body: Record<string, unknown>) => {
    const file = new Blob([JSON.stringify(body)], {
      type: "application/json",
    });
    return api.oauthFlows.importVertexCredential(file);
  },
};

export { APIError };
export { configPath as resolveConfigPath };
