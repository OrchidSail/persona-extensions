package main

import "encoding/json"

// discoveryInfo mirrors GET /api/v1 in Mika's RemoteTaskApi.kt.
type discoveryInfo struct {
	APIVersion     int            `json:"api_version"`
	ServerVersion  string         `json:"server_version"`
	Capabilities   []string       `json:"capabilities"`
	HistoryEnabled *bool          `json:"history_enabled,omitempty"`
	Limits         discoveryLimit `json:"limits"`
}

type discoveryLimit struct {
	MaxWaitS  int `json:"max_wait_s"`
	MaxGoal   int `json:"max_goal"`
	MaxAnswer int `json:"max_answer"`
	MaxSteer  int `json:"max_steer"`
}

// deviceResponse mirrors GET /api/v1/device (DeviceAccessResult.toJson in DeviceAccessJson.kt).
type deviceResponse struct {
	Status               string       `json:"status"`
	Enabled              bool         `json:"enabled"`
	CredentialConfigured bool         `json:"credential_configured"`
	AttemptBlocked       bool         `json:"attempt_blocked"`
	StoredKind           string       `json:"stored_kind,omitempty"`
	DetectedKind         string       `json:"detected_kind,omitempty"`
	KeyguardDiagnostics  []string     `json:"keyguard_diagnostics,omitempty"`
	Message              string       `json:"message,omitempty"`
	Device               *deviceState `json:"device,omitempty"`
}

type deviceState struct {
	Interactive     bool `json:"interactive"`
	KeyguardShowing bool `json:"keyguard_showing"`
	DeviceLocked    bool `json:"device_locked"`
	Secure          bool `json:"secure"`
	HelperAvailable bool `json:"helper_available"`
	UserUnlocked    bool `json:"user_unlocked"`
	Ready           bool `json:"ready"`
}

// remoteTask mirrors RemoteStatus.describe in RemoteTasks.kt (and adds changed from POST /wait).
type remoteTask struct {
	TaskID           string        `json:"task_id"`
	Status           string        `json:"status"`
	Conversation     string        `json:"conversation"`
	Goal             string        `json:"goal"`
	StartedAtMs      int64         `json:"started_at_ms"`
	RunID            string        `json:"run_id,omitempty"`
	Step             *int          `json:"step,omitempty"`
	Progress         string        `json:"progress,omitempty"`
	WaitingFor       string        `json:"waiting_for,omitempty"`
	App              string        `json:"app,omitempty"`
	Question         string        `json:"question,omitempty"`
	Options          []string      `json:"options,omitempty"`
	Note             string        `json:"note,omitempty"`
	Reason           string        `json:"reason,omitempty"`
	Message          string        `json:"message,omitempty"`
	Answer           string        `json:"answer,omitempty"`
	Verified         *bool         `json:"verified,omitempty"`
	VerificationNote string        `json:"verification_note,omitempty"`
	Unconfirmed      []string      `json:"unconfirmed,omitempty"`
	FailureKind      string        `json:"failure_kind,omitempty"`
	Files            []taskFile    `json:"files,omitempty"`
	Answerable       bool          `json:"answerable"`
	Steers           []remoteSteer `json:"steers,omitempty"`
	Changed          *bool         `json:"changed,omitempty"`
}

type taskFile struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

type remoteSteer struct {
	MessageID string `json:"message_id"`
	State     string `json:"state"`
}

type taskListResponse struct {
	Tasks      []remoteTask `json:"tasks"`
	NextBefore string       `json:"next_before,omitempty"`
}

type startTaskRequest struct {
	Goal         string `json:"goal"`
	Conversation string `json:"conversation,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
}

type waitTaskRequest struct {
	TimeoutS int `json:"timeout_s"`
}

type textRequest struct {
	Text string `json:"text"`
}

type answerResponse struct {
	TaskID   string `json:"task_id"`
	Accepted bool   `json:"accepted"`
}

type steerResponse struct {
	TaskID    string `json:"task_id"`
	MessageID string `json:"message_id"`
	Accepted  bool   `json:"accepted"`
}

type cancelResponse struct {
	TaskID   string `json:"task_id"`
	Accepted bool   `json:"accepted"`
	Status   string `json:"status,omitempty"`
}

// exportRecord mirrors RunRecordExport.of in RunRecordExport.kt.
type exportRecord struct {
	ExportFormat        int               `json:"exportFormat"`
	Partial             bool              `json:"partial"`
	IncludesScreenshots bool              `json:"includesScreenshots"`
	TaskID              string            `json:"task_id"`
	ConversationID      string            `json:"conversation_id"`
	RunID               string            `json:"run_id"`
	Run                 json.RawMessage   `json:"run"`
	Events              []json.RawMessage `json:"events"`
}

func isTerminalStatus(status string) bool {
	return status == "done" || status == "failed" || status == "cancelled"
}
