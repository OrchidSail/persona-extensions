---
name: mika
description: 通过局域网或 Tailscale 将手机端任务委托给安卓手机上的 Mika 手机助手执行，支持健康检查、设备只读状态查询、派发任务、长轮询跟进进度、回复追问、运行中追加指示、取消任务及导出脱敏运行记录。当用户希望让另一台安卓手机上的 Mika 打开 App、处理手机端操作或查看手机任务状态时使用。
---

# Mika 手机助手（`mika_*` 工具）

Mika 运行在用户的安卓手机上，通过 `/api/v1` HTTP 接口接受远程任务。电脑/Persona 负责说明目标与跟进进度，具体手机操作由手机上的 Mika 自主规划和执行。

## 推荐工作流

1. **检查状态（按需）**：
   - 首次连接或排障时调用 `mika_doctor` 检查 Mika 版本、能力与手机助手状态（不消耗模型、不派发任务）。
   - 若不确定手机是否已解锁，可调用只读的 `mika_device_state`；若屏幕锁定或助手未连接，请提示用户在手机上解锁或打开 Mika（本扩展不提供远程解锁）。
2. **派发任务**：
   - 调用 `mika_run_task` 提交 `goal`；若要接着同一远程会话继续，传入前一次任务返回的 `conversation_id`（格式 `mcp-<uuid>`）。
   - 需要幂等恢复时，可先生成小写 UUID 作为 `request_id`；若网络中断导致回执不明，先用 `mika_task_status` 查询该 `request_id`，不要盲目重复派发。
3. **跟进进度**：
   - 派发后反复调用 `mika_wait_task`（每次最多等待 50 秒，默认采用扩展设置的等待秒数），等待状态变化或终态（`done` / `failed` / `cancelled`）。
   - `changed: false` 且任务仍在 `starting` / `running` / `queued` 时，不代表任务失败，直接再次调用 `mika_wait_task`，不要取消或重新提交。
4. **处理追问与手机接管（关键）**：
   - **`waiting_for_answer`（`answerable: true`）**：Mika 正在向用户追问。立即把 `question`（及 `options`）原样转告用户；收到用户回复后，调用 `mika_answer` 提交，再继续 `mika_wait_task`。
   - **`waiting_on_phone`**：付款确认、登录认证或 `hand_to_user` 接管只能在安卓手机上由用户本人完成，`mika_answer` 无法代答。立即通知用户去手机上处理，随后按用户意愿继续 `mika_wait_task` 或调用 `mika_cancel_task`。
   - **`paused`**：通常表示手机已锁屏，提醒用户在手机上解锁后任务会自动继续。
5. **运行中补充、取消与复盘**：
   - 任务处于 `running` 时可用 `mika_steer_task` 追加补充指示，并通过 `mika_task_status` 查看 `steers` 中该 `message_id` 是否变为 `joined`（`accepted` 仅表示已入队）。
   - 调用 `mika_cancel_task` 后，必须继续查 `mika_task_status` 直到状态变为 `cancelled` 才可报告取消完成。
   - 使用 `mika_list_tasks` 查看近期远程任务，使用 `mika_export_task` 导出脱敏运行记录（不含截图字节）。

## 安全与结果汇报原则

- 不要让用户在对话中发送访问令牌；令牌配置或更换一律指引用户前往 **设置 › 扩展 › Mika 手机助手**。
- 汇报完成结果时如实传达 `answer`、`verified` 与 `unconfirmed`；缺少 `verified` 或 `verified: false` 时不要编造「已核验成功」。
