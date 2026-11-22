package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
)

const (
	vaultSchemaVersion = 1
	maxDynamicDevices  = 50
	maxTokenBytes      = 4096
	vaultRPCTimeout    = 10 * time.Second
)

var deviceIDRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

type hubClient interface {
	Invalidate(string) error
	LoadSecret(context.Context, string) ([]byte, error)
	SaveSecret(context.Context, string, []byte) error
}

var _ hubClient = (*ext.Hub)(nil)

// deviceVault is the encrypted JSON payload stored in the hub via connector.secrets.save ("mika").
// Device URLs and Bearer tokens live exclusively inside this sealed hub record, never in plaintext files or logs.
type deviceVault struct {
	Version        int            `json:"version"`
	MigratedLegacy bool           `json:"migrated_legacy,omitempty"`
	Devices        []storedDevice `json:"devices"`
}

type storedDevice struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Token   string `json:"token"`
	Default bool   `json:"default"`
}

type legacySlotsConfig struct {
	InstanceName  string
	InstanceURL   string
	SecondaryName string
	SecondaryURL  string
	TertiaryName  string
	TertiaryURL   string
}

func validDeviceID(id string) bool {
	return deviceIDRe.MatchString(id)
}

func newDeviceID(existing []storedDevice) (string, error) {
	used := make(map[string]bool, len(existing))
	for _, d := range existing {
		used[d.ID] = true
	}
	var buf [6]byte
	for range 16 {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", fmt.Errorf("could not generate device id: %w", err)
		}
		id := "dev_" + hex.EncodeToString(buf[:])
		if !used[id] {
			return id, nil
		}
	}
	return "", errors.New("could not allocate unique device id")
}

func normalizeRequiredDeviceName(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("device name is required")
	}
	if utf8.RuneCountInString(trimmed) > maxDeviceName {
		return "", fmt.Errorf("device name must be at most %d characters", maxDeviceName)
	}
	for _, r := range trimmed {
		if r == 0 || unicode.IsControl(r) {
			return "", errors.New("device name must not contain control characters")
		}
	}
	return trimmed, nil
}

func validateDeviceToken(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("access token is required")
	}
	if len(trimmed) > maxTokenBytes {
		return "", fmt.Errorf("access token must be at most %d bytes", maxTokenBytes)
	}
	for _, r := range trimmed {
		if r == 0 || unicode.IsControl(r) {
			return "", errors.New("access token must not contain control characters")
		}
	}
	return trimmed, nil
}

func displayEditableURL(normalized string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(normalized), "/")
	return strings.TrimSuffix(trimmed, "/api/v1")
}

func parseBoolValue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

func dedupeMigratedName(name string, existing []storedDevice) string {
	hasName := func(cand string) bool {
		for _, d := range existing {
			if strings.EqualFold(d.Name, cand) {
				return true
			}
		}
		return false
	}
	if !hasName(name) {
		return name
	}
	for n := 2; n <= maxDynamicDevices+10; n++ {
		suffix := " " + strconv.Itoa(n)
		base := name
		maxBase := maxDeviceName - utf8.RuneCountInString(suffix)
		if utf8.RuneCountInString(base) > maxBase {
			r := []rune(base)
			base = string(r[:max(0, maxBase)])
		}
		cand := strings.TrimSpace(base) + suffix
		if !hasName(cand) {
			return cand
		}
	}
	return name
}

// normalizeVaultDevices enforces invariants on stored devices:
// valid unique IDs, normalized URLs, trimmed tokens, and exactly one default device positioned at index 0 (#1).
func normalizeVaultDevices(raw []storedDevice) []storedDevice {
	if len(raw) == 0 {
		return []storedDevice{}
	}
	seenID := make(map[string]bool, len(raw))
	cleaned := make([]storedDevice, 0, len(raw))
	defaultIdx := -1
	for _, d := range raw {
		id := strings.TrimSpace(d.ID)
		if !validDeviceID(id) || seenID[id] {
			continue
		}
		name, err := normalizeRequiredDeviceName(d.Name)
		if err != nil {
			continue
		}
		base, err := normalizeInstanceURL(d.URL)
		if err != nil {
			continue
		}
		tok, err := validateDeviceToken(d.Token)
		if err != nil {
			continue
		}
		seenID[id] = true
		entry := storedDevice{
			ID:      id,
			Name:    name,
			URL:     base,
			Token:   tok,
			Default: false,
		}
		if d.Default && defaultIdx < 0 {
			defaultIdx = len(cleaned)
		}
		cleaned = append(cleaned, entry)
	}
	if len(cleaned) == 0 {
		return []storedDevice{}
	}
	if defaultIdx < 0 {
		defaultIdx = 0
	}
	cleaned[defaultIdx].Default = true
	if defaultIdx > 0 {
		def := cleaned[defaultIdx]
		out := make([]storedDevice, 0, len(cleaned))
		out = append(out, def)
		out = append(out, cleaned[:defaultIdx]...)
		out = append(out, cleaned[defaultIdx+1:]...)
		return out
	}
	return cleaned
}

func decodeDeviceVault(raw []byte, scrubTokens []string) (deviceVault, error) {
	var v deviceVault
	if err := json.Unmarshal(raw, &v); err != nil {
		return deviceVault{}, fmt.Errorf("stored Mika device vault is invalid: %s", scrubSecrets(err.Error(), scrubTokens))
	}
	if v.Version > vaultSchemaVersion {
		return deviceVault{}, fmt.Errorf("unsupported Mika device vault version %d", v.Version)
	}
	if v.Version == 0 {
		v.Version = vaultSchemaVersion
	}
	v.Devices = normalizeVaultDevices(v.Devices)
	return v, nil
}

func encodeDeviceVault(v deviceVault) ([]byte, error) {
	v.Version = vaultSchemaVersion
	v.Devices = normalizeVaultDevices(v.Devices)
	return json.Marshal(v)
}

func collectVaultTokens(devices []storedDevice, extra ...string) []string {
	tokens := make([]string, 0, len(devices)+len(extra))
	for _, d := range devices {
		if tok := strings.TrimSpace(d.Token); tok != "" {
			tokens = append(tokens, tok)
		}
	}
	tokens = append(tokens, extra...)
	return collectScrubTokens("", tokens...)
}

func buildDeviceEntries(stored []storedDevice, allTokens []string) []deviceEntry {
	normalized := normalizeVaultDevices(stored)
	out := make([]deviceEntry, 0, len(normalized))
	for i, d := range normalized {
		out = append(out, deviceEntry{
			id:          d.ID,
			index:       i + 1,
			name:        d.Name,
			rawURL:      d.URL,
			maskedURL:   maskInstanceURL(d.URL),
			defaultDev:  d.Default,
			configState: deviceStateReady,
			client:      newClient(d.URL, d.Token, allTokens...),
		})
	}
	return out
}

// migrateLegacySlots converts 0.1.0 / 0.2.x 3-slot configuration and secrets into dynamic stored devices.
// Valid ready slots are preserved without requiring token re-entry; incomplete optional slots are isolated.
func migrateLegacySlots(cfg legacySlotsConfig, secrets map[string]string, allTokens []string) ([]storedDevice, string, error) {
	rawInputs := []struct {
		spec  slotSpec
		name  string
		url   string
		token string
	}{
		{deviceSlots[0], cfg.InstanceName, cfg.InstanceURL, secrets[deviceSlots[0].secretField]},
		{deviceSlots[1], cfg.SecondaryName, cfg.SecondaryURL, secrets[deviceSlots[1].secretField]},
		{deviceSlots[2], cfg.TertiaryName, cfg.TertiaryURL, secrets[deviceSlots[2].secretField]},
	}

	var (
		migrated      []storedDevice
		firstFatalErr string
	)
	for _, r := range rawInputs {
		rawName := strings.TrimSpace(r.name)
		rawURL := strings.TrimSpace(r.url)
		rawToken := strings.TrimSpace(r.token)

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

		name, nameErr := normalizeDeviceName(rawName, r.spec.defaultName)
		if nameErr != nil {
			if firstFatalErr == "" {
				firstFatalErr = fmt.Sprintf("%s: %s", r.spec.nameField, scrubSecrets(nameErr.Error(), allTokens))
			}
			continue
		}
		if rawURL == "" {
			continue
		}
		base, urlErr := normalizeInstanceURL(rawURL)
		if urlErr != nil {
			if firstFatalErr == "" {
				firstFatalErr = fmt.Sprintf("%s: %s", r.spec.urlField, scrubSecrets(urlErr.Error(), allTokens))
			}
			continue
		}
		tok, tokErr := validateDeviceToken(rawToken)
		if tokErr != nil {
			continue
		}

		uniqueName := dedupeMigratedName(name, migrated)
		migrated = append(migrated, storedDevice{
			ID:      r.spec.slot,
			Name:    uniqueName,
			URL:     base,
			Token:   tok,
			Default: r.spec.slot == slotPrimary,
		})
	}

	if len(migrated) == 0 && firstFatalErr != "" {
		return nil, firstFatalErr, errors.New(firstFatalErr)
	}
	return normalizeVaultDevices(migrated), firstFatalErr, nil
}

func hasDeviceNameConflict(devices []storedDevice, name, excludeID string) bool {
	for _, d := range devices {
		if d.ID != excludeID && strings.EqualFold(d.Name, name) {
			return true
		}
	}
	return false
}

func (a *app) commitVault(ctx context.Context, nextStored []storedDevice, extraScrubTokens []string, clearHealthID string) error {
	nextStored = normalizeVaultDevices(nextStored)
	a.mu.RLock()
	hub := a.hub
	prevTokens := slices.Clone(a.allTokens)
	prevRetired := slices.Clone(a.retiredTokens)
	a.mu.RUnlock()

	scrubList := collectVaultTokens(nextStored, append(append(prevTokens, prevRetired...), extraScrubTokens...)...)
	encoded, err := encodeDeviceVault(deviceVault{
		Version:        vaultSchemaVersion,
		MigratedLegacy: true,
		Devices:        nextStored,
	})
	if err != nil {
		return errors.New(scrubSecrets(err.Error(), scrubList))
	}
	if hub != nil {
		saveCtx, cancel := context.WithTimeout(ctx, vaultRPCTimeout)
		saveErr := hub.SaveSecret(saveCtx, connectorID, encoded)
		cancel()
		if saveErr != nil {
			return fmt.Errorf("could not save encrypted Mika devices to hub: %s", scrubSecrets(saveErr.Error(), scrubList))
		}
	}

	a.mu.Lock()
	// Retain all previously seen tokens in retiredTokens so stale/old tokens remain scrubbed in memory.
	a.retiredTokens = collectScrubTokens("", append(append(a.retiredTokens, a.allTokens...), extraScrubTokens...)...)
	a.stored = slices.Clone(nextStored)
	a.migratedLegacy = true
	a.allTokens = collectVaultTokens(nextStored, a.retiredTokens...)
	a.devices = buildDeviceEntries(nextStored, a.allTokens)
	if a.health == nil {
		a.health = map[string]deviceHealth{}
	}
	if clearHealthID != "" {
		delete(a.health, clearHealthID)
	}
	validIDs := make(map[string]bool, len(nextStored))
	for _, d := range nextStored {
		validIDs[d.ID] = true
	}
	for id := range a.health {
		if !validIDs[id] {
			delete(a.health, id)
		}
	}
	a.mu.Unlock()

	if hub != nil {
		_ = hub.Invalidate(pageUI)
	}
	return nil
}

func (a *app) addDevice(ctx context.Context, rawName, rawURL, rawToken string, makeDefault bool) (deviceEntry, error) {
	a.vaultMu.Lock()
	defer a.vaultMu.Unlock()

	a.mu.RLock()
	cur := slices.Clone(a.stored)
	baseScrub := slices.Clone(a.allTokens)
	a.mu.RUnlock()

	scrub := collectScrubTokens(rawToken, baseScrub...)

	if len(cur) >= maxDynamicDevices {
		return deviceEntry{}, fmt.Errorf("at most %d Mika devices can be configured", maxDynamicDevices)
	}
	name, err := normalizeRequiredDeviceName(rawName)
	if err != nil {
		return deviceEntry{}, errors.New(scrubSecrets(err.Error(), scrub))
	}
	if hasDeviceNameConflict(cur, name, "") {
		return deviceEntry{}, fmt.Errorf("device name %q already exists; choose a different name", scrubSecrets(name, scrub))
	}
	base, err := normalizeInstanceURL(rawURL)
	if err != nil {
		return deviceEntry{}, errors.New(scrubSecrets(err.Error(), scrub))
	}
	tok, err := validateDeviceToken(rawToken)
	if err != nil {
		return deviceEntry{}, errors.New(scrubSecrets(err.Error(), scrub))
	}
	id, err := newDeviceID(cur)
	if err != nil {
		return deviceEntry{}, err
	}

	next := slices.Clone(cur)
	if len(next) == 0 {
		makeDefault = true
	}
	if makeDefault {
		for i := range next {
			next[i].Default = false
		}
	}
	next = append(next, storedDevice{
		ID:      id,
		Name:    name,
		URL:     base,
		Token:   tok,
		Default: makeDefault,
	})
	if err := a.commitVault(ctx, next, []string{tok}, ""); err != nil {
		return deviceEntry{}, err
	}
	dev, _ := a.snapshot().deviceByID(id)
	return dev, nil
}

func (a *app) editDevice(ctx context.Context, id, rawName, rawURL, rawToken string, makeDefault bool) (deviceEntry, error) {
	a.vaultMu.Lock()
	defer a.vaultMu.Unlock()

	a.mu.RLock()
	cur := slices.Clone(a.stored)
	baseScrub := slices.Clone(a.allTokens)
	a.mu.RUnlock()

	scrub := collectScrubTokens(rawToken, baseScrub...)

	idx := slices.IndexFunc(cur, func(d storedDevice) bool { return d.ID == id })
	if idx < 0 {
		return deviceEntry{}, fmt.Errorf("device %q not found", scrubSecrets(id, scrub))
	}
	old := cur[idx]

	name, err := normalizeRequiredDeviceName(rawName)
	if err != nil {
		return deviceEntry{}, errors.New(scrubSecrets(err.Error(), scrub))
	}
	if hasDeviceNameConflict(cur, name, id) {
		return deviceEntry{}, fmt.Errorf("device name %q already exists; choose a different name", scrubSecrets(name, scrub))
	}
	base, err := normalizeInstanceURL(rawURL)
	if err != nil {
		return deviceEntry{}, errors.New(scrubSecrets(err.Error(), scrub))
	}

	tok := old.Token
	if trimmed := strings.TrimSpace(rawToken); trimmed != "" {
		tok, err = validateDeviceToken(trimmed)
		if err != nil {
			return deviceEntry{}, errors.New(scrubSecrets(err.Error(), scrub))
		}
	}

	next := slices.Clone(cur)
	next[idx].Name = name
	next[idx].URL = base
	next[idx].Token = tok
	if makeDefault {
		for i := range next {
			next[i].Default = (i == idx)
		}
	} else if old.Default {
		next[idx].Default = false
		if len(next) == 1 {
			next[idx].Default = true
		} else {
			// Promote the next device when the user explicitly unchecks default on the current default.
			next[(idx+1)%len(next)].Default = true
		}
	}

	clearHealth := ""
	if base != old.URL || tok != old.Token {
		clearHealth = id
	}
	if err := a.commitVault(ctx, next, []string{tok, old.Token}, clearHealth); err != nil {
		return deviceEntry{}, err
	}
	dev, _ := a.snapshot().deviceByID(id)
	return dev, nil
}

func (a *app) setDefaultDevice(ctx context.Context, id string) (deviceEntry, error) {
	a.vaultMu.Lock()
	defer a.vaultMu.Unlock()

	a.mu.RLock()
	cur := slices.Clone(a.stored)
	baseScrub := slices.Clone(a.allTokens)
	a.mu.RUnlock()

	idx := slices.IndexFunc(cur, func(d storedDevice) bool { return d.ID == id })
	if idx < 0 {
		return deviceEntry{}, fmt.Errorf("device %q not found", scrubSecrets(id, baseScrub))
	}
	next := slices.Clone(cur)
	for i := range next {
		next[i].Default = (i == idx)
	}
	if err := a.commitVault(ctx, next, nil, ""); err != nil {
		return deviceEntry{}, err
	}
	dev, _ := a.snapshot().deviceByID(id)
	return dev, nil
}

func (a *app) deleteDevice(ctx context.Context, id string) (string, string, error) {
	a.vaultMu.Lock()
	defer a.vaultMu.Unlock()

	a.mu.RLock()
	cur := slices.Clone(a.stored)
	baseScrub := slices.Clone(a.allTokens)
	a.mu.RUnlock()

	idx := slices.IndexFunc(cur, func(d storedDevice) bool { return d.ID == id })
	if idx < 0 {
		return "", "", fmt.Errorf("device %q not found", scrubSecrets(id, baseScrub))
	}
	removed := cur[idx]
	next := append(slices.Clone(cur[:idx]), cur[idx+1:]...)
	if err := a.commitVault(ctx, next, []string{removed.Token}, id); err != nil {
		return "", "", err
	}
	snap := a.snapshot()
	nextID := ""
	if len(snap.devices) > 0 {
		nextID = snap.devices[0].id
	}
	return removed.Name, nextID, nil
}
