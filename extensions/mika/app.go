package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nwverse/persona/sdk/go/ext"
)

const (
	connectorID   = "mika"
	pageUI        = "mika"
	slotPrimary   = "primary"
	slotSecondary = "secondary"
	slotTertiary  = "tertiary"
	maxDeviceName = 40
	maxDeviceSel  = 64
)

const (
	deviceStateReady         = "ready"
	deviceStateNotConfigured = "not_configured"
	deviceStateMissingToken  = "missing_token"
	deviceStateMissingURL    = "missing_url"
	deviceStateInvalidURL    = "invalid_url"
	deviceStateInvalidName   = "invalid_name"
)

type slotSpec struct {
	slot        string
	index       int
	defaultName string
	nameField   string
	urlField    string
	secretField string
}

var deviceSlots = []slotSpec{
	{
		slot:        slotPrimary,
		index:       1,
		defaultName: "default",
		nameField:   "instance_name",
		urlField:    "instance_url",
		secretField: "api_token",
	},
	{
		slot:        slotSecondary,
		index:       2,
		defaultName: "secondary",
		nameField:   "secondary_name",
		urlField:    "secondary_url",
		secretField: "secondary_api_token",
	},
	{
		slot:        slotTertiary,
		index:       3,
		defaultName: "tertiary",
		nameField:   "tertiary_name",
		urlField:    "tertiary_url",
		secretField: "tertiary_api_token",
	},
}

type deviceEntry struct {
	slot        string
	index       int
	name        string
	maskedURL   string
	configState string
	configErr   string
	client      *client
}

func (d deviceEntry) ready() bool {
	return d.configState == deviceStateReady && d.client != nil && d.client.configured()
}

func (d deviceEntry) isPrimary() bool {
	return d.slot == slotPrimary
}

func (d deviceEntry) label() string {
	return fmt.Sprintf("%s (%s)", d.name, d.slot)
}

func (d deviceEntry) confirmLabel() string {
	return fmt.Sprintf("%s (%s, %s)", d.name, d.slot, d.maskedURL)
}

type deviceHealth struct {
	lastOK  time.Time
	lastErr error
}

type app struct {
	mu          sync.RWMutex
	devices     []deviceEntry
	allTokens   []string
	defaultWait int
	hub         hubNotifier
	receipts    *writeStore
	health      map[string]deviceHealth
}

type hubNotifier interface {
	Invalidate(string) error
}

type appState struct {
	devices     []deviceEntry
	allTokens   []string
	defaultWait int
	hub         hubNotifier
	receipts    *writeStore
	health      map[string]deviceHealth
}

func newApp() *app {
	return &app{
		defaultWait: defaultWaitSeconds,
		receipts:    newWriteStore(""),
		health:      map[string]deviceHealth{},
	}
}

func (a *app) snapshot() appState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	wait := a.defaultWait
	if wait < minWaitSeconds || wait > maxWaitSeconds {
		wait = defaultWaitSeconds
	}
	healthCopy := make(map[string]deviceHealth, len(a.health))
	for k, v := range a.health {
		healthCopy[k] = v
	}
	return appState{
		devices:     slices.Clone(a.devices),
		allTokens:   slices.Clone(a.allTokens),
		defaultWait: wait,
		hub:         a.hub,
		receipts:    a.receipts,
		health:      healthCopy,
	}
}

func (st appState) scrub(text string) string {
	return scrubSecrets(text, st.allTokens)
}

func (st appState) isMultiDevice() bool {
	return len(st.devices) > 1
}

func (st appState) readyDevices() []deviceEntry {
	out := make([]deviceEntry, 0, len(st.devices))
	for _, d := range st.devices {
		if d.ready() {
			out = append(out, d)
		}
	}
	return out
}

func (st appState) deviceBySlot(slot string) (deviceEntry, bool) {
	for _, d := range st.devices {
		if d.slot == slot {
			return d, true
		}
	}
	return deviceEntry{}, false
}

func (st appState) hasDuplicateName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	count := 0
	for _, d := range st.devices {
		if strings.EqualFold(d.name, name) {
			count++
			if count > 1 {
				return true
			}
		}
	}
	return false
}

func (st appState) availableDevicesSummary() string {
	if len(st.devices) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(st.devices))
	for _, d := range st.devices {
		s := fmt.Sprintf("%s (%s)", d.name, d.slot)
		if !d.ready() {
			s += " [" + d.configState + "]"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func validDeviceSelector(raw string) (string, error) {
	sel := strings.TrimSpace(raw)
	if len([]rune(sel)) > maxDeviceSel {
		return "", fmt.Errorf("device selector must be at most %d characters", maxDeviceSel)
	}
	for _, r := range sel {
		if r == 0 || unicode.IsControl(r) {
			return "", errors.New("device selector contains unsupported control characters")
		}
	}
	return sel, nil
}

func normalizeDeviceName(raw, fallback string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback, nil
	}
	if len([]rune(trimmed)) > maxDeviceName {
		return "", fmt.Errorf("device name must be at most %d characters", maxDeviceName)
	}
	for _, r := range trimmed {
		if r == 0 || unicode.IsControl(r) {
			return "", errors.New("device name must not contain control characters")
		}
	}
	return trimmed, nil
}

// resolveDevice finds a configured device by optional selector (empty -> primary slot 1,
// else case-insensitive match against custom name or slot aliases primary/secondary/tertiary/1/2/3).
func (st appState) resolveDevice(rawSelector string) (deviceEntry, error) {
	sel, err := validDeviceSelector(rawSelector)
	if err != nil {
		return deviceEntry{}, err
	}
	if len(st.devices) == 0 {
		return deviceEntry{}, errNotConfigured
	}
	if sel == "" {
		if primary, ok := st.deviceBySlot(slotPrimary); ok {
			return primary, nil
		}
		return deviceEntry{}, fmt.Errorf("%w: primary device (slot 1) is not configured; available devices: %s",
			errNotConfigured, st.availableDevicesSummary())
	}

	var matches []deviceEntry
	for _, d := range st.devices {
		matchesName := strings.EqualFold(d.name, sel)
		matchesSlot := strings.EqualFold(d.slot, sel) || sel == strconv.Itoa(d.index)
		if matchesName || matchesSlot {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return deviceEntry{}, fmt.Errorf("unknown Mika device %q; available devices: %s (you can match by device name, primary/secondary/tertiary, or 1/2/3)",
			st.scrub(sel), st.availableDevicesSummary())
	default:
		labels := make([]string, 0, len(matches))
		for _, m := range matches {
			labels = append(labels, fmt.Sprintf("%s [%s / %d]", m.name, m.slot, m.index))
		}
		return deviceEntry{}, fmt.Errorf("ambiguous Mika device %q matches multiple slots (%s); use a slot alias (primary/secondary/tertiary or 1/2/3) or give each phone a unique name in Settings › Extensions. Available devices: %s",
			st.scrub(sel), strings.Join(labels, ", "), st.availableDevicesSummary())
	}
}

func (st appState) resolveReadyDevice(rawSelector string) (deviceEntry, error) {
	dev, err := st.resolveDevice(rawSelector)
	if err != nil {
		return deviceEntry{}, err
	}
	if !dev.ready() {
		reason := st.scrub(dev.configErr)
		if reason == "" {
			reason = dev.configState
		}
		ready := st.readyDevices()
		if len(ready) > 0 {
			readyNames := make([]string, 0, len(ready))
			for _, r := range ready {
				readyNames = append(readyNames, r.label())
			}
			return deviceEntry{}, fmt.Errorf("%w: device %s is not ready (%s); ready devices: %s",
				errNotConfigured, dev.label(), reason, strings.Join(readyNames, ", "))
		}
		return deviceEntry{}, fmt.Errorf("%w: device %s is not ready (%s)", errNotConfigured, dev.label(), reason)
	}
	return dev, nil
}

func isValidSlot(slot string) bool {
	return slot == slotPrimary || slot == slotSecondary || slot == slotTertiary
}

func (a *app) init(_ context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
	var cfg struct {
		InstanceName       string `json:"instance_name"`
		InstanceURL        string `json:"instance_url"`
		SecondaryName      string `json:"secondary_name"`
		SecondaryURL       string `json:"secondary_url"`
		TertiaryName       string `json:"tertiary_name"`
		TertiaryURL        string `json:"tertiary_url"`
		DefaultWaitSeconds int    `json:"default_wait_seconds"`
	}
	if err := in.Config(&cfg); err != nil {
		return nil, err
	}
	wait := cfg.DefaultWaitSeconds
	if wait == 0 {
		wait = defaultWaitSeconds
	}
	if wait < minWaitSeconds || wait > maxWaitSeconds {
		return nil, fmt.Errorf("default_wait_seconds must be between %d and %d", minWaitSeconds, maxWaitSeconds)
	}

	rawInputs := []struct {
		spec  slotSpec
		name  string
		url   string
		token string
	}{
		{deviceSlots[0], cfg.InstanceName, cfg.InstanceURL, in.Secrets[deviceSlots[0].secretField]},
		{deviceSlots[1], cfg.SecondaryName, cfg.SecondaryURL, in.Secrets[deviceSlots[1].secretField]},
		{deviceSlots[2], cfg.TertiaryName, cfg.TertiaryURL, in.Secrets[deviceSlots[2].secretField]},
	}

	// Collect all configured secrets upfront so any error from any slot scrubs every token.
	var tokenList []string
	for _, r := range rawInputs {
		if tok := strings.TrimSpace(r.token); tok != "" {
			tokenList = append(tokenList, tok)
		}
	}
	allTokens := collectScrubTokens("", tokenList...)

	var (
		devices       []deviceEntry
		readyCount    int
		firstFatalErr string
	)
	for _, r := range rawInputs {
		rawName := strings.TrimSpace(r.name)
		rawURL := strings.TrimSpace(r.url)
		rawToken := strings.TrimSpace(r.token)

		// Treat untouched optional slots (and empty primary with only default name) as absent.
		touched := rawURL != "" || rawToken != ""
		if !touched {
			if r.spec.slot == slotPrimary {
				touched = rawName != "" && !strings.EqualFold(rawName, r.spec.defaultName)
			} else {
				touched = rawName != ""
			}
		}
		if !touched {
			continue
		}

		entry := deviceEntry{
			slot:      r.spec.slot,
			index:     r.spec.index,
			name:      r.spec.defaultName,
			maskedURL: "(not set)",
		}

		name, nameErr := normalizeDeviceName(rawName, r.spec.defaultName)
		if nameErr != nil {
			entry.configState = deviceStateInvalidName
			entry.configErr = fmt.Sprintf("%s: %s", r.spec.nameField, scrubSecrets(nameErr.Error(), allTokens))
			if firstFatalErr == "" {
				firstFatalErr = entry.configErr
			}
			devices = append(devices, entry)
			continue
		}
		entry.name = name

		switch {
		case rawURL == "" && rawToken == "":
			entry.configState = deviceStateNotConfigured
			entry.configErr = fmt.Sprintf("%s and %s are not configured in Settings › Extensions", r.spec.urlField, r.spec.secretField)
		case rawURL == "" && rawToken != "":
			entry.configState = deviceStateMissingURL
			entry.configErr = fmt.Sprintf("%s is not configured in Settings › Extensions", r.spec.urlField)
		default:
			base, urlErr := normalizeInstanceURL(rawURL)
			if urlErr != nil {
				entry.configState = deviceStateInvalidURL
				entry.maskedURL = "(invalid URL)"
				entry.configErr = fmt.Sprintf("%s: %s", r.spec.urlField, scrubSecrets(urlErr.Error(), allTokens))
				if firstFatalErr == "" {
					firstFatalErr = entry.configErr
				}
			} else {
				entry.maskedURL = maskInstanceURL(base)
				if rawToken == "" {
					entry.configState = deviceStateMissingToken
					entry.configErr = fmt.Sprintf("%s is not configured in Settings › Extensions", r.spec.secretField)
				} else {
					entry.configState = deviceStateReady
					entry.client = newClient(base, rawToken, allTokens...)
					readyCount++
				}
			}
		}
		devices = append(devices, entry)
	}

	// Fail fast on invalid URL/name only when no device is ready; otherwise isolate the bad slot.
	if readyCount == 0 && firstFatalErr != "" {
		return nil, errors.New(firstFatalErr)
	}

	store, err := openWriteStore(in.Info().DataDir)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.devices = devices
	a.allTokens = allTokens
	a.defaultWait = wait
	if hub != nil {
		a.hub = hub
	} else {
		a.hub = nil
	}
	a.receipts = store
	a.health = map[string]deviceHealth{}
	return nil, nil
}

func (a *app) recordDevice(slot string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.health == nil {
		a.health = map[string]deviceHealth{}
	}
	h := a.health[slot]
	if err == nil {
		h.lastOK = time.Now().UTC()
		h.lastErr = nil
	} else {
		h.lastErr = err
	}
	a.health[slot] = h
}

func (a *app) status() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if len(a.devices) == 0 {
		return map[string]any{
			"state":   "not_configured",
			"account": "Mika 手机助手",
		}
	}

	ready := make([]deviceEntry, 0, len(a.devices))
	var firstConfigErr string
	for _, d := range a.devices {
		if d.ready() {
			ready = append(ready, d)
		} else if firstConfigErr == "" && d.configErr != "" {
			firstConfigErr = fmt.Sprintf("%s: %s", d.label(), d.configErr)
		}
	}

	var account string
	if len(a.devices) == 1 {
		d := a.devices[0]
		if !d.ready() {
			account = "Mika 手机助手"
		} else if strings.EqualFold(d.name, "default") {
			account = "Mika (" + d.maskedURL + ")"
		} else {
			account = fmt.Sprintf("Mika · %s (%s)", d.name, d.maskedURL)
		}
	} else {
		names := make([]string, 0, len(a.devices))
		for _, d := range a.devices {
			names = append(names, d.name)
		}
		account = fmt.Sprintf("Mika (%s)", strings.Join(names, ", "))
	}

	if len(ready) == 0 {
		out := map[string]any{"state": "not_configured", "account": account}
		if firstConfigErr != "" {
			out["error"] = clipText(scrubSecrets(firstConfigErr, a.allTokens), 200)
		}
		return out
	}

	var (
		latestOK     time.Time
		healthyReady int
		firstErr     error
		firstErrSlot string
	)
	for _, d := range ready {
		h := a.health[d.slot]
		if h.lastOK.After(latestOK) {
			latestOK = h.lastOK
		}
		if h.lastErr == nil {
			healthyReady++
		} else if firstErr == nil {
			firstErr = h.lastErr
			firstErrSlot = d.label()
		}
	}

	state := "ok"
	out := map[string]any{"state": state, "account": account}
	if !latestOK.IsZero() {
		out["last_sync"] = latestOK
	}

	// Only report non-ok state when every ready device has errored; otherwise keep state "ok"
	// so the hub keeps offering tools for the healthy device(s) (per connector.schema.json).
	if healthyReady == 0 && firstErr != nil {
		msg := scrubSecrets(firstErr.Error(), a.allTokens)
		if len(a.devices) > 1 && firstErrSlot != "" {
			msg = firstErrSlot + ": " + msg
		}
		out["error"] = clipText(msg, 200)
		switch {
		case errors.Is(firstErr, errUnauthorized):
			out["state"] = "not_signed_in"
		case errors.Is(firstErr, errRateLimited):
			out["state"] = "rate_limited"
		default:
			out["state"] = "error"
		}
	}
	return out
}

func (a *app) connector(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var in struct {
		Contribution string          `json:"contribution"`
		Name         string          `json:"name"`
		Args         json.RawMessage `json:"args"`
		RequestID    string          `json:"request_id"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, &ext.Error{Code: "invalid", Message: "invalid connector parameters"}
		}
	}
	if in.Contribution != "" && in.Contribution != connectorID {
		return nil, ext.NoContribution(in.Contribution)
	}
	switch method {
	case "status":
		return a.status(), nil
	case "tools.list":
		return map[string]any{"tools": readTools}, nil
	case "tools.call":
		timeout := 30 * time.Second
		if in.Name == "mika_wait_task" {
			timeout = time.Duration(maxWaitSeconds)*time.Second + waitTimeoutSlack + 5*time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		text, err := a.callTool(ctx, in.Name, in.Args)
		if err != nil {
			return nil, mapConnectorError(err)
		}
		return map[string]any{"text": text}, nil
	case "actions.list":
		return map[string]any{"tools": actionTools}, nil
	case "actions.describe":
		return a.describeAction(in.Name, in.Args)
	case "actions.do":
		if len(in.RequestID) == 0 || len(in.RequestID) > 128 || strings.TrimSpace(in.RequestID) == "" {
			return nil, errors.New("actions.do needs a nonempty request_id of at most 128 characters")
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := a.doAction(ctx, in.RequestID, in.Name, in.Args)
		if err != nil {
			return nil, mapConnectorError(err)
		}
		return out, nil
	}
	return nil, ext.UnknownMethod("connector." + method)
}

func mapConnectorError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) && errors.Is(err, errRateLimited) {
		var data json.RawMessage
		if ae.RetryAfter > 0 {
			data, _ = json.Marshal(map[string]any{"retry_after": ae.RetryAfter})
		}
		return &ext.Error{Code: "rate_limited", Message: err.Error(), Data: data}
	}
	return err
}

func main() {
	log.SetFlags(0)
	ext.Main(program(newApp()))
}

var about = func() (out struct{ ID, Version string }) {
	if err := json.Unmarshal(manifest, &out); err != nil {
		panic(err)
	}
	return out
}()

func program(a *app) ext.Program {
	return ext.Program{
		ID:         about.ID,
		Version:    about.Version,
		Manifest:   manifest,
		Init:       a.init,
		UI:         map[string]ext.Page{pageUI: {Render: a.render, Action: a.action}},
		Namespaces: map[string]ext.Handler{"connector": a.connector},
	}
}
