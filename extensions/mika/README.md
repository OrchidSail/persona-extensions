# Mika Android phone agent

Connect Persona to one or multiple [Mika](https://github.com/wangzan101/Mika) instances running on Android phones (up to 3 named devices simultaneously, e.g. `A16` + `S25+`) over LAN, Tailscale, loopback (`adb forward`) or a trusted HTTPS gateway: list configured devices, check server and phone helper health, inspect read-only device state and recent tasks, export redacted run records, and dispatch, answer, steer or cancel remote phone tasks with explicit confirmation.

This extension speaks Mika's documented `/api/v1` HTTP/JSON task API (`RemoteTaskApi`). It does not include remote wake or unlock tools; payments, logins and takeover confirmations always stay on the phone.

## Set up

1. On each Android phone, open Mika **Settings › Connect a computer** (**设置 › 连接电脑**), enable **Wi-Fi / VPN** (or USB loopback forwarding), and note the displayed address (for example `http://<lan-or-tailscale-ip>:8765`) and access token.
2. In Persona **Settings › Extensions › Mika**, configure the primary device (fully backward compatible with v0.1.0 single-device settings):
   - **Primary device name (`instance_name`)**: optional custom name (default `default`, e.g. `A16`).
   - **Primary instance URL (`instance_url`, required)**: server root or `/api/v1` (do not enter `/mcp`). HTTPS is required for public hosts; plaintext HTTP is allowed only for loopback (`127.0.0.1`, `localhost`, `::1`, `host.persona.internal`) and private LAN / Tailscale targets (`10/8`, `172.16/12`, `192.168/16`, `100.64/10`, `169.254/16`, `fc00::/7`, `fe80::/10`, `.local`, `.ts.net`). Embedded URL credentials, query strings and fragments are rejected.
   - **Primary access token (`api_token`, required secret)**: stored encrypted on the hub and sent only as `Authorization: Bearer <token>` to the primary device.
   - **Default wait seconds (`default_wait_seconds`)**: integer `1`–`50` (default `30`).
3. Optionally configure a 2nd and/or 3rd Android phone:
   - **Slot 2 (`secondary`)**: `secondary_name` (optional, default `secondary`, e.g. `S25+`), `secondary_url` (same URL security rules), and encrypted secret `secondary_api_token` (`required: false`).
   - **Slot 3 (`tertiary`)**: `tertiary_name` (optional, default `tertiary`), `tertiary_url` (same URL security rules), and encrypted secret `tertiary_api_token` (`required: false`).

### Isolation and security guarantees

- Each device's Bearer token is stored in its own encrypted hub secret (`api_token`, `secondary_api_token`, `tertiary_api_token`), never in plaintext `configuration`, and is sent **only** to that slot's normalized URL.
- Redirects and environment HTTP proxies are disabled. All configured tokens across all slots are defensively scrubbed (`[redacted]`) from every error message, status summary, UI view and receipt file.
- Single-slot failure isolation: if one optional slot is incomplete (for example URL set without token, token set without URL, or invalid URL) or offline, the extension still starts and serves all other validly configured phones normally while reporting clear redacted status for the affected slot.

## Network and sandbox

Because the phones' addresses are user-configured, `permissions.network` is left empty (`[]`), following the same convention as self-hosted extensions such as `anytype`, `gitea` and `homeassistant`.

When installing from a local package archive (`personad extension install file:/path/to/mika-0.2.0.tar.gz`), the hub starts the extension inside its sandbox, whose egress proxy blocks private LAN / Tailscale HTTP addresses. Disable the sandbox for `mika` on the hub machine before connecting to phones over LAN or Tailscale:

```sh
personad extension sandbox mika off
```

## Multi-device targeting (`device` parameter)

All existing read tools and write actions accept an optional `device` parameter:

- Omit `device` to target the primary device (slot 1, `primary`).
- Match by custom device name (case-insensitive, e.g. `A16`, `s25+`) or by slot alias (`primary`, `secondary`, `tertiary`, `1`, `2`, `3`).
- Unknown names or duplicate-name collisions return a clear error listing available devices and slot aliases so the caller can disambiguate safely.
- Confirmation cards (`actions.describe`) always display the resolved target device name, slot and masked URL at the top of the card summary to prevent sending a task to the wrong phone.

## Tools and actions

Read-only tools (`tools.call`):

- `mika_list_devices` — lists all configured device slots with custom name, slot alias, masked URL, configuration status (`ready`, `missing_token`, `missing_url`, `invalid_url`, `not_configured`) and last observed health.
- `mika_doctor` — `GET /api/v1` (and `GET /api/v1/device` when advertised): reports server version, API version, capabilities, limits, masked URL and phone helper readiness (`model_configuration` is reported as unverified). When `device` is omitted and multiple devices are configured, probes and summarizes all configured devices; pass `device` to check one specific phone.
- `mika_device_state` — `GET /api/v1/device` (optional `device`): read-only screen, lock, helper and remote-unlock toggle state (no wake/unlock, no credentials).
- `mika_task_status` — `GET /api/v1/tasks/{task_id}` (optional `device`): task status, progress, open question, takeover note, steers, files and result.
- `mika_wait_task` — `POST /api/v1/tasks/{task_id}/wait` (optional `device`): waits `1`–`50` seconds for a status change or terminal state (HTTP timeout `timeout_s + 15s`).
- `mika_list_tasks` — `GET /api/v1/tasks?limit=N[&before=CURSOR]` (optional `device`): lists recent remote tasks (`limit` 1–50, default 10).
- `mika_export_task` — `GET /api/v1/tasks/{task_id}/export` (optional `device`): exports the redacted run summary and events without screenshot bytes.

Confirmed write actions (`actions.do`, all in `confirm_always`, protected by per-`(device_slot, request_id)` replay receipts in `write-receipts.json`):

- `mika_run_task` — `POST /api/v1/tasks` (`goal`, optional `conversation_id` mapped to Mika's `conversation` field, optional lowercase UUID `request_id`, optional `device`).
- `mika_answer` — `POST /api/v1/tasks/{task_id}/answer` (`task_id`, `text`, optional `device`).
- `mika_steer_task` — `POST /api/v1/tasks/{task_id}/steer` (`task_id`, `text`, optional `device`; returns HTTP 202 with `message_id`).
- `mika_cancel_task` — `POST /api/v1/tasks/{task_id}/cancel` (`task_id`, optional `device`; sends `{}` JSON body; returns 202 when accepted or 200 if already finished).

Before executing a write, the extension records its canonical `(device_slot, action, args)` and `(device_slot, request_id)` key in `write-receipts.json` (`0600`, without any tokens). Replaying the same `request_id` on the same device slot (even via a different alias such as `A16` vs `primary` vs omitted) returns the stored result; reusing it with different parameters on the same slot is refused; the same `request_id` on a different device slot is isolated; an interrupted write with an uncertain transport/503 outcome stays pending so retries stop and prompt inspection first.

## UI view (`nav_page`)

- With 1 configured phone, the Mika page keeps a concise overview (connection status + recent tasks + task detail).
- With 2–3 configured phones, the page displays live connection/configuration status for every phone, provides in-page device switching (segmented control on UI v2+ and tappable device rows on all UI versions), and scopes refresh, pagination and task detail views to the selected phone.

## Verify

```sh
go -C extensions/mika vet ./...
go -C extensions/mika test -count=1 ./...
python3 -I scripts/manifest.py check
python3 -I scripts/check-layout.py
scripts/package.sh mika
```

All tests use synthetic `httptest.NewServer` fixtures and cover single-device backward compatibility, multi-device configuration & routing, name/slot alias resolution, unknown/duplicate device handling, single-slot failure isolation, per-device Bearer token isolation, all-token scrubbing, confirmation card target display, `(device_slot, request_id)` write receipt isolation, HTTP error mapping, URL security rules, redirect rejection, and single/multi-device localized UI rendering.

## Upstream reference

- [Mika repository](https://github.com/wangzan101/Mika) (Apache-2.0)
- [Mika CLI and HTTP `/api/v1` contract](https://github.com/wangzan101/Mika/blob/main/docs/cli.md)
- [Mika computer connection and security boundary](https://github.com/wangzan101/Mika/blob/main/docs/mcp.md)
