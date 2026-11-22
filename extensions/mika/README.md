# Mika Android phone agent

Connect Persona to one or many [Mika](https://github.com/wangzan101/Mika) instances running on Android phones (no 3-device cap; manage devices dynamically from the Mika page) over LAN, Tailscale, loopback (`adb forward`) or a trusted HTTPS gateway: manage encrypted device credentials in the hub, list devices, check server and phone helper health, inspect read-only device state and recent tasks, export redacted run records, and dispatch, answer, steer or cancel remote phone tasks.

This extension speaks Mika's documented `/api/v1` HTTP/JSON task API (`RemoteTaskApi`). It does not include remote wake or unlock tools; payments, logins and takeover confirmations always stay on the phone.

## Set up

1. On each Android phone, open Mika **Settings › Connect a computer** (**设置 › 连接电脑**), enable **Wi-Fi / VPN** (or USB loopback forwarding), and note the displayed address (for example `http://<lan-or-tailscale-ip>:8765`) and access token.
2. Open the **Mika** navigation page in Persona and click **+ Add device** (**+ 添加设备**):
   - **Device name**: required unique name (case-insensitive, up to 40 characters, e.g. `A16`, `S25+`, `Pixel 9`).
   - **Mika instance URL**: server root or `/api/v1` (do not enter `/mcp`). HTTPS is required for public hosts; plaintext HTTP is allowed only for loopback (`127.0.0.1`, `localhost`, `::1`, `host.persona.internal`) and private LAN / Tailscale targets (`10/8`, `172.16/12`, `192.168/16`, `100.64/10`, `169.254/16`, `fc00::/7`, `fe80::/10`, `.local`, `.ts.net`). Embedded URL credentials, query strings and fragments are rejected.
   - **Access token (`secret`)**: never pre-filled or echoed; stored together with the dynamic device list inside the hub's encrypted connector secret store (`Hub.SaveSecret` / `connector.secrets.save`).
   - **Set as default**: chooses which phone handles tool calls when `device` is omitted (or `default` / `primary` / `1`).
3. In Persona **Settings › Extensions › Mika**, configure global behavior:
   - **Default wait seconds (`default_wait_seconds`)**: integer `1`–`50` (default `30`; calling `mika_wait_task` with `timeout_s: 25` is recommended to stay below the hub's 30s tool timeout).
   - **Require confirmation (`require_confirmation`)**: boolean (default `true`). When enabled, write tools (`mika_run_task`, `mika_answer`, `mika_steer_task`, `mika_cancel_task`) are exposed under `actions.list` and require explicit Persona confirmation cards. When disabled (`false`), those 4 tools move dynamically to `tools.list` for direct dispatch without Persona confirmation cards — while Mika on the Android phone still enforces on-device confirmation for logins, payments and `hand_to_user` handovers.

### Zero-loss migration from 0.1.0 / 0.2.x

Persona Hub validates saved `plugins.config` keys strictly against `manifest.configuration` (`Schema.CheckSaved`), filters `InitializeParams.Secrets` by `manifest.secrets`, and verifies `InitializeResult.Version == manifest.Version`. Because `0.3.0` removes the legacy 3-slot `configuration` keys and `manifest.secrets` to keep Settings streamlined (only `default_wait_seconds` and `require_confirmation`):

- `Init` includes an automatic one-shot migration (with a 10s RPC timeout on `Hub.LoadSecret` / `Hub.SaveSecret`): when the dynamic vault is empty and `migrated_legacy` is not yet set, it converts any valid legacy 3-slot configuration and secrets (such as existing `A16` and `S25+`) into the dynamic device list and seals them into the hub via `Hub.SaveSecret(ctx, "mika", ...)` with `migrated_legacy: true` — **without requiring users to re-enter any tokens**.
- For in-place upgrades from `0.2.x` on Persona Hub, install the companion transitional migration bundle `mika-0.2.9-migrate.tar.gz` first (same source code with version `0.2.9` and optional legacy 3-slot config/secrets): `0.2.x -> 0.2.9` automatically migrates existing phones into encrypted `connector_secrets` on startup -> clear the obsolete 3-slot config fields -> upgrade `0.2.9 -> 0.3.0`.
- Deleting all migrated devices writes an empty vault with `migrated_legacy: true` so restarting the extension never resurrects deleted legacy devices.

### Isolation and security guarantees

- Dynamic device entries (`id`, `name`, `url`, `token`, `default`) are stored exclusively inside the hub's encrypted connector secret (`Hub.SaveSecret(ctx, "mika", ...)`), never written to plaintext files or logs.
- Each phone's Bearer token is sent **only** to that device's normalized URL. Redirects and environment HTTP proxies are disabled.
- All active, submitted, rotated/retired and legacy tokens are defensively scrubbed (`[redacted]`) from every error message, toast, status summary, UI view and receipt file.

## Network and sandbox

Because the phones' addresses are user-configured, `permissions.network` is left empty (`[]`), following the same convention as self-hosted extensions such as `anytype`, `gitea` and `homeassistant`.

When installing from a local package archive (`personad extension install file:/path/to/mika-0.3.0.tar.gz`), the hub starts the extension inside its sandbox, whose egress proxy blocks private LAN / Tailscale HTTP addresses. Disable the sandbox for `mika` on the hub machine before connecting to phones over LAN or Tailscale:

```sh
personad extension sandbox mika off
```

## Multi-device targeting (`device` parameter)

All read and write tools accept an optional `device` parameter:

- Omit `device` or pass `default`, `primary` or `1` to target the current default device (`#1`).
- Match by custom device name (case-insensitive, e.g. `A16`, `s25+`), by 1-based index (`1`..`N`), or by backward-compatible aliases (`secondary` -> `#2`, `tertiary` -> `#3`).
- Unknown names return a clear error listing available devices so the caller can target safely.
- When `require_confirmation` is enabled, confirmation cards (`actions.describe`) always display the resolved target device name, default/index tag and masked URL at the top of the card summary.

## Tools and actions

Read-only tools (always in `tools.list`):

- `mika_list_devices` — lists all configured dynamic devices with 1-based index, name, default indicator, masked URL, configuration status and last observed health.
- `mika_doctor` — `GET /api/v1` (and `GET /api/v1/device` when advertised): reports server version, API version, capabilities, limits, masked URL and phone helper readiness (`model_configuration` is reported as unverified). When `device` is omitted and multiple devices are configured, probes and summarizes all configured devices; pass `device` to check one specific phone.
- `mika_device_state` — `GET /api/v1/device` (optional `device`): read-only screen, lock, helper and remote-unlock toggle state (no wake/unlock, no credentials).
- `mika_task_status` — `GET /api/v1/tasks/{task_id}` (optional `device`): task status, progress, open question, takeover note, steers, files and result.
- `mika_wait_task` — `POST /api/v1/tasks/{task_id}/wait` (optional `device`): waits `1`–`50` seconds for a status change or terminal state (HTTP timeout `timeout_s + 15s`; `timeout_s: 25` recommended).
- `mika_list_tasks` — `GET /api/v1/tasks?limit=N[&before=CURSOR]` (optional `device`): lists recent remote tasks (`limit` 1–50, default 10).
- `mika_export_task` — `GET /api/v1/tasks/{task_id}/export` (optional `device`): exports the redacted run summary and events without screenshot bytes.

Write tools (in `actions.list` with confirmation cards when `require_confirmation: true`, or dynamically in `tools.list` for direct dispatch when `require_confirmation: false`):

- `mika_run_task` — `POST /api/v1/tasks` (`goal`, optional `conversation_id` mapped to Mika's `conversation` field, optional lowercase UUID `request_id`, optional `device`).
- `mika_answer` — `POST /api/v1/tasks/{task_id}/answer` (`task_id`, `text`, optional `device`).
- `mika_steer_task` — `POST /api/v1/tasks/{task_id}/steer` (`task_id`, `text`, optional `device`; returns HTTP 202 with `message_id`).
- `mika_cancel_task` — `POST /api/v1/tasks/{task_id}/cancel` (`task_id`, optional `device`; sends `{}` JSON body; returns 202 when accepted or 200 if already finished).

Confirmed write actions (`actions.do`) remain protected by per-`(device_id, request_id)` replay receipts in `write-receipts.json` (`0600`, without any tokens).

## UI view (`nav_page`)

- **Empty state**: prompts to add the first Android phone with a **+ Add device** pushed form subpage.
- **Dynamic device management**: lists all configured devices with live status badges, unique per-row switching (`device:<id>`) and task detail (`detail:<id>:<task_id>`) actions, quick segmented switcher for 2–5 devices on UI v2+, and full CRUD on every device (**+ Add device**, **Edit** with blank-keeps-token / non-blank-replaces-token secret field, **Set as default**, **Test connection (Doctor)**, and **Delete** with native `confirm` prompt).

## Verify

```sh
go -C extensions/mika vet ./...
go -C extensions/mika test -count=1 ./...
python3 -I scripts/manifest.py check
python3 -I scripts/check-layout.py
python3 -I scripts/check-docs.py
scripts/package.sh mika
```

All tests use synthetic `httptest.NewServer` fixtures and cover dynamic CRUD via `Hub.SaveSecret`/`LoadSecret`, >3-device support, leave-blank-keeps-token vs replace-token edits, name conflict and URL validation, zero-loss migration from 0.1.0/0.2.x 3-slot config/secrets, `require_confirmation` toggle and zero-value safety, routing compatibility, per-device Bearer token isolation, all-token scrubbing, write receipt isolation, HTTP error mapping, redirect rejection, and localized UI conformance across UI versions.

## Upstream reference

- [Mika repository](https://github.com/wangzan101/Mika) (Apache-2.0)
- [Mika CLI and HTTP `/api/v1` contract](https://github.com/wangzan101/Mika/blob/main/docs/cli.md)
- [Mika computer connection and security boundary](https://github.com/wangzan101/Mika/blob/main/docs/mcp.md)
