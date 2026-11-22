package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

type fakeHub struct {
	mu          sync.Mutex
	secrets     map[string][]byte
	invalidated []string
	loadErr     error
	saveErr     error
	saveCalls   int
}

func newFakeHub() *fakeHub {
	return &fakeHub{secrets: map[string][]byte{}}
}

func (h *fakeHub) Invalidate(ui string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.invalidated = append(h.invalidated, ui)
	return nil
}

func (h *fakeHub) LoadSecret(_ context.Context, contribution string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loadErr != nil {
		return nil, h.loadErr
	}
	return slices.Clone(h.secrets[contribution]), nil
}

func (h *fakeHub) SaveSecret(_ context.Context, contribution string, data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.saveErr != nil {
		return h.saveErr
	}
	h.saveCalls++
	h.secrets[contribution] = slices.Clone(data)
	return nil
}

func (h *fakeHub) rawSecret(contribution string) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.secrets[contribution])
}

func TestDynamicDeviceCRUDAndHubVault(t *testing.T) {
	hub := newFakeHub()
	dataDir := t.TempDir()
	a, err := initTestAppWithHub(t, map[string]any{}, nil, dataDir, hub)
	if err != nil {
		t.Fatalf("fresh 0.3.0 init failed: %v", err)
	}
	if st := a.status(); st["state"] != "not_configured" {
		t.Fatalf("fresh status = %+v, want not_configured", st)
	}

	// Empty state offers "+ 添加设备" push subpage button and validates cleanly.
	emptyView, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}})
	if err != nil || wire.CheckView(emptyView) != nil {
		t.Fatalf("empty view invalid: err=%v check=%v", err, wire.CheckView(emptyView))
	}
	emptyItem := emptyView.Sections[0].Items[0]
	if emptyItem.Type != wire.ComponentEmpty || emptyItem.Button == nil || emptyItem.Button.Action.Push == nil {
		t.Fatalf("empty state missing push add_device button: %+v", emptyItem)
	}

	// Render Add Device subpage across locales and UI versions: token field must be kind:secret and NEVER filled in.
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, uiVer := range []int{1, 2, 3, 6} {
			addView, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
				Locale:    locale,
				UIVersion: uiVer,
				State:     emptyItem.Button.Action.Push.State,
			}})
			if err != nil || wire.CheckView(addView) != nil {
				t.Fatalf("add_device view %s v%d invalid: err=%v check=%v", locale, uiVer, err, wire.CheckView(addView))
			}
			if addView.RefreshAfter != 0 {
				t.Fatalf("add_device form must not auto-refresh, got RefreshAfter=%d", addView.RefreshAfter)
			}
			var foundSecret, foundDefault bool
			for _, s := range addView.Sections {
				for _, c := range s.Items {
					if c.Type == wire.ComponentField && c.Name == "token" {
						foundSecret = true
						if c.Kind != wire.FieldKindSecret || c.Value != "" {
							t.Fatalf("token field must be empty secret, got kind=%q value=%q", c.Kind, c.Value)
						}
					}
					if c.Type == wire.ComponentField && c.Name == "default" {
						foundDefault = true
						if c.Kind != wire.FieldKindToggle || c.Value != "true" {
							t.Fatalf("first device default toggle should be true, got kind=%q value=%q", c.Kind, c.Value)
						}
					}
				}
			}
			if !foundSecret || !foundDefault {
				t.Fatalf("add_device %s v%d missing expected fields (secret=%v default=%v)", locale, uiVer, foundSecret, foundDefault)
			}
		}
	}

	// Validation errors must reject invalid inputs, never call SaveSecret, and never leak submitted secrets.
	submittedSecret := "synthetic-rejected-form-secret-998877"
	badAdds := []map[string]string{
		{"name": "   ", "url": "http://127.0.0.1:8765", "token": submittedSecret, "default": "true"},
		{"name": strings.Repeat("a", maxDeviceName+1), "url": "http://127.0.0.1:8765", "token": submittedSecret, "default": "true"},
		{"name": "Bad\nName", "url": "http://127.0.0.1:8765", "token": submittedSecret, "default": "true"},
		{"name": "A16", "url": "http://127.0.0.1:8765/mcp", "token": submittedSecret, "default": "true"},
		{"name": "A16", "url": "http://public.example.com:8765", "token": submittedSecret, "default": "true"},
		{"name": "A16", "url": "https://user:pass@mika.example.com", "token": submittedSecret, "default": "true"},
		{"name": "A16", "url": "http://127.0.0.1:8765?tok=" + submittedSecret, "token": submittedSecret, "default": "true"},
		{"name": "A16", "url": "http://127.0.0.1:8765", "token": "   ", "default": "true"},
	}
	for i, vals := range badAdds {
		res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "zh-Hans", UIVersion: 6, Action: "save_add",
			State:  ext.EncodeState(pageState{Mode: "add_device"}),
			Values: vals,
		}})
		if err != nil {
			t.Fatalf("badAdd[%d] returned RPC error instead of form toast: %v", i, err)
		}
		if res.Pop || res.Toast == "" {
			t.Fatalf("badAdd[%d] should keep form open with toast, got %+v", i, res)
		}
		if strings.Contains(res.Toast, submittedSecret) {
			t.Fatalf("badAdd[%d] leaked submitted token in toast: %s", i, res.Toast)
		}
	}
	if hub.saveCalls != 0 {
		t.Fatalf("invalid add attempts should not write hub secret, got %d saves", hub.saveCalls)
	}

	// Add 6 distinct devices (proving the old 3-device limit is removed!).
	peers := make([]*fakeMika, 6)
	names := []string{"A16", "S25+", "Pixel9", "Xiaomi15", "OnePlus13", "HonorMagic"}
	tokens := make([]string, 6)
	for i := range 6 {
		tokens[i] = fmt.Sprintf("synthetic-dyn-token-%d-secret-xyz", i+1)
		peers[i] = newFakeMikaWith(t, tokens[i], fmt.Sprintf("0.3.%d", i), sampleTasks())
		res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "zh-Hans", UIVersion: 6, Action: "save_add",
			State: ext.EncodeState(pageState{Mode: "add_device"}),
			Values: map[string]string{
				"name":    names[i],
				"url":     peers[i].srv.URL,
				"token":   tokens[i],
				"default": strconv.FormatBool(i == 0),
			},
		}})
		if err != nil || !res.Pop || !strings.Contains(res.Toast, names[i]) {
			t.Fatalf("add device %s failed: err=%v res=%+v", names[i], err, res)
		}
	}
	if len(a.snapshot().devices) != 6 {
		t.Fatalf("expected 6 dynamic devices, got %d", len(a.snapshot().devices))
	}

	// Verify DataDir contains NO plaintext device file or tokens.
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		raw, _ := os.ReadFile(filepath.Join(dataDir, ent.Name()))
		for _, tok := range tokens {
			if strings.Contains(string(raw), tok) {
				t.Fatalf("token leaked into DataDir file %s", ent.Name())
			}
		}
	}

	// Verify hub secret contains all 6 devices encrypted via SaveSecret.
	vault, err := decodeDeviceVault(hub.rawSecret(connectorID), nil)
	if err != nil || len(vault.Devices) != 6 || !vault.MigratedLegacy {
		t.Fatalf("hub vault unexpected: err=%v len=%d migrated=%v", err, len(vault.Devices), vault.MigratedLegacy)
	}

	// With 6 devices (>5), overview MUST omit ComponentSegmented (maxItems=5) and pass CheckView across UI versions & locales!
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, uiVer := range []int{1, 2, 3, 4, 6, 8} {
			v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: uiVer}})
			if err != nil || wire.CheckView(v) != nil {
				t.Fatalf("6-device overview %s v%d invalid: err=%v check=%v", locale, uiVer, err, wire.CheckView(v))
			}
			for _, sec := range v.Sections {
				for _, item := range sec.Items {
					if item.Type == wire.ComponentSegmented {
						t.Fatalf("6-device overview %s v%d must not emit segmented picker (>5 choices)", locale, uiVer)
					}
					// Verify every destructive button and row action has a non-empty native confirm prompt.
					if item.Type == wire.ComponentButton && item.Role == wire.ButtonRoleDestructive {
						if item.Action == nil || strings.TrimSpace(item.Action.Confirm) == "" {
							t.Fatalf("destructive button %q missing confirm prompt", item.Title)
						}
					}
					for _, ra := range item.Actions {
						if ra.Role == wire.ButtonRoleDestructive && strings.TrimSpace(ra.Action.Confirm) == "" {
							t.Fatalf("destructive row action %q missing confirm prompt", ra.Title)
						}
					}
				}
			}
		}
	}

	// Duplicate name (case-insensitive) must be rejected on add AND on edit.
	dupAdd, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "save_add",
		State:  ext.EncodeState(pageState{Mode: "add_device"}),
		Values: map[string]string{"name": "s25+", "url": peers[1].srv.URL, "token": "synthetic-dup-tok", "default": "false"},
	}})
	if err != nil || dupAdd.Pop || !strings.Contains(dupAdd.Toast, "already exists") {
		t.Fatalf("expected duplicate name rejection on add, got err=%v res=%+v", err, dupAdd)
	}

	s25Dev := a.snapshot().devices[1]
	dupEdit, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "save_edit:" + s25Dev.id,
		State:  ext.EncodeState(pageState{Mode: "device_edit", Device: s25Dev.id}),
		Values: map[string]string{"name": "a16", "url": peers[1].srv.URL, "token": "", "default": "false"},
	}})
	if err != nil || dupEdit.Pop || !strings.Contains(dupEdit.Toast, "already exists") {
		t.Fatalf("expected duplicate name rejection on edit, got err=%v res=%+v", err, dupEdit)
	}

	// Render edit subpage: token field MUST be empty (never echoed), URL pre-filled cleanly.
	editView, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6,
		State: ext.EncodeState(pageState{Mode: "device_edit", Device: s25Dev.id}),
	}})
	if err != nil || wire.CheckView(editView) != nil {
		t.Fatalf("edit view invalid: err=%v check=%v", err, wire.CheckView(editView))
	}
	editJSON, _ := json.Marshal(editView)
	for _, tok := range tokens {
		if strings.Contains(string(editJSON), tok) {
			t.Fatalf("edit view leaked token %q", tok)
		}
	}

	// Edit S25+ -> rename to "Galaxy S25+" and LEAVE TOKEN BLANK -> original Bearer token MUST be preserved!
	editKeepRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "save_edit:" + s25Dev.id,
		State: ext.EncodeState(pageState{Mode: "device_edit", Device: s25Dev.id}),
		Values: map[string]string{
			"name":    "Galaxy S25+",
			"url":     peers[1].srv.URL,
			"token":   "   ", // blank keeps existing token
			"default": "false",
		},
	}})
	if err != nil || !editKeepRes.Pop {
		t.Fatalf("edit keeping token failed: err=%v res=%+v", err, editKeepRes)
	}
	if docOut, err := a.callTool(t.Context(), "mika_doctor", json.RawMessage(`{"device":"Galaxy S25+"}`)); err != nil || !strings.Contains(docOut, "0.3.1") {
		t.Fatalf("doctor after blank-token edit failed (original token not preserved?): err=%v out=%s", err, docOut)
	}

	// Edit Galaxy S25+ WITH a new token -> rotates token, and old token remains scrubbed from errors!
	rotatedTok := "synthetic-rotated-s25-token-445566"
	peers[1].mu.Lock()
	peers[1].expectedToken = rotatedTok
	peers[1].mu.Unlock()
	editRotRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "save_edit:" + s25Dev.id,
		State: ext.EncodeState(pageState{Mode: "device_edit", Device: s25Dev.id}),
		Values: map[string]string{
			"name":    "Galaxy S25+",
			"url":     peers[1].srv.URL,
			"token":   rotatedTok,
			"default": "false",
		},
	}})
	if err != nil || !editRotRes.Pop {
		t.Fatalf("edit rotating token failed: err=%v res=%+v", err, editRotRes)
	}
	if docOut, err := a.callTool(t.Context(), "mika_doctor", json.RawMessage(`{"device":"Galaxy S25+"}`)); err != nil || !strings.Contains(docOut, "0.3.1") {
		t.Fatalf("doctor after rotated token failed: err=%v out=%s", err, docOut)
	}
	// Make peer echo BOTH old and new tokens in an error; verify Doctor toast scrubs BOTH!
	peers[1].mu.Lock()
	peers[1].forceStatus = http.StatusUnauthorized
	peers[1].forceErrCode = "access_denied"
	peers[1].forceErrMsg = "old=" + tokens[1] + " new=" + rotatedTok
	peers[1].mu.Unlock()
	testFailRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "test:" + s25Dev.id,
		State: ext.EncodeState(pageState{Mode: "device_edit", Device: s25Dev.id}),
	}})
	if err != nil || testFailRes.View != nil || testFailRes.Pop || !strings.Contains(testFailRes.Toast, "连接失败") {
		t.Fatalf("test connection from edit page unexpected: err=%v res=%+v", err, testFailRes)
	}
	if strings.Contains(testFailRes.Toast, tokens[1]) || strings.Contains(testFailRes.Toast, rotatedTok) {
		t.Fatalf("test connection toast leaked old or rotated token: %s", testFailRes.Toast)
	}
	// Restore peer 1 and test connection from overview (should return both view and online toast).
	peers[1].mu.Lock()
	peers[1].forceStatus = 0
	peers[1].mu.Unlock()
	testOKRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "test:" + s25Dev.id,
		State: ext.EncodeState(pageState{Mode: "overview", Device: s25Dev.id}),
	}})
	if err != nil || testOKRes.View == nil || wire.CheckView(testOKRes.View) != nil || !strings.Contains(testOKRes.Toast, "连接正常") || !strings.Contains(testOKRes.Toast, "v0.3.1") {
		t.Fatalf("test connection from overview unexpected: err=%v res=%+v", err, testOKRes)
	}

	// Set default -> Galaxy S25+ becomes #1 (default), so omitted / "default" / "primary" / "1" all route to it!
	defRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "set_default:" + s25Dev.id,
	}})
	if err != nil || defRes.View == nil || wire.CheckView(defRes.View) != nil || !strings.Contains(defRes.Toast, "Galaxy S25+") {
		t.Fatalf("set_default failed: err=%v res=%+v", err, defRes)
	}
	for _, sel := range []string{"", "default", "primary", "1", "galaxy s25+"} {
		args := `{}`
		if sel != "" {
			args = `{"device":"` + sel + `"}`
		}
		out, err := a.callTool(t.Context(), "mika_doctor", json.RawMessage(args))
		if err != nil || !strings.Contains(out, "Device: Galaxy S25+ (#1, default)") || !strings.Contains(out, "0.3.1") {
			t.Fatalf("selector %q did not route to new default Galaxy S25+: err=%v out=%s", sel, err, out)
		}
	}
	// 6th device still routable by 1-based index "6" and by name "honormagic".
	for _, sel := range []string{"6", "honormagic"} {
		out, err := a.callTool(t.Context(), "mika_doctor", json.RawMessage(`{"device":"`+sel+`"}`))
		if err != nil || !strings.Contains(out, "Device: HonorMagic (#6)") || !strings.Contains(out, "0.3.5") {
			t.Fatalf("selector %q did not route to 6th device HonorMagic: err=%v out=%s", sel, err, out)
		}
	}

	// Hub SaveSecret error leaves in-memory state completely intact and scrubs token from error!
	hub.mu.Lock()
	hub.saveErr = errors.New("hub db busy with " + rotatedTok)
	hub.mu.Unlock()
	if _, err := a.addDevice(t.Context(), "TempPhone", peers[0].srv.URL, "synthetic-fail-tok", false); err == nil || strings.Contains(err.Error(), rotatedTok) || strings.Contains(err.Error(), "synthetic-fail-tok") {
		t.Fatalf("expected scrubbed SaveSecret error, got %v", err)
	}
	if len(a.snapshot().devices) != 6 {
		t.Fatalf("failed SaveSecret mutated in-memory device count to %d", len(a.snapshot().devices))
	}
	hub.mu.Lock()
	hub.saveErr = nil
	hub.mu.Unlock()

	// Delete default device from edit_device subpage -> returns Pop: true and promotes next device to default!
	delEditRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "zh-Hans", UIVersion: 6, Action: "delete:" + s25Dev.id,
		State: ext.EncodeState(pageState{Mode: "device_edit", Device: s25Dev.id}),
	}})
	if err != nil || !delEditRes.Pop || !strings.Contains(delEditRes.Toast, "Galaxy S25+") {
		t.Fatalf("delete from edit page failed: err=%v res=%+v", err, delEditRes)
	}
	if devs := a.snapshot().devices; len(devs) != 5 || !devs[0].defaultDev || devs[0].name != "A16" {
		t.Fatalf("deleting default should promote A16 to default #1, got: %+v", devs)
	}

	// Delete all remaining devices from overview -> returns empty state view & saves migrated_legacy=true empty vault.
	for len(a.snapshot().devices) > 0 {
		id := a.snapshot().devices[0].id
		delRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "zh-Hans", UIVersion: 6, Action: "delete:" + id,
			State: ext.EncodeState(pageState{Mode: "overview", Device: id}),
		}})
		if err != nil || delRes.View == nil || wire.CheckView(delRes.View) != nil || delRes.Pop {
			t.Fatalf("delete %s from overview failed: err=%v res=%+v", id, err, delRes)
		}
	}
	if a.status()["state"] != "not_configured" {
		t.Fatalf("expected not_configured after deleting all devices, got %+v", a.status())
	}
	emptyVault, err := decodeDeviceVault(hub.rawSecret(connectorID), nil)
	if err != nil || len(emptyVault.Devices) != 0 || !emptyVault.MigratedLegacy {
		t.Fatalf("empty vault after full delete unexpected: err=%v vault=%+v", err, emptyVault)
	}
}

func TestZeroLossLegacyMigrationAndNoResurrection(t *testing.T) {
	fA16 := newFakeMikaWith(t, testToken, "0.1.28", sampleTasks())
	fS25 := newFakeMikaWith(t, testSecondaryToken, "0.2.1", secondarySampleTasks())
	hub := newFakeHub()
	dataDir := t.TempDir()

	// Exact 0.2.x saved config & secrets for existing A16 + S25+ user.
	legacyCfg := map[string]any{
		"instance_name":        "A16",
		"instance_url":         fA16.srv.URL,
		"secondary_name":       "S25+",
		"secondary_url":        fS25.srv.URL,
		"default_wait_seconds": 25,
	}
	legacySecrets := map[string]string{
		"api_token":           testToken,
		"secondary_api_token": testSecondaryToken,
	}

	// Verify 0.3.0 formal manifest is streamlined to the 2 global fields (no legacy 3-slot config or secrets).
	var rawManifest map[string]any
	if err := json.Unmarshal(manifest, &rawManifest); err != nil {
		t.Fatal(err)
	}
	props := rawManifest["configuration"].(map[string]any)["properties"].(map[string]any)
	if len(props) != 2 || props["default_wait_seconds"] == nil || props["require_confirmation"] == nil {
		t.Fatalf("0.3.0 formal manifest should only declare default_wait_seconds and require_confirmation, got %v", props)
	}

	// Step 1 (0.2.1 -> 0.2.9 transitional upgrade): automatically migrates A16 + S25+ to Hub.SaveSecret!
	a1, err := initTestAppWithHub(t, legacyCfg, legacySecrets, dataDir, hub)
	if err != nil {
		t.Fatalf("upgrade init failed: %v", err)
	}
	if hub.saveCalls != 1 {
		t.Fatalf("expected 1 SaveSecret call during legacy migration, got %d", hub.saveCalls)
	}
	for _, tc := range []struct {
		dev, ver string
	}{{"A16", "0.1.28"}, {"S25+", "0.2.1"}} {
		out, err := a1.callTool(t.Context(), "mika_doctor", json.RawMessage(`{"device":"`+tc.dev+`"}`))
		if err != nil || !strings.Contains(out, tc.ver) {
			t.Fatalf("migrated device %s doctor failed without re-entering token: err=%v out=%s", tc.dev, err, out)
		}
	}

	// Step 2 (0.2.9 -> 0.3.0 after clearing legacy config/secrets): boots with ONLY 0.3.0 global settings and
	// zero legacy secrets, yet keeps both migrated A16 and S25+ intact from Hub.LoadSecret!
	saveCallsBefore030 := hub.saveCalls
	a030, err := initTestAppWithHub(t, map[string]any{
		"default_wait_seconds": 25,
		"require_confirmation": true,
	}, nil, dataDir, hub)
	if err != nil {
		t.Fatalf("0.3.0 post-migration init failed: %v", err)
	}
	if hub.saveCalls != saveCallsBefore030 {
		t.Fatalf("0.3.0 boot should load vault without re-saving (saves %d -> %d)", saveCallsBefore030, hub.saveCalls)
	}
	for _, tc := range []struct {
		dev, ver string
	}{{"A16", "0.1.28"}, {"S25+", "0.2.1"}} {
		out, err := a030.callTool(t.Context(), "mika_doctor", json.RawMessage(`{"device":"`+tc.dev+`"}`))
		if err != nil || !strings.Contains(out, tc.ver) {
			t.Fatalf("0.3.0 post-migration device %s doctor failed: err=%v out=%s", tc.dev, err, out)
		}
	}

	// Rename A16 -> A16-Main via dynamic edit, then simulate restart with legacyCfg STILL in Hub DB.
	a16ID := a030.snapshot().devices[0].id
	if _, err := a030.editDevice(t.Context(), a16ID, "A16-Main", fA16.srv.URL, "", true); err != nil {
		t.Fatalf("rename migrated A16 failed: %v", err)
	}
	saveCallsBeforeRestart := hub.saveCalls
	a2, err := initTestAppWithHub(t, legacyCfg, legacySecrets, dataDir, hub)
	if err != nil {
		t.Fatalf("restart init failed: %v", err)
	}
	if hub.saveCalls != saveCallsBeforeRestart {
		t.Fatalf("restart should load existing vault without re-migrating (saves %d -> %d)", saveCallsBeforeRestart, hub.saveCalls)
	}
	if devs := a2.snapshot().devices; len(devs) != 2 || devs[0].name != "A16-Main" || devs[1].name != "S25+" {
		t.Fatalf("restarted devices unexpected: %+v", devs)
	}

	// Delete ALL migrated devices and restart again with legacyCfg & legacySecrets still present:
	// migrated_legacy=true must prevent resurrecting deleted devices!
	for _, d := range a2.snapshot().devices {
		if _, _, err := a2.deleteDevice(t.Context(), d.id); err != nil {
			t.Fatalf("delete %s failed: %v", d.name, err)
		}
	}
	a3, err := initTestAppWithHub(t, legacyCfg, legacySecrets, dataDir, hub)
	if err != nil {
		t.Fatalf("restart after deleting all migrated devices failed: %v", err)
	}
	if len(a3.snapshot().devices) != 0 {
		t.Fatalf("deleted legacy devices resurrected on restart: %+v", a3.snapshot().devices)
	}
}

func TestRequireConfirmationSwitchAndZeroValueSafety(t *testing.T) {
	// Zero-value struct safety: uninitialized app{} MUST require confirmation (skipConfirmation == false).
	var zeroApp app
	if zeroApp.snapshot().skipConfirmation {
		t.Fatal("zero-value app must require confirmation")
	}
	res, err := zeroApp.connector(t.Context(), "tools.list", json.RawMessage(`{"contribution":"mika"}`))
	if err != nil || len(res.(map[string]any)["tools"].([]tool)) != 7 {
		t.Fatalf("zero-value tools.list unexpected: err=%v res=%+v", err, res)
	}
	res, err = zeroApp.connector(t.Context(), "actions.list", json.RawMessage(`{"contribution":"mika"}`))
	if err != nil || len(res.(map[string]any)["tools"].([]tool)) != 4 {
		t.Fatalf("zero-value actions.list unexpected: err=%v res=%+v", err, res)
	}
	if _, err := zeroApp.connector(t.Context(), "tools.call", json.RawMessage(`{"contribution":"mika","name":"mika_run_task","args":{"goal":"test"}}`)); err == nil || !strings.Contains(err.Error(), "require_confirmation") {
		t.Fatalf("zero-value app allowed direct write tool call: %v", err)
	}

	f1 := newFakeMikaWith(t, testToken, "0.1.28", sampleTasks())
	f2 := newFakeMikaWith(t, testSecondaryToken, "0.2.1", secondarySampleTasks())
	baseCfg := func(extra map[string]any) map[string]any {
		m := map[string]any{
			"instance_name":  "A16",
			"instance_url":   f1.srv.URL,
			"secondary_name": "S25+",
			"secondary_url":  f2.srv.URL,
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	secs := map[string]string{"api_token": testToken, "secondary_api_token": testSecondaryToken}

	// Default (omitted) and explicit true: 7 read tools + 4 confirmed actions; direct tools.call refused.
	for _, cfg := range []map[string]any{baseCfg(nil), baseCfg(map[string]any{"require_confirmation": true})} {
		a, err := initTestApp(t, cfg, secs, t.TempDir())
		if err != nil {
			t.Fatalf("init failed: %v", err)
		}
		if a.snapshot().skipConfirmation {
			t.Fatalf("expected skipConfirmation=false for cfg=%v", cfg)
		}
		tRes, _ := a.connector(t.Context(), "tools.list", json.RawMessage(`{"contribution":"mika"}`))
		aRes, _ := a.connector(t.Context(), "actions.list", json.RawMessage(`{"contribution":"mika"}`))
		if len(tRes.(map[string]any)["tools"].([]tool)) != 7 || len(aRes.(map[string]any)["tools"].([]tool)) != 4 {
			t.Fatalf("unexpected tool/action split when require_confirmation=true")
		}
		for _, writeName := range []string{"mika_run_task", "mika_answer", "mika_steer_task", "mika_cancel_task"} {
			if _, err := a.connector(t.Context(), "tools.call", json.RawMessage(`{"contribution":"mika","name":"`+writeName+`","args":{}}`)); err == nil {
				t.Fatalf("direct tools.call(%s) should be refused when require_confirmation=true", writeName)
			}
		}
	}

	// Explicit false: all 11 tools in tools.list, 0 in actions.list, direct tools.call executes on targeted device!
	directApp, err := initTestApp(t, baseCfg(map[string]any{"require_confirmation": false}), secs, t.TempDir())
	if err != nil {
		t.Fatalf("init require_confirmation=false failed: %v", err)
	}
	if !directApp.snapshot().skipConfirmation {
		t.Fatal("expected skipConfirmation=true when require_confirmation=false")
	}
	tRes, err := directApp.connector(t.Context(), "tools.list", json.RawMessage(`{"contribution":"mika"}`))
	if err != nil || len(tRes.(map[string]any)["tools"].([]tool)) != 11 {
		t.Fatalf("expected 11 tools when require_confirmation=false, got err=%v res=%+v", err, tRes)
	}
	aRes, err := directApp.connector(t.Context(), "actions.list", json.RawMessage(`{"contribution":"mika"}`))
	if err != nil || len(aRes.(map[string]any)["tools"].([]tool)) != 0 {
		t.Fatalf("expected 0 actions when require_confirmation=false, got err=%v res=%+v", err, aRes)
	}

	directCalls := []struct {
		name string
		args string
		want string
	}{
		{"mika_run_task", `{"goal":"Check S25+ calendar","device":"S25+"}`, "S25+ (#2)"},
		{"mika_answer", `{"task_id":"s25-task-ask-2","text":"Yes","device":"S25+"}`, "accepted: true"},
		{"mika_steer_task", `{"task_id":"s25-task-run-3","text":"Skip drafts","device":"2"}`, "msg-steer-42"},
		{"mika_cancel_task", `{"task_id":"task-run-2","device":"A16"}`, "accepted: true"},
	}
	for _, tc := range directCalls {
		rawParams, _ := json.Marshal(map[string]any{
			"contribution": "mika",
			"name":         tc.name,
			"args":         json.RawMessage(tc.args),
		})
		out, err := directApp.connector(t.Context(), "tools.call", rawParams)
		if err != nil {
			t.Fatalf("direct tools.call(%s) failed: %v", tc.name, err)
		}
		text := out.(map[string]any)["text"].(string)
		if !strings.Contains(text, tc.want) {
			t.Fatalf("direct tools.call(%s) missing %q in:\n%s", tc.name, tc.want, text)
		}
	}
	f1.mu.Lock()
	f1Cancels := f1.cancelCount
	f1.mu.Unlock()
	f2.mu.Lock()
	f2Starts, f2Answers, f2Steers := f2.startCount, f2.answerCount, f2.steerCount
	f2.mu.Unlock()
	if f1Cancels != 1 || f2Starts != 1 || f2Answers != 1 || f2Steers != 1 {
		t.Fatalf("direct write dispatch counts mismatch: f1Cancels=%d f2=(%d,%d,%d)", f1Cancels, f2Starts, f2Answers, f2Steers)
	}
}

func TestEndToEndHubRPCVaultAndConformance(t *testing.T) {
	f1 := newFakeMikaWith(t, testToken, "0.1.28", sampleTasks())
	f2 := newFakeMikaWith(t, testSecondaryToken, "0.2.1", secondarySampleTasks())
	f3 := newFakeMikaWith(t, testTertiaryToken, "0.2.2", tertiarySampleTasks())

	// 1. Full protocol conformance with require_confirmation=false (11 tools, empty actions.list).
	exttest.Check(t, manifest, exttest.Serve(t, program(newApp())), json.RawMessage(`{"require_confirmation":false}`))

	// 2. End-to-end RPC host: boot with legacy 0.2.x A16 + S25+ -> migrates via connector.secrets.save ->
	//    add 3rd device via ui.action ("save_add") -> verifies RPC secret updated and older UI v2 push/pop fallback!
	legacyCfg, _ := json.Marshal(map[string]any{
		"instance_name":  "A16",
		"instance_url":   f1.srv.URL,
		"secondary_name": "S25+",
		"secondary_url":  f2.srv.URL,
	})
	host := exttest.StartWith(t, manifest, exttest.Serve(t, program(newApp())), func(p *wire.InitializeParams) {
		p.Config = legacyCfg
		p.Secrets = map[string]string{
			"api_token":           testToken,
			"secondary_api_token": testSecondaryToken,
		}
	})

	// Verify connector.secrets.save persisted the migrated A16 + S25+ vault over JSON-RPC.
	vault, err := decodeDeviceVault(host.Secret(connectorID), nil)
	if err != nil || len(vault.Devices) != 2 || vault.Devices[0].Name != "A16" || vault.Devices[1].Name != "S25+" {
		t.Fatalf("RPC migrated secret unexpected: err=%v vault=%+v", err, vault)
	}

	// Render overview on UI v6 via RPC, push add_device, submit save_add for Pixel9.
	var ov wire.View
	if err := host.Call("ui.render", wire.UIParams{Contribution: pageUI, Locale: "zh-Hans", UIVersion: 6}, &ov); err != nil {
		t.Fatalf("RPC ui.render failed: %v", err)
	}
	if err := wire.CheckView(&ov); err != nil {
		t.Fatalf("RPC overview CheckView failed: %v", err)
	}

	var addRes wire.ActionResult
	if err := host.Call("ui.action", wire.UIParams{
		Contribution: pageUI, Locale: "zh-Hans", UIVersion: 6,
		Action: "save_add",
		State:  ext.EncodeState(pageState{Mode: "add_device"}),
		Values: map[string]string{
			"name":    "Pixel9",
			"url":     f3.srv.URL,
			"token":   testTertiaryToken,
			"default": "false",
		},
	}, &addRes); err != nil {
		t.Fatalf("RPC save_add failed: %v", err)
	}
	if !addRes.Pop || !strings.Contains(addRes.Toast, "Pixel9") {
		t.Fatalf("RPC save_add result unexpected: %+v", addRes)
	}
	vault, err = decodeDeviceVault(host.Secret(connectorID), nil)
	if err != nil || len(vault.Devices) != 3 {
		t.Fatalf("RPC vault after adding 3rd device unexpected: err=%v len=%d", err, len(vault.Devices))
	}

	// Verify UI v2 older-app compatibility: ext SDK rewrites push action and converts Pop: true into a rendered overview!
	var v2View wire.View
	if err := host.Call("ui.render", wire.UIParams{Contribution: pageUI, Locale: "zh-Hans", UIVersion: 2}, &v2View); err != nil {
		t.Fatalf("RPC v2 ui.render failed: %v", err)
	}
	var addBtnID string
	for _, sec := range v2View.Sections {
		for _, item := range sec.Items {
			if item.Key == "btn-add-device" && item.Action != nil {
				addBtnID = item.Action.ID
			}
		}
	}
	if addBtnID == "" {
		t.Fatalf("v2 view missing btn-add-device")
	}
	var pushRes wire.ActionResult
	if err := host.Call("ui.action", wire.UIParams{
		Contribution: pageUI, Locale: "zh-Hans", UIVersion: 2, Action: addBtnID, State: v2View.State,
	}, &pushRes); err != nil {
		t.Fatalf("RPC v2 push action failed: %v", err)
	}
	if pushRes.View == nil || wire.CheckView(pushRes.View) != nil {
		t.Fatalf("RPC v2 push did not render add form view: %+v", pushRes)
	}

	// Verify connector.status over RPC reflects all 3 devices.
	var st wire.ConnectorStatus
	if err := host.Call("connector.status", wire.ConnectorParams{Contribution: connectorID}, &st); err != nil {
		t.Fatalf("RPC connector.status failed: %v", err)
	}
	if st.State != wire.ConnectorStateOk || !strings.Contains(st.Account, "Pixel9") {
		t.Fatalf("RPC connector.status unexpected: %+v", st)
	}
}
