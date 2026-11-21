---
name: mika
description: 通过局域网或 Tailscale 同时连接并管理多台安卓手机上的 Mika 手机助手（支持最多 3 台命名设备独立令牌隔离），提供设备列表、健康检查、只读设备状态、派发任务、长轮询跟进进度、回复追问、运行中追加指示、取消任务及导出脱敏运行记录。当用户希望让某台安卓手机（如 A16、S25+）上的 Mika 打开 App、处理手机端操作或查看手机任务状态时使用。
---

# Mika 手机助手（`mika_*` 工具，支持多设备）

Mika 运行在用户的安卓手机上，通过 `/api/v1` HTTP 接口接受远程任务。电脑/Persona 负责说明目标与跟进进度，具体手机操作由手机上的 Mika 自主规划和执行。本扩展支持同时配置最多 3 台安卓手机（`primary` / `secondary` / `tertiary`，每台拥有独立加密令牌）。

## 多设备路由规则

- **查看已配置设备**：先调用只读的 `mika_list_devices` 查看所有已配置设备的槽位、名称、脱敏地址与配置状态；或直接调用不带 `device` 参数的 `mika_doctor` 汇总检查全部已配置手机的在线与助手就绪状态。
- **`device` 参数（全部任务/状态工具均支持）**：
  - 省略 `device` 时默认走主设备（slot 1 / `primary`）。
  - 可按设备自定义名称（忽略大小写，如 `A16`、`S25+`）或槽位别名（`primary`/`secondary`/`tertiary` 或 `1`/`2`/`3`）指定目标手机。
  - 若多台手机已配置且用户未明确指出哪一台，先依据上下文判断或调用 `mika_list_devices` 核对后向用户确认；确认卡片会自动显示目标设备名称、槽位和脱敏地址，避免派错手机。
  - 若遇到重名冲突或未知设备名，按报错中列出的可用设备与槽位别名指定目标（如改传 `primary` / `secondary` / `tertiary`）。
- **单设备故障隔离**：某台手机缺令牌、配置不完整或离线时，不影响其他已配好手机的正常调用。

## 推荐工作流

1. **检查设备与状态（按需）**：
   - 调用 `mika_list_devices` 查看已配置的手机列表与配置状态。
   - 首次连接或排障时调用 `mika_doctor`（不传 `device` 汇总检查全部已配置手机；传入 `device` 检查单台手机），检查 Mika 版本、能力与手机助手状态（不消耗模型、不派发任务）。
   - 若不确定目标手机是否已解锁，可调用只读的 `mika_device_state`（可传 `device`）；若屏幕锁定或助手未连接，请提示用户在对应手机上解锁或打开 Mika（本扩展不提供远程解锁）。
2. **派发任务**：
   - 调用 `mika_run_task` 提交 `goal`（多手机时务必传入正确的 `device`）；若要接着同一台手机的同一远程会话继续，传入前一次任务返回的 `conversation_id`（格式 `mcp-<uuid>`）。
   - 需要幂等恢复时，可先生成小写 UUID 作为 `request_id`；若网络中断导致回执不明，先在对应 `device` 上用 `mika_task_status` 查询该 `request_id`，不要盲目重复派发。
3. **跟进进度**：
   - 派发后在**同一 `device`** 上反复调用 `mika_wait_task`（每次最多等待 50 秒，默认采用扩展设置的等待秒数），等待状态变化或终态（`done` / `failed` / `cancelled`）。
   - `changed: false` 且任务仍在 `starting` / `running` / `queued` 时，不代表任务失败，直接再次调用 `mika_wait_task`，不要取消或重新提交。
4. **处理追问与手机接管（关键）**：
   - **`waiting_for_answer`（`answerable: true`）**：Mika 正在向用户追问。立即把 `question`（及 `options`）和对应手机名原样转告用户；收到用户回复后，在对应 `device` 上调用 `mika_answer` 提交，再继续 `mika_wait_task`。
   - **`waiting_on_phone`**：付款确认、登录认证或 `hand_to_user` 接管只能在对应安卓手机上由用户本人完成，`mika_answer` 无法代答。立即通知用户去指定手机上处理，随后按用户意愿继续 `mika_wait_task` 或调用 `mika_cancel_task`。
   - **`paused`**：通常表示该手机已锁屏，提醒用户在对应手机上解锁后任务会自动继续。
5. **运行中补充、取消与复盘**：
   - 任务处于 `running` 时可用 `mika_steer_task`（带对应 `device`）追加补充指示，并通过 `mika_task_status` 查看 `steers` 中该 `message_id` 是否变为 `joined`（`accepted` 仅表示已入队）。
   - 调用 `mika_cancel_task`（带对应 `device`）后，必须继续查 `mika_task_status` 直到状态变为 `cancelled` 才可报告取消完成。
   - 使用 `mika_list_tasks` 查看某台手机的近期远程任务，使用 `mika_export_task` 导出脱敏运行记录（不含截图字节）。

## 安全与结果汇报原则

- 不要让用户在对话中发送任何设备的访问令牌；令牌配置或更换一律指引用户前往 **设置 › 扩展 › Mika 手机助手**（每台手机使用各自的独立加密 Secret 槽位）。
- 汇报完成结果时标明目标设备名，并如实传达 `answer`、`verified` 与 `unconfirmed`；缺少 `verified` 或 `verified: false` 时不要编造「已核验成功」。
