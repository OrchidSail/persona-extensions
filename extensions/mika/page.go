package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	pageBatchSize    = 20
	maxOverviewPages = 5
	maxViewTasks     = 100
	maxDetailItems   = 240
)

type pageState struct {
	Mode   string `json:"m,omitempty"`
	TaskID string `json:"t,omitempty"`
	Pages  int    `json:"p,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Pages < 0 || st.Pages > maxOverviewPages || len(st.TaskID) > 128 {
		return pageState{}, errors.New("invalid page state")
	}
	if st.Mode != "" && st.Mode != "overview" && st.Mode != "detail" {
		return pageState{}, errors.New("invalid page mode")
	}
	if st.TaskID != "" && validTaskID(st.TaskID) != nil {
		return pageState{}, errors.New("invalid task_id in page state")
	}
	return st, nil
}

func (a *app) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	st, err := readState(r)
	if err != nil {
		return nil, err
	}
	return a.page(ctx, r, st)
}

func (a *app) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	st, err := readState(r)
	if err != nil {
		return nil, err
	}
	switch r.Action {
	case "refresh":
		// Keep current mode and task selection while re-fetching fresh state.
	case "more":
		if st.Pages < 1 {
			st.Pages = 1
		}
		if st.Pages < maxOverviewPages {
			st.Pages++
		}
		st.Mode = "overview"
	case "detail":
		id := strings.TrimSpace(r.Values["task_id"])
		if err := validTaskID(id); err != nil {
			return nil, err
		}
		st.Mode, st.TaskID = "detail", id
	case "back":
		st.Mode, st.TaskID = "overview", ""
	default:
		return nil, fmt.Errorf("unknown page action %q", r.Action)
	}
	v, err := a.page(ctx, r, st)
	if err != nil {
		return nil, err
	}
	return &wire.ActionResult{View: v}, nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	snap := a.snapshot()
	if !snap.client.configured() {
		return &wire.View{
			Title: t.title,
			Sections: []wire.Section{{
				Items: []wire.Component{{
					Type:  wire.ComponentEmpty,
					Title: t.notConfigured,
				}},
			}},
		}, nil
	}
	if st.Mode == "detail" && st.TaskID != "" {
		return a.taskDetailView(ctx, r, st, snap.client, t)
	}
	return a.overviewView(ctx, r, st, snap.client, t)
}

func (a *app) overviewView(ctx context.Context, r *ext.UIRequest, st pageState, c *client, t *texts) (*wire.View, error) {
	if st.Pages < 1 {
		st.Pages = 1
	}
	st.Mode = "overview"
	st.TaskID = ""
	maskedURL := maskInstanceURL(c.base)

	info, err := c.discover(ctx)
	a.record(err)
	if err != nil {
		return &wire.View{
			Title:        t.title,
			State:        ext.EncodeState(st),
			RefreshAfter: 30,
			Sections: []wire.Section{{
				Title: t.connection,
				Items: []wire.Component{
					{
						Type:     wire.ComponentRow,
						Key:      "conn-status",
						Title:    t.serverRow,
						Subtitle: clipText(maskedURL, 1800),
						Badge:    &wire.Badge{Text: t.offline, Tone: wire.ToneDanger},
					},
					{
						Type:  wire.ComponentText,
						Key:   "conn-error",
						Text:  clipText(scrubSecret(err.Error(), c.token), 1800),
						Style: wire.TextStyleSecondary,
					},
					{
						Type:   wire.ComponentButton,
						Key:    "btn-refresh",
						Title:  t.refresh,
						Action: &wire.Action{ID: "refresh"},
					},
				},
			}},
		}, nil
	}

	versionLabel := info.ServerVersion
	if versionLabel != "" && !strings.HasPrefix(versionLabel, "v") {
		versionLabel = "v" + versionLabel
	}
	if versionLabel == "" {
		versionLabel = fmt.Sprintf("API v%d", info.APIVersion)
	} else {
		versionLabel = fmt.Sprintf("%s (API v%d)", versionLabel, info.APIVersion)
	}

	connItems := []wire.Component{{
		Type:     wire.ComponentRow,
		Key:      "conn-status",
		Title:    t.serverRow,
		Subtitle: clipText(maskedURL, 1800),
		Value:    clipText(versionLabel, 200),
		Badge:    &wire.Badge{Text: t.online, Tone: wire.TonePositive},
	}}

	if slices.Contains(info.Capabilities, "device_state") {
		if dev, derr := c.deviceState(ctx); derr == nil {
			connItems = append(connItems, deviceSummaryRow(dev, t))
		} else {
			connItems = append(connItems, wire.Component{
				Type:     wire.ComponentRow,
				Key:      "device-status",
				Title:    t.deviceRow,
				Subtitle: clipText(scrubSecret(derr.Error(), c.token), 1800),
				Badge:    &wire.Badge{Text: t.deviceUnavailable, Tone: wire.ToneNeutral},
			})
		}
	}
	if len(info.Capabilities) > 0 {
		connItems = append(connItems, wire.Component{
			Type:  wire.ComponentText,
			Key:   "server-caps",
			Text:  clipText(t.capabilitiesPrefix+strings.Join(info.Capabilities, ", "), 1800),
			Style: wire.TextStyleSecondary,
		})
	}
	connItems = append(connItems, wire.Component{
		Type:   wire.ComponentButton,
		Key:    "btn-refresh",
		Title:  t.refresh,
		Action: &wire.Action{ID: "refresh"},
	})

	tasks, hasMore, listErr := collectTasks(ctx, c, st.Pages)
	taskSection := wire.Section{Title: t.recentTasks}
	if listErr != nil {
		taskSection.Items = []wire.Component{{
			Type:  wire.ComponentText,
			Key:   "tasks-error",
			Text:  clipText(scrubSecret(listErr.Error(), c.token), 1800),
			Style: wire.TextStyleSecondary,
		}}
	} else if len(tasks) == 0 {
		taskSection.Items = []wire.Component{{
			Type:  wire.ComponentEmpty,
			Key:   "tasks-empty",
			Title: t.noTasks,
		}}
	} else {
		loc := loadLocation(r.TZ)
		seen := make(map[string]bool, len(tasks))
		for _, item := range tasks {
			if item.TaskID == "" || seen[item.TaskID] {
				continue
			}
			seen[item.TaskID] = true
			goal := strings.TrimSpace(item.Goal)
			if goal == "" {
				goal = t.noGoal
			}
			sub := formatTaskSubtitle(item, loc)
			badge := statusBadge(item.Status, t)
			taskSection.Items = append(taskSection.Items, wire.Component{
				Type:     wire.ComponentRow,
				Key:      "task-" + item.TaskID,
				Title:    clipText(goal, 1800),
				Subtitle: clipText(sub, 1800),
				Badge:    &badge,
				Action:   &wire.Action{ID: "detail", Values: map[string]string{"task_id": item.TaskID}},
			})
		}
		if len(taskSection.Items) == 0 {
			taskSection.Items = []wire.Component{{
				Type:  wire.ComponentEmpty,
				Key:   "tasks-empty",
				Title: t.noTasks,
			}}
		}
		if hasMore {
			taskSection.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
		}
	}

	return &wire.View{
		Title:        t.title,
		State:        ext.EncodeState(st),
		RefreshAfter: 30,
		Sections: []wire.Section{
			{Title: t.connection, Items: connItems},
			taskSection,
		},
	}, nil
}

func (a *app) taskDetailView(ctx context.Context, r *ext.UIRequest, st pageState, c *client, t *texts) (*wire.View, error) {
	task, err := c.taskStatus(ctx, st.TaskID)
	a.record(err)
	if err != nil {
		return &wire.View{
			Title: t.taskDetail,
			State: ext.EncodeState(st),
			Sections: []wire.Section{{
				Title: t.taskDetail,
				Items: []wire.Component{
					{Type: wire.ComponentText, Text: clipText(scrubSecret(err.Error(), c.token), 1800), Style: wire.TextStyleSecondary},
					{Type: wire.ComponentButton, Title: t.refresh, Action: &wire.Action{ID: "refresh"}},
					{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
				},
			}},
		}, nil
	}

	loc := loadLocation(r.TZ)
	badge := statusBadge(task.Status, t)
	headItems := []wire.Component{
		{
			Type:     wire.ComponentRow,
			Key:      "detail-status",
			Title:    clipText(fmt.Sprintf(t.taskMetaFmt, task.TaskID, task.Conversation), 1800),
			Subtitle: formatTimestamp(task.StartedAtMs, loc),
			Badge:    &badge,
		},
	}
	if task.RunID != "" {
		headItems = append(headItems, wire.Component{
			Type:  wire.ComponentText,
			Text:  clipText("Run ID: "+task.RunID, 1800),
			Style: wire.TextStyleSecondary,
		})
	}
	headItems = append(headItems,
		wire.Component{Type: wire.ComponentButton, Key: "btn-detail-refresh", Title: t.refresh, Action: &wire.Action{ID: "refresh"}},
		wire.Component{Type: wire.ComponentButton, Key: "btn-detail-back", Title: t.back, Action: &wire.Action{ID: "back"}},
	)

	sections := []wire.Section{{Title: t.taskDetail, Items: headItems}}
	used := len(headItems)

	addTextSection := func(title, body string) {
		body = strings.TrimSpace(body)
		if body == "" || used >= maxDetailItems {
			return
		}
		items := textComponents(body, maxDetailItems-used)
		used += len(items)
		sections = append(sections, wire.Section{Title: title, Items: items})
	}

	addTextSection(t.goalLabel, task.Goal)
	if task.Progress != "" {
		prog := task.Progress
		if task.Step != nil {
			prog = fmt.Sprintf("#%d · %s", *task.Step, prog)
		}
		addTextSection(t.progressLabel, prog)
	}
	if task.Question != "" {
		q := task.Question
		if len(task.Options) > 0 {
			q += "\n" + strings.Join(task.Options, " / ")
		}
		addTextSection(t.questionLabel, q)
	}
	if task.Note != "" {
		addTextSection(t.noteLabel, task.Note)
	}
	if task.Answer != "" {
		ans := task.Answer
		if task.Verified != nil {
			if *task.Verified {
				ans = "[" + t.verifiedYes + "]\n" + ans
			} else {
				ans = "[" + t.verifiedNo + "]\n" + ans
			}
		}
		if task.VerificationNote != "" {
			ans += "\n\n" + task.VerificationNote
		}
		addTextSection(t.answerLabel, ans)
	}
	if task.Reason != "" || task.Message != "" {
		parts := []string{}
		if task.Reason != "" {
			rline := task.Reason
			if task.FailureKind != "" {
				rline += " (" + task.FailureKind + ")"
			}
			parts = append(parts, rline)
		}
		if task.Message != "" {
			parts = append(parts, task.Message)
		}
		addTextSection(t.reasonLabel, strings.Join(parts, "\n"))
	}
	if len(task.Steers) > 0 {
		lines := make([]string, 0, len(task.Steers))
		for _, s := range task.Steers {
			lines = append(lines, s.MessageID+" · "+s.State)
		}
		addTextSection(t.steersLabel, strings.Join(lines, "\n"))
	}
	if len(task.Files) > 0 {
		lines := make([]string, 0, len(task.Files))
		for _, f := range task.Files {
			lines = append(lines, fmt.Sprintf("%s (%d B)", f.Name, f.Size))
		}
		addTextSection(t.filesLabel, strings.Join(lines, "\n"))
	}

	return &wire.View{
		Title:        t.taskDetail,
		State:        ext.EncodeState(st),
		RefreshAfter: 30,
		Sections:     sections,
	}, nil
}

func collectTasks(ctx context.Context, c *client, pages int) ([]remoteTask, bool, error) {
	var out []remoteTask
	before := ""
	hasMore := false
	for p := 0; p < pages && len(out) < maxViewTasks; p++ {
		res, err := c.listTasks(ctx, pageBatchSize, before)
		if err != nil {
			return nil, false, err
		}
		out = append(out, res.Tasks...)
		if res.NextBefore == "" || len(res.Tasks) == 0 {
			hasMore = false
			break
		}
		before = res.NextBefore
		hasMore = true
	}
	if len(out) > maxViewTasks {
		out = out[:maxViewTasks]
	}
	return out, hasMore && len(out) < maxViewTasks && pages < maxOverviewPages, nil
}

func deviceSummaryRow(dev deviceResponse, t *texts) wire.Component {
	row := wire.Component{
		Type:  wire.ComponentRow,
		Key:   "device-status",
		Title: t.deviceRow,
	}
	if dev.Device == nil {
		row.Badge = &wire.Badge{Text: t.deviceUnavailable, Tone: wire.ToneNeutral}
		row.Subtitle = clipText(dev.Status, 200)
		return row
	}
	d := dev.Device
	switch {
	case !d.HelperAvailable:
		row.Badge = &wire.Badge{Text: t.helperOffline, Tone: wire.ToneDanger}
	case d.Ready:
		row.Badge = &wire.Badge{Text: t.ready, Tone: wire.TonePositive}
	default:
		row.Badge = &wire.Badge{Text: t.locked, Tone: wire.ToneCaution}
	}
	row.Subtitle = fmt.Sprintf("interactive=%t · locked=%t · helper=%t", d.Interactive, d.DeviceLocked, d.HelperAvailable)
	return row
}

func statusBadge(status string, t *texts) wire.Badge {
	switch status {
	case "done":
		return wire.Badge{Text: t.statusDone, Tone: wire.TonePositive}
	case "running":
		return wire.Badge{Text: t.statusRunning, Tone: wire.ToneNeutral}
	case "starting":
		return wire.Badge{Text: t.statusStarting, Tone: wire.ToneNeutral}
	case "queued":
		return wire.Badge{Text: t.statusQueued, Tone: wire.ToneNeutral}
	case "paused":
		return wire.Badge{Text: t.statusPaused, Tone: wire.ToneCaution}
	case "waiting_for_answer":
		return wire.Badge{Text: t.statusWaitAnswer, Tone: wire.ToneCaution}
	case "waiting_on_phone":
		return wire.Badge{Text: t.statusWaitPhone, Tone: wire.ToneCaution}
	case "failed":
		return wire.Badge{Text: t.statusFailed, Tone: wire.ToneDanger}
	case "cancelled":
		return wire.Badge{Text: t.statusCancelled, Tone: wire.ToneDanger}
	default:
		if status == "" {
			status = "unknown"
		}
		return wire.Badge{Text: clipText(status, 40), Tone: wire.ToneNeutral}
	}
}

func formatTaskSubtitle(item remoteTask, loc *time.Location) string {
	parts := []string{}
	if ts := formatTimestamp(item.StartedAtMs, loc); ts != "" {
		parts = append(parts, ts)
	}
	summary := firstNonEmpty(item.Answer, item.Question, item.Progress, item.Reason, item.Message, item.Note)
	if summary != "" {
		summary = strings.ReplaceAll(strings.TrimSpace(summary), "\n", " ")
		parts = append(parts, clipText(summary, 240))
	}
	return strings.Join(parts, " · ")
}

func formatTimestamp(ms int64, loc *time.Location) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).In(loc).Format("2006-01-02 15:04")
}

func loadLocation(tz string) *time.Location {
	if tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.UTC
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func textComponents(text string, limit int) []wire.Component {
	if limit <= 0 {
		return nil
	}
	var out []wire.Component
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		r := []rune(line)
		for len(r) > 0 {
			n := min(len(r), 1800)
			out = append(out, wire.Component{Type: wire.ComponentText, Text: string(r[:n])})
			r = r[n:]
			if len(out) >= limit {
				out[len(out)-1].Text = clipText(out[len(out)-1].Text, 1700) + " […]"
				return out
			}
		}
	}
	if len(out) == 0 {
		out = append(out, wire.Component{Type: wire.ComponentText, Text: " ", Style: wire.TextStyleSecondary})
	}
	return out
}
