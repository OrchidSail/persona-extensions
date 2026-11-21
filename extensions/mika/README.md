# Mika Android phone agent

Connect Persona to [Mika](https://github.com/wangzan101/Mika) running on an Android phone over LAN, Tailscale, loopback (`adb forward`) or a trusted HTTPS gateway: check server and phone helper health, inspect read-only device state and recent tasks, export redacted run records, and dispatch, answer, steer or cancel remote phone tasks with explicit confirmation.

This extension speaks Mika's documented `/api/v1` HTTP/JSON task API (`RemoteTaskApi`). It does not include remote wake or unlock tools; payments, logins and takeover confirmations always stay on the phone.

## Set up

1. On the Android phone, open Mika **Settings › Connect a computer** (**设置 › 连接电脑**), enable **Wi-Fi / VPN** (or USB loopback forwarding), and note the displayed address (for example `http://<lan-or-tailscale-ip>:8765`) and access token.
2. In Persona **Settings › Extensions › Mika**, enter:
   - **Instance URL (`instance_url`)**: server root or `/api/v1` (do not enter `/mcp`). HTTPS is required for public hosts; plaintext HTTP is allowed only for loopback (`127.0.0.1`, `localhost`, `::1`, `host.persona.internal`) and private LAN / Tailscale targets (`10/8`, `172.16/12`, `192.168/16`, `100.64/10`, `169.254/16`, `fc00::/7`, `fe80::/10`, `.local`, `.ts.net`). Embedded URL credentials, query strings and fragments are rejected.
   - **Default wait seconds (`default_wait_seconds`)**: integer `1`–`50` (default `30`).
   - **Access token (`api_token`)**: stored encrypted on the hub and sent only as `Authorization: Bearer <token>` to the configured instance. Redirects and environment HTTP proxies are disabled, and tokens are never logged or returned in errors.

## Network and sandbox

Because the phone's address is user-configured, `permissions.network` is left empty (`[]`), following the same convention as self-hosted extensions such as `anytype`, `gitea` and `homeassistant`.

When installing from a local package archive (`personad extension install file:/path/to/mika-0.1.0.tar.gz`), the hub starts the extension inside its sandbox, whose egress proxy blocks private LAN / Tailscale HTTP addresses. Disable the sandbox for `mika` on the hub machine before connecting to a phone over LAN or Tailscale:

```sh
personad extension sandbox mika off
```

## Tools and actions

Read-only tools (`tools.call`):

- `mika_doctor` — `GET /api/v1` (and `GET /api/v1/device` when advertised): reports server version, API version, capabilities, limits, masked URL and phone helper readiness (`model_configuration` is reported as unverified).
- `mika_device_state` — `GET /api/v1/device`: read-only screen, lock, helper and remote-unlock toggle state (no wake/unlock, no credentials).
- `mika_task_status` — `GET /api/v1/tasks/{task_id}`: task status, progress, open question, takeover note, steers, files and result.
- `mika_wait_task` — `POST /api/v1/tasks/{task_id}/wait`: waits `1`–`50` seconds for a status change or terminal state (HTTP timeout `timeout_s + 15s`).
- `mika_list_tasks` — `GET /api/v1/tasks?limit=N[&before=CURSOR]`: lists recent remote tasks (`limit` 1–50, default 10).
- `mika_export_task` — `GET /api/v1/tasks/{task_id}/export`: exports the redacted run summary and events without screenshot bytes.

Confirmed write actions (`actions.do`, all in `confirm_always`, protected by `write-receipts.json` replay receipts):

- `mika_run_task` — `POST /api/v1/tasks` (`goal`, optional `conversation_id` mapped to Mika's `conversation` field, optional lowercase UUID `request_id`).
- `mika_answer` — `POST /api/v1/tasks/{task_id}/answer` (`task_id`, `text`).
- `mika_steer_task` — `POST /api/v1/tasks/{task_id}/steer` (`task_id`, `text`; returns HTTP 202 with `message_id`).
- `mika_cancel_task` — `POST /api/v1/tasks/{task_id}/cancel` (`task_id`; sends `{}` JSON body; returns 202 when accepted or 200 if already finished).

Before executing a write, the extension records its normalized action and hub `request_id` in `write-receipts.json` (`0600`, without `api_token`). Replaying the same `request_id` after completion returns the stored result; reusing it with different parameters is refused; an interrupted write with an uncertain transport/503 outcome stays pending so retries stop and prompt inspection first.

## Verify

```sh
go -C extensions/mika vet ./...
go -C extensions/mika test -count=1 ./...
python3 -I scripts/manifest.py check
scripts/package.sh mika
```

All tests use synthetic `httptest.NewServer` fixtures and cover every tool/action, Bearer authentication, HTTP 400/401/404/409/410/429/503 error mapping, URL security rules, redirect rejection, token non-leakage, write replay protection, and localized UI rendering.

## Upstream reference

- [Mika repository](https://github.com/wangzan101/Mika) (Apache-2.0)
- [Mika CLI and HTTP `/api/v1` contract](https://github.com/wangzan101/Mika/blob/main/docs/cli.md)
- [Mika computer connection and security boundary](https://github.com/wangzan101/Mika/blob/main/docs/mcp.md)
