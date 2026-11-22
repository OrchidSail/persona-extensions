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
	id          string
	index       int
	name        string
	rawURL      string
	maskedURL   string
	defaultDev  bool
	configState string
	configErr   string
	client      *client
}

func (d deviceEntry) ready() bool {
	return d.configState == deviceStateReady && d.client != nil && d.client.configured()
}

func (d deviceEntry) isDefault() bool {
	return d.defaultDev
}

func (d deviceEntry) label() string {
	if d.defaultDev {
		return fmt.Sprintf("%s (#%d, default)", d.name, d.index)
	}
	return fmt.Sprintf("%s (#%d)", d.name, d.index)
}

func (d deviceEntry) confirmLabel() string {
	if d.defaultDev {
		return fmt.Sprintf("%s (default, %s)", d.name, d.maskedURL)
	}
	return fmt.Sprintf("%s (#%d, %s)", d.name, d.index, d.maskedURL)
}

type deviceHealth struct {
	lastOK  time.Time
	lastErr error
}

type app struct {
	mu               sync.RWMutex
	vaultMu          sync.Mutex
	stored           []storedDevice
	migratedLegacy   bool
	retiredTokens    []string
	devices          []deviceEntry
	allTokens        []string
	defaultWait      int
	skipConfirmation bool // zero-value safe: false means write tools require confirmation via actions.list
	hub              hubClient
	receipts         *writeStore
	health           map[string]deviceHealth
}

type appState struct {
	devices          []deviceEntry
	allTokens        []string
	defaultWait      int
	skipConfirmation bool
	hub              hubClient
	receipts         *writeStore
	health           map[string]deviceHealth
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
		devices:          slices.Clone(a.devices),
		allTokens:        slices.Clone(a.allTokens),
		defaultWait:      wait,
		skipConfirmation: a.skipConfirmation,
		hub:              a.hub,
		receipts:         a.receipts,
		health:           healthCopy,
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

func (st appState) defaultDevice() (deviceEntry, bool) {
	for _, d := range st.devices {
		if d.defaultDev {
			return d, true
		}
	}
	if len(st.devices) > 0 {
		return st.devices[0], true
	}
	return deviceEntry{}, false
}

func (st appState) deviceByID(id string) (deviceEntry, bool) {
	for _, d := range st.devices {
		if d.id == id {
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
		s := d.label()
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

// resolveDevice finds a configured device by optional selector:
// empty / "default" / "primary" -> default device (#1);
// case-insensitive match against device name;
// backward-compatible legacy slot aliases ("secondary" -> #2, "tertiary" -> #3);
// 1-based index ("1" -> default device #1, "2" -> #2, ...);
// or exact device ID.
func (st appState) resolveDevice(rawSelector string) (deviceEntry, error) {
	sel, err := validDeviceSelector(rawSelector)
	if err != nil {
		return deviceEntry{}, err
	}
	if len(st.devices) == 0 {
		return deviceEntry{}, errNotConfigured
	}
	if sel == "" || strings.EqualFold(sel, "default") || strings.EqualFold(sel, slotPrimary) {
		if def, ok := st.defaultDevice(); ok {
			return def, nil
		}
		return deviceEntry{}, errNotConfigured
	}

	var nameMatches []deviceEntry
	for _, d := range st.devices {
		if strings.EqualFold(d.name, sel) {
			nameMatches = append(nameMatches, d)
		}
	}
	if len(nameMatches) == 1 {
		return nameMatches[0], nil
	}
	if len(nameMatches) > 1 {
		labels := make([]string, 0, len(nameMatches))
		for _, m := range nameMatches {
			labels = append(labels, fmt.Sprintf("%s [#%d]", m.name, m.index))
		}
		return deviceEntry{}, fmt.Errorf("ambiguous Mika device %q matches multiple devices (%s); use a 1-based index (1..%d) or give each phone a unique name on the Mika page. Available devices: %s",
			st.scrub(sel), strings.Join(labels, ", "), len(st.devices), st.availableDevicesSummary())
	}

	if strings.EqualFold(sel, slotSecondary) && len(st.devices) >= 2 {
		return st.devices[1], nil
	}
	if strings.EqualFold(sel, slotTertiary) && len(st.devices) >= 3 {
		return st.devices[2], nil
	}

	if n, err := strconv.Atoi(sel); err == nil && n >= 1 && n <= len(st.devices) {
		return st.devices[n-1], nil
	}

	if d, ok := st.deviceByID(sel); ok {
		return d, nil
	}

	return deviceEntry{}, fmt.Errorf("unknown Mika device %q; available devices: %s (match by device name, default/primary, or 1-based index 1..%d)",
		st.scrub(sel), st.availableDevicesSummary(), len(st.devices))
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

func (a *app) init(ctx context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
	var hc hubClient
	if hub != nil {
		hc = hub
	}
	return a.initWithHub(ctx, in, hc)
}

func (a *app) initWithHub(ctx context.Context, in *ext.Init, hub hubClient) (map[string]any, error) {
	var cfg struct {
		DefaultWaitSeconds  int    `json:"default_wait_seconds"`
		RequireConfirmation *bool  `json:"require_confirmation"`
		InstanceName        string `json:"instance_name"`
		InstanceURL         string `json:"instance_url"`
		SecondaryName       string `json:"secondary_name"`
		SecondaryURL        string `json:"secondary_url"`
		TertiaryName        string `json:"tertiary_name"`
		TertiaryURL         string `json:"tertiary_url"`
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

	// Zero-value safe: require confirmation unless require_confirmation is explicitly false.
	skipConfirmation := cfg.RequireConfirmation != nil && !*cfg.RequireConfirmation

	// Collect all legacy secrets upfront so any error during vault loading or migration scrubs every token.
	var legacySecretValues []string
	for _, tok := range in.Secrets {
		if trimmed := strings.TrimSpace(tok); trimmed != "" {
			legacySecretValues = append(legacySecretValues, trimmed)
		}
	}
	legacyTokens := collectScrubTokens("", legacySecretValues...)

	var (
		stored         []storedDevice
		migratedLegacy bool
	)
	if hub != nil {
		loadCtx, cancel := context.WithTimeout(ctx, vaultRPCTimeout)
		rawVault, err := hub.LoadSecret(loadCtx, connectorID)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("could not load encrypted Mika devices from hub: %s", scrubSecrets(err.Error(), legacyTokens))
		}
		if len(rawVault) > 0 {
			vault, err := decodeDeviceVault(rawVault, legacyTokens)
			if err != nil {
				return nil, err
			}
			stored = vault.Devices
			migratedLegacy = vault.MigratedLegacy
		}
	}

	// Zero-loss automatic migration from 0.1.0 / 0.2.x 3-slot config & secrets into Hub encrypted vault.
	if len(stored) == 0 && !migratedLegacy {
		legacyCfg := legacySlotsConfig{
			InstanceName:  cfg.InstanceName,
			InstanceURL:   cfg.InstanceURL,
			SecondaryName: cfg.SecondaryName,
			SecondaryURL:  cfg.SecondaryURL,
			TertiaryName:  cfg.TertiaryName,
			TertiaryURL:   cfg.TertiaryURL,
		}
		migrated, _, err := migrateLegacySlots(legacyCfg, in.Secrets, legacyTokens)
		if err != nil {
			return nil, err
		}
		if len(migrated) > 0 {
			stored = migrated
			migratedLegacy = true
			if hub != nil {
				encoded, err := encodeDeviceVault(deviceVault{
					Version:        vaultSchemaVersion,
					MigratedLegacy: true,
					Devices:        stored,
				})
				if err != nil {
					return nil, errors.New(scrubSecrets(err.Error(), collectVaultTokens(stored, legacyTokens...)))
				}
				saveCtx, cancel := context.WithTimeout(ctx, vaultRPCTimeout)
				saveErr := hub.SaveSecret(saveCtx, connectorID, encoded)
				cancel()
				if saveErr != nil {
					return nil, fmt.Errorf("could not persist migrated Mika devices to hub: %s",
						scrubSecrets(saveErr.Error(), collectVaultTokens(stored, legacyTokens...)))
				}
			}
		}
	}

	store, err := openWriteStore(in.Info().DataDir)
	if err != nil {
		return nil, err
	}

	allTokens := collectVaultTokens(stored, legacyTokens...)
	devices := buildDeviceEntries(stored, allTokens)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.stored = slices.Clone(stored)
	a.migratedLegacy = migratedLegacy
	a.retiredTokens = slices.Clone(legacyTokens)
	a.devices = devices
	a.allTokens = allTokens
	a.defaultWait = wait
	a.skipConfirmation = skipConfirmation
	a.hub = hub
	a.receipts = store
	a.health = map[string]deviceHealth{}
	return nil, nil
}

func (a *app) recordDevice(deviceID string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.health == nil {
		a.health = map[string]deviceHealth{}
	}
	h := a.health[deviceID]
	if err == nil {
		h.lastOK = time.Now().UTC()
		h.lastErr = nil
	} else {
		h.lastErr = err
	}
	a.health[deviceID] = h
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
		account = fmt.Sprintf("Mika (%s)", clipText(strings.Join(names, ", "), 120))
	}

	if len(ready) == 0 {
		out := map[string]any{"state": "not_configured", "account": account}
		if firstConfigErr != "" {
			out["error"] = clipText(scrubSecrets(firstConfigErr, a.allTokens), 200)
		}
		return out
	}

	var (
		latestOK      time.Time
		healthyReady  int
		firstErr      error
		firstErrLabel string
	)
	for _, d := range ready {
		h := a.health[d.id]
		if h.lastOK.After(latestOK) {
			latestOK = h.lastOK
		}
		if h.lastErr == nil {
			healthyReady++
		} else if firstErr == nil {
			firstErr = h.lastErr
			firstErrLabel = d.label()
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
		if len(a.devices) > 1 && firstErrLabel != "" {
			msg = firstErrLabel + ": " + msg
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
		st := a.snapshot()
		if st.skipConfirmation {
			return map[string]any{"tools": allDirectTools()}, nil
		}
		return map[string]any{"tools": readTools}, nil
	case "tools.call":
		timeout := 30 * time.Second
		if in.Name == "mika_wait_task" {
			timeout = time.Duration(maxWaitSeconds)*time.Second + waitTimeoutSlack + 5*time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if isWriteTool(in.Name) {
			if !a.snapshot().skipConfirmation {
				return nil, fmt.Errorf("tool %s requires confirmation via actions.do while require_confirmation is enabled", in.Name)
			}
			text, err := a.callWriteTool(ctx, in.Name, in.Args)
			if err != nil {
				return nil, mapConnectorError(err)
			}
			return map[string]any{"text": text}, nil
		}
		text, err := a.callTool(ctx, in.Name, in.Args)
		if err != nil {
			return nil, mapConnectorError(err)
		}
		return map[string]any{"text": text}, nil
	case "actions.list":
		st := a.snapshot()
		if st.skipConfirmation {
			return map[string]any{"tools": []tool{}}, nil
		}
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
