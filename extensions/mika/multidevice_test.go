package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func secondarySampleTasks() []remoteTask {
	return []remoteTask{
		{
			TaskID:       "s25-task-done-1",
			Status:       "done",
			Conversation: "mcp-22222222-2222-3333-4444-555555555555",
			Goal:         "Check S25+ battery report",
			StartedAtMs:  1763714000000,
			RunID:        "20261121-130000-s25abc",
			Answer:       "S25+ battery is at 88%.",
			Answerable:   false,
		},
		{
			TaskID:       "s25-task-ask-2",
			Status:       "waiting_for_answer",
			Conversation: "mcp-22222222-2222-3333-4444-666666666666",
			Goal:         "Send S25+ note",
			StartedAtMs:  1763714100000,
			Question:     "Save to work folder on S25+?",
			Answerable:   true,
		},
		{
			TaskID:       "s25-task-run-3",
			Status:       "running",
			Conversation: "mcp-22222222-2222-3333-4444-777777777777",
			Goal:         "Sync S25+ reminders",
			StartedAtMs:  1763714200000,
			Progress:     "Reading reminder list",
		},
	}
}

func tertiarySampleTasks() []remoteTask {
	return []remoteTask{
		{
			TaskID:       "p9-task-done-1",
			Status:       "done",
			Conversation: "mcp-33333333-2222-3333-4444-555555555555",
			Goal:         "Check Pixel9 notifications",
			StartedAtMs:  1763715000000,
			Answer:       "No unread notifications on Pixel9.",
		},
	}
}

func initTestApp(t *testing.T, cfg map[string]any, secrets map[string]string, dataDir string) (*app, error) {
	t.Helper()
	rawCfg, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp()
	in := &ext.Init{
		InitializeParams: wire.InitializeParams{
			Config:    rawCfg,
			Secrets:   secrets,
			Extension: &wire.ExtensionInfo{ID: "mika", Version: "0.2.0", DataDir: dataDir},
		},
	}
	_, err = a.init(t.Context(), in, nil)
	return a, err
}

func TestMultiDeviceConfigAndBackwardCompat(t *testing.T) {
	// Legacy v0.1.0 single-device config without instance_name or secondary/tertiary slots.
	f1 := newFakeMikaWith(t, testToken, "0.1.28", sampleTasks())
	legacy, err := initTestApp(t, map[string]any{
		"instance_url":         f1.srv.URL,
		"default_wait_seconds": 20,
	}, map[string]string{
		"api_token": testToken,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("legacy single-device init failed: %v", err)
	}
	listText, err := legacy.callTool(t.Context(), "mika_list_devices", nil)
	if err != nil {
		t.Fatalf("mika_list_devices failed: %v", err)
	}
	for _, want := range []string{"Configured Mika devices (1)", "default", "primary (1, default)", "status: ready"} {
		if !strings.Contains(listText, want) {
			t.Fatalf("legacy mika_list_devices missing %q in:\n%s", want, listText)
		}
	}
	docText, err := legacy.callTool(t.Context(), "mika_doctor", nil)
	if err != nil || !strings.Contains(docText, "Mika Doctor: OK") || !strings.Contains(docText, "0.1.28") {
		t.Fatalf("legacy mika_doctor failed: err=%v text=%s", err, docText)
	}

	// Full 3-device configuration with distinct names and distinct Bearer tokens.
	f2 := newFakeMikaWith(t, testSecondaryToken, "0.2.1", secondarySampleTasks())
	f3 := newFakeMikaWith(t, testTertiaryToken, "0.2.2", tertiarySampleTasks())
	multi, err := initTestApp(t, map[string]any{
		"instance_name":  "A16",
		"instance_url":   f1.srv.URL,
		"secondary_name": "S25+",
		"secondary_url":  f2.srv.URL,
		"tertiary_name":  "Pixel9",
		"tertiary_url":   f3.srv.URL,
	}, map[string]string{
		"api_token":           testToken,
		"secondary_api_token": testSecondaryToken,
		"tertiary_api_token":  testTertiaryToken,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("3-device init failed: %v", err)
	}

	listText, err = multi.callTool(t.Context(), "mika_list_devices", nil)
	if err != nil {
		t.Fatalf("3-device mika_list_devices failed: %v", err)
	}
	for _, want := range []string{"Configured Mika devices (3)", "A16", "S25+", "Pixel9", "primary (1, default)", "secondary (2)", "tertiary (3)"} {
		if !strings.Contains(listText, want) {
			t.Fatalf("3-device mika_list_devices missing %q in:\n%s", want, listText)
		}
	}

	// Verify each device receives ONLY its own Bearer token.
	for _, dev := range []string{"A16", "S25+", "Pixel9"} {
		if _, err := multi.callTool(t.Context(), "mika_device_state", json.RawMessage(`{"device":"`+dev+`"}`)); err != nil {
			t.Fatalf("mika_device_state(%s) failed: %v", dev, err)
		}
	}
	checkPeerToken := func(name string, peer *fakeMika, wantTok string) {
		t.Helper()
		peer.mu.Lock()
		defer peer.mu.Unlock()
		if len(peer.requests) == 0 {
			t.Fatalf("%s received no requests", name)
		}
		for _, rq := range peer.requests {
			if rq.AuthHdr != "Bearer "+wantTok {
				t.Fatalf("%s received wrong Authorization header %q, want Bearer %s", name, rq.AuthHdr, wantTok)
			}
		}
	}
	checkPeerToken("A16", f1, testToken)
	checkPeerToken("S25+", f2, testSecondaryToken)
	checkPeerToken("Pixel9", f3, testTertiaryToken)
}

func TestMultiDeviceRoutingAndAliases(t *testing.T) {
	f1 := newFakeMikaWith(t, testToken, "0.1.28", sampleTasks())
	f2 := newFakeMikaWith(t, testSecondaryToken, "0.2.1", secondarySampleTasks())
	f3 := newFakeMikaWith(t, testTertiaryToken, "0.2.2", tertiarySampleTasks())

	a, err := initTestApp(t, map[string]any{
		"instance_name":  "A16",
		"instance_url":   f1.srv.URL,
		"secondary_name": "S25+",
		"secondary_url":  f2.srv.URL,
		"tertiary_name":  "Pixel9",
		"tertiary_url":   f3.srv.URL,
	}, map[string]string{
		"api_token":           testToken,
		"secondary_api_token": testSecondaryToken,
		"tertiary_api_token":  testTertiaryToken,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Default (omitted device) -> slot 1 (A16).
	text, err := a.callTool(t.Context(), "mika_list_tasks", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(text, "A16 (primary)") || !strings.Contains(text, "task-done-1") {
		t.Fatalf("default mika_list_tasks unexpected: err=%v text=%s", err, text)
	}

	// Case-insensitive custom names and slot aliases across read tools.
	readCases := []struct {
		tool string
		args string
		want []string
	}{
		{"mika_doctor", `{"device":"s25+"}`, []string{"Device: S25+ (secondary)", "0.2.1"}},
		{"mika_doctor", `{"device":"3"}`, []string{"Device: Pixel9 (tertiary)", "0.2.2"}},
		{"mika_device_state", `{"device":"PRIMARY"}`, []string{"Device: A16 (primary)", "Device status: state"}},
		{"mika_task_status", `{"task_id":"s25-task-done-1","device":"2"}`, []string{"Device: S25+ (secondary)", "88%"}},
		{"mika_wait_task", `{"task_id":"s25-task-run-3","timeout_s":5,"device":"secondary"}`, []string{"Device: S25+ (secondary)", "Changed: true"}},
		{"mika_list_tasks", `{"device":"pixel9"}`, []string{"Device: Pixel9 (tertiary)", "p9-task-done-1"}},
		{"mika_export_task", `{"task_id":"s25-task-done-1","device":"S25+"}`, []string{"Device: S25+ (secondary)", "s25-task-done-1"}},
	}
	for _, tc := range readCases {
		got, err := a.callTool(t.Context(), tc.tool, json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("%s(%s) failed: %v", tc.tool, tc.args, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Fatalf("%s(%s) missing %q in:\n%s", tc.tool, tc.args, w, got)
			}
		}
	}

	// Write actions routed to secondary (S25+) and tertiary (Pixel9).
	if _, err := a.doAction(t.Context(), "act-run-s25", "mika_run_task", json.RawMessage(`{"goal":"Check S25+ settings","device":"s25+"}`)); err != nil {
		t.Fatalf("mika_run_task on S25+ failed: %v", err)
	}
	if _, err := a.doAction(t.Context(), "act-ans-s25", "mika_answer", json.RawMessage(`{"task_id":"s25-task-ask-2","text":"Yes","device":"2"}`)); err != nil {
		t.Fatalf("mika_answer on S25+ failed: %v", err)
	}
	if _, err := a.doAction(t.Context(), "act-steer-s25", "mika_steer_task", json.RawMessage(`{"task_id":"s25-task-run-3","text":"Skip archived","device":"secondary"}`)); err != nil {
		t.Fatalf("mika_steer_task on S25+ failed: %v", err)
	}
	if _, err := a.doAction(t.Context(), "act-cancel-p9", "mika_cancel_task", json.RawMessage(`{"task_id":"p9-task-done-1","device":"tertiary"}`)); err != nil {
		t.Fatalf("mika_cancel_task on Pixel9 failed: %v", err)
	}
	f1.mu.Lock()
	f1Starts := f1.startCount
	f1.mu.Unlock()
	f2.mu.Lock()
	f2Starts, f2Answers, f2Steers := f2.startCount, f2.answerCount, f2.steerCount
	f2.mu.Unlock()
	f3.mu.Lock()
	f3Cancels := f3.cancelCount
	f3.mu.Unlock()
	if f1Starts != 0 || f2Starts != 1 || f2Answers != 1 || f2Steers != 1 || f3Cancels != 1 {
		t.Fatalf("routing counts mismatch: f1Starts=%d f2=(%d,%d,%d) f3Cancels=%d", f1Starts, f2Starts, f2Answers, f2Steers, f3Cancels)
	}

	// Unknown device returns clear error listing available devices.
	_, err = a.callTool(t.Context(), "mika_device_state", json.RawMessage(`{"device":"iPhone16"}`))
	if err == nil {
		t.Fatal("expected error for unknown device")
	}
	for _, want := range []string{"unknown Mika device", "A16 (primary)", "S25+ (secondary)", "Pixel9 (tertiary)"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("unknown device error missing %q: %v", want, err)
		}
	}

	// Duplicate custom names: rejected by name with clear error, still disambiguated by slot alias & default.
	dupApp, err := initTestApp(t, map[string]any{
		"instance_name":  "Phone",
		"instance_url":   f1.srv.URL,
		"secondary_name": "phone",
		"secondary_url":  f2.srv.URL,
	}, map[string]string{
		"api_token":           testToken,
		"secondary_api_token": testSecondaryToken,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("duplicate-name init should succeed for slot disambiguation: %v", err)
	}
	if listOut, err := dupApp.callTool(t.Context(), "mika_list_devices", nil); err != nil || !strings.Contains(listOut, "duplicate device name") {
		t.Fatalf("mika_list_devices should warn on duplicate names: err=%v out=%s", err, listOut)
	}
	if _, err := dupApp.callTool(t.Context(), "mika_device_state", json.RawMessage(`{"device":"PHONE"}`)); err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "primary") || !strings.Contains(err.Error(), "secondary") {
		t.Fatalf("expected ambiguous duplicate-name error listing slots, got %v", err)
	}
	if got, err := dupApp.callTool(t.Context(), "mika_doctor", json.RawMessage(`{"device":"secondary"}`)); err != nil || !strings.Contains(got, "0.2.1") {
		t.Fatalf("slot alias disambiguation failed: err=%v got=%s", err, got)
	}
	if got, err := dupApp.callTool(t.Context(), "mika_device_state", json.RawMessage(`{}`)); err != nil || !strings.Contains(got, "Phone (primary)") {
		t.Fatalf("default fallback to slot 1 failed with duplicate names: err=%v got=%s", err, got)
	}
}

func TestMultiDeviceDoctorSummaryAndIsolation(t *testing.T) {
	f1 := newFakeMikaWith(t, testToken, "0.1.28", sampleTasks())
	f2 := newFakeMikaWith(t, testSecondaryToken, "0.2.1", secondarySampleTasks())

	// Case 1: Secondary URL set without secondary_api_token; tertiary token set without tertiary_url.
	// Primary must start and operate normally; partial slots must report clear redacted config errors.
	a, err := initTestApp(t, map[string]any{
		"instance_name":  "A16",
		"instance_url":   f1.srv.URL,
		"secondary_name": "S25+",
		"secondary_url":  "http://192.168.10.42:8765",
		"tertiary_name":  "Backup",
	}, map[string]string{
		"api_token":          testToken,
		"tertiary_api_token": testTertiaryToken,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("init should isolate incomplete optional slots: %v", err)
	}
	if st := a.status(); st["state"] != "ok" {
		t.Fatalf("connector status should remain ok while primary is healthy: %+v", st)
	}
	listText, err := a.callTool(t.Context(), "mika_list_devices", nil)
	if err != nil {
		t.Fatalf("mika_list_devices failed: %v", err)
	}
	for _, want := range []string{"A16", "status: ready", "S25+", "http://192.168.*.*:8765", "status: missing_token", "secondary_api_token", "Backup", "status: missing_url", "tertiary_url"} {
		if !strings.Contains(listText, want) {
			t.Fatalf("mika_list_devices missing %q in:\n%s", want, listText)
		}
	}
	if strings.Contains(listText, "192.168.10.42") || strings.Contains(listText, testTertiaryToken) {
		t.Fatalf("mika_list_devices leaked unmasked IP or token:\n%s", listText)
	}

	// mika_doctor without device summarizes all 3 slots without failing.
	docSummary, err := a.callTool(t.Context(), "mika_doctor", nil)
	if err != nil {
		t.Fatalf("multi-device mika_doctor summary failed: %v", err)
	}
	for _, want := range []string{"1/3 devices reachable", "Device: A16 (primary, default)", "Mika Doctor: OK", "Mika Doctor: NOT READY (missing_token)", "Mika Doctor: NOT READY (missing_url)"} {
		if !strings.Contains(docSummary, want) {
			t.Fatalf("mika_doctor summary missing %q in:\n%s", want, docSummary)
		}
	}
	// Targeting incomplete slot directly returns a clear error naming the missing secret.
	if _, err := a.callTool(t.Context(), "mika_device_state", json.RawMessage(`{"device":"S25+"}`)); err == nil || !errors.Is(err, errNotConfigured) || !strings.Contains(err.Error(), "secondary_api_token") {
		t.Fatalf("expected missing_token error for S25+, got %v", err)
	}

	// Case 2: Secondary has invalid URL (public HTTP with embedded creds) while primary is valid.
	invalidSec, err := initTestApp(t, map[string]any{
		"instance_name":  "A16",
		"instance_url":   f1.srv.URL,
		"secondary_name": "BadPhone",
		"secondary_url":  "http://user:superpass@public.example.com:8765",
	}, map[string]string{
		"api_token":           testToken,
		"secondary_api_token": testSecondaryToken,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("init should isolate invalid secondary URL when primary is ready: %v", err)
	}
	if got, err := invalidSec.callTool(t.Context(), "mika_list_devices", nil); err != nil || !strings.Contains(got, "invalid_url") || strings.Contains(got, "superpass") || strings.Contains(got, testSecondaryToken) {
		t.Fatalf("invalid secondary URL status unexpected: err=%v got=%s", err, got)
	}
	if _, err := invalidSec.callTool(t.Context(), "mika_doctor", json.RawMessage(`{"device":"A16"}`)); err != nil {
		t.Fatalf("primary A16 should stay healthy when secondary URL is invalid: %v", err)
	}

	// Case 3: Primary missing token while secondary (S25+) IS valid -> secondary still works!
	secOnly, err := initTestApp(t, map[string]any{
		"instance_name":  "A16",
		"instance_url":   f1.srv.URL,
		"secondary_name": "S25+",
		"secondary_url":  f2.srv.URL,
	}, map[string]string{
		"secondary_api_token": testSecondaryToken,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("init should succeed when secondary is ready even if primary lacks token: %v", err)
	}
	if got, err := secOnly.callTool(t.Context(), "mika_doctor", json.RawMessage(`{"device":"S25+"}`)); err != nil || !strings.Contains(got, "0.2.1") {
		t.Fatalf("secondary should work when primary lacks token: err=%v got=%s", err, got)
	}
	if _, err := secOnly.callTool(t.Context(), "mika_device_state", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "api_token") || !strings.Contains(err.Error(), "S25+ (secondary)") {
		t.Fatalf("default primary error should mention missing api_token and ready secondary: %v", err)
	}

	// Case 4: Runtime failure isolation (secondary returns 503 while primary is online).
	f2.mu.Lock()
	f2.forceStatus = http.StatusServiceUnavailable
	f2.forceErrCode = "unavailable"
	f2.forceErrMsg = "S25+ is asleep"
	f2.mu.Unlock()
	both, err := initTestApp(t, map[string]any{
		"instance_name":  "A16",
		"instance_url":   f1.srv.URL,
		"secondary_name": "S25+",
		"secondary_url":  f2.srv.URL,
	}, map[string]string{
		"api_token":           testToken,
		"secondary_api_token": testSecondaryToken,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	summary, err := both.callTool(t.Context(), "mika_doctor", nil)
	if err != nil {
		t.Fatalf("mika_doctor summary should succeed when 1 of 2 devices is offline: %v", err)
	}
	for _, want := range []string{"1/2 devices reachable", "Mika Doctor: OK", "Mika Doctor: ERROR", "S25+ is asleep"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("runtime isolation summary missing %q in:\n%s", want, summary)
		}
	}
	if st := both.status(); st["state"] != "ok" {
		t.Fatalf("connector status should remain ok when at least one device is healthy: %+v", st)
	}
}

func TestMultiDeviceConfirmationCards(t *testing.T) {
	f1 := newFakeMikaWith(t, testToken, "0.1.28", sampleTasks())
	f2 := newFakeMikaWith(t, testSecondaryToken, "0.2.1", secondarySampleTasks())

	a, err := initTestApp(t, map[string]any{
		"instance_name":  "A16",
		"instance_url":   f1.srv.URL,
		"secondary_name": "S25+",
		"secondary_url":  f2.srv.URL,
		"tertiary_name":  "NoTokenPhone",
		"tertiary_url":   "http://100.64.1.9:8765",
	}, map[string]string{
		"api_token":           testToken,
		"secondary_api_token": testSecondaryToken,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	masked1 := maskInstanceURL(f1.srv.URL)
	masked2 := maskInstanceURL(f2.srv.URL)

	cases := []struct {
		action string
		args   string
		wants  []string
	}{
		{"mika_run_task", `{"goal":"Open Maps"}`, []string{"A16 (primary, " + masked1 + ")", "Open Maps"}},
		{"mika_run_task", `{"goal":"Check battery","device":"S25+"}`, []string{"S25+ (secondary, " + masked2 + ")", "Check battery"}},
		{"mika_answer", `{"task_id":"s25-task-ask-2","text":"Yes","device":"2"}`, []string{"S25+ (secondary, " + masked2 + ")", "s25-task-ask-2", "Yes"}},
		{"mika_steer_task", `{"task_id":"s25-task-run-3","text":"Faster","device":"secondary"}`, []string{"S25+ (secondary, " + masked2 + ")", "s25-task-run-3", "Faster"}},
		{"mika_cancel_task", `{"task_id":"task-run-2","device":"a16"}`, []string{"A16 (primary, " + masked1 + ")", "task-run-2"}},
	}
	for _, tc := range cases {
		desc, err := a.describeAction(tc.action, json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("describeAction(%s, %s) failed: %v", tc.action, tc.args, err)
		}
		summary := desc["summary"].(string)
		for _, w := range tc.wants {
			if !strings.Contains(summary, w) {
				t.Fatalf("confirmation card for %s(%s) missing %q in:\n%s", tc.action, tc.args, w, summary)
			}
		}
	}

	// Unready or unknown device must fail before confirmation card is shown.
	if _, err := a.describeAction("mika_run_task", json.RawMessage(`{"goal":"hi","device":"NoTokenPhone"}`)); err == nil {
		t.Fatal("describeAction accepted unready device missing token")
	}
	if _, err := a.describeAction("mika_run_task", json.RawMessage(`{"goal":"hi","device":"GhostPhone"}`)); err == nil {
		t.Fatal("describeAction accepted unknown device")
	}
}

func TestMultiDeviceReceiptIsolation(t *testing.T) {
	f1 := newFakeMikaWith(t, testToken, "0.1.28", sampleTasks())
	f2 := newFakeMikaWith(t, testSecondaryToken, "0.2.1", secondarySampleTasks())
	dir := t.TempDir()

	// Seed a legacy v0.1.0 bare-key receipt to prove upgrade compatibility.
	legacyReceipt := map[string]writeReceipt{
		"legacy-v01-req": {Key: `{"action":"mika_run_task","args":{"goal":"old"}}`, Status: "done", Text: "legacy ok"},
	}
	rawLegacy, _ := json.Marshal(legacyReceipt)
	if err := os.WriteFile(filepath.Join(dir, "write-receipts.json"), rawLegacy, 0600); err != nil {
		t.Fatal(err)
	}

	a, err := initTestApp(t, map[string]any{
		"instance_name":  "A16",
		"instance_url":   f1.srv.URL,
		"secondary_name": "S25+",
		"secondary_url":  f2.srv.URL,
	}, map[string]string{
		"api_token":           testToken,
		"secondary_api_token": testSecondaryToken,
	}, dir)
	if err != nil {
		t.Fatalf("init with legacy write-receipts.json failed: %v", err)
	}

	// Same request_id on primary and secondary must be isolated and both succeed.
	res1, err := a.doAction(t.Context(), "shared-req-1", "mika_run_task", json.RawMessage(`{"goal":"Task on A16","device":"A16"}`))
	if err != nil || !strings.Contains(res1["text"].(string), "A16 (primary)") {
		t.Fatalf("primary action failed: err=%v res=%+v", err, res1)
	}
	res2, err := a.doAction(t.Context(), "shared-req-1", "mika_run_task", json.RawMessage(`{"goal":"Task on S25+","device":"S25+"}`))
	if err != nil || !strings.Contains(res2["text"].(string), "S25+ (secondary)") {
		t.Fatalf("secondary action with same request_id failed: err=%v res=%+v", err, res2)
	}

	// Replaying the same request_id on primary via different aliases ("", "primary", "1", "a16")
	// must hit the cached primary receipt without starting another task.
	for _, args := range []string{
		`{"goal":"Task on A16"}`,
		`{"goal":"Task on A16","device":"primary"}`,
		`{"goal":"Task on A16","device":"1"}`,
		`{"goal":"Task on A16","device":"a16"}`,
	} {
		replayed, err := a.doAction(t.Context(), "shared-req-1", "mika_run_task", json.RawMessage(args))
		if err != nil || replayed["text"] != res1["text"] {
			t.Fatalf("alias replay %s failed: err=%v got=%v want=%v", args, err, replayed, res1)
		}
	}

	// Reusing shared-req-1 on primary with different goal must be refused.
	if _, err := a.doAction(t.Context(), "shared-req-1", "mika_run_task", json.RawMessage(`{"goal":"Different goal","device":"A16"}`)); err == nil {
		t.Fatal("accepted same (device_slot, request_id) with different goal")
	}

	f1.mu.Lock()
	c1 := f1.startCount
	f1.mu.Unlock()
	f2.mu.Lock()
	c2 := f2.startCount
	f2.mu.Unlock()
	if c1 != 1 || c2 != 1 {
		t.Fatalf("expected 1 start per device, got primary=%d secondary=%d", c1, c2)
	}
}

func TestMultiDeviceAllTokenScrubbing(t *testing.T) {
	f1 := newFakeMikaWith(t, testToken, "0.1.28", sampleTasks())
	f2 := newFakeMikaWith(t, testSecondaryToken, "0.2.1", secondarySampleTasks())
	dir := t.TempDir()

	// Hostile peer 1 echoes ALL THREE configured tokens (including secondary + unconfigured tertiary).
	f1.mu.Lock()
	f1.forceStatus = http.StatusUnauthorized
	f1.forceErrCode = "access_denied"
	f1.forceErrMsg = "leaked " + testToken + " and " + testSecondaryToken + " and " + testTertiaryToken
	f1.mu.Unlock()

	a, err := initTestApp(t, map[string]any{
		"instance_name":  "A16",
		"instance_url":   f1.srv.URL,
		"secondary_name": "S25+",
		"secondary_url":  f2.srv.URL,
		"tertiary_name":  "Spare",
	}, map[string]string{
		"api_token":           testToken,
		"secondary_api_token": testSecondaryToken,
		"tertiary_api_token":  testTertiaryToken,
	}, dir)
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	allSecrets := []string{testToken, testSecondaryToken, testTertiaryToken}
	assertNoSecret := func(label, s string) {
		t.Helper()
		for _, sec := range allSecrets {
			if strings.Contains(s, sec) {
				t.Fatalf("%s leaked token %q in: %s", label, sec, s)
			}
		}
	}

	if _, err := a.callTool(t.Context(), "mika_doctor", json.RawMessage(`{"device":"A16"}`)); err == nil {
		t.Fatal("expected error from hostile peer 1")
	} else {
		assertNoSecret("single mika_doctor error", err.Error())
	}

	if summary, err := a.callTool(t.Context(), "mika_doctor", nil); err != nil {
		t.Fatalf("multi mika_doctor failed: %v", err)
	} else {
		assertNoSecret("multi mika_doctor summary", summary)
	}

	if listText, err := a.callTool(t.Context(), "mika_list_devices", nil); err != nil {
		t.Fatalf("mika_list_devices failed: %v", err)
	} else {
		assertNoSecret("mika_list_devices", listText)
	}

	stJSON, _ := json.Marshal(a.status())
	assertNoSecret("connector status", string(stJSON))

	view, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}})
	if err != nil || wire.CheckView(view) != nil {
		t.Fatalf("render failed: err=%v check=%v", err, wire.CheckView(view))
	}
	viewJSON, _ := json.Marshal(view)
	assertNoSecret("UI view", string(viewJSON))

	// Run a valid write action on S25+ and verify write-receipts.json contains no tokens.
	if _, err := a.doAction(t.Context(), "scrub-act-1", "mika_run_task", json.RawMessage(`{"goal":"Check notes","device":"S25+"}`)); err != nil {
		t.Fatalf("S25+ doAction failed: %v", err)
	}
	rawReceipts, err := os.ReadFile(filepath.Join(dir, "write-receipts.json"))
	if err != nil {
		t.Fatalf("read receipts failed: %v", err)
	}
	assertNoSecret("write-receipts.json", string(rawReceipts))
}

func TestMultiDeviceUIViewsAndSwitching(t *testing.T) {
	f1 := newFakeMikaWith(t, testToken, "0.1.28", sampleTasks())
	f2 := newFakeMikaWith(t, testSecondaryToken, "0.2.1", secondarySampleTasks())

	// Verify single-device UI remains concise (no segmented switcher or Devices section).
	singleApp := f1.app()
	singleView, err := singleApp.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}})
	if err != nil || wire.CheckView(singleView) != nil {
		t.Fatalf("single-device view invalid: err=%v check=%v", err, wire.CheckView(singleView))
	}
	if len(singleView.Sections) != 2 {
		t.Fatalf("single-device view should have 2 concise sections, got %d", len(singleView.Sections))
	}

	// Multi-device: A16 (online), Samsung Galaxy S25 Ultra (>12 chars, online), Spare (missing_token).
	a, err := initTestApp(t, map[string]any{
		"instance_name":  "A16",
		"instance_url":   f1.srv.URL,
		"secondary_name": "Samsung Galaxy S25 Ultra",
		"secondary_url":  f2.srv.URL,
		"tertiary_name":  "SparePhone",
		"tertiary_url":   "http://100.64.2.5:8765",
	}, map[string]string{
		"api_token":           testToken,
		"secondary_api_token": testSecondaryToken,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Validate across UI versions 1, 2, 3, 4, 6 and all locales (including long >12-char device name).
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, uiVer := range []int{1, 2, 3, 4, 6} {
			req := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: uiVer, TZ: "Asia/Shanghai"}}
			v, err := a.render(t.Context(), req)
			if err != nil || wire.CheckView(v) != nil {
				t.Fatalf("%s v%d multi-device overview invalid: err=%v check=%v", locale, uiVer, err, wire.CheckView(v))
			}
			hasSegmented := false
			for _, s := range v.Sections {
				for _, item := range s.Items {
					if item.Type == wire.ComponentSegmented {
						hasSegmented = true
					}
				}
			}
			if uiVer >= 2 && !hasSegmented {
				t.Fatalf("%s v%d missing segmented device picker", locale, uiVer)
			}
			if uiVer == 1 && hasSegmented {
				t.Fatalf("%s v1 should not emit segmented component", locale)
			}
		}
	}

	// Initial zh-Hans view shows all 3 devices (online + missing_token) and A16's tasks.
	ov, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}})
	if err != nil {
		t.Fatal(err)
	}
	ovJSON, _ := json.Marshal(ov)
	for _, want := range []string{"设备列表", "A16", "Samsung Galaxy S25 Ultra", "SparePhone", "配置不完整", "task-done-1"} {
		if !strings.Contains(string(ovJSON), want) {
			t.Fatalf("multi-device overview missing %q in %s", want, ovJSON)
		}
	}

	// Switch to secondary device via action "device".
	swRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "device", State: ov.State,
		Values: map[string]string{"device": slotSecondary},
	}})
	if err != nil || swRes.View == nil || wire.CheckView(swRes.View) != nil {
		t.Fatalf("switch to secondary failed: err=%v check=%v", err, wire.CheckView(swRes.View))
	}
	swJSON, _ := json.Marshal(swRes.View)
	for _, want := range []string{"0.2.1", "s25-task-done-1", "Check S25+ battery report"} {
		if !strings.Contains(string(swJSON), want) {
			t.Fatalf("switched secondary view missing %q in %s", want, swJSON)
		}
	}
	if strings.Contains(string(swJSON), "task-done-1") && !strings.Contains(string(swJSON), "s25-task-done-1") {
		t.Fatalf("switched secondary view still showed primary task: %s", swJSON)
	}

	// Row action "device:<slot>" works even when client omits Action.Values or sends stale segmented value.
	for _, vals := range []map[string]string{nil, {"device": slotPrimary}} {
		rowSw, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "zh-Hans", UIVersion: 6, Action: "device:" + slotSecondary, State: ov.State,
			Values: vals,
		}})
		if err != nil || rowSw.View == nil || wire.CheckView(rowSw.View) != nil {
			t.Fatalf("row switch device:secondary (vals=%v) failed: err=%v check=%v", vals, err, wire.CheckView(rowSw.View))
		}
		if b, _ := json.Marshal(rowSw.View); !strings.Contains(string(b), "s25-task-done-1") {
			t.Fatalf("row switch device:secondary (vals=%v) did not select secondary: %s", vals, b)
		}
	}

	// Row action "detail:<slot>:<task_id>" works even when client omits Action.Values.
	rowDet, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "detail:" + slotSecondary + ":s25-task-done-1", State: ov.State,
	}})
	if err != nil || rowDet.View == nil || wire.CheckView(rowDet.View) != nil {
		t.Fatalf("row detail action failed: err=%v check=%v", err, wire.CheckView(rowDet.View))
	}
	if b, _ := json.Marshal(rowDet.View); !strings.Contains(string(b), "S25+ battery is at 88%") {
		t.Fatalf("row detail view missing secondary answer: %s", b)
	}

	// Refresh preserves secondary device selection.
	refRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "refresh", State: swRes.View.State,
	}})
	if err != nil || refRes.View == nil || wire.CheckView(refRes.View) != nil {
		t.Fatalf("refresh on secondary failed: err=%v check=%v", err, wire.CheckView(refRes.View))
	}
	if b, _ := json.Marshal(refRes.View); !strings.Contains(string(b), "s25-task-done-1") {
		t.Fatalf("refresh lost secondary device selection: %s", b)
	}

	// Task detail opens S25+'s task and shows device header, then Back returns to S25+ overview.
	detRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "detail", State: refRes.View.State,
		Values: map[string]string{"task_id": "s25-task-done-1", "device": slotSecondary},
	}})
	if err != nil || detRes.View == nil || wire.CheckView(detRes.View) != nil {
		t.Fatalf("detail on secondary failed: err=%v check=%v", err, wire.CheckView(detRes.View))
	}
	if b, _ := json.Marshal(detRes.View); !strings.Contains(string(b), "S25+ battery is at 88%") || !strings.Contains(string(b), "Samsung Galaxy S25 Ultra") {
		t.Fatalf("secondary detail view missing expected content: %s", b)
	}

	backRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "back", State: detRes.View.State,
	}})
	if err != nil || backRes.View == nil || wire.CheckView(backRes.View) != nil {
		t.Fatalf("back on secondary failed: err=%v check=%v", err, wire.CheckView(backRes.View))
	}
	if b, _ := json.Marshal(backRes.View); !strings.Contains(string(b), "s25-task-done-1") {
		t.Fatalf("back from secondary detail lost selected device: %s", b)
	}

	// Switching to tertiary (missing_token) renders cleanly with config error badge and no crash.
	terRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "device", State: backRes.View.State,
		Values: map[string]string{"device": slotTertiary},
	}})
	if err != nil || terRes.View == nil || wire.CheckView(terRes.View) != nil {
		t.Fatalf("switch to unready tertiary failed: err=%v check=%v", err, wire.CheckView(terRes.View))
	}
	if b, _ := json.Marshal(terRes.View); !strings.Contains(string(b), "tertiary_api_token") || !strings.Contains(string(b), "100.64.*.*:8765") {
		t.Fatalf("unready tertiary view missing config error or masked URL: %s", b)
	}

	// Single-device missing_token renders clean config error guidance view.
	noTokApp, err := initTestApp(t, map[string]any{
		"instance_name": "A16",
		"instance_url":  "http://192.168.1.99:8765",
	}, map[string]string{}, t.TempDir())
	if err != nil {
		t.Fatalf("single-device missing_token init failed: %v", err)
	}
	if noTokApp.status()["state"] != "not_configured" {
		t.Fatalf("expected not_configured status, got %+v", noTokApp.status())
	}
	noTokView, err := noTokApp.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}})
	if err != nil || wire.CheckView(noTokView) != nil {
		t.Fatalf("single-device missing_token view invalid: err=%v check=%v", err, wire.CheckView(noTokView))
	}
	if b, _ := json.Marshal(noTokView); !strings.Contains(string(b), "api_token") || !strings.Contains(string(b), "192.168.*.*:8765") || strings.Contains(string(b), "192.168.1.99") {
		t.Fatalf("single-device missing_token view unexpected: %s", b)
	}
}

func TestConformanceMultiDevice(t *testing.T) {
	f1 := newFakeMikaWith(t, testToken, "0.1.28", sampleTasks())
	f2 := newFakeMikaWith(t, testSecondaryToken, "0.2.1", secondarySampleTasks())
	a, err := initTestApp(t, map[string]any{
		"instance_name":  "A16",
		"instance_url":   f1.srv.URL,
		"secondary_name": "S25+",
		"secondary_url":  f2.srv.URL,
	}, map[string]string{
		"api_token":           testToken,
		"secondary_api_token": testSecondaryToken,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("multi-device init failed: %v", err)
	}
	exttest.Check(t, manifest, exttest.Serve(t, program(a)))
}
