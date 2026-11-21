package main

import "strings"

type texts struct {
	title, connection, recentTasks, taskDetail               string
	online, offline, serverRow, deviceRow, notConfigured     string
	ready, locked, helperOffline, deviceUnavailable          string
	refresh, back, more, noTasks, noGoal                     string
	statusStarting, statusRunning, statusQueued              string
	statusPaused, statusWaitAnswer, statusWaitPhone          string
	statusDone, statusFailed, statusCancelled                string
	goalLabel, progressLabel, questionLabel, answerLabel     string
	reasonLabel, noteLabel, steersLabel, filesLabel          string
	capabilitiesPrefix, taskMetaFmt, verifiedYes, verifiedNo string
}

var locales = map[string]*texts{
	"en": {
		title: "Mika", connection: "Connection", recentTasks: "Recent tasks", taskDetail: "Task details",
		online: "Online", offline: "Offline", serverRow: "Mika server", deviceRow: "Phone status",
		notConfigured: "Set the Mika instance URL and api_token in Settings › Extensions first.",
		ready:         "Ready", locked: "Locked / screen off", helperOffline: "Helper offline", deviceUnavailable: "Unavailable",
		refresh: "Refresh", back: "Back", more: "Load more", noTasks: "No remote tasks yet.", noGoal: "(untitled task)",
		statusStarting: "Starting", statusRunning: "Running", statusQueued: "Queued",
		statusPaused: "Paused", statusWaitAnswer: "Needs answer", statusWaitPhone: "On phone",
		statusDone: "Done", statusFailed: "Failed", statusCancelled: "Cancelled",
		goalLabel: "Goal", progressLabel: "Progress", questionLabel: "Question", answerLabel: "Answer",
		reasonLabel: "Reason", noteLabel: "Note", steersLabel: "Follow-up instructions", filesLabel: "Files",
		capabilitiesPrefix: "Capabilities: ", taskMetaFmt: "Task %s · Conversation %s", verifiedYes: "Verified", verifiedNo: "Unverified",
	},
	"ja": {
		title: "Mika", connection: "接続状態", recentTasks: "最近のタスク", taskDetail: "タスク詳細",
		online: "オンライン", offline: "オフライン", serverRow: "Mika サーバー", deviceRow: "端末状態",
		notConfigured: "設定 › 拡張機能で Mika インスタンス URL とアクセストークンを設定してください。",
		ready:         "準備完了", locked: "ロック中 / 画面オフ", helperOffline: "ヘルパー未接続", deviceUnavailable: "取得不可",
		refresh: "更新", back: "戻る", more: "さらに読み込む", noTasks: "リモートタスクはまだありません。", noGoal: "(無題のタスク)",
		statusStarting: "開始中", statusRunning: "実行中", statusQueued: "待機中",
		statusPaused: "一時停止", statusWaitAnswer: "回答待ち", statusWaitPhone: "端末操作待ち",
		statusDone: "完了", statusFailed: "失敗", statusCancelled: "キャンセル",
		goalLabel: "目標", progressLabel: "進捗", questionLabel: "質問", answerLabel: "結果",
		reasonLabel: "理由", noteLabel: "注意", steersLabel: "追加指示", filesLabel: "ファイル",
		capabilitiesPrefix: "機能: ", taskMetaFmt: "タスク %s · 会話 %s", verifiedYes: "検証済み", verifiedNo: "未検証",
	},
	"zh-Hans": {
		title: "Mika 手机助手", connection: "连接状态", recentTasks: "最近任务", taskDetail: "任务详情",
		online: "在线", offline: "离线", serverRow: "Mika 服务", deviceRow: "手机状态",
		notConfigured: "请先在「设置 › 扩展」中填写 Mika 实例地址和访问令牌。",
		ready:         "就绪", locked: "锁屏 / 息屏", helperOffline: "助手未连接", deviceUnavailable: "不可用",
		refresh: "刷新", back: "返回", more: "加载更多", noTasks: "暂无远程任务。", noGoal: "（未命名任务）",
		statusStarting: "启动中", statusRunning: "执行中", statusQueued: "排队中",
		statusPaused: "已暂停", statusWaitAnswer: "等待回答", statusWaitPhone: "需手机处理",
		statusDone: "已完成", statusFailed: "失败", statusCancelled: "已取消",
		goalLabel: "目标", progressLabel: "进度", questionLabel: "追问", answerLabel: "结果",
		reasonLabel: "原因", noteLabel: "提示", steersLabel: "追加指示", filesLabel: "输出文件",
		capabilitiesPrefix: "服务端能力：", taskMetaFmt: "任务 %s · 会话 %s", verifiedYes: "已核验", verifiedNo: "未核验",
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
