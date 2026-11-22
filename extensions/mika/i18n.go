package main

import "strings"

type texts struct {
	title, connection, recentTasks, taskDetail               string
	devices, configError, viewing, defaultTag, devicePrefix  string
	online, offline, serverRow, deviceRow, notConfigured     string
	ready, locked, helperOffline, deviceUnavailable          string
	refresh, back, more, noTasks, noGoal                     string
	statusStarting, statusRunning, statusQueued              string
	statusPaused, statusWaitAnswer, statusWaitPhone          string
	statusDone, statusFailed, statusCancelled                string
	goalLabel, progressLabel, questionLabel, answerLabel     string
	reasonLabel, noteLabel, steersLabel, filesLabel          string
	capabilitiesPrefix, taskMetaFmt, verifiedYes, verifiedNo string

	// Dynamic device management UI strings.
	noDevicesTitle, noDevicesDetail             string
	addDeviceBtn, addDeviceTitle                string
	editDeviceBtn, editDeviceTitle              string
	testConnBtn, setDefaultBtn, deleteDeviceBtn string
	rowTest, rowEdit, rowSetDefault, rowDelete  string
	saveAddBtn, saveEditBtn                     string
	fieldNameLabel, fieldNamePlaceholder        string
	fieldURLLabel, fieldURLPlaceholder          string
	fieldURLDetail                              string
	fieldTokenLabel                             string
	fieldTokenAddPlaceholder                    string
	fieldTokenEditPlaceholder                   string
	fieldTokenEditDetail                        string
	fieldDefaultLabel                           string
	formSecurityFooter, editTokenFooter         string
	deleteConfirmFmt                            string
	deviceAddedFmt, deviceSavedFmt              string
	deviceDefaultFmt, deviceDeletedFmt          string
	testOKFmt, testFailFmt                      string
	deviceNotFound                              string
}

var locales = map[string]*texts{
	"en": {
		title: "Mika", connection: "Connection", recentTasks: "Recent tasks", taskDetail: "Task details",
		devices: "Devices", configError: "Not ready", viewing: "Viewing", defaultTag: "Default", devicePrefix: "Device: ",
		online: "Online", offline: "Offline", serverRow: "Mika server", deviceRow: "Phone status",
		notConfigured: "Add a Mika device on this page to get started.",
		ready:         "Ready", locked: "Locked / screen off", helperOffline: "Helper offline", deviceUnavailable: "Unavailable",
		refresh: "Refresh", back: "Back", more: "Load more", noTasks: "No remote tasks yet.", noGoal: "(untitled task)",
		statusStarting: "Starting", statusRunning: "Running", statusQueued: "Queued",
		statusPaused: "Paused", statusWaitAnswer: "Needs answer", statusWaitPhone: "On phone",
		statusDone: "Done", statusFailed: "Failed", statusCancelled: "Cancelled",
		goalLabel: "Goal", progressLabel: "Progress", questionLabel: "Question", answerLabel: "Answer",
		reasonLabel: "Reason", noteLabel: "Note", steersLabel: "Follow-up instructions", filesLabel: "Files",
		capabilitiesPrefix: "Capabilities: ", taskMetaFmt: "Task %s · Conversation %s", verifiedYes: "Verified", verifiedNo: "Unverified",

		noDevicesTitle:            "No Mika devices yet",
		noDevicesDetail:           "Add an Android phone running Mika. URLs and Bearer tokens are stored encrypted in the hub.",
		addDeviceBtn:              "+ Add device",
		addDeviceTitle:            "Add Mika device",
		editDeviceBtn:             "Edit device",
		editDeviceTitle:           "Edit Mika device",
		testConnBtn:               "Test connection (Doctor)",
		setDefaultBtn:             "Set as default",
		deleteDeviceBtn:           "Delete device",
		rowTest:                   "Test",
		rowEdit:                   "Edit",
		rowSetDefault:             "Set default",
		rowDelete:                 "Delete",
		saveAddBtn:                "Save device",
		saveEditBtn:               "Save changes",
		fieldNameLabel:            "Device name",
		fieldNamePlaceholder:      "e.g. A16, S25+, Pixel 9",
		fieldURLLabel:             "Mika instance URL",
		fieldURLPlaceholder:       "http://192.168.1.20:8765 or https://...",
		fieldURLDetail:            "LAN, Tailscale or loopback HTTP, or public HTTPS. Do not append /mcp.",
		fieldTokenLabel:           "Access token",
		fieldTokenAddPlaceholder:  "Paste token from Mika › Connect a computer",
		fieldTokenEditPlaceholder: "Leave blank to keep the existing token",
		fieldTokenEditDetail:      "Never pre-filled. Leave blank to keep the current token, or enter a new value to replace it.",
		fieldDefaultLabel:         "Set as default device",
		formSecurityFooter:        "Credentials are encrypted in the hub via connector secrets and never written to plaintext files or logs.",
		editTokenFooter:           "Leave the access token blank to keep the current token. Enter a new token only when rotating it.",
		deleteConfirmFmt:          "Delete device %q? Its encrypted URL and token will be removed from Mika.",
		deviceAddedFmt:            "Added %s.",
		deviceSavedFmt:            "Saved changes to %s.",
		deviceDefaultFmt:          "Set %s as the default device.",
		deviceDeletedFmt:          "Deleted %s.",
		testOKFmt:                 "%s online · %s · %s",
		testFailFmt:               "%s connection failed: %s",
		deviceNotFound:            "Device not found. It may have been deleted.",
	},
	"ja": {
		title: "Mika", connection: "接続状態", recentTasks: "最近のタスク", taskDetail: "タスク詳細",
		devices: "端末一覧", configError: "設定不備", viewing: "表示中", defaultTag: "デフォルト", devicePrefix: "端末: ",
		online: "オンライン", offline: "オフライン", serverRow: "Mika サーバー", deviceRow: "端末状態",
		notConfigured: "このページから Mika 端末を追加してください。",
		ready:         "準備完了", locked: "ロック中 / 画面オフ", helperOffline: "ヘルパー未接続", deviceUnavailable: "取得不可",
		refresh: "更新", back: "戻る", more: "さらに読み込む", noTasks: "リモートタスクはまだありません。", noGoal: "(無題のタスク)",
		statusStarting: "開始中", statusRunning: "実行中", statusQueued: "待機中",
		statusPaused: "一時停止", statusWaitAnswer: "回答待ち", statusWaitPhone: "端末操作待ち",
		statusDone: "完了", statusFailed: "失敗", statusCancelled: "キャンセル",
		goalLabel: "目標", progressLabel: "進捗", questionLabel: "質問", answerLabel: "結果",
		reasonLabel: "理由", noteLabel: "注意", steersLabel: "追加指示", filesLabel: "ファイル",
		capabilitiesPrefix: "機能: ", taskMetaFmt: "タスク %s · 会話 %s", verifiedYes: "検証済み", verifiedNo: "未検証",

		noDevicesTitle:            "Mika 端末がまだありません",
		noDevicesDetail:           "Mika を実行中の Android 端末を追加してください。URL とトークンは Hub に暗号化保存されます。",
		addDeviceBtn:              "+ 端末を追加",
		addDeviceTitle:            "Mika 端末を追加",
		editDeviceBtn:             "端末を編集",
		editDeviceTitle:           "Mika 端末を編集",
		testConnBtn:               "接続テスト (Doctor)",
		setDefaultBtn:             "デフォルトに設定",
		deleteDeviceBtn:           "端末を削除",
		rowTest:                   "接続テスト",
		rowEdit:                   "編集",
		rowSetDefault:             "デフォルト設定",
		rowDelete:                 "削除",
		saveAddBtn:                "端末を保存",
		saveEditBtn:               "変更を保存",
		fieldNameLabel:            "端末名",
		fieldNamePlaceholder:      "例: A16、S25+、Pixel 9",
		fieldURLLabel:             "Mika インスタンス URL",
		fieldURLPlaceholder:       "http://192.168.1.20:8765 または https://...",
		fieldURLDetail:            "LAN・Tailscale・ローカル HTTP または公開 HTTPS。/mcp は付けないでください。",
		fieldTokenLabel:           "アクセストークン",
		fieldTokenAddPlaceholder:  "Mika › パソコン接続のトークンを入力",
		fieldTokenEditPlaceholder: "空欄のままにすると現在のトークンを保持",
		fieldTokenEditDetail:      "既存のトークンは表示されません。空欄で保持、新しい値を入力すると置き換わります。",
		fieldDefaultLabel:         "デフォルト端末に設定",
		formSecurityFooter:        "URL とトークンは Hub に暗号化保存され、平文ファイルやログには出力されません。",
		editTokenFooter:           "アクセストークンを空欄のまま保存すると現在のトークンが維持されます。",
		deleteConfirmFmt:          "端末「%s」を削除しますか？暗号化保存された URL とトークンが削除されます。",
		deviceAddedFmt:            "%s を追加しました。",
		deviceSavedFmt:            "%s の変更を保存しました。",
		deviceDefaultFmt:          "%s をデフォルト端末に設定しました。",
		deviceDeletedFmt:          "%s を削除しました。",
		testOKFmt:                 "%s オンライン · %s · %s",
		testFailFmt:               "%s 接続失敗: %s",
		deviceNotFound:            "端末が見つかりません。すでに削除された可能性があります。",
	},
	"zh-Hans": {
		title: "Mika 手机助手", connection: "连接状态", recentTasks: "最近任务", taskDetail: "任务详情",
		devices: "设备列表", configError: "配置不完整", viewing: "当前查看", defaultTag: "默认", devicePrefix: "设备：",
		online: "在线", offline: "离线", serverRow: "Mika 服务", deviceRow: "手机状态",
		notConfigured: "请先在此页面添加一台 Mika 手机设备。",
		ready:         "就绪", locked: "锁屏 / 息屏", helperOffline: "助手未连接", deviceUnavailable: "不可用",
		refresh: "刷新", back: "返回", more: "加载更多", noTasks: "暂无远程任务。", noGoal: "（未命名任务）",
		statusStarting: "启动中", statusRunning: "执行中", statusQueued: "排队中",
		statusPaused: "已暂停", statusWaitAnswer: "等待回答", statusWaitPhone: "需手机处理",
		statusDone: "已完成", statusFailed: "失败", statusCancelled: "已取消",
		goalLabel: "目标", progressLabel: "进度", questionLabel: "追问", answerLabel: "结果",
		reasonLabel: "原因", noteLabel: "提示", steersLabel: "追加指示", filesLabel: "输出文件",
		capabilitiesPrefix: "服务端能力：", taskMetaFmt: "任务 %s · 会话 %s", verifiedYes: "已核验", verifiedNo: "未核验",

		noDevicesTitle:            "尚未添加 Mika 设备",
		noDevicesDetail:           "点击下方按钮添加运行 Mika 的安卓手机；地址与令牌将整体加密存储在 Hub 中。",
		addDeviceBtn:              "+ 添加设备",
		addDeviceTitle:            "添加 Mika 设备",
		editDeviceBtn:             "编辑设备",
		editDeviceTitle:           "编辑 Mika 设备",
		testConnBtn:               "测试连接（Doctor）",
		setDefaultBtn:             "设为默认",
		deleteDeviceBtn:           "删除设备",
		rowTest:                   "测试连接",
		rowEdit:                   "编辑",
		rowSetDefault:             "设为默认",
		rowDelete:                 "删除",
		saveAddBtn:                "保存设备",
		saveEditBtn:               "保存修改",
		fieldNameLabel:            "设备名称",
		fieldNamePlaceholder:      "例如：A16、S25+、主力机",
		fieldURLLabel:             "Mika 实例地址",
		fieldURLPlaceholder:       "http://192.168.1.20:8765 或 https://...",
		fieldURLDetail:            "支持局域网 / Tailscale / 本机 HTTP 或公网 HTTPS，请勿带 /mcp。",
		fieldTokenLabel:           "访问令牌",
		fieldTokenAddPlaceholder:  "粘贴 Mika › 连接电脑 中的访问令牌",
		fieldTokenEditPlaceholder: "留空保留原令牌，输入新值则替换",
		fieldTokenEditDetail:      "令牌不会回显；留空保留原令牌，输入新值将替换并脱敏旧令牌。",
		fieldDefaultLabel:         "设为默认设备",
		formSecurityFooter:        "所有设备地址与访问令牌仅加密存储在 Persona Hub，绝不落明文文件或打印日志。",
		editTokenFooter:           "留空访问令牌可保留原令牌；仅在需要更换令牌时输入新值。",
		deleteConfirmFmt:          "确认删除设备「%s」？其加密保存的地址与访问令牌将一并移除。",
		deviceAddedFmt:            "已添加设备 %s。",
		deviceSavedFmt:            "已保存设备 %s 的修改。",
		deviceDefaultFmt:          "已将 %s 设为默认设备。",
		deviceDeletedFmt:          "已删除设备 %s。",
		testOKFmt:                 "%s 连接正常 · %s · %s",
		testFailFmt:               "%s 连接失败：%s",
		deviceNotFound:            "未找到该设备，可能已被删除。",
	},
}

func textsFor(locale string) *texts {
	if t := locales[locale]; t != nil {
		return t
	}
	if strings.HasPrefix(locale, "zh") {
		return locales["zh-Hans"]
	}
	if i := strings.IndexAny(locale, "-_"); i > 0 {
		if t := locales[locale[:i]]; t != nil {
			return t
		}
	}
	return locales["en"]
}
