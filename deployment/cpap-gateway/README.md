# CLIProxyAPI release-pull deploy

Server-side, console-installed updater for a systemd-managed CLIProxyAPI
instance. The server pulls published GitHub Releases over HTTPS; there is no
inbound SSH deployment path, no deploy key, and no GitHub token on the server.

The installer and unit files live in the same directory. `install.sh --help`
is authoritative for installer options and exact unit names; this document
describes the updater CLI and the operating contract.

## At a glance

| Item | Behavior |
| --- | --- |
| Source of releases | Public GitHub Releases of `SCNU-PIONEER/CLIProxyAPI` |
| Release metadata | GitHub REST Releases API, not the Atom feed |
| Eligible version | Stable `vX.Y.Z` only; drafts and pre-releases ignored |
| Linux asset | `CLIProxyAPI_<version>_linux_amd64.tar.gz` or `..._aarch64.tar.gz` |
| Build flavor | Default glibc build (plugin-capable); `_no-plugin` is never selected |
| Integrity | SHA-256 from the release's `checksums.txt`, plus tar member validation |
| What is replaced | Only the live `cli-proxy-api` binary |
| What is never written by the updater | `config.yaml`, the auth directory, the application log directory, the systemd unit |
| Failure handling | Previous binary restored automatically on deploy failure; if rollback also fails, the journal requires manual recovery. Config snapshot is for manual recovery only |
| Credentials on server | None: no GitHub token, no SSH key, no API key |

## Requirements

- Linux `x86_64` (amd64) or `aarch64` (arm64).
- glibc 2.17 or newer. The default release build is CGO/glibc and supports
  plugins; the portable `_no-plugin` build is intentionally not deployable.
- systemd, with the existing service (default `cliproxyapi.service`) already
  running the binary at `/root/cliproxyapi/cli-proxy-api`.
- Root access for installation and deployment.
- Python 3.8+ with the standard library. No pip packages are required.
- `git` for the one-time checkout. The updater itself uses HTTPS only.
- Outbound HTTPS to `github.com`, `api.github.com`, and
  `objects.githubusercontent.com`.
- The repository must remain public. The unauthenticated GitHub API allows
  roughly 60 requests per hour per IP; the updater makes a small number of
  calls per run.

The existing `config.yaml`, `/root/.cli-proxy-api`, and logs stay in place. The
installer does not modify `cliproxyapi.service`.

## How it works

1. Query the GitHub REST Releases API and keep only formal Releases
   (`draft=false`, `prerelease=false`, non-empty `published_at`) whose tag is a
   stable `vX.Y.Z`.
2. Require the exact Linux asset for the host architecture and an entry for
   that asset in the release's `checksums.txt`. A release that exists without
   both is not deployable yet.
3. Download the archive to a root-only staging area, verify its SHA-256, then
   validate every tar member. Only the regular `cli-proxy-api` file is
   extracted; links, devices, absolute paths, and traversal are rejected.
4. Run the staged binary with `-h -config /dev/null` in a scratch directory and
   require the printed version to equal the release tag. The Go banner is
   printed before flag parsing, so this neither starts the server nor reads the
   real configuration.
5. Snapshot `config.yaml` (mode 0600), back up the live binary, write
   and fsync a journal, then atomically replace the binary with `os.replace`.
6. Restart the existing unit. Wait for the health endpoint and for systemd's
   `MainPID` to settle on the new binary, verified through
   `/proc/<pid>/exe` and its SHA-256.
7. On success, record the deployed tag, asset digest, binary digest, and config
   snapshot path. On restart/health/process-identity failure, restore the
   previous binary and restart again. Config is not restored automatically.

Downgrades are refused. The updater retains the 20 most recent failed tags; a
retained tag is skipped until an operator passes `--retry`. Older entries are
evicted, so a release whose failure has aged out can be attempted again by the
timer. A major-version crossing is skipped unless an operator explicitly runs
with `--allow-major`; the timer never passes that flag.

## Install

### Pinned checkout

Use a reviewed, immutable commit. Do not install from a branch head, and do not
use `curl ... | bash`.

```bash
sudo git clone https://github.com/SCNU-PIONEER/CLIProxyAPI.git /opt/cliproxyapi-release
sudo git -C /opt/cliproxyapi-release checkout --detach <PINNED_COMMIT>
test "$(sudo git -C /opt/cliproxyapi-release rev-parse HEAD)" = "<PINNED_COMMIT>"
```

Replace `<PINNED_COMMIT>` with the reviewed commit SHA supplied with this
deployment. The checkout is installation/recovery material; the running
updater reads Releases over HTTPS, not this checkout. The checkout must be
root-owned and not writable by unprivileged users, because `install.sh` runs its
`cpap_deploy.py` under `sudo`.

### Console install, disabled by default

```bash
cd /opt/cliproxyapi-release
sudo bash deployment/cpap-gateway/install.sh
```

The installer places the `cpap-deploy` command and the systemd units. It also
runs a source and an installed `--check`, records the baseline if none exists,
and previews the latest release with `--dry-run`; it never deploys. On a fresh
host the timer remains disabled. On a reinstall it leaves an already-enabled
timer enabled: the installer never disables it. Without `--enable`, a failed
release preview is only a warning; the installer still exits successfully and
does not change the timer state.

Confirm the resulting state:

```bash
systemctl is-enabled cpap-deploy.timer   # disabled on a fresh install
systemctl status cpap-deploy.timer --no-pager
```

### First-run checks

The installer already runs these checks. You can rerun them from the console
before enabling automatic updates:

```bash
sudo cpap-deploy --check          # host, unit, binary, config, health
sudo cpap-deploy --baseline       # record baseline if absent (probe + snapshot)
sudo cpap-deploy --dry-run        # show the release that would be deployed
```

`--baseline` runs the live binary in a scratch directory and parses its
`CLIProxyAPI Version: ...` line. When it creates the baseline, it records the
probed version, binary SHA-256, and — if a config file exists — a config
snapshot. It is a no-op if a baseline is already recorded. It does not use
`version.txt` or any historical version record. If the probe cannot run, the
command fails rather than guessing; use `--baseline-version vX.Y.Z` only when
the version has been established independently.

For the first install, do one manual deployment and complete the operator
checks in [Verification and limits](#verification-and-limits) before arming the
timer:

```bash
sudo cpap-deploy --latest
```

This is a recommendation for the first fork/compatibility change, not a
per-release confirmation requirement. `--enable` is the explicit authorization
for future automatic deployments.

### Enable future automatic updates

The installer repeats the preflight checks, records the baseline if absent,
previews the latest release, and only then enables and starts the timer:

```bash
sudo bash deployment/cpap-gateway/install.sh --enable
```

On a host where the timer was already enabled, it stays enabled even if a later
step of the reinstall fails; disable it explicitly if you need a hard stop.
If you have already checked the deployment state, the advanced direct
alternative is:

```bash
sudo systemctl enable --now cpap-deploy.timer
```

Do not enable the unit before the preview/state is understood. `--enable` is the
explicit authorization for future automatic deployments. The timer then runs
`cpap-deploy --latest` about every five minutes (5 min plus up to 60 s jitter)
without asking again for each release. Major-version upgrades remain manual and
require `--allow-major`.

To pause automatic updates:

```bash
sudo systemctl disable --now cpap-deploy.timer
```

### If the Releases page is empty

The repository may currently show zero published Releases. That is a starting
state, not a permanent limitation: publish the first stable tag through the
normal release workflow (see [Publishing a release](#publishing-a-release)),
wait until the release and its `checksums.txt` are published, then run
`--dry-run` again.

## CLI

| Mode | Purpose |
| --- | --- |
| no mode | Print local deployment state; no network |
| `--check` | Validate host, Python, binary, config, auth dir, unit, health |
| `--baseline` | Record an initial baseline (probe, digest, config snapshot); no-op if already recorded |
| `--dry-run` | Resolve and print the planned deployment; no download or writes |
| `--latest` | Deploy the newest deployable stable release |
| `--tag vX.Y.Z` | Deploy one explicit stable release; downgrade refused |
| `--abort-journal` | Discard a journal only after manual recovery; does not touch the binary |

Common options:

```text
--binary PATH          live binary (default /root/cliproxyapi/cli-proxy-api)
--app-dir PATH         application directory (default /root/cliproxyapi)
--config PATH          config.yaml (default <app-dir>/config.yaml)
--auth-dir PATH        OAuth/auth directory (default /root/.cli-proxy-api)
--unit NAME            systemd unit (default cliproxyapi.service)
--port PORT            local API port for /healthz (default 8317)
--health-url URL       override the health endpoint
--state-dir PATH       deployer state (default /root/cliproxyapi.deploy)
--repo OWNER/NAME      release repository (default SCNU-PIONEER/CLIProxyAPI)
--keep-backups N       binary backups to keep (default 5)
--retry                retry a release that previously failed
--allow-major          permit a major-version crossing (manual runs only)
--baseline-version TAG baseline version when the probe cannot run
--allow-non-root       offline preview/tests only; never use in the timer
```

Advanced and diagnostic options:

```text
--github-base URL      GitHub web base; must be exactly https://github.com
--arch ARCH            override detected architecture (amd64/aarch64)
--health-attempts N    health checks before failing (default 20)
--health-interval SEC  delay between health checks (default 2)
--http-timeout SEC     HTTP timeout (default 20)
--debug                print tracebacks for unexpected errors
--version              print the updater version and exit
```

Exit codes:

| Code | Meaning |
| --- | --- |
| 0 | Success or nothing to do |
| 1 | Deployment or network failure |
| 2 | Configuration or usage error |
| 3 | Refused by policy (downgrade, major without opt-in, retained failed tag without `--retry`) |
| 4 | Another run holds the deployment lock |
| 5 | Unsafe state requiring operator action (journal mismatch, binary drift, mutated release) |

`--allow-major` is accepted only with `--latest` or `--tag`. It is intentionally
absent from the timer's command line.

## Automatic updates and policy

- The timer runs `cpap-deploy --latest` about every five minutes (5 min plus
  up to 60 s jitter; see the installed unit for the exact schedule) with a
  lock, so overlapping runs are safe.
- Patch and minor releases deploy automatically after `--enable`.
- Major releases are skipped by the timer. Run them by hand:
  `sudo cpap-deploy --tag vX.Y.Z --allow-major`.
- The updater retains up to 20 recent failed tags and skips them until
  `sudo cpap-deploy --latest --retry` (or `--tag ... --retry`). Older failures
  are evicted and may be retried automatically; `--retry` retries immediately.
- A release missing its asset or the exact `checksums.txt` entry is ignored;
  the updater does not treat release existence as readiness.
- A tag whose published checksum changes after deployment causes a fail-closed
  integrity error on the next run.
- The timer never downgrades and never replaces a binary that the systemd unit
  does not actually start (`ExecStart` must resolve to the configured binary).

## Rollback and config recovery

### Automatic binary rollback

If restart, `/healthz`, or the MainPID/executable-digest check fails, the
updater restores the backup created immediately before the swap, restarts the
unit, and records the failure. Backups are kept next to the live binary as
`cli-proxy-api.bak.<tag>` (default: keep 5).

If rollback itself cannot make the old binary healthy, the journal is kept with
phase `rollback_failed` and the command exits 5. This state requires manual
recovery: subsequent runs, including `--retry`, refuse to proceed until the
service is fixed and the journal is cleared with `--abort-journal`.

### Config is not rolled back

The updater snapshots `config.yaml` before every deployment, but it never
restores it automatically. This is deliberate: the application may rewrite the
config at startup, and another operator or the management API may have changed
it concurrently. Blindly restoring an old snapshot could discard valid changes.

A v8 binary can normalize the config during startup, for example by replacing a
plaintext management secret with a bcrypt hash or by removing conflicting
legacy fields. This is application behavior, not an updater write. A successful
start may therefore leave `config.yaml` changed even though deployment
succeeded.

If the new binary fails and you need the pre-deployment config:

```bash
# Stop first: startup is when the application writes config.
sudo systemctl stop cliproxyapi

# Keep the current file for comparison/merge.
sudo cp -a /root/cliproxyapi/config.yaml /root/cliproxyapi/config.yaml.before-restore

# Review the difference before restoring.
sudo diff -u /root/cliproxyapi/config.yaml \
  /root/cliproxyapi.deploy/snapshots/config.<tag>.<stamp>.yaml

# Restore the snapshot with restrictive ownership and mode.
sudo install -o root -g root -m 0600 \
  /root/cliproxyapi.deploy/snapshots/config.<tag>.<stamp>.yaml \
  /root/cliproxyapi/config.yaml

sudo systemctl start cliproxyapi
```

Warnings:

- Any management/API or manual change made after the snapshot will be lost if
  you restore the whole file. Diff first and merge if there were concurrent
  changes.
- If `config.yaml` is a symlink, restore the symlink target, not the link.
- Preserve the original owner/group if the deployment does not run as root.
- After restoring, verify the service with the operator checks below; a healthy
  `/healthz` alone is not sufficient.

## Verification and limits

The updater considers a deployment successful when:

- the unit is active;
- `GET /healthz` returns HTTP 200 with JSON `{"status":"ok"}`;
- systemd's `MainPID` is stable across two samples;
- `/proc/<pid>/exe` resolves to the installed binary and its SHA-256 equals the
  staged binary's digest.

That proves process identity and a listening HTTP endpoint. It does **not**
prove:

- that API keys or the access manager are loaded and working;
- that OAuth/auth credential files are valid;
- that providers are registered, authenticated, or reachable;
- that plugins loaded;
- that the management API, dashboard assets, or the RESP usage queue used by
  other systems still work;
- that the config is semantically compatible with the previous version.

`/healthz` is a static handler. A 200 can coexist with a broken authenticated
data plane.

### Operator post-checks (first upgrade and any major change)

Run these by hand with existing credentials. The deployment script must not
read, store, or pass API keys.

1. Authenticated models call with an existing key, using the same header your
   clients use:

   ```bash
   read -r -s -p "Existing API key: " CPA_KEY; echo
   curl -fsS -H "Authorization: Bearer $CPA_KEY" \
     http://127.0.0.1:8317/v1/models >/dev/null && echo "models OK"
   unset CPA_KEY
   ```

2. One cheap request through an existing provider/client path.
3. The management v0 endpoints used by your portal and integrations.
4. The RESP usage queue consumers (the deployment host historically has more
   than 35 integration consumers; check the ones you operate).
5. Plugin load messages in the application log and `plugins.enabled` behavior.
6. Dashboard/management assets used by operators.

For the first upgrade from the current commercial build to this fork's release,
treat the deployment as a compatibility test even when the major version is
unchanged. Keep the binary backup and config snapshot, watch the startup log,
and be ready to restore both. If the service restart-loops after the swap, the
updater should roll the binary back automatically; config may still need the
manual restore above.

## Publishing a release

Maintainers publish with the normal GitHub workflow; the server needs no
publish or tag permission.

```bash
git tag v8.0.39
git push origin v8.0.39
```

The release workflow builds the Linux glibc assets (amd64 and aarch64) and
`checksums.txt`. Wait until the GitHub Release is published (not a draft, not a
pre-release) and `checksums.txt` lists the exact asset. Until then the release
is intentionally not deployable.

Notes:

- The workflow triggers on any tag, but only stable `vX.Y.Z` tags are eligible
  for deployment. Scratch tags create releases that the updater ignores.
- Release assets can be replaced by a workflow re-run (`--clobber`). The updater
  records the deployed digest and fails closed if the published checksum for
  that tag later changes.
- The repository may have zero published Releases at first. Publish one before
  enabling the timer; this is not a permanent limitation.

## Files and state

| Path | Purpose |
| --- | --- |
| `/root/cliproxyapi/cli-proxy-api` | Live binary; the only application file replaced |
| `/root/cliproxyapi/cli-proxy-api.bak.<tag>` | Rollback copies (default keep 5) |
| `/root/cliproxyapi/config.yaml` | Application config; updater never writes it |
| `/root/.cli-proxy-api/` | OAuth/auth material; never touched |
| `/root/cliproxyapi/logs/` | Application logs; never touched |
| `/root/cliproxyapi.deploy/deployed.json` | Current tag, asset/binary digests, snapshot path |
| `/root/cliproxyapi.deploy/last_failure.json` | Bounded map of the 20 most recent failed tags and reasons |
| `/root/cliproxyapi.deploy/journal.json` | In-flight transaction; used for recovery |
| `/root/cliproxyapi.deploy/deploy.log` | Updater log (rotated at 2 MiB) |
| `/root/cliproxyapi.deploy/snapshots/` | Config snapshots, mode 0600 |
| `/root/cliproxyapi.deploy/staging/` | Download/extract workspace |
| `cliproxyapi.service` | Existing application unit; installer does not modify it |
| `cpap-deploy.service`, `cpap-deploy.timer` | Separate updater units installed by `install.sh` |

## Troubleshooting

| Symptom | Action |
| --- | --- |
| `no published stable releases` | Publish a stable `vX.Y.Z` release; drafts/pre-releases are ignored |
| Release exists but `--dry-run` finds nothing | Wait for the exact asset and its `checksums.txt` entry; release existence alone is not readiness |
| GitHub API 403/429 | Unauthenticated rate limit; wait and retry. No local changes were made |
| Exit 3, major upgrade refused | Expected. Run manually with `--tag ... --allow-major` |
| Exit 3, failed release refused | Inspect the failure, then use `--retry` while the tag is still retained |
| Exit 4 | Another deployment run holds the lock; wait for it |
| Exit 5, journal present | Inspect `journal.json` and the service. If the phase is `failed` or `rollback_failed`, the updater refuses to continue and `--retry` does not bypass it; recover manually, then use `--abort-journal`. Other phases are recovered on the next run |
| Exit 5, live binary drift | The binary was replaced outside this tool. Reconcile it deliberately; a recorded baseline is not silently overwritten |
| `ExecStart` mismatch | Fix the unit manually or pass the correct `--binary`; the installer never edits `cliproxyapi.service` |
| Health check fails after restart | The updater restores the binary automatically. Check `deploy.log`, `journalctl -u cliproxyapi`, and restore the config snapshot manually if needed |
| Timer does not run | `systemctl status cpap-deploy.timer`; re-run `install.sh --enable` |
| Disk full during download | Free space in `/root/cliproxyapi.deploy`; the updater does not touch the live binary until verification completes |

## Security notes

- Transport trust is GitHub HTTPS plus control of the public repository. The
  release `checksums.txt` is unsigned; it is not a cryptographic signature.
- No GitHub token, SSH deploy key, or API key is installed on the server. The
  timer cannot publish releases or push tags.
- Archive members are validated before extraction, and only the regular
  `cli-proxy-api` file is written from the archive.
- Config snapshots contain the same secrets as `config.yaml`; they are mode
  0600 inside a root-only state directory. Protect and prune them accordingly.
- Keep the repository public; a private repository would require a credential
  on the server and is outside this design.
