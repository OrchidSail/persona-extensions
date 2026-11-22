package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	testToken          = "synthetic-mika-bearer-secret-9876543210"
	testSecondaryToken = "synthetic-mika-secondary-token-1122334455"
	testTertiaryToken  = "synthetic-mika-tertiary-token-6677889900"
)

type fakeMika struct {
	srv            *httptest.Server
	mu             sync.Mutex
	expectedToken  string
	serverVersion  string
	requests       []recordedReq
	forceStatus    int
	forceErrCode   string
	forceErrMsg    string
	retryAfter     string
	redirectTarget string
	startCount     int
	answerCount    int
	steerCount     int
	cancelCount    int
	cancelFinished bool
	waitChanged    bool
	deviceStatus   int
	tasks          []remoteTask
}

type recordedReq struct {
	Method      string
	Path        string
	RawQuery    string
	ContentType string
	ClientHdr   string
	AuthHdr     string
	Origin      string
	Body        string
}

func sampleTasks() []remoteTask {
	step := 3
	verified := true
	return []remoteTask{
		{
			TaskID:           "task-done-1",
			Status:           "done",
			Conversation:     "mcp-11111111-2222-3333-4444-555555555555",
			Goal:             "Check calendar schedule for tomorrow",
			StartedAtMs:      1763712000000,
			RunID:            "20261121-120000-abc123",
			Answer:           "You have two meetings tomorrow: 10:00 Standup and 15:00 Design Review.",
			Verified:         &verified,
			VerificationNote: "Confirmed on calendar screen",
			Answerable:       false,
			Files: []taskFile{
				{ID: "file-1", Name: "schedule.txt", Size: 128, SHA256: "abcdef0123456789"},
			},
		},
		{
			TaskID:       "task-run-2",
			Status:       "running",
			Conversation: "mcp-11111111-2222-3333-4444-666666666666",
			Goal:         "Summarize today's unread notes",
			StartedAtMs:  1763712300000,
			Step:         &step,
			Progress:     "Opening notes list",
			Answerable:   false,
			Steers:       []remoteSteer{{MessageID: "steer-1", State: "joined"}},
		},
		{
			TaskID:       "task-ask-3",
			Status:       "waiting_for_answer",
			Conversation: "mcp-11111111-2222-3333-4444-777777777777",
			Goal:         "Reply to family group message",
			StartedAtMs:  1763712600000,
			Question:     "Which group chat should I open?",
			Options:      []string{"Family", "Weekend"},
			Note:         "Mika is asking a question. Answer this question remotely or on the phone.",
			Answerable:   true,
		},
		{
			TaskID:       "task-phone-4",
			Status:       "waiting_on_phone",
			Conversation: "mcp-11111111-2222-3333-4444-888888888888",
			Goal:         "Confirm online order",
			StartedAtMs:  1763712900000,
			WaitingFor:   "confirmation",
			App:          "ShopApp",
			Note:         "This step needs the user on the phone and cannot be answered remotely.",
			Answerable:   false,
		},
	}
}

func newFakeMika(t *testing.T) *fakeMika {
	return newFakeMikaWith(t, testToken, "0.1.28", sampleTasks())
}

func newFakeMikaWith(t *testing.T, expectedToken, serverVersion string, tasks []remoteTask) *fakeMika {
	t.Helper()
	f := &fakeMika{
		expectedToken: expectedToken,
		serverVersion: serverVersion,
		waitChanged:   true,
		tasks:         slices.Clone(tasks),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedReq{
			Method:      r.Method,
			Path:        r.URL.Path,
			RawQuery:    r.URL.RawQuery,
			ContentType: r.Header.Get("Content-Type"),
			ClientHdr:   r.Header.Get(clientHeaderName),
			AuthHdr:     r.Header.Get("Authorization"),
			Origin:      r.Header.Get("Origin"),
			Body:        string(bodyBytes),
		})
		expectedTok := f.expectedToken
		srvVer := f.serverVersion
		forceStatus := f.forceStatus
		forceCode := f.forceErrCode
		forceMsg := f.forceErrMsg
		retryAfter := f.retryAfter
		redirectTarget := f.redirectTarget
		cancelFinished := f.cancelFinished
		waitChanged := f.waitChanged
		deviceStatus := f.deviceStatus
		tasks := slices.Clone(f.tasks)
		f.mu.Unlock()

		if redirectTarget != "" {
			http.Redirect(w, r, redirectTarget, http.StatusFound)
			return
		}
		if r.Header.Get("Origin") != "" {
			writeMikaError(w, http.StatusForbidden, "access_denied", "Browser requests are not allowed", nil)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+expectedTok {
			writeMikaError(w, http.StatusUnauthorized, "access_denied", "Missing or wrong access token", map[string]string{"WWW-Authenticate": "Bearer"})
			return
		}
		if forceStatus != 0 {
			headers := map[string]string{}
			if retryAfter != "" {
				headers["Retry-After"] = retryAfter
			}
			writeMikaError(w, forceStatus, forceCode, forceMsg, headers)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1":
			hist := false
			_ = json.NewEncoder(w).Encode(discoveryInfo{
				APIVersion:     1,
				ServerVersion:  srvVer,
				Capabilities:   []string{"tasks", "task_steer", "task_recovery", "device_state", "run_export"},
				HistoryEnabled: &hist,
				Limits:         discoveryLimit{MaxWaitS: 50, MaxGoal: 4000, MaxAnswer: 2000, MaxSteer: 4000},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/device":
			if deviceStatus != 0 {
				writeMikaError(w, deviceStatus, "device_unavailable", "Device state is unavailable", nil)
				return
			}
			_ = json.NewEncoder(w).Encode(deviceResponse{
				Status:               "state",
				Enabled:              false,
				CredentialConfigured: false,
				AttemptBlocked:       false,
				Device: &deviceState{
					Interactive:     true,
					KeyguardShowing: false,
					DeviceLocked:    false,
					Secure:          true,
					HelperAvailable: true,
					UserUnlocked:    true,
					Ready:           true,
				},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks":
			nextBefore := ""
			if r.URL.Query().Get("before") == "" && len(tasks) > 0 {
				nextBefore = "cursor-page-2"
			} else if r.URL.Query().Get("before") == "cursor-page-2" {
				tasks = []remoteTask{{
					TaskID:       "task-older-5",
					Status:       "done",
					Conversation: "mcp-11111111-2222-3333-4444-999999999999",
					Goal:         "Older archive task",
					StartedAtMs:  1763700000000,
					Answer:       "Completed earlier",
				}}
			}
			_ = json.NewEncoder(w).Encode(taskListResponse{Tasks: tasks, NextBefore: nextBefore})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks":
			if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
				writeMikaError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", nil)
				return
			}
			var in map[string]any
			if err := json.Unmarshal(bodyBytes, &in); err != nil {
				writeMikaError(w, http.StatusBadRequest, "invalid_argument", "Body must be a JSON object", nil)
				return
			}
			for k := range in {
				if k != "goal" && k != "conversation" && k != "request_id" && k != "attachments" {
					writeMikaError(w, http.StatusBadRequest, "invalid_argument", "Unsupported request fields", nil)
					return
				}
			}
			goal, _ := in["goal"].(string)
			conv, _ := in["conversation"].(string)
			reqID, _ := in["request_id"].(string)
			if strings.TrimSpace(goal) == "" {
				writeMikaError(w, http.StatusBadRequest, "invalid_argument", "goal is required", nil)
				return
			}
			if conv == "" {
				conv = "mcp-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			}
			taskID := reqID
			if taskID == "" {
				taskID = "task-new-101"
			}
			f.mu.Lock()
			f.startCount++
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(remoteTask{
				TaskID:       taskID,
				Status:       "starting",
				Conversation: conv,
				Goal:         goal,
				StartedAtMs:  1763713000000,
			})

		case strings.HasPrefix(r.URL.Path, "/api/v1/tasks/"):
			rest := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
			parts := strings.Split(rest, "/")
			id := parts[0]
			if len(parts) == 1 && r.Method == http.MethodGet {
				for _, item := range tasks {
					if item.TaskID == id {
						_ = json.NewEncoder(w).Encode(item)
						return
					}
				}
				writeMikaError(w, http.StatusNotFound, "not_found", "No remote task with id "+id, nil)
				return
			}
			if len(parts) != 2 {
				writeMikaError(w, http.StatusNotFound, "not_found", "No such endpoint", nil)
				return
			}
			switch parts[1] {
			case "wait":
				if r.Method != http.MethodPost {
					writeMikaError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed", nil)
					return
				}
				var in map[string]any
				if err := json.Unmarshal(bodyBytes, &in); err != nil {
					writeMikaError(w, http.StatusBadRequest, "invalid_argument", "Bad JSON", nil)
					return
				}
				for _, item := range tasks {
					if item.TaskID == id {
						c := waitChanged
						item.Changed = &c
						_ = json.NewEncoder(w).Encode(item)
						return
					}
				}
				writeMikaError(w, http.StatusNotFound, "not_found", "No remote task with id "+id, nil)

			case "answer":
				if r.Method != http.MethodPost {
					writeMikaError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed", nil)
					return
				}
				var in textRequest
				if err := json.Unmarshal(bodyBytes, &in); err != nil || strings.TrimSpace(in.Text) == "" {
					writeMikaError(w, http.StatusBadRequest, "invalid_argument", "text is required", nil)
					return
				}
				if id == "task-phone-4" {
					writeMikaError(w, http.StatusConflict, "conflict", "This step needs the user on the phone and cannot be answered remotely.", nil)
					return
				}
				f.mu.Lock()
				f.answerCount++
				f.mu.Unlock()
				_ = json.NewEncoder(w).Encode(answerResponse{TaskID: id, Accepted: true})

			case "steer":
				if r.Method != http.MethodPost {
					writeMikaError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed", nil)
					return
				}
				var in textRequest
				if err := json.Unmarshal(bodyBytes, &in); err != nil || strings.TrimSpace(in.Text) == "" {
					writeMikaError(w, http.StatusBadRequest, "invalid_argument", "text is required", nil)
					return
				}
				if id == "task-done-1" {
					writeMikaError(w, http.StatusConflict, "conflict", "Task is not running (status done).", nil)
					return
				}
				f.mu.Lock()
				f.steerCount++
				f.mu.Unlock()
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(steerResponse{TaskID: id, MessageID: "msg-steer-42", Accepted: true})

			case "cancel":
				if r.Method != http.MethodPost {
					writeMikaError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed", nil)
					return
				}
				if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") || strings.TrimSpace(string(bodyBytes)) != "{}" {
					writeMikaError(w, http.StatusBadRequest, "invalid_argument", "Cancel requires empty JSON object {}", nil)
					return
				}
				f.mu.Lock()
				f.cancelCount++
				f.mu.Unlock()
				if cancelFinished {
					_ = json.NewEncoder(w).Encode(cancelResponse{TaskID: id, Accepted: false, Status: "done"})
					return
				}
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(cancelResponse{TaskID: id, Accepted: true})

			case "export":
				if r.Method != http.MethodGet {
					writeMikaError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed", nil)
					return
				}
				if id == "task-gone" {
					writeMikaError(w, http.StatusGone, "gone", "Run record is no longer available", nil)
					return
				}
				_ = json.NewEncoder(w).Encode(exportRecord{
					ExportFormat:        1,
					Partial:             false,
					IncludesScreenshots: false,
					TaskID:              id,
					ConversationID:      "mcp-11111111-2222-3333-4444-555555555555",
					RunID:               "20261121-120000-abc123",
					Run:                 json.RawMessage(`{"goal":"Check calendar","outcome":"succeeded"}`),
					Events: []json.RawMessage{
						json.RawMessage(`{"type":"state","atMs":1763712000000,"name":"started"}`),
						json.RawMessage(`{"type":"media","atMs":1763712001000,"mediaId":"m1","step":1}`),
						json.RawMessage(`{"type":"task_ended","atMs":1763712005000,"status":"done"}`),
					},
				})
			default:
				writeMikaError(w, http.StatusNotFound, "not_found", "No such endpoint", nil)
			}

		default:
			writeMikaError(w, http.StatusNotFound, "not_found", "No such endpoint", nil)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeMikaError(w http.ResponseWriter, status int, code, message string, headers map[string]string) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func (f *fakeMika) app() *app {
	base, err := normalizeInstanceURL(f.srv.URL)
	if err != nil {
		panic(err)
	}
	return &app{
		devices: []deviceEntry{{
			slot:        slotPrimary,
			index:       1,
			name:        "default",
			maskedURL:   maskInstanceURL(base),
			configState: deviceStateReady,
			client:      newClient(base, f.expectedToken),
		}},
		allTokens:   collectScrubTokens(f.expectedToken),
		defaultWait: 25,
		receipts:    newWriteStore(""),
		health:      map[string]deviceHealth{},
	}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeMika(t).app())))
}

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "mika" || m.Version != "0.2.1" {
		t.Fatalf("unexpected identity: %+v", m)
	}
	if len(m.Secrets) != 3 ||
		m.Secrets[0].Name != "api_token" || !m.Secrets[0].Required ||
		m.Secrets[1].Name != "secondary_api_token" || m.Secrets[1].Required ||
		m.Secrets[2].Name != "tertiary_api_token" || m.Secrets[2].Required {
		t.Fatalf("unexpected secrets: %+v", m.Secrets)
	}
	// Configuration must keep only instance_url required and must never store plaintext tokens.
	var rawManifest map[string]any
	if err := json.Unmarshal(manifest, &rawManifest); err != nil {
		t.Fatal(err)
	}
	cfg := rawManifest["configuration"].(map[string]any)
	req := cfg["required"].([]any)
	if len(req) != 1 || req[0] != "instance_url" {
		t.Fatalf("unexpected required config fields: %v", req)
	}
	props := cfg["properties"].(map[string]any)
	for _, wantProp := range []string{"instance_name", "instance_url", "secondary_name", "secondary_url", "tertiary_name", "tertiary_url", "default_wait_seconds"} {
		if _, ok := props[wantProp]; !ok {
			t.Fatalf("missing configuration property %q", wantProp)
		}
	}
	for k := range props {
		if strings.Contains(strings.ToLower(k), "token") || strings.Contains(strings.ToLower(k), "secret") {
			t.Fatalf("plaintext credential property %q must not live in configuration", k)
		}
	}
	if m.Contributes == nil || len(m.Contributes.Connectors) != 1 {
		t.Fatalf("unexpected connectors: %+v", m.Contributes)
	}
	wantConfirm := []string{"mika_run_task", "mika_answer", "mika_steer_task", "mika_cancel_task"}
	if !slices.Equal(m.Contributes.Connectors[0].ConfirmAlways, wantConfirm) {
		t.Fatalf("confirm_always = %v, want %v", m.Contributes.Connectors[0].ConfirmAlways, wantConfirm)
	}
	if len(m.Contributes.Skills) != 1 || m.Contributes.Skills[0].Dir != "skills/mika" {
		t.Fatalf("unexpected skills: %+v", m.Contributes.Skills)
	}
	if m.Permissions == nil || !slices.Equal(m.Permissions.Network, []string{}) {
		t.Fatalf("unexpected network permissions: %+v", m.Permissions)
	}
}

func TestInitValidation(t *testing.T) {
	f := newFakeMika(t)
	a := newApp()
	in := &ext.Init{
		InitializeParams: wire.InitializeParams{
			Config:    json.RawMessage(`{"instance_url":"` + f.srv.URL + `","default_wait_seconds":42}`),
			Secrets:   map[string]string{"api_token": testToken},
			Extension: &wire.ExtensionInfo{ID: "mika", Version: "0.2.0", DataDir: t.TempDir()},
		},
	}
	if _, err := a.init(t.Context(), in, nil); err != nil {
		t.Fatalf("valid init failed: %v", err)
	}
	snap := a.snapshot()
	if snap.defaultWait != 42 || len(snap.devices) != 1 || snap.devices[0].name != "default" || !strings.HasSuffix(snap.devices[0].client.base, "/api/v1") {
		t.Fatalf("unexpected init state: wait=%d devices=%+v", snap.defaultWait, snap.devices)
	}

	badWait := &ext.Init{
		InitializeParams: wire.InitializeParams{
			Config:  json.RawMessage(`{"instance_url":"http://127.0.0.1:8765","default_wait_seconds":51}`),
			Secrets: map[string]string{"api_token": testToken},
		},
	}
	if _, err := newApp().init(t.Context(), badWait, nil); err == nil {
		t.Fatal("accepted default_wait_seconds > 50")
	}

	badPublic := &ext.Init{
		InitializeParams: wire.InitializeParams{
			Config:  json.RawMessage(`{"instance_url":"http://mika.example.com:8765"}`),
			Secrets: map[string]string{"api_token": testToken},
		},
	}
	if _, err := newApp().init(t.Context(), badPublic, nil); err == nil {
		t.Fatal("accepted public plaintext HTTP URL in init")
	}
}

func TestInstanceURLValidation(t *testing.T) {
	valid := map[string]string{
		"https://mika.example.com":               "https://mika.example.com/api/v1",
		"https://mika.example.com/":              "https://mika.example.com/api/v1",
		"https://mika.example.com/api/v1":        "https://mika.example.com/api/v1",
		"https://mika.example.com/api/v1/":       "https://mika.example.com/api/v1",
		"https://mika.example.com/phone":         "https://mika.example.com/phone/api/v1",
		"https://mika.example.com/phone/api/v1/": "https://mika.example.com/phone/api/v1",
		"http://127.0.0.1:8765":                  "http://127.0.0.1:8765/api/v1",
		"http://localhost:8765/api/v1":           "http://localhost:8765/api/v1",
		"http://[::1]:8765":                      "http://[::1]:8765/api/v1",
		"http://host.persona.internal:8765":      "http://host.persona.internal:8765/api/v1",
		"http://10.0.0.8:8765":                   "http://10.0.0.8:8765/api/v1",
		"http://172.16.0.10:8765":                "http://172.16.0.10:8765/api/v1",
		"http://172.31.255.1:8765":               "http://172.31.255.1:8765/api/v1",
		"http://192.168.1.23:8765":               "http://192.168.1.23:8765/api/v1",
		"http://100.64.0.1:8765":                 "http://100.64.0.1:8765/api/v1",
		"http://100.127.255.254:8765":            "http://100.127.255.254:8765/api/v1",
		"http://169.254.10.20:8765":              "http://169.254.10.20:8765/api/v1",
		"http://[fd7a:115c:a1e0::1]:8765":        "http://[fd7a:115c:a1e0::1]:8765/api/v1",
		"http://[fe80::1]:8765":                  "http://[fe80::1]:8765/api/v1",
		"http://[::ffff:192.168.1.23]:8765":      "http://[::ffff:192.168.1.23]:8765/api/v1",
		"http://a15.local:8765":                  "http://a15.local:8765/api/v1",
		"http://a15.tailnet.ts.net:8765":         "http://a15.tailnet.ts.net:8765/api/v1",
	}
	for raw, want := range valid {
		got, err := normalizeInstanceURL(raw)
		if err != nil || got != want {
			t.Errorf("normalizeInstanceURL(%q) = (%q, %v), want (%q, nil)", raw, got, err, want)
		}
	}

	invalid := []string{
		"",
		"not-a-url",
		"ftp://127.0.0.1:8765",
		"ws://127.0.0.1:8765",
		"http://mika.example.com:8765",
		"http://8.8.8.8:8765",
		"http://1.1.1.1:8765",
		"http://100.128.0.1:8765",
		"http://172.32.0.1:8765",
		"http://192.169.1.1:8765",
		"http://0.0.0.0:8765",
		"http://[::]:8765",
		"http://[2001:4860:4860::8888]:8765",
		"http://[::ffff:8.8.8.8]:8765",
		"http://evil.ts.net.example.com:8765",
		"https://user:pass@mika.example.com",
		"http://user@127.0.0.1:8765",
		"http://127.0.0.1:8765?foo=bar",
		"http://127.0.0.1:8765/?",
		"http://127.0.0.1:8765#section",
		"http://127.0.0.1:8765/mcp",
		"https://mika.example.com/proxy/mcp/",
		"http://127.0.0.1:8765/api/v1/tasks",
		"http://127.0.0.1:8765/../secret",
		"http://127.0.0.1:99999",
	}
	for _, raw := range invalid {
		if got, err := normalizeInstanceURL(raw); err == nil {
			t.Errorf("normalizeInstanceURL(%q) unexpectedly succeeded with %q", raw, got)
		}
	}
}

func TestMaskInstanceURL(t *testing.T) {
	cases := map[string]string{
		"http://192.168.1.23:8765/api/v1":        "http://192.168.*.*:8765",
		"http://100.88.77.66:8765/api/v1":        "http://100.88.*.*:8765",
		"http://127.0.0.1:8765/api/v1":           "http://127.0.0.1:8765",
		"http://[::1]:8765/api/v1":               "http://[::1]:8765",
		"http://[fd7a:115c:a1e0::1]:8765/api/v1": "http://[fd7a:***:1]:8765",
		"http://a15.tailnet.ts.net:8765/api/v1":  "http://a***.ts.net:8765",
		"https://mika.example.com/api/v1":        "https://m***.example.com",
	}
	for in, want := range cases {
		if got := maskInstanceURL(in); got != want {
			t.Errorf("maskInstanceURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadTools(t *testing.T) {
	f := newFakeMika(t)
	a := f.app()

	// mika_doctor
	text, err := a.callTool(t.Context(), "mika_doctor", nil)
	if err != nil {
		t.Fatalf("mika_doctor: %v", err)
	}
	for _, want := range []string{"Mika Doctor: OK", "0.1.28", "API version: 1", "task_steer", "Ready: true", "unverified"} {
		if !strings.Contains(text, want) {
			t.Fatalf("mika_doctor missing %q in:\n%s", want, text)
		}
	}

	// mika_device_state
	text, err = a.callTool(t.Context(), "mika_device_state", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(text, "Device status: state") || !strings.Contains(text, "helper_available: true") {
		t.Fatalf("mika_device_state: err=%v text=%s", err, text)
	}

	// mika_task_status
	text, err = a.callTool(t.Context(), "mika_task_status", json.RawMessage(`{"task_id":"task-done-1"}`))
	if err != nil || !strings.Contains(text, "Verified: true") || !strings.Contains(text, "schedule.txt") {
		t.Fatalf("mika_task_status done: err=%v text=%s", err, text)
	}
	text, err = a.callTool(t.Context(), "mika_task_status", json.RawMessage(`{"task_id":"task-ask-3"}`))
	if err != nil || !strings.Contains(text, "Which group chat") || !strings.Contains(text, "mika_answer") {
		t.Fatalf("mika_task_status waiting_for_answer: err=%v text=%s", err, text)
	}
	text, err = a.callTool(t.Context(), "mika_task_status", json.RawMessage(`{"task_id":"task-phone-4"}`))
	if err != nil || !strings.Contains(text, "waiting_on_phone") || !strings.Contains(text, "directly on the Android phone") {
		t.Fatalf("mika_task_status waiting_on_phone: err=%v text=%s", err, text)
	}
	if _, err := a.callTool(t.Context(), "mika_task_status", json.RawMessage(`{"task_id":"../escape"}`)); err == nil {
		t.Fatal("accepted path traversal in task_id")
	}

	// mika_wait_task (default wait from app=25 and explicit timeout_s=5)
	text, err = a.callTool(t.Context(), "mika_wait_task", json.RawMessage(`{"task_id":"task-run-2"}`))
	if err != nil || !strings.Contains(text, "Changed: true") {
		t.Fatalf("mika_wait_task default: err=%v text=%s", err, text)
	}
	f.mu.Lock()
	lastWaitBody := f.requests[len(f.requests)-1].Body
	f.waitChanged = false
	f.mu.Unlock()
	if !strings.Contains(lastWaitBody, `"timeout_s":25`) {
		t.Fatalf("expected default timeout_s=25, got %s", lastWaitBody)
	}
	text, err = a.callTool(t.Context(), "mika_wait_task", json.RawMessage(`{"task_id":"task-run-2","timeout_s":5}`))
	if err != nil || !strings.Contains(text, "Changed: false") || !strings.Contains(text, "Wait window elapsed") {
		t.Fatalf("mika_wait_task unchanged: err=%v text=%s", err, text)
	}
	if _, err := a.callTool(t.Context(), "mika_wait_task", json.RawMessage(`{"task_id":"task-run-2","timeout_s":0}`)); err == nil {
		t.Fatal("accepted timeout_s=0")
	}
	if _, err := a.callTool(t.Context(), "mika_wait_task", json.RawMessage(`{"task_id":"task-run-2","timeout_s":51}`)); err == nil {
		t.Fatal("accepted timeout_s=51")
	}

	// mika_list_tasks
	text, err = a.callTool(t.Context(), "mika_list_tasks", json.RawMessage(`{"limit":5}`))
	if err != nil || !strings.Contains(text, "task-done-1") || !strings.Contains(text, "cursor-page-2") {
		t.Fatalf("mika_list_tasks: err=%v text=%s", err, text)
	}
	if _, err := a.callTool(t.Context(), "mika_list_tasks", json.RawMessage(`{"limit":0}`)); err == nil {
		t.Fatal("accepted limit=0")
	}

	// mika_export_task
	text, err = a.callTool(t.Context(), "mika_export_task", json.RawMessage(`{"task_id":"task-done-1","max_events":2}`))
	if err != nil || !strings.Contains(text, "Mika Run Export (format 1)") || !strings.Contains(text, "1 more events omitted") {
		t.Fatalf("mika_export_task: err=%v text=%s", err, text)
	}

	// Verify headers sent on all requests (no Origin, proper X-Mika-Client)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rq := range f.requests {
		if rq.Origin != "" {
			t.Fatalf("request %s %s sent Origin header %q", rq.Method, rq.Path, rq.Origin)
		}
		if rq.ClientHdr != clientHeaderValue {
			t.Fatalf("request %s %s missing %s header, got %q", rq.Method, rq.Path, clientHeaderName, rq.ClientHdr)
		}
	}
}

func TestWriteActions(t *testing.T) {
	f := newFakeMika(t)
	a := f.app()

	conv := "mcp-11111111-2222-3333-4444-555555555555"
	reqUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// mika_run_task describe + do (verifies conversation_id -> conversation wire mapping)
	runArgs := json.RawMessage(`{"goal":"Open Notes and draft agenda","conversation_id":"` + conv + `","request_id":"` + reqUUID + `"}`)
	desc, err := a.describeAction("mika_run_task", runArgs)
	if err != nil || !strings.Contains(desc["summary"].(string), "Open Notes and draft agenda") || !strings.Contains(desc["summary"].(string), "default (primary, ") {
		t.Fatalf("describe mika_run_task: err=%v desc=%+v", err, desc)
	}
	res, err := a.doAction(t.Context(), "hub-run-1", "mika_run_task", runArgs)
	if err != nil || !strings.Contains(res["text"].(string), reqUUID) || !strings.Contains(res["text"].(string), "Android phone") {
		t.Fatalf("do mika_run_task: err=%v res=%+v", err, res)
	}
	f.mu.Lock()
	lastBody := f.requests[len(f.requests)-1].Body
	f.mu.Unlock()
	if !strings.Contains(lastBody, `"conversation":"`+conv+`"`) || strings.Contains(lastBody, `"conversation_id"`) {
		t.Fatalf("expected wire field 'conversation', got %s", lastBody)
	}

	// Validation errors on mika_run_task
	if _, err := a.describeAction("mika_run_task", json.RawMessage(`{"goal":"hi","conversation_id":"bad-conv"}`)); err == nil {
		t.Fatal("accepted invalid conversation_id")
	}
	if _, err := a.describeAction("mika_run_task", json.RawMessage(`{"goal":"hi","request_id":"NOT-A-UUID"}`)); err == nil {
		t.Fatal("accepted invalid request_id UUID")
	}

	// mika_answer
	ansArgs := json.RawMessage(`{"task_id":"task-ask-3","text":"Open Family group"}`)
	if _, err := a.describeAction("mika_answer", ansArgs); err != nil {
		t.Fatalf("describe mika_answer: %v", err)
	}
	res, err = a.doAction(t.Context(), "hub-ans-1", "mika_answer", ansArgs)
	if err != nil || !strings.Contains(res["text"].(string), "accepted: true") {
		t.Fatalf("do mika_answer: err=%v res=%+v", err, res)
	}

	// mika_steer_task (HTTP 202)
	steerArgs := json.RawMessage(`{"task_id":"task-run-2","text":"Only include today's notes"}`)
	if _, err := a.describeAction("mika_steer_task", steerArgs); err != nil {
		t.Fatalf("describe mika_steer_task: %v", err)
	}
	res, err = a.doAction(t.Context(), "hub-steer-1", "mika_steer_task", steerArgs)
	if err != nil || !strings.Contains(res["text"].(string), "msg-steer-42") {
		t.Fatalf("do mika_steer_task: err=%v res=%+v", err, res)
	}

	// mika_cancel_task (HTTP 202 and HTTP 200 already finished; checks JSON {} + Content-Type)
	cancelArgs := json.RawMessage(`{"task_id":"task-run-2"}`)
	if _, err := a.describeAction("mika_cancel_task", cancelArgs); err != nil {
		t.Fatalf("describe mika_cancel_task: %v", err)
	}
	res, err = a.doAction(t.Context(), "hub-cancel-1", "mika_cancel_task", cancelArgs)
	if err != nil || !strings.Contains(res["text"].(string), "accepted: true") {
		t.Fatalf("do mika_cancel_task active: err=%v res=%+v", err, res)
	}
	f.mu.Lock()
	cancelReq := f.requests[len(f.requests)-1]
	f.cancelFinished = true
	f.mu.Unlock()
	if cancelReq.Body != "{}" || !strings.Contains(cancelReq.ContentType, "application/json") {
		t.Fatalf("cancel request did not send {} with application/json: %+v", cancelReq)
	}
	res, err = a.doAction(t.Context(), "hub-cancel-2", "mika_cancel_task", json.RawMessage(`{"task_id":"task-done-1"}`))
	if err != nil || !strings.Contains(res["text"].(string), "already finished") {
		t.Fatalf("do mika_cancel_task finished: err=%v res=%+v", err, res)
	}
}

func TestHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		code       string
		msg        string
		retryAfter string
		wantErr    error
		wantState  string
	}{
		{"400", http.StatusBadRequest, "invalid_argument", "goal is too long", "", errInvalidArgument, "error"},
		{"401", http.StatusUnauthorized, "access_denied", "Missing or wrong access token", "", errUnauthorized, "not_signed_in"},
		{"403", http.StatusForbidden, "access_denied", "Browser requests are not allowed", "", errForbidden, "error"},
		{"404", http.StatusNotFound, "not_found", "No remote task", "", errNotFound, "error"},
		{"409", http.StatusConflict, "conflict", "Task is not running", "", errConflict, "error"},
		{"410", http.StatusGone, "gone", "Run record deleted", "", errGone, "error"},
		{"429", http.StatusTooManyRequests, "access_denied", "Too many failed attempts", "45", errRateLimited, "rate_limited"},
		{"503", http.StatusServiceUnavailable, "unavailable", "Task service refused to start", "", errUnavailable, "error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMika(t)
			f.mu.Lock()
			f.forceStatus = tc.status
			f.forceErrCode = tc.code
			f.forceErrMsg = tc.msg
			f.retryAfter = tc.retryAfter
			f.mu.Unlock()

			a := f.app()
			_, err := a.callTool(t.Context(), "mika_doctor", nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("status %d: got err=%v, want errors.Is(_, %v)", tc.status, err, tc.wantErr)
			}
			st := a.status()
			if st["state"] != tc.wantState {
				t.Fatalf("status %d: connector state=%v, want %q", tc.status, st["state"], tc.wantState)
			}
			if tc.status == http.StatusTooManyRequests {
				mapped := mapConnectorError(err)
				var e *ext.Error
				if !errors.As(mapped, &e) || e.Code != "rate_limited" || !strings.Contains(string(e.Data), `"retry_after":45`) {
					t.Fatalf("429 wire error mapping unexpected: %+v", mapped)
				}
			}
		})
	}
}

func TestRedirectRejected(t *testing.T) {
	targetHits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	f := newFakeMika(t)
	f.mu.Lock()
	f.redirectTarget = target.URL + "/api/v1"
	f.mu.Unlock()

	a := f.app()
	_, err := a.callTool(t.Context(), "mika_doctor", nil)
	if !errors.Is(err, errRedirect) {
		t.Fatalf("expected errRedirect, got %v", err)
	}
	if targetHits != 0 {
		t.Fatalf("client followed HTTP redirect (%d hits)", targetHits)
	}
}

func TestTokenNeverLeaks(t *testing.T) {
	f := newFakeMika(t)
	// Simulate a hostile or buggy peer that echoes the secret back in its error body.
	f.mu.Lock()
	f.forceStatus = http.StatusUnauthorized
	f.forceErrCode = "access_denied"
	f.forceErrMsg = "rejected token " + testToken + " from peer"
	f.mu.Unlock()

	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store

	_, toolErr := a.callTool(t.Context(), "mika_doctor", nil)
	if toolErr == nil {
		t.Fatal("expected error on 401")
	}
	if strings.Contains(toolErr.Error(), testToken) {
		t.Fatalf("token leaked in tool error: %s", toolErr.Error())
	}

	stJSON, _ := json.Marshal(a.status())
	if strings.Contains(string(stJSON), testToken) {
		t.Fatalf("token leaked in connector status: %s", stJSON)
	}

	view, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}})
	if err != nil {
		t.Fatalf("offline render failed: %v", err)
	}
	viewJSON, _ := json.Marshal(view)
	if strings.Contains(string(viewJSON), testToken) {
		t.Fatalf("token leaked in offline UI view: %s", viewJSON)
	}

	// Verify receipt file written on success also never contains the token.
	f.mu.Lock()
	f.forceStatus = 0
	f.mu.Unlock()
	if _, err := a.doAction(t.Context(), "leak-check-1", "mika_run_task", json.RawMessage(`{"goal":"Check weather"}`)); err != nil {
		t.Fatalf("doAction failed: %v", err)
	}
	rawReceipts, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("read receipts: %v", err)
	}
	if strings.Contains(string(rawReceipts), testToken) {
		t.Fatalf("token leaked in write-receipts.json")
	}
}

func TestDirectTransportIgnoresEnvProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")

	f := newFakeMika(t)
	a := f.app()
	if _, err := a.callTool(t.Context(), "mika_doctor", nil); err != nil {
		t.Fatalf("client should ignore env proxy for direct LAN/loopback access: %v", err)
	}
}

func TestActionReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeMika(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store

	runArgs := json.RawMessage(`{"goal":"Open calendar"}`)
	first, err := a.doAction(t.Context(), "replay-1", "mika_run_task", runArgs)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Reload store from disk to test persistence across restart.
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.doAction(t.Context(), "replay-1", "mika_run_task", runArgs)
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if first["text"] != second["text"] || f.startCount != 1 {
		t.Fatalf("replay failed: first=%v second=%v startCount=%d", first, second, f.startCount)
	}

	// Same request_id with different goal or different action must be refused.
	if _, err := a.doAction(t.Context(), "replay-1", "mika_run_task", json.RawMessage(`{"goal":"Different goal"}`)); err == nil {
		t.Fatal("accepted same request_id with different arguments")
	}
	if _, err := a.doAction(t.Context(), "replay-1", "mika_cancel_task", json.RawMessage(`{"task_id":"task-run-2"}`)); err == nil {
		t.Fatal("accepted same request_id with different action")
	}

	// Definite 409 refusal does not leave a stuck pending receipt.
	if _, err := a.doAction(t.Context(), "conflict-1", "mika_steer_task", json.RawMessage(`{"task_id":"task-done-1","text":"extra"}`)); !errors.Is(err, errConflict) {
		t.Fatalf("expected 409 conflict, got %v", err)
	}

	// 503 failure leaves outcome unknown and blocks blind retry on the same request_id.
	f.mu.Lock()
	f.forceStatus = http.StatusServiceUnavailable
	f.forceErrCode = "unavailable"
	f.forceErrMsg = "busy"
	f.mu.Unlock()
	_, err = a.doAction(t.Context(), "unknown-1", "mika_run_task", json.RawMessage(`{"goal":"Check inbox","request_id":"12345678-1234-1234-1234-1234567890ab"}`))
	if err == nil || !strings.Contains(err.Error(), "unknown") || !strings.Contains(err.Error(), "12345678-1234-1234-1234-1234567890ab") {
		t.Fatalf("expected unknown outcome hint with request_id, got %v", err)
	}
	f.mu.Lock()
	f.forceStatus = 0
	f.mu.Unlock()
	if _, err := a.doAction(t.Context(), "unknown-1", "mika_run_task", json.RawMessage(`{"goal":"Check inbox","request_id":"12345678-1234-1234-1234-1234567890ab"}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected retry of pending receipt to refuse, got %v", err)
	}
	if f.startCount != 1 {
		t.Fatalf("pending receipt caused unexpected extra start (%d)", f.startCount)
	}
}

func TestUIViewsAndActions(t *testing.T) {
	// Unconfigured app renders empty guidance view.
	unconfigured := newApp()
	if unconfigured.status()["state"] != "not_configured" {
		t.Fatalf("unexpected status: %+v", unconfigured.status())
	}
	v, err := unconfigured.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}})
	if err != nil || wire.CheckView(v) != nil {
		t.Fatalf("unconfigured view invalid: err=%v check=%v", err, wire.CheckView(v))
	}

	f := newFakeMika(t)
	a := f.app()

	// Localized overview & detail views across UI versions 1, 3, 6.
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, uiVer := range []int{1, 3, 6} {
			req := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: uiVer, TZ: "Asia/Shanghai"}}
			ov, err := a.page(t.Context(), req, pageState{Mode: "overview", Pages: 1})
			if err != nil {
				t.Fatalf("%s v%d overview: %v", locale, uiVer, err)
			}
			if err := wire.CheckView(ov); err != nil {
				t.Fatalf("%s v%d overview CheckView: %v", locale, uiVer, err)
			}
			dt, err := a.page(t.Context(), req, pageState{Mode: "detail", TaskID: "task-done-1"})
			if err != nil {
				t.Fatalf("%s v%d detail: %v", locale, uiVer, err)
			}
			if err := wire.CheckView(dt); err != nil {
				t.Fatalf("%s v%d detail CheckView: %v", locale, uiVer, err)
			}
		}
	}

	// Verify UI actions: refresh, detail, back, more.
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6, Action: "refresh"}}
	res, err := a.action(t.Context(), req)
	if err != nil || res.View == nil || wire.CheckView(res.View) != nil {
		t.Fatalf("refresh action failed: err=%v res=%+v", err, res)
	}
	viewJSON, _ := json.Marshal(res.View)
	for _, want := range []string{"在线", "0.1.28", "就绪", "task-done-1", "刷新"} {
		if !strings.Contains(string(viewJSON), want) {
			t.Fatalf("overview view missing %q in %s", want, viewJSON)
		}
	}

	detailReq := &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "detail",
		Values: map[string]string{"task_id": "task-ask-3"},
	}}
	res, err = a.action(t.Context(), detailReq)
	if err != nil || res.View == nil || wire.CheckView(res.View) != nil {
		t.Fatalf("detail action failed: err=%v res=%+v", err, res)
	}
	if b, _ := json.Marshal(res.View); !strings.Contains(string(b), "Which group chat") {
		t.Fatalf("detail view missing question: %s", b)
	}

	moreReq := &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "more",
		State: ext.EncodeState(pageState{Mode: "overview", Pages: 1}),
	}}
	res, err = a.action(t.Context(), moreReq)
	if err != nil || res.View == nil || wire.CheckView(res.View) != nil {
		t.Fatalf("more action failed: err=%v res=%+v", err, res)
	}
	if b, _ := json.Marshal(res.View); !strings.Contains(string(b), "task-older-5") {
		t.Fatalf("more action did not load second page: %s", b)
	}

	backReq := &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "back",
		State: ext.EncodeState(pageState{Mode: "detail", TaskID: "task-ask-3"}),
	}}
	res, err = a.action(t.Context(), backReq)
	if err != nil || res.View == nil || wire.CheckView(res.View) != nil {
		t.Fatalf("back action failed: err=%v res=%+v", err, res)
	}

	// Oversized answer is safely bounded within wire.CheckView limits.
	f.mu.Lock()
	f.tasks[0].Answer = strings.Repeat("long line of mika task output\n", 400)
	f.mu.Unlock()
	dt, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "detail", TaskID: "task-done-1"})
	if err != nil || wire.CheckView(dt) != nil {
		t.Fatalf("oversized detail view failed: err=%v check=%v", err, wire.CheckView(dt))
	}

	// Offline state renders cleanly without failing ui.render.
	f.mu.Lock()
	f.forceStatus = http.StatusServiceUnavailable
	f.forceErrCode = "unavailable"
	f.forceErrMsg = "phone asleep"
	f.mu.Unlock()
	off, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}})
	if err != nil || wire.CheckView(off) != nil {
		t.Fatalf("offline view invalid: err=%v check=%v", err, wire.CheckView(off))
	}
	if b, _ := json.Marshal(off); !strings.Contains(string(b), "离线") {
		t.Fatalf("offline view missing offline badge: %s", b)
	}
}

func TestConnectorDispatch(t *testing.T) {
	f := newFakeMika(t)
	a := f.app()

	if _, err := a.connector(context.Background(), "status", json.RawMessage(`{"contribution":"other"}`)); err == nil {
		t.Fatal("accepted wrong contribution id")
	}
	res, err := a.connector(context.Background(), "tools.list", json.RawMessage(`{"contribution":"mika"}`))
	if err != nil {
		t.Fatalf("tools.list: %v", err)
	}
	m := res.(map[string]any)
	readList := m["tools"].([]tool)
	if len(readList) != 7 {
		t.Fatalf("expected 7 read tools, got %d", len(readList))
	}
	for _, tl := range readList {
		props := tl.InputSchema["properties"].(map[string]any)
		if tl.Name == "mika_list_devices" {
			continue
		}
		if _, ok := props["device"]; !ok {
			t.Fatalf("read tool %s missing optional device parameter", tl.Name)
		}
	}
	res, err = a.connector(context.Background(), "actions.list", json.RawMessage(`{"contribution":"mika"}`))
	if err != nil {
		t.Fatalf("actions.list: %v", err)
	}
	m = res.(map[string]any)
	actList := m["tools"].([]tool)
	if len(actList) != 4 {
		t.Fatalf("expected 4 action tools, got %d", len(actList))
	}
	for _, tl := range actList {
		props := tl.InputSchema["properties"].(map[string]any)
		if _, ok := props["device"]; !ok {
			t.Fatalf("action tool %s missing optional device parameter", tl.Name)
		}
	}
	if _, err := a.connector(context.Background(), "actions.do", json.RawMessage(`{"contribution":"mika","name":"mika_cancel_task","args":{"task_id":"task-run-2"}}`)); err == nil {
		t.Fatal("accepted actions.do without request_id")
	}
}
