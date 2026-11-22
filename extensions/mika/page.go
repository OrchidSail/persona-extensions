package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	pageBatchSize        = 20
	maxOverviewPages     = 5
	maxViewTasks         = 100
	maxDetailItems       = 240
	peerProbeTimeout     = 5 * time.Second
	doctorActionTimeout  = 8 * time.Second
	maxProbeConcurrency  = 8
	maxSegmentedDevices  = 5
	maxSegmentedLabel    = 12
	maxOverviewTaskItems = 200
)

type pageState struct {
	Mode   string `json:"m,omitempty"`
	TaskID string `json:"t,omitempty"`
	Pages  int    `json:"p,omitempty"`
	Device string `json:"d,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Pages < 0 || st.Pages > maxOverviewPages || len(st.TaskID) > 128 {
		return pageState{}, errors.New("invalid page state")
	}
	switch st.Mode {
	case "", "overview", "detail", "add_device", "device_edit":
	default:
		return pageState{}, errors.New("invalid page mode")
	}
	if st.TaskID != "" && validTaskID(st.TaskID) != nil {
		return pageState{}, errors.New("invalid task_id in page state")
	}
	if st.Device != "" && !validDeviceID(st.Device) {
		return pageState{}, errors.New("invalid device id in page state")
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

func resolvePageDeviceID(snap appState, raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if d, ok := snap.deviceByID(raw); ok {
		return d.id, true
	}
	if d, err := snap.resolveDevice(raw); err == nil {
		return d.id, true
	}
	return "", false
}

func (a *app) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	st, err := readState(r)
	if err != nil {
		return nil, err
	}
	t := textsFor(r.Locale)
	snap := a.snapshot()

	switch {
	case r.Action == "refresh":
		// Keep current mode, device, and task selection while re-fetching fresh state.

	case r.Action == "add_device":
		st.Mode = "add_device"
		st.TaskID = ""

	case r.Action == "edit" || strings.HasPrefix(r.Action, "edit:"):
		id := strings.TrimSpace(strings.TrimPrefix(r.Action, "edit:"))
		if r.Action == "edit" || id == "" {
			id = strings.TrimSpace(r.Values["device"])
		}
		if id == "" {
			id = st.Device
		}
		resolvedID, ok := resolvePageDeviceID(snap, id)
		if !ok {
			return &wire.ActionResult{Toast: t.deviceNotFound}, nil
		}
		st.Mode = "device_edit"
		st.Device = resolvedID
		st.TaskID = ""

	case r.Action == "save_add":
		name := r.Values["name"]
		rawURL := r.Values["url"]
		token := r.Values["token"]
		makeDefault := parseBoolValue(r.Values["default"])
		dev, addErr := a.addDevice(ctx, name, rawURL, token, makeDefault)
		if addErr != nil {
			scrubbed := scrubSecrets(addErr.Error(), collectScrubTokens(token, a.snapshot().allTokens...))
			return &wire.ActionResult{Toast: clipText(scrubbed, 300)}, nil
		}
		return &wire.ActionResult{
			Toast: fmt.Sprintf(t.deviceAddedFmt, dev.name),
			Pop:   true,
		}, nil

	case r.Action == "save_edit" || strings.HasPrefix(r.Action, "save_edit:"):
		id := ""
		if strings.HasPrefix(r.Action, "save_edit:") {
			id = strings.TrimSpace(strings.TrimPrefix(r.Action, "save_edit:"))
		}
		if id == "" {
			id = st.Device
		}
		if id == "" {
			id = strings.TrimSpace(r.Values["device"])
		}
		if !validDeviceID(id) {
			return &wire.ActionResult{Toast: t.deviceNotFound}, nil
		}
		name := r.Values["name"]
		rawURL := r.Values["url"]
		token := r.Values["token"]
		makeDefault := parseBoolValue(r.Values["default"])
		dev, editErr := a.editDevice(ctx, id, name, rawURL, token, makeDefault)
		if editErr != nil {
			scrubbed := scrubSecrets(editErr.Error(), collectScrubTokens(token, a.snapshot().allTokens...))
			return &wire.ActionResult{Toast: clipText(scrubbed, 300)}, nil
		}
		return &wire.ActionResult{
			Toast: fmt.Sprintf(t.deviceSavedFmt, dev.name),
			Pop:   true,
		}, nil

	case r.Action == "set_default" || strings.HasPrefix(r.Action, "set_default:"):
		id := ""
		if strings.HasPrefix(r.Action, "set_default:") {
			id = strings.TrimSpace(strings.TrimPrefix(r.Action, "set_default:"))
		}
		if id == "" {
			id = strings.TrimSpace(r.Values["device"])
		}
		if id == "" {
			id = st.Device
		}
		dev, defErr := a.setDefaultDevice(ctx, id)
		if defErr != nil {
			return &wire.ActionResult{Toast: clipText(a.snapshot().scrub(defErr.Error()), 300)}, nil
		}
		st.Device = dev.id
		st.Mode = "overview"
		st.TaskID = ""
		st.Pages = 1
		v, perr := a.page(ctx, r, st)
		if perr != nil {
			return nil, perr
		}
		return &wire.ActionResult{
			View:  v,
			Toast: fmt.Sprintf(t.deviceDefaultFmt, dev.name),
		}, nil

	case r.Action == "test" || strings.HasPrefix(r.Action, "test:"):
		id := ""
		if strings.HasPrefix(r.Action, "test:") {
			id = strings.TrimSpace(strings.TrimPrefix(r.Action, "test:"))
		}
		if id == "" {
			id = strings.TrimSpace(r.Values["device"])
		}
		if id == "" {
			id = st.Device
		}
		resolvedID, ok := resolvePageDeviceID(snap, id)
		if !ok {
			return &wire.ActionResult{Toast: t.deviceNotFound}, nil
		}
		dev, _ := snap.deviceByID(resolvedID)
		toast := a.testDeviceConn(ctx, dev, snap, t)
		if st.Mode == "device_edit" {
			return &wire.ActionResult{Toast: clipText(toast, 300)}, nil
		}
		if st.Device == "" {
			st.Device = dev.id
		}
		v, perr := a.page(ctx, r, st)
		if perr != nil {
			return nil, perr
		}
		return &wire.ActionResult{View: v, Toast: clipText(toast, 300)}, nil

	case r.Action == "delete" || strings.HasPrefix(r.Action, "delete:"):
		id := ""
		if strings.HasPrefix(r.Action, "delete:") {
			id = strings.TrimSpace(strings.TrimPrefix(r.Action, "delete:"))
		}
		if id == "" {
			id = strings.TrimSpace(r.Values["device"])
		}
		if id == "" {
			id = st.Device
		}
		removedName, nextID, delErr := a.deleteDevice(ctx, id)
		if delErr != nil {
			return &wire.ActionResult{Toast: clipText(a.snapshot().scrub(delErr.Error()), 300)}, nil
		}
		toast := fmt.Sprintf(t.deviceDeletedFmt, removedName)
		if st.Mode == "device_edit" {
			return &wire.ActionResult{Toast: toast, Pop: true}, nil
		}
		st.Device = nextID
		st.Mode = "overview"
		st.TaskID = ""
		st.Pages = 1
		v, perr := a.page(ctx, r, st)
		if perr != nil {
			return nil, perr
		}
		return &wire.ActionResult{View: v, Toast: toast}, nil

	case r.Action == "device" || strings.HasPrefix(r.Action, "device:"):
		var rawID string
		if strings.HasPrefix(r.Action, "device:") {
			rawID = strings.TrimSpace(strings.TrimPrefix(r.Action, "device:"))
		}
		if rawID == "" {
			rawID = strings.TrimSpace(r.Values["device"])
		}
		resolvedID, ok := resolvePageDeviceID(snap, rawID)
		if !ok {
			return nil, fmt.Errorf("unknown device %q", snap.scrub(rawID))
		}
		st.Device = resolvedID
		st.Mode = "overview"
		st.TaskID = ""
		st.Pages = 1

	case r.Action == "more":
		if st.Pages < 1 {
			st.Pages = 1
		}
		if st.Pages < maxOverviewPages {
			st.Pages++
		}
		st.Mode = "overview"

	case r.Action == "detail" || strings.HasPrefix(r.Action, "detail:"):
		var id, devID string
		if strings.HasPrefix(r.Action, "detail:") {
			rest := strings.TrimPrefix(r.Action, "detail:")
			if pDev, pID, ok := strings.Cut(rest, ":"); ok && validDeviceID(pDev) && pID != "" {
				devID = pDev
				id = pID
			} else {
				id = strings.TrimSpace(rest)
			}
		}
		if id == "" {
			id = strings.TrimSpace(r.Values["task_id"])
		}
		if devID == "" {
			devID = strings.TrimSpace(r.Values["device"])
		}
		if err := validTaskID(id); err != nil {
			return nil, err
		}
		if devID != "" {
			resolvedID, ok := resolvePageDeviceID(snap, devID)
			if !ok {
				return nil, fmt.Errorf("unknown device %q", snap.scrub(devID))
			}
			st.Device = resolvedID
		}
		st.Mode, st.TaskID = "detail", id

	case r.Action == "back":
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

func (a *app) testDeviceConn(ctx context.Context, dev deviceEntry, snap appState, t *texts) string {
	if !dev.ready() {
		reason := snap.scrub(dev.configErr)
		if reason == "" {
			reason = t.notConfigured
		}
		return fmt.Sprintf(t.testFailFmt, dev.name, reason)
	}
	probeCtx, cancel := context.WithTimeout(ctx, doctorActionTimeout)
	defer cancel()
	info, err := dev.client.discover(probeCtx)
	a.recordDevice(dev.id, err)
	if err != nil {
		return fmt.Sprintf(t.testFailFmt, dev.name, snap.scrub(err.Error()))
	}
	phoneStatus := t.ready
	if slices.Contains(info.Capabilities, "device_state") {
		if ds, derr := dev.client.deviceState(probeCtx); derr == nil && ds.Device != nil {
			switch {
			case !ds.Device.HelperAvailable:
				phoneStatus = t.helperOffline
			case ds.Device.Ready:
				phoneStatus = t.ready
			default:
				phoneStatus = t.locked
			}
		} else {
			phoneStatus = t.deviceUnavailable
		}
	}
	return fmt.Sprintf(t.testOKFmt, dev.name, formatVersionLabel(info), phoneStatus)
}

func selectPageDevice(snap appState, requestedID string) (deviceEntry, bool) {
	if len(snap.devices) == 0 {
		return deviceEntry{}, false
	}
	if requestedID != "" {
		if d, ok := snap.deviceByID(requestedID); ok {
			return d, true
		}
	}
	if def, ok := snap.defaultDevice(); ok && def.ready() {
		return def, true
	}
	for _, d := range snap.devices {
		if d.ready() {
			return d, true
		}
	}
	return snap.devices[0], true
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	snap := a.snapshot()

	if st.Mode == "add_device" {
		return a.addDeviceView(r, snap, t), nil
	}
	if st.Mode == "device_edit" {
		return a.editDeviceView(r, st, snap, t), nil
	}

	sel, ok := selectPageDevice(snap, st.Device)
	if !ok {
		return &wire.View{
			Title: t.title,
			Sections: []wire.Section{{
				Items: []wire.Component{{
					Type:   wire.ComponentEmpty,
					Key:    "mika-empty",
					Title:  t.noDevicesTitle,
					Detail: t.noDevicesDetail,
					Icon:   wire.NavIconExtension,
					Button: &wire.EmptyButton{
						Title:  t.addDeviceBtn,
						Action: *ext.Open("add_device", "", pageState{Mode: "add_device"}, t.addDeviceTitle),
					},
				}},
			}},
		}, nil
	}

	st.Device = sel.id
	if st.Mode == "detail" && st.TaskID != "" {
		return a.taskDetailView(ctx, r, st, snap, sel, t)
	}
	return a.overviewView(ctx, r, st, snap, sel, t)
}

func (a *app) addDeviceView(r *ext.UIRequest, snap appState, t *texts) *wire.View {
	st := pageState{Mode: "add_device"}
	defaultVal := "false"
	if len(snap.devices) == 0 {
		defaultVal = "true"
	}
	v6 := r.UIVersion == 0 || r.UIVersion >= 6
	urlField := wire.Component{
		Type:        wire.ComponentField,
		Key:         "field-url",
		Name:        "url",
		Label:       t.fieldURLLabel,
		Kind:        wire.FieldKindText,
		Placeholder: t.fieldURLPlaceholder,
	}
	if v6 {
		urlField.Detail = t.fieldURLDetail
	}
	actions := []wire.Component{{
		Type:   wire.ComponentButton,
		Key:    "btn-save-add",
		Title:  t.saveAddBtn,
		Action: &wire.Action{ID: "save_add", Submit: true},
	}}
	if r.UIVersion > 0 && r.UIVersion < 3 {
		actions = append(actions, wire.Component{
			Type:   wire.ComponentButton,
			Key:    "btn-add-back",
			Title:  t.back,
			Action: &wire.Action{ID: "back"},
		})
	}
	return &wire.View{
		Title: t.addDeviceTitle,
		State: ext.EncodeState(st),
		Sections: []wire.Section{
			{
				Title:  t.addDeviceTitle,
				Footer: t.formSecurityFooter,
				Items: []wire.Component{
					{
						Type:        wire.ComponentField,
						Key:         "field-name",
						Name:        "name",
						Label:       t.fieldNameLabel,
						Kind:        wire.FieldKindText,
						Placeholder: t.fieldNamePlaceholder,
					},
					urlField,
					{
						Type:        wire.ComponentField,
						Key:         "field-token",
						Name:        "token",
						Label:       t.fieldTokenLabel,
						Kind:        wire.FieldKindSecret,
						Placeholder: t.fieldTokenAddPlaceholder,
					},
					{
						Type:  wire.ComponentField,
						Key:   "field-default",
						Name:  "default",
						Label: t.fieldDefaultLabel,
						Kind:  wire.FieldKindToggle,
						Value: defaultVal,
					},
				},
			},
			{Items: actions},
		},
	}
}

func (a *app) editDeviceView(r *ext.UIRequest, st pageState, snap appState, t *texts) *wire.View {
	dev, ok := snap.deviceByID(st.Device)
	if !ok {
		return &wire.View{
			Title: t.editDeviceTitle,
			State: ext.EncodeState(pageState{Mode: "overview"}),
			Sections: []wire.Section{{
				Items: []wire.Component{
					{
						Type:  wire.ComponentEmpty,
						Key:   "edit-missing",
						Title: t.deviceNotFound,
					},
					{
						Type:   wire.ComponentButton,
						Key:    "btn-edit-missing-back",
						Title:  t.back,
						Action: &wire.Action{ID: "back"},
					},
				},
			}},
		}
	}

	st = pageState{Mode: "device_edit", Device: dev.id}
	viewTitle := clipText(fmt.Sprintf("%s · %s", t.editDeviceTitle, dev.name), 200)
	v6 := r.UIVersion == 0 || r.UIVersion >= 6
	urlField := wire.Component{
		Type:        wire.ComponentField,
		Key:         "field-url",
		Name:        "url",
		Label:       t.fieldURLLabel,
		Kind:        wire.FieldKindText,
		Value:       displayEditableURL(dev.rawURL),
		Placeholder: t.fieldURLPlaceholder,
	}
	tokenField := wire.Component{
		Type:        wire.ComponentField,
		Key:         "field-token",
		Name:        "token",
		Label:       t.fieldTokenLabel,
		Kind:        wire.FieldKindSecret,
		Placeholder: t.fieldTokenEditPlaceholder,
	}
	if v6 {
		urlField.Detail = t.fieldURLDetail
		tokenField.Detail = t.fieldTokenEditDetail
	}
	actions := []wire.Component{
		{
			Type:   wire.ComponentButton,
			Key:    "btn-save-edit",
			Title:  t.saveEditBtn,
			Action: &wire.Action{ID: "save_edit:" + dev.id, Submit: true},
		},
		{
			Type:   wire.ComponentButton,
			Key:    "btn-edit-test",
			Title:  t.testConnBtn,
			Action: &wire.Action{ID: "test:" + dev.id},
		},
		{
			Type:  wire.ComponentButton,
			Key:   "btn-edit-delete",
			Title: t.deleteDeviceBtn,
			Role:  wire.ButtonRoleDestructive,
			Action: &wire.Action{
				ID:      "delete:" + dev.id,
				Confirm: fmt.Sprintf(t.deleteConfirmFmt, dev.name),
			},
		},
	}
	if r.UIVersion > 0 && r.UIVersion < 3 {
		actions = append(actions, wire.Component{
			Type:   wire.ComponentButton,
			Key:    "btn-edit-back",
			Title:  t.back,
			Action: &wire.Action{ID: "back"},
		})
	}

	return &wire.View{
		Title: viewTitle,
		State: ext.EncodeState(st),
		Sections: []wire.Section{
			{
				Title:  t.editDeviceTitle,
				Footer: t.editTokenFooter,
				Items: []wire.Component{
					{
						Type:        wire.ComponentField,
						Key:         "field-name",
						Name:        "name",
						Label:       t.fieldNameLabel,
						Kind:        wire.FieldKindText,
						Value:       dev.name,
						Placeholder: t.fieldNamePlaceholder,
					},
					urlField,
					tokenField,
					{
						Type:  wire.ComponentField,
						Key:   "field-default",
						Name:  "default",
						Label: t.fieldDefaultLabel,
						Kind:  wire.FieldKindToggle,
						Value: strconv.FormatBool(dev.defaultDev),
					},
				},
			},
			{Items: actions},
		},
	}
}

type uiDeviceProbe struct {
	entry deviceEntry
	info  discoveryInfo
	err   error
}

func (a *app) probeOverviewDevices(ctx context.Context, snap appState, selectedID string) map[string]uiDeviceProbe {
	results := make([]uiDeviceProbe, len(snap.devices))
	sem := make(chan struct{}, maxProbeConcurrency)
	var wg sync.WaitGroup
	for i, d := range snap.devices {
		if !d.ready() {
			results[i] = uiDeviceProbe{entry: d}
			continue
		}
		wg.Add(1)
		go func(idx int, entry deviceEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			probeCtx := ctx
			var cancel context.CancelFunc
			if entry.id != selectedID {
				probeCtx, cancel = context.WithTimeout(ctx, peerProbeTimeout)
				defer cancel()
			}
			info, err := entry.client.discover(probeCtx)
			a.recordDevice(entry.id, err)
			results[idx] = uiDeviceProbe{entry: entry, info: info, err: err}
		}(i, d)
	}
	wg.Wait()
	out := make(map[string]uiDeviceProbe, len(results))
	for _, r := range results {
		out[r.entry.id] = r
	}
	return out
}

func segmentedChoiceTitle(d deviceEntry, snap appState) string {
	label := d.name
	if snap.hasDuplicateName(d.name) {
		suffix := " #" + strconv.Itoa(d.index)
		maxBase := maxSegmentedLabel - utf8.RuneCountInString(suffix)
		if utf8.RuneCountInString(label) > maxBase {
			label = clipText(label, maxBase)
		}
		label += suffix
	}
	return clipText(label, maxSegmentedLabel)
}

func formatVersionLabel(info discoveryInfo) string {
	versionLabel := info.ServerVersion
	if versionLabel != "" && !strings.HasPrefix(versionLabel, "v") {
		versionLabel = "v" + versionLabel
	}
	if versionLabel == "" {
		return fmt.Sprintf("API v%d", info.APIVersion)
	}
	return fmt.Sprintf("%s (API v%d)", versionLabel, info.APIVersion)
}

func deviceRowActions(d deviceEntry, t *texts) []wire.RowAction {
	editTitle := clipText(fmt.Sprintf("%s · %s", t.editDeviceTitle, d.name), 200)
	acts := []wire.RowAction{
		{
			Title:  t.rowTest,
			Icon:   wire.NavIconRefresh,
			Action: wire.Action{ID: "test:" + d.id},
		},
		{
			Title:  t.rowEdit,
			Icon:   wire.NavIconEdit,
			Action: *ext.Open("edit:"+d.id, "", pageState{Mode: "device_edit", Device: d.id}, editTitle),
		},
	}
	if !d.defaultDev {
		acts = append(acts, wire.RowAction{
			Title:  t.rowSetDefault,
			Icon:   wire.NavIconStar,
			Action: wire.Action{ID: "set_default:" + d.id},
		})
	}
	acts = append(acts, wire.RowAction{
		Title: t.rowDelete,
		Icon:  wire.NavIconTrash,
		Role:  wire.ButtonRoleDestructive,
		Action: wire.Action{
			ID:      "delete:" + d.id,
			Confirm: fmt.Sprintf(t.deleteConfirmFmt, d.name),
		},
	})
	return acts
}

func selectedDeviceActionButtons(sel deviceEntry, t *texts) []wire.Component {
	editTitle := clipText(fmt.Sprintf("%s · %s", t.editDeviceTitle, sel.name), 200)
	btns := []wire.Component{
		{
			Type:   wire.ComponentButton,
			Key:    "btn-test",
			Title:  t.testConnBtn,
			Action: &wire.Action{ID: "test:" + sel.id},
		},
		{
			Type:   wire.ComponentButton,
			Key:    "btn-edit",
			Title:  t.editDeviceBtn,
			Action: ext.Open("edit:"+sel.id, "", pageState{Mode: "device_edit", Device: sel.id}, editTitle),
		},
	}
	if !sel.defaultDev {
		btns = append(btns, wire.Component{
			Type:   wire.ComponentButton,
			Key:    "btn-set-default",
			Title:  t.setDefaultBtn,
			Action: &wire.Action{ID: "set_default:" + sel.id},
		})
	}
	btns = append(btns,
		wire.Component{
			Type:  wire.ComponentButton,
			Key:   "btn-delete",
			Title: t.deleteDeviceBtn,
			Role:  wire.ButtonRoleDestructive,
			Action: &wire.Action{
				ID:      "delete:" + sel.id,
				Confirm: fmt.Sprintf(t.deleteConfirmFmt, sel.name),
			},
		},
		wire.Component{
			Type:   wire.ComponentButton,
			Key:    "btn-refresh",
			Title:  t.refresh,
			Action: &wire.Action{ID: "refresh"},
		},
	)
	return btns
}

func (a *app) overviewView(ctx context.Context, r *ext.UIRequest, st pageState, snap appState, sel deviceEntry, t *texts) (*wire.View, error) {
	if st.Pages < 1 {
		st.Pages = 1
	}
	st.Mode = "overview"
	st.TaskID = ""

	probes := a.probeOverviewDevices(ctx, snap, sel.id)
	selProbe := probes[sel.id]

	var sections []wire.Section
	multi := snap.isMultiDevice()
	v6 := r.UIVersion == 0 || r.UIVersion >= 6

	if len(snap.devices) >= 2 && len(snap.devices) <= maxSegmentedDevices && r.UIVersion >= 2 {
		choices := make([]wire.Choice, 0, len(snap.devices))
		for _, d := range snap.devices {
			choices = append(choices, wire.Choice{
				Value: d.id,
				Title: segmentedChoiceTitle(d, snap),
			})
		}
		sections = append(sections, wire.Section{
			Items: []wire.Component{{
				Type:    wire.ComponentSegmented,
				Key:     "device-picker",
				Name:    "device",
				Value:   sel.id,
				Choices: choices,
				Action:  &wire.Action{ID: "device"},
			}},
		})
	}

	devItems := make([]wire.Component, 0, len(snap.devices)+1)
	for _, d := range snap.devices {
		p := probes[d.id]
		rowTitle := d.name
		if d.defaultDev {
			rowTitle = fmt.Sprintf("%s · %s", d.name, t.defaultTag)
		}
		row := wire.Component{
			Type:   wire.ComponentRow,
			Key:    "dev-" + d.id,
			Title:  clipText(rowTitle, 1800),
			Action: &wire.Action{ID: "device:" + d.id, Values: map[string]string{"device": d.id}},
		}
		if v6 {
			row.Actions = deviceRowActions(d, t)
		}
		switch {
		case !d.ready():
			row.Badge = &wire.Badge{Text: t.configError, Tone: wire.ToneCaution}
			sub := d.maskedURL
			if d.configErr != "" {
				sub += " · " + snap.scrub(d.configErr)
			}
			row.Subtitle = clipText(sub, 1800)
		case p.err != nil:
			row.Badge = &wire.Badge{Text: t.offline, Tone: wire.ToneDanger}
			row.Subtitle = clipText(d.maskedURL+" · "+snap.scrub(p.err.Error()), 1800)
		default:
			row.Badge = &wire.Badge{Text: t.online, Tone: wire.TonePositive}
			row.Subtitle = clipText(d.maskedURL, 1800)
			row.Value = clipText(formatVersionLabel(p.info), 200)
		}
		if d.id == sel.id {
			row.Value = t.viewing
		}
		devItems = append(devItems, row)
	}
	devItems = append(devItems, wire.Component{
		Type:   wire.ComponentButton,
		Key:    "btn-add-device",
		Title:  t.addDeviceBtn,
		Action: ext.Open("add_device", "", pageState{Mode: "add_device"}, t.addDeviceTitle),
	})
	sections = append(sections, wire.Section{Title: t.devices, Items: devItems})

	connTitle := fmt.Sprintf("%s · %s", t.connection, sel.name)
	serverRowTitle := fmt.Sprintf("%s · %s", t.serverRow, sel.name)
	if sel.defaultDev {
		serverRowTitle = fmt.Sprintf("%s (%s)", sel.name, t.defaultTag)
	}

	if !sel.ready() {
		errText := snap.scrub(sel.configErr)
		if errText == "" {
			errText = t.notConfigured
		}
		connItems := []wire.Component{
			{
				Type:     wire.ComponentRow,
				Key:      "conn-status",
				Title:    clipText(serverRowTitle, 1800),
				Subtitle: clipText(sel.maskedURL, 1800),
				Badge:    &wire.Badge{Text: t.configError, Tone: wire.ToneCaution},
			},
			{
				Type:  wire.ComponentText,
				Key:   "conn-error",
				Text:  clipText(errText, 1800),
				Style: wire.TextStyleSecondary,
			},
		}
		connItems = append(connItems, selectedDeviceActionButtons(sel, t)...)
		sections = append(sections, wire.Section{Title: connTitle, Items: connItems})
		return &wire.View{
			Title:        t.title,
			State:        ext.EncodeState(st),
			RefreshAfter: 30,
			Sections:     sections,
		}, nil
	}

	if selProbe.err != nil {
		connItems := []wire.Component{
			{
				Type:     wire.ComponentRow,
				Key:      "conn-status",
				Title:    clipText(serverRowTitle, 1800),
				Subtitle: clipText(sel.maskedURL, 1800),
				Badge:    &wire.Badge{Text: t.offline, Tone: wire.ToneDanger},
			},
			{
				Type:  wire.ComponentText,
				Key:   "conn-error",
				Text:  clipText(snap.scrub(selProbe.err.Error()), 1800),
				Style: wire.TextStyleSecondary,
			},
		}
		connItems = append(connItems, selectedDeviceActionButtons(sel, t)...)
		sections = append(sections, wire.Section{Title: connTitle, Items: connItems})
		return &wire.View{
			Title:        t.title,
			State:        ext.EncodeState(st),
			RefreshAfter: 30,
			Sections:     sections,
		}, nil
	}

	info := selProbe.info
	versionLabel := formatVersionLabel(info)
	connItems := []wire.Component{{
		Type:     wire.ComponentRow,
		Key:      "conn-status",
		Title:    clipText(serverRowTitle, 1800),
		Subtitle: clipText(sel.maskedURL, 1800),
		Value:    clipText(versionLabel, 200),
		Badge:    &wire.Badge{Text: t.online, Tone: wire.TonePositive},
	}}

	if slices.Contains(info.Capabilities, "device_state") {
		if dev, derr := sel.client.deviceState(ctx); derr == nil {
			connItems = append(connItems, deviceSummaryRow(dev, t))
		} else {
			connItems = append(connItems, wire.Component{
				Type:     wire.ComponentRow,
				Key:      "device-status",
				Title:    t.deviceRow,
				Subtitle: clipText(snap.scrub(derr.Error()), 1800),
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
	connItems = append(connItems, selectedDeviceActionButtons(sel, t)...)
	sections = append(sections, wire.Section{Title: connTitle, Items: connItems})

	tasksTitle := t.recentTasks
	if multi || !strings.EqualFold(sel.name, "default") {
		tasksTitle = fmt.Sprintf("%s · %s", t.recentTasks, sel.name)
	}
	taskBudget := max(20, min(maxViewTasks, maxOverviewTaskItems-len(snap.devices)))
	tasks, hasMore, listErr := collectTasks(ctx, sel.client, st.Pages, taskBudget)
	taskSection := wire.Section{Title: tasksTitle}
	if listErr != nil {
		taskSection.Items = []wire.Component{{
			Type:  wire.ComponentText,
			Key:   "tasks-error",
			Text:  clipText(snap.scrub(listErr.Error()), 1800),
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
			vals := map[string]string{"task_id": item.TaskID, "device": sel.id}
			detailActionID := "detail:" + sel.id + ":" + item.TaskID
			taskSection.Items = append(taskSection.Items, wire.Component{
				Type:     wire.ComponentRow,
				Key:      "task-" + item.TaskID,
				Title:    clipText(goal, 1800),
				Subtitle: clipText(sub, 1800),
				Badge:    &badge,
				Action:   &wire.Action{ID: detailActionID, Values: vals},
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
	sections = append(sections, taskSection)

	return &wire.View{
		Title:        t.title,
		State:        ext.EncodeState(st),
		RefreshAfter: 30,
		Sections:     sections,
	}, nil
}

func (a *app) taskDetailView(ctx context.Context, r *ext.UIRequest, st pageState, snap appState, sel deviceEntry, t *texts) (*wire.View, error) {
	viewTitle := t.taskDetail
	if snap.isMultiDevice() || !strings.EqualFold(sel.name, "default") {
		viewTitle = fmt.Sprintf("%s · %s", t.taskDetail, sel.name)
	}
	if !sel.ready() {
		errText := snap.scrub(sel.configErr)
		if errText == "" {
			errText = t.notConfigured
		}
		return &wire.View{
			Title: clipText(viewTitle, 200),
			State: ext.EncodeState(st),
			Sections: []wire.Section{{
				Title: clipText(viewTitle, 200),
				Items: []wire.Component{
					{Type: wire.ComponentText, Text: clipText(errText, 1800), Style: wire.TextStyleSecondary},
					{Type: wire.ComponentButton, Title: t.refresh, Action: &wire.Action{ID: "refresh"}},
					{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
				},
			}},
		}, nil
	}

	task, err := sel.client.taskStatus(ctx, st.TaskID)
	a.recordDevice(sel.id, err)
	if err != nil {
		return &wire.View{
			Title: clipText(viewTitle, 200),
			State: ext.EncodeState(st),
			Sections: []wire.Section{{
				Title: clipText(viewTitle, 200),
				Items: []wire.Component{
					{Type: wire.ComponentText, Text: clipText(snap.scrub(err.Error()), 1800), Style: wire.TextStyleSecondary},
					{Type: wire.ComponentButton, Title: t.refresh, Action: &wire.Action{ID: "refresh"}},
					{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
				},
			}},
		}, nil
	}

	loc := loadLocation(r.TZ)
	badge := statusBadge(task.Status, t)
	var headItems []wire.Component
	if snap.isMultiDevice() {
		headItems = append(headItems, wire.Component{
			Type:  wire.ComponentText,
			Key:   "detail-device",
			Text:  clipText(fmt.Sprintf("%s%s · %s", t.devicePrefix, sel.label(), sel.maskedURL), 1800),
			Style: wire.TextStyleSecondary,
		})
	}
	headItems = append(headItems, wire.Component{
		Type:     wire.ComponentRow,
		Key:      "detail-status",
		Title:    clipText(fmt.Sprintf(t.taskMetaFmt, task.TaskID, task.Conversation), 1800),
		Subtitle: formatTimestamp(task.StartedAtMs, loc),
		Badge:    &badge,
	})
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

	sections := []wire.Section{{Title: clipText(viewTitle, 200), Items: headItems}}
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
		Title:        clipText(viewTitle, 200),
		State:        ext.EncodeState(st),
		RefreshAfter: 30,
		Sections:     sections,
	}, nil
}

func collectTasks(ctx context.Context, c *client, pages, maxItems int) ([]remoteTask, bool, error) {
	if maxItems <= 0 {
		maxItems = maxViewTasks
	}
	var out []remoteTask
	before := ""
	hasMore := false
	for p := 0; p < pages && len(out) < maxItems; p++ {
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
	if len(out) > maxItems {
		out = out[:maxItems]
	}
	return out, hasMore && len(out) < maxItems && pages < maxOverviewPages, nil
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
