---
name: mika
description: 通过局域网或 Tailscale 同时连接并管理任意多台安卓手机上的 Mika 手机助手（在 Mika 导航页动态增删改查、不限 3 台，地址与令牌整体加密存在 Hub），提供设备列表、健康检查、只读设备状态、派发任务、长轮询跟进进度、回复追问、运行中追加指示、取消任务及导出脱敏运行记录。当用户希望让某台安卓手机（如 A16、S25+ 等）上的 Mika 打开 App、处理手机端操作或查看手机任务状态时使用。
---

# Mika 手机助手（`mika_*` 工具，支持动态多设备）

Mika 运行在用户的安卓手机上，通过 `/api/v1` HTTP 接口接受远程任务。电脑/Persona 负责说明目标与跟进进度，具体手机操作由手机上的 Mika 自主规划和执行。本扩展支持在 **Mika 导航页** 动态添加、编辑、删除、测试连接和切换默认设备（不限 3 台），所有设备地址与独立访问令牌均通过 Hub `connector.secrets` 整体加密保存。

## 多设备管理与路由规则

- **设备管理入口**：添加新手机、改名/改地址、更换访问令牌、设为默认、测试连接或删除设备，一律引导用户前往 Persona 内的 **Mika 导航页** 操作（扩展设置页仅保留 `default_wait_seconds` 与 `require_confirmation` 全局开关）。
- **免确认开关（`require_confirmation`）**：
  - 默认开启（`true`）：`mika_run_task`、`mika_answer`、`mika_steer_task`、`mika_cancel_task` 需在 Persona 弹确认卡审批后下发。
  - 关闭（`false`）后：上述 4 个写工具动态切入免确认直接下发，但**手机端登录、付款与 `hand_to_user` 接管确认仍由 Mika 在手机上强制保留**。
- **查看已配置设备**：先调用只读的 `mika_list_devices` 查看全部动态设备编号、名称、默认标记、脱敏地址与健康状态；或直接调用不带 `device` 参数的 `mika_doctor` 汇总检查全部已配置手机的在线与助手就绪状态。
- **`device` 参数（全部任务/状态工具均支持）**：
  - 省略 `device` 或传入 `default` / `primary` / `1`：自动路由到**当前默认设备**（`#1`）。
  - 传入自定义设备名（忽略大小写，如 `A16`、`S25+`）或 1-based 编号（`1`、`2`、`3`、`4`…，并向后兼容 `secondary` / `tertiary`）指定目标手机。
  - 若已配置多台手机且用户未指明哪一台，先结合上下文判断或调用 `mika_list_devices` 核对后向用户确认；开启确认卡时，卡片会自动展示目标设备名称、编号与脱敏地址。
- **单设备故障隔离**：某台手机离线或令牌失效时，不影响其他已配置手机的正常调用。

## 推荐工作流

1. **检查设备与状态（按需）**：
   - 调用 `mika_list_devices` 查看已配置的手机列表与默认设备。
   - 首次连接或排障时调用 `mika_doctor`（不传 `device` 汇总检查全部手机；传入 `device` 检查单台手机），核查 Mika 版本、能力与手机助手状态（不消耗模型、不派发任务）。
   - 若不确定目标手机是否已解锁，可调用只读的 `mika_device_state`（可传 `device`）；若屏幕锁定或助手未连接，请提示用户在对应手机上解锁或打开 Mika（本扩展不提供远程解锁）。
2. **派发任务**：
   - 调用 `mika_run_task` 提交 `goal`（多手机时务必传入正确的 `device`）；若要接着同一台手机的同一远程会话继续，传入前一次任务返回的 `conversation_id`（格式 `mcp-<uuid>`）。
   - 需要幂等恢复时，可先生成小写 UUID 作为 `request_id`；若网络中断导致回执不明，先在对应 `device` 上用 `mika_task_status` 查询该 `request_id`，不要盲目重复派发。
3. **长轮询跟进进度（推荐 `timeout_s: 25`）**：
   - 派发后在**同一 `device`** 上反复调用 `mika_wait_task`，**每次推荐传 `timeout_s: 25`**（低于 Hub 30 秒工具调用超时阈值，避免临界超时），持续等待状态变化或终态（`done` / `failed` / `cancelled`）。
   - `changed: false` 且任务仍在 `starting` / `running` / `queued` 时，不代表任务失败，直接再次调用 `mika_wait_task(timeout_s: 25)`，不要取消或重新提交。
4. **处理追问与手机接管（关键）**：
   - **`waiting_for_answer`（`answerable: true`）**：Mika 正在向用户追问。立即把 `question`（及 `options`）和对应手机名原样转告用户；收到用户回复后，在对应 `device` 上调用 `mika_answer` 提交，再继续 `mika_wait_task(timeout_s: 25)`。
   - **`waiting_on_phone`（后台守候规则）**：付款确认、登录认证、人脸/指纹验证或 `hand_to_user` 接管只能在对应安卓手机上由用户本人完成，`mika_answer` 无法代答。此时必须**双轨处理**：
     1. **主对话**：立即告知用户前往指定手机完成操作（说明当前 App 与 `note` 提示），无需用户做完后再手动发消息催促；
     2. **同步在后台启动轻量守候子代理**：在后台循环调用 `mika_wait_task`（每次 `timeout_s: 25`，指向同一 `device` 与 `task_id`），当用户在手机上点完并交回控制权（任务离开 `waiting_on_phone` 或进入终态 / 新的追问）时，子代理立即回报结果并**自动唤醒主对话续跑**（若任务取消或超时等待过久则如实回报）。
   - **`paused`**：通常表示该手机已锁屏，提醒用户在对应手机上解锁后任务会自动继续。
5. **运行中补充、取消与复盘**：
   - 任务处于 `running` 时可用 `mika_steer_task`（带对应 `device`）追加补充指示，并通过 `mika_task_status` 查看 `steers` 中该 `message_id` 是否变为 `joined`（`accepted` 仅表示已入队）。
   - 调用 `mika_cancel_task`（带对应 `device`）后，必须继续查 `mika_task_status` 直到状态变为 `cancelled` 才可报告取消完成。
   - 使用 `mika_list_tasks` 查看某台手机的近期远程任务，使用 `mika_export_task` 导出脱敏运行记录（不含截图字节）。

## 安全与结果汇报原则

- 不要让用户在对话中发送任何设备的访问令牌；添加设备或更换令牌一律指引用户前往 **Mika 导航页**（令牌框为 `secret` 类型、绝不回显，编辑时留空保留原令牌）。
- 汇报完成结果时标明目标设备名，并如实传达 `answer`、`verified` 与 `unconfirmed`；缺少 `verified` 或 `verified: false` 时不要编造「已核验成功」。
