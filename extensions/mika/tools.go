package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

var taskIDSchema = map[string]any{
	"type":      "string",
	"minLength": 1,
	"maxLength": 128,
}

var deviceSchema = map[string]any{
	"type":        "string",
	"minLength":   1,
	"maxLength":   maxDeviceSel,
	"description": "Optional target phone name (case-insensitive, e.g. A16, S25+) or slot alias (primary/secondary/tertiary or 1/2/3). Omit to use the primary device (slot 1).",
}

var readTools = []tool{
	{
		Name:        "mika_list_devices",
		Description: "List all configured Mika Android devices with their slot, custom name, masked URL, configuration status and last known health (read-only).",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	},
	{
		Name:        "mika_doctor",
		Description: "Check Mika server reachability, API/server versions, capabilities, limits and phone helper readiness (read-only; does not invoke the model or start a task). When device is omitted and multiple devices are configured, summarizes all configured devices; pass device to check one specific device.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"device": deviceSchema,
			},
		},
	},
	{
		Name:        "mika_device_state",
		Description: "Read phone screen, lock screen, helper availability and remote-unlock toggle state via Mika GET /api/v1/device (read-only; never wakes or unlocks the phone or returns credentials).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"device": deviceSchema,
			},
		},
	},
	{
		Name:        "mika_task_status",
		Description: "Read the current status, step, progress, open question, phone takeover notice, steers, output files and final answer of a remote Mika task.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"task_id"},
			"properties": map[string]any{
				"task_id": taskIDSchema,
				"device":  deviceSchema,
			},
		},
	},
	{
		Name:        "mika_wait_task",
		Description: "Wait up to timeout_s (1-50s, default from extension settings) for a Mika task's status to change or finish. Returns current status and changed flag; timeout without change is not a failure.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"task_id"},
			"properties": map[string]any{
				"task_id":   taskIDSchema,
				"timeout_s": map[string]any{"type": "integer", "minimum": minWaitSeconds, "maximum": maxWaitSeconds},
				"device":    deviceSchema,
			},
		},
	},
	{
		Name:        "mika_list_tasks",
		Description: "List recent remote tasks submitted to Mika (newest first, limit 1-50, default 10, optional before pagination cursor).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": maxListLimit},
				"before": map[string]any{"type": "string", "minLength": 1, "maxLength": 256},
				"device": deviceSchema,
			},
		},
	},
	{
		Name:        "mika_export_task",
		Description: "Export the redacted run record (header, summary and events without screenshot bytes) for a remote Mika task ID or run ID.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"task_id"},
			"properties": map[string]any{
				"task_id":    taskIDSchema,
				"max_events": map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
				"device":     deviceSchema,
			},
		},
	},
}

var actionTools = []tool{
	{
		Name:        "mika_run_task",
		Description: "Start a remote task on the target Android phone through Mika after explicit confirmation. Payments, logins and takeover confirmations always stay on the phone.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"goal"},
			"properties": map[string]any{
				"goal":            map[string]any{"type": "string", "minLength": 1, "maxLength": maxGoalChars},
				"conversation_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
				"request_id":      map[string]any{"type": "string", "minLength": 36, "maxLength": 36},
				"device":          deviceSchema,
			},
		},
	},
	{
		Name:        "mika_answer",
		Description: "Answer Mika's currently open question for a task in waiting_for_answer state after explicit confirmation. Cannot approve payments or phone takeovers.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"task_id", "text"},
			"properties": map[string]any{
				"task_id": taskIDSchema,
				"text":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxAnswerChars},
				"device":  deviceSchema,
			},
		},
	},
	{
		Name:        "mika_steer_task",
		Description: "Append follow-up instructions to a currently running Mika task after explicit confirmation, keeping the same task_id and context.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"task_id", "text"},
			"properties": map[string]any{
				"task_id": taskIDSchema,
				"text":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxSteerChars},
				"device":  deviceSchema,
			},
		},
	},
	{
		Name:        "mika_cancel_task",
		Description: "Request cancellation of an active remote Mika task after explicit confirmation. Confirm completion by polling mika_task_status until cancelled.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"task_id"},
			"properties": map[string]any{
				"task_id": taskIDSchema,
				"device":  deviceSchema,
			},
		},
	},
}

func decode(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if dec.More() {
		return errors.New("invalid arguments: trailing JSON value")
	}
	return nil
}

type doctorProbeResult struct {
	entry  deviceEntry
	info   discoveryInfo
	dev    *deviceResponse
	devErr error
	err    error
}

func (a *app) probeDeviceDoctor(ctx context.Context, d deviceEntry) doctorProbeResult {
	res := doctorProbeResult{entry: d}
	if !d.ready() {
		return res
	}
	info, err := d.client.discover(ctx)
	a.recordDevice(d.slot, err)
	if err != nil {
		res.err = err
		return res
	}
	res.info = info
	if slices.Contains(info.Capabilities, "device_state") {
		if dev, derr := d.client.deviceState(ctx); derr == nil {
			res.dev = &dev
		} else {
			res.devErr = derr
		}
	}
	return res
}

func (a *app) callTool(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	st := a.snapshot()
	switch name {
	case "mika_list_devices":
		var empty struct{}
		if err := decode(raw, &empty); err != nil {
			return "", err
		}
		return clipText(formatDeviceList(st), 90000), nil

	case "mika_doctor":
		var in struct {
			Device string `json:"device"`
		}
		if err := decode(raw, &in); err != nil {
			return "", err
		}
		sel, err := validDeviceSelector(in.Device)
		if err != nil {
			return "", err
		}
		// When device is omitted and multiple devices are configured, summarize every configured slot.
		if sel == "" && len(st.devices) > 1 {
			results := make([]doctorProbeResult, len(st.devices))
			var wg sync.WaitGroup
			for i, d := range st.devices {
				wg.Add(1)
				go func(idx int, entry deviceEntry) {
					defer wg.Done()
					results[idx] = a.probeDeviceDoctor(ctx, entry)
				}(i, d)
			}
			wg.Wait()
			return clipText(formatMultiDoctor(st, results), 90000), nil
		}
		dev, err := st.resolveReadyDevice(sel)
		if err != nil {
			return "", err
		}
		res := a.probeDeviceDoctor(ctx, dev)
		if res.err != nil {
			return "", res.err
		}
		return clipText(formatDoctor(dev, res.info, res.dev, res.devErr, st.allTokens), 90000), nil

	case "mika_device_state":
		var in struct {
			Device string `json:"device"`
		}
		if err := decode(raw, &in); err != nil {
			return "", err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return "", err
		}
		resp, err := dev.client.deviceState(ctx)
		a.recordDevice(dev.slot, err)
		if err != nil {
			return "", err
		}
		return clipText(formatDeviceHeader(dev)+formatDeviceState(resp), 90000), nil

	case "mika_task_status":
		var in struct {
			TaskID string `json:"task_id"`
			Device string `json:"device"`
		}
		if err := decode(raw, &in); err != nil {
			return "", err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return "", err
		}
		task, err := dev.client.taskStatus(ctx, strings.TrimSpace(in.TaskID))
		a.recordDevice(dev.slot, err)
		if err != nil {
			return "", err
		}
		return clipText(formatDeviceHeader(dev)+formatTask(task, true), 90000), nil

	case "mika_wait_task":
		var in struct {
			TaskID   string `json:"task_id"`
			TimeoutS *int   `json:"timeout_s"`
			Device   string `json:"device"`
		}
		if err := decode(raw, &in); err != nil {
			return "", err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return "", err
		}
		waitS := st.defaultWait
		if in.TimeoutS != nil {
			waitS = *in.TimeoutS
		}
		task, err := dev.client.waitTask(ctx, strings.TrimSpace(in.TaskID), waitS)
		a.recordDevice(dev.slot, err)
		if err != nil {
			return "", err
		}
		return clipText(formatDeviceHeader(dev)+formatTask(task, true), 90000), nil

	case "mika_list_tasks":
		var in struct {
			Limit  *int   `json:"limit"`
			Before string `json:"before"`
			Device string `json:"device"`
		}
		if err := decode(raw, &in); err != nil {
			return "", err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return "", err
		}
		limit := defaultListLimit
		if in.Limit != nil {
			limit = *in.Limit
		}
		res, err := dev.client.listTasks(ctx, limit, strings.TrimSpace(in.Before))
		a.recordDevice(dev.slot, err)
		if err != nil {
			return "", err
		}
		return clipText(formatDeviceHeader(dev)+formatTaskList(res), 90000), nil

	case "mika_export_task":
		var in struct {
			TaskID    string `json:"task_id"`
			MaxEvents *int   `json:"max_events"`
			Device    string `json:"device"`
		}
		if err := decode(raw, &in); err != nil {
			return "", err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return "", err
		}
		maxEvents := 50
		if in.MaxEvents != nil {
			if *in.MaxEvents < 1 || *in.MaxEvents > 200 {
				return "", errors.New("max_events must be between 1 and 200")
			}
			maxEvents = *in.MaxEvents
		}
		rec, err := dev.client.exportTask(ctx, strings.TrimSpace(in.TaskID))
		a.recordDevice(dev.slot, err)
		if err != nil {
			return "", err
		}
		return clipText(formatDeviceHeader(dev)+formatExport(rec, maxEvents), 90000), nil
	}
	return "", fmt.Errorf("no tool %s", name)
}

func formatDeviceHeader(d deviceEntry) string {
	return fmt.Sprintf("Device: %s (%s) | Instance: %s\n", d.name, d.slot, d.maskedURL)
}

func formatDeviceList(st appState) string {
	if len(st.devices) == 0 {
		return "No Mika devices configured. Set instance_url and api_token in Settings › Extensions › Mika.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Configured Mika devices (%d):\n", len(st.devices))
	var dupNames []string
	for _, d := range st.devices {
		slotTag := fmt.Sprintf("%s (%d)", d.slot, d.index)
		if d.isPrimary() {
			slotTag = fmt.Sprintf("%s (%d, default)", d.slot, d.index)
		}
		fmt.Fprintf(&b, "- %s | slot: %s | instance: %s | status: %s\n",
			d.name, slotTag, d.maskedURL, d.configState)
		if d.configErr != "" {
			fmt.Fprintf(&b, "  Config issue: %s\n", clipText(st.scrub(d.configErr), 300))
		}
		if h, ok := st.health[d.slot]; ok {
			if h.lastErr != nil {
				fmt.Fprintf(&b, "  Last error: %s\n", clipText(st.scrub(h.lastErr.Error()), 200))
			} else if !h.lastOK.IsZero() {
				fmt.Fprintf(&b, "  Last check: OK at %s\n", h.lastOK.UTC().Format(time.RFC3339))
			}
		}
		if st.hasDuplicateName(d.name) && !slices.ContainsFunc(dupNames, func(s string) bool { return strings.EqualFold(s, d.name) }) {
			dupNames = append(dupNames, d.name)
		}
	}
	if len(dupNames) > 0 {
		fmt.Fprintf(&b, "\nWarning: duplicate device name(s) detected (%s). Address devices by slot alias (primary/secondary/tertiary or 1/2/3) or assign unique names in Settings › Extensions.\n",
			strings.Join(dupNames, ", "))
	}
	b.WriteString("\nPass optional device=\"<name>\" or slot alias (primary/secondary/tertiary, 1/2/3) to any mika_* tool; omit device to use slot 1 (primary).\n")
	return b.String()
}

func formatMultiDoctor(st appState, results []doctorProbeResult) string {
	okCount := 0
	for _, r := range results {
		if r.entry.ready() && r.err == nil {
			okCount++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Mika Doctor Summary: %d/%d devices reachable\n", okCount, len(results))
	var dupNames []string
	for _, d := range st.devices {
		if st.hasDuplicateName(d.name) && !slices.ContainsFunc(dupNames, func(s string) bool { return strings.EqualFold(s, d.name) }) {
			dupNames = append(dupNames, d.name)
		}
	}
	if len(dupNames) > 0 {
		fmt.Fprintf(&b, "Warning: duplicate device name(s) (%s); use slot aliases (primary/secondary/tertiary or 1/2/3) or rename devices in Settings › Extensions.\n",
			strings.Join(dupNames, ", "))
	}
	for _, r := range results {
		slotTag := r.entry.slot
		if r.entry.isPrimary() {
			slotTag += ", default"
		}
		fmt.Fprintf(&b, "\n=== Device: %s (%s) ===\n", r.entry.name, slotTag)
		switch {
		case !r.entry.ready():
			fmt.Fprintf(&b, "Mika Doctor: NOT READY (%s)\n", r.entry.configState)
			fmt.Fprintf(&b, "Instance: %s\n", r.entry.maskedURL)
			if r.entry.configErr != "" {
				fmt.Fprintf(&b, "Error: %s\n", clipText(st.scrub(r.entry.configErr), 300))
			}
		case r.err != nil:
			b.WriteString("Mika Doctor: ERROR\n")
			fmt.Fprintf(&b, "Instance: %s\n", r.entry.maskedURL)
			fmt.Fprintf(&b, "Error: %s\n", clipText(st.scrub(r.err.Error()), 300))
		default:
			b.WriteString(formatDoctor(r.entry, r.info, r.dev, r.devErr, st.allTokens))
		}
	}
	return b.String()
}

func formatDoctor(d deviceEntry, info discoveryInfo, dev *deviceResponse, devErr error, allTokens []string) string {
	var b strings.Builder
	b.WriteString("Mika Doctor: OK\n")
	fmt.Fprintf(&b, "Device: %s (%s)\n", d.name, d.slot)
	fmt.Fprintf(&b, "Instance: %s\n", d.maskedURL)
	fmt.Fprintf(&b, "Server version: %s | API version: %d\n", info.ServerVersion, info.APIVersion)
	if len(info.Capabilities) > 0 {
		fmt.Fprintf(&b, "Capabilities: %s\n", strings.Join(info.Capabilities, ", "))
	}
	if info.HistoryEnabled != nil {
		fmt.Fprintf(&b, "History enabled: %t\n", *info.HistoryEnabled)
	}
	fmt.Fprintf(&b, "Limits: max_wait_s=%d, max_goal=%d, max_answer=%d, max_steer=%d\n",
		info.Limits.MaxWaitS, info.Limits.MaxGoal, info.Limits.MaxAnswer, info.Limits.MaxSteer)
	if dev != nil {
		b.WriteString("\n" + formatDeviceState(*dev))
	} else if devErr != nil {
		fmt.Fprintf(&b, "Device state: unavailable (%s)\n", clipText(scrubSecrets(devErr.Error(), allTokens), 200))
	} else {
		b.WriteString("Device state: not advertised by server\n")
	}
	b.WriteString("Model configuration: unverified (doctor does not call the model or start a task)\n")
	return b.String()
}

func formatDeviceState(dev deviceResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Device status: %s\n", dev.Status)
	if dev.Device != nil {
		d := dev.Device
		fmt.Fprintf(&b, "Ready: %t | interactive: %t | device_locked: %t | keyguard_showing: %t | secure: %t | helper_available: %t | user_unlocked: %t\n",
			d.Ready, d.Interactive, d.DeviceLocked, d.KeyguardShowing, d.Secure, d.HelperAvailable, d.UserUnlocked)
	}
	fmt.Fprintf(&b, "Remote wake/unlock setting on phone: enabled=%t, credential_configured=%t, attempt_blocked=%t\n",
		dev.Enabled, dev.CredentialConfigured, dev.AttemptBlocked)
	if dev.Message != "" {
		fmt.Fprintf(&b, "Message: %s\n", clipText(dev.Message, 300))
	}
	if dev.Device != nil && !dev.Device.Ready {
		b.WriteString("Hint: phone is not ready for unattended UI tasks (locked or screen off). Ask the user to unlock the phone directly.\n")
	}
	return b.String()
}

func formatTaskList(res taskListResponse) string {
	if len(res.Tasks) == 0 {
		return "No remote tasks found.\n"
	}
	var b strings.Builder
	for _, item := range res.Tasks {
		b.WriteString(formatTask(item, false))
	}
	if res.NextBefore != "" {
		fmt.Fprintf(&b, "Next before cursor: %s\n", res.NextBefore)
	}
	return b.String()
}

func formatTask(t remoteTask, detail bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- Task %s [%s]\n", t.TaskID, t.Status)
	if t.Goal != "" {
		fmt.Fprintf(&b, "  Goal: %s\n", clipText(t.Goal, 500))
	}
	if t.StartedAtMs > 0 {
		fmt.Fprintf(&b, "  Started: %s\n", time.UnixMilli(t.StartedAtMs).UTC().Format(time.RFC3339))
	}
	if detail {
		if t.Conversation != "" {
			fmt.Fprintf(&b, "  Conversation: %s\n", t.Conversation)
		}
		if t.RunID != "" {
			fmt.Fprintf(&b, "  Run ID: %s\n", t.RunID)
		}
		if t.Changed != nil {
			fmt.Fprintf(&b, "  Changed: %t\n", *t.Changed)
		}
		if t.Step != nil {
			fmt.Fprintf(&b, "  Step: %d\n", *t.Step)
		}
		fmt.Fprintf(&b, "  Answerable: %t\n", t.Answerable)
	}
	if t.Progress != "" {
		fmt.Fprintf(&b, "  Progress: %s\n", clipText(t.Progress, 1000))
	}
	if t.WaitingFor != "" {
		line := "  Waiting for: " + t.WaitingFor
		if t.App != "" {
			line += " (app: " + t.App + ")"
		}
		b.WriteString(line + "\n")
	}
	if t.Question != "" {
		fmt.Fprintf(&b, "  Question: %s\n", clipText(t.Question, 2000))
	}
	if len(t.Options) > 0 {
		fmt.Fprintf(&b, "  Options: %s\n", strings.Join(t.Options, " | "))
	}
	if t.Message != "" {
		fmt.Fprintf(&b, "  Message: %s\n", clipText(t.Message, 1000))
	}
	if t.Note != "" {
		fmt.Fprintf(&b, "  Note: %s\n", clipText(t.Note, 1000))
	}
	if t.Answer != "" {
		limit := 600
		if detail {
			limit = 30000
		}
		fmt.Fprintf(&b, "  Answer: %s\n", clipText(t.Answer, limit))
	}
	if t.Verified != nil {
		fmt.Fprintf(&b, "  Verified: %t\n", *t.Verified)
	}
	if t.VerificationNote != "" {
		fmt.Fprintf(&b, "  Verification note: %s\n", clipText(t.VerificationNote, 1000))
	}
	if len(t.Unconfirmed) > 0 {
		fmt.Fprintf(&b, "  Unconfirmed: %s\n", strings.Join(t.Unconfirmed, "; "))
	}
	if t.Reason != "" {
		line := "  Reason: " + clipText(t.Reason, 1000)
		if t.FailureKind != "" {
			line += " (" + t.FailureKind + ")"
		}
		b.WriteString(line + "\n")
	}
	if detail && len(t.Steers) > 0 {
		parts := make([]string, 0, len(t.Steers))
		for _, s := range t.Steers {
			parts = append(parts, s.MessageID+":"+s.State)
		}
		fmt.Fprintf(&b, "  Steers: %s\n", strings.Join(parts, ", "))
	}
	if detail && len(t.Files) > 0 {
		parts := make([]string, 0, len(t.Files))
		for _, f := range t.Files {
			parts = append(parts, fmt.Sprintf("%s (%s, %d bytes)", f.Name, f.ID, f.Size))
		}
		fmt.Fprintf(&b, "  Files: %s\n", strings.Join(parts, ", "))
	}
	if detail {
		if hint := taskNextStepHint(t); hint != "" {
			fmt.Fprintf(&b, "  Next: %s\n", hint)
		}
	}
	return b.String()
}

func taskNextStepHint(t remoteTask) string {
	switch t.Status {
	case "waiting_for_answer":
		return "Mika is waiting for an answer. Relay the question to the user and reply with mika_answer."
	case "waiting_on_phone":
		return "User action is required directly on the Android phone (payment, login, confirmation or takeover). Inform the user, then wait with mika_wait_task or cancel with mika_cancel_task."
	case "paused":
		return "Task is paused (commonly because the phone screen is locked). Ask the user to unlock the phone, then continue waiting."
	case "starting", "running", "queued":
		if t.Changed != nil && !*t.Changed {
			return "Wait window elapsed with no status change; task is still active. Call mika_wait_task again rather than re-submitting."
		}
		return "Task is still in progress. Follow up with mika_wait_task."
	default:
		return ""
	}
}

func formatExport(rec exportRecord, maxEvents int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Mika Run Export (format %d)\n", rec.ExportFormat)
	fmt.Fprintf(&b, "Task ID: %s | Conversation ID: %s | Run ID: %s\n", rec.TaskID, rec.ConversationID, rec.RunID)
	fmt.Fprintf(&b, "Partial: %t | Includes screenshots: %t | Events: %d\n", rec.Partial, rec.IncludesScreenshots, len(rec.Events))
	if len(rec.Run) > 0 && string(rec.Run) != "null" {
		fmt.Fprintf(&b, "\nRun summary:\n%s\n", clipText(string(rec.Run), 8000))
	}
	shown := min(len(rec.Events), maxEvents)
	if shown > 0 {
		fmt.Fprintf(&b, "\nEvents (showing %d of %d):\n", shown, len(rec.Events))
		for i := 0; i < shown; i++ {
			fmt.Fprintf(&b, "%d. %s\n", i+1, clipText(string(rec.Events[i]), 1200))
		}
	}
	if len(rec.Events) > shown {
		fmt.Fprintf(&b, "... (%d more events omitted)\n", len(rec.Events)-shown)
	}
	return b.String()
}
