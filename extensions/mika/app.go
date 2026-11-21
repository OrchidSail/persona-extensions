package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

const (
	connectorID = "mika"
	pageUI      = "mika"
)

type app struct {
	mu          sync.RWMutex
	client      *client
	defaultWait int
	hub         hubNotifier
	receipts    *writeStore
	lastOK      time.Time
	lastErr     error
}

type hubNotifier interface {
	Invalidate(string) error
}

type appState struct {
	client      *client
	defaultWait int
	hub         hubNotifier
	receipts    *writeStore
}

func newApp() *app {
	return &app{
		client:      newClient("", ""),
		defaultWait: defaultWaitSeconds,
		receipts:    newWriteStore(""),
	}
}

func (a *app) snapshot() appState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	wait := a.defaultWait
	if wait < minWaitSeconds || wait > maxWaitSeconds {
		wait = defaultWaitSeconds
	}
	return appState{
		client:      a.client,
		defaultWait: wait,
		hub:         a.hub,
		receipts:    a.receipts,
	}
}

func (a *app) init(_ context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
	var cfg struct {
		InstanceURL        string `json:"instance_url"`
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
	rawURL := strings.TrimSpace(cfg.InstanceURL)
	token := strings.TrimSpace(in.Secrets["api_token"])
	if rawURL == "" && token == "" {
		a.mu.Lock()
		a.defaultWait = wait
		a.hub = hub
		a.mu.Unlock()
		return nil, nil
	}
	base, err := normalizeInstanceURL(rawURL)
	if err != nil {
		return nil, err
	}
	store, err := openWriteStore(in.Info().DataDir)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.client = newClient(base, token)
	a.defaultWait = wait
	a.hub = hub
	a.receipts = store
	return nil, nil
}

func (a *app) record(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.lastOK, a.lastErr = time.Now().UTC(), nil
	} else {
		a.lastErr = err
	}
}

func (a *app) status() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	state := "ok"
	account := "Mika 手机助手"
	if !a.client.configured() {
		state = "not_configured"
	} else {
		account = "Mika (" + maskInstanceURL(a.client.base) + ")"
	}
	out := map[string]any{"state": state, "account": account}
	if a.lastErr != nil {
		out["error"] = clipText(scrubSecret(a.lastErr.Error(), a.client.token), 200)
		switch {
		case errors.Is(a.lastErr, errUnauthorized):
			out["state"] = "not_signed_in"
		case errors.Is(a.lastErr, errRateLimited):
			out["state"] = "rate_limited"
		case state == "ok":
			out["state"] = "error"
		}
	}
	if !a.lastOK.IsZero() {
		out["last_sync"] = a.lastOK
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
		return describeAction(in.Name, in.Args)
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
