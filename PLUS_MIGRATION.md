# Plus extensions on CPA v8

This integration uses CPA v8 as the base and ports the independent changes from
CLIProxyAPIPlus. It does not replace current CPA files with their older v7 copies.
Current CPA security checks, plugin routing, Home behavior, and the v8 management
API remain authoritative. Upstream-deleted Amp and Gemini CLI functionality is not
restored merely because it existed in the common ancestor.

## Providers and credentials

Native executors are registered for Kiro, Kilo, Cursor, GitHub Copilot, CodeBuddy,
CodeBuddy AI, CodeArts, JoyCode, GitLab Duo, BT, Qoder, and iFlow. The command-line
login switches are listed by `cli-proxy-api -help`. BT uses configured accounts;
GitLab supports OAuth and personal access tokens. Kiro supports AWS Builder ID,
IAM Identity Center, authorization code login, and token-file import. Unsupported
Google/GitHub Kiro login methods are not advertised as working.

The commented examples in `config.example.yaml` show `credentials.kiro`,
`credentials.bt`, and `oauth.providers.kiro`. Inline Kiro fields override token-file
fields. Keep token files and configuration private. BT's base64 password is not
encrypted; it is used for activation but is not copied into generated auth JSON.
Credential activation must not block configuration locks or publish results from
a superseded configuration generation.

`oauth.incognito-browser: true` requests a private browser window for CLI logins.
Chrome/Chromium, Edge, or Firefox must be installed. When no supported browser is
found, the launcher fails rather than silently opening a non-private window; use
`-no-browser` and open a private window manually. The service never kills an
existing browser process. Remote management logins never launch a server browser.

## Management UI and API

The optional Plus dashboard is served at `/plus/`. The upstream `/management.html`
control panel is unchanged. Home mode and `management.disable-control-panel`
disable the Plus dashboard as well. Static assets contain no management secret;
the operator supplies a management key and API calls use the protected
`/v8/management` endpoints. Do not expose the management API without transport
security and access controls. As with existing CPA API-key configuration, a
management key grants access to configuration secrets: BT passwords and Kiro
access/refresh tokens are visible in authenticated configuration exports. Base64 is not
redaction or encryption; grant management access only to trusted operators.

Plus login flows use `/oauth/auth-url`, `/oauth/status`, and `/oauth/session` under
the v8 management prefix. Device/browser challenges are polled asynchronously.
Some remote-browser providers require pasting the callback URL from the address
bar when a loopback redirect cannot load on the operator's machine. Prompt input
is submitted to `/oauth/session/input` with both the session state and prompt ID.
A cancellation rejected because persistence already began is not reported as a
successful cancellation. GitLab PAT import uses `/oauth/import?provider=gitlab`;
only HTTPS GitLab base URLs and same-origin redirects are accepted, including
legitimate private self-hosted installations.

Usage aggregation is available at `/observability/usage`, with export and import
at `/observability/usage/export` and `/observability/usage/import`, under the v8
management prefix and authenticated management boundary. The usage-statistics
toggle controls recording, not endpoint availability. Home mode disables the
management endpoints. The existing v8 usage queue and API-key usage routes are
preserved. Kiro quota is fetched
on demand through `/observability/quota/kiro?auth_index=...`; it uses caller
cancellation and the canonical credential refresh manager rather than launching
an unbounded background quota poller.

## Build

Requirements: Go 1.26 and Node.js 24 with npm.

```sh
cd web
npm ci
npm test
npm run lint
npm run build:embed
cd ..
go test ./...
go build -o cli-proxy-api ./cmd/server
```

`build:embed` exports Next.js statically and copies it into the Go embed directory.
Generated browser assets are not committed. A plain Go build without that step
still works, but `/plus/` is unavailable. Docker builds the static dashboard in a
separate Node stage before compiling Go; no Node server is included at runtime.
The integration CI builds the dashboard before testing real Go static routes.

## Request semantics and verification limits

New executors use the CPA v8 thinking pipeline and apply payload rules at the
final provider payload boundary. Kiro rules target the native `kiro` payload, not
an intermediate OpenAI message that is later converted. Cursor rules are applied
to its semantic request before protobuf encoding. Retry payloads must pass through
the same final rule boundary. Caller cancellation controls inference requests;
provider credential acquisition is separate from inference streaming.

Local token-count implementations for providers without an authoritative token
count endpoint are estimates, not billing guarantees. Unit/contract tests cover
registration, configuration, payload order, callback validation, and cancellation.
Live provider login and inference require actual accounts and are not established
by mocked tests or a successful build.

Configuration file coordination prevents participating in-process readers from
observing a management write's truncate/write window while preserving the mounted
file's inode, symlink behavior and permissions. It does not make writes by external
editors or Git repository synchronization/staging atomic, and it is not a crash-safe
filesystem transaction. Object-store and PostgreSQL configuration persistence use
the coordinated reader; the inherited Git-store synchronization design is unchanged.

The frontend was upgraded from Next.js 16.2.4 to 16.4.2. The unused shadcn CLI
package was removed while its MIT-licensed stylesheet was preserved locally.
Compatible dependency fixes leave an unresolved development-only ESLint chain
through `braces` (GHSA-vfj7-8cjw-p6xm); npm counts five affected packages. No forced
downgrade to an older Next.js lint configuration is used as a purported fix.
