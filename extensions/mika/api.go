package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	defaultRequestTimeout = 20 * time.Second
	defaultWaitSeconds    = 30
	minWaitSeconds        = 1
	maxWaitSeconds        = 50
	waitTimeoutSlack      = 15 * time.Second
	defaultListLimit      = 10
	maxListLimit          = 50
	maxGoalChars          = 4000
	maxAnswerChars        = 2000
	maxSteerChars         = 4000
	maxJSONBytes          = 4 << 20
	maxExportBytes        = 8 << 20
	clientHeaderName      = "X-Mika-Client"
	clientHeaderValue     = "persona-mika"
)

var (
	errNotConfigured   = errors.New("no Mika device is configured; open the Mika page to add an Android phone")
	errInvalidArgument = errors.New("Mika rejected the request")
	errUnauthorized    = errors.New("Mika rejected the access token (401); check the device token on the Mika page")
	errForbidden       = errors.New("Mika denied access (403)")
	errNotFound        = errors.New("Mika could not find that task or endpoint (404)")
	errConflict        = errors.New("Mika rejected the action for the current task state (409)")
	errGone            = errors.New("Mika task or run record is no longer available (410)")
	errRateLimited     = errors.New("Mika rate-limited requests (429); wait before retrying")
	errUnavailable     = errors.New("Mika could not be reached or refused the request (503)")
	errRedirect        = errors.New("Mika endpoint returned an HTTP redirect; redirects are disabled")
	errUnexpectedHTTP  = errors.New("Mika returned an unexpected HTTP status")
)

var (
	cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")
	ulaPrefix   = netip.MustParsePrefix("fc00::/7")
	taskIDRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	uuidRe      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	convIDRe    = regexp.MustCompile(`^mcp-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type apiError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter int
	Err        error
}

func (e *apiError) Error() string {
	base := e.Err.Error()
	if e.Status == http.StatusTooManyRequests && e.RetryAfter > 0 {
		if e.Message != "" {
			return fmt.Sprintf("Mika rate-limited requests (429, retry after %ds): %s", e.RetryAfter, e.Message)
		}
		return fmt.Sprintf("Mika rate-limited requests (429); wait %d seconds before retrying", e.RetryAfter)
	}
	if e.Message != "" {
		if e.Code != "" {
			return fmt.Sprintf("%s (%s): %s", base, e.Code, e.Message)
		}
		return fmt.Sprintf("%s: %s", base, e.Message)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s (%s)", base, e.Code)
	}
	return base
}

func (e *apiError) Unwrap() error { return e.Err }

type apiErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// normalizeInstanceURL validates and normalizes a Mika instance URL to its /api/v1 root.
// HTTPS is allowed for any host; plaintext HTTP is restricted to loopback and private/Tailscale targets.
func normalizeInstanceURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("Mika instance URL is required")
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("Mika instance URL must not contain control characters")
		}
	}
	if strings.ContainsAny(trimmed, "?#") {
		return "", errors.New("Mika instance URL must not contain query parameters or fragments")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("Mika instance URL must be a valid absolute HTTP(S) URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", errors.New("Mika instance URL must use http or https")
	}
	if u.User != nil {
		return "", errors.New("Mika instance URL must not contain embedded credentials")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("Mika instance URL must not contain query parameters or fragments")
	}
	host := u.Hostname()
	if u.Host == "" || host == "" {
		return "", errors.New("Mika instance URL must include a host")
	}
	if port := u.Port(); port != "" {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return "", errors.New("Mika instance URL has an invalid port")
		}
	}
	if u.Scheme == "http" && !isAllowedHTTPHost(host) {
		return "", errors.New("Mika requires HTTPS for public hosts; plaintext HTTP is allowed only for loopback, private LAN, Tailscale, .local or .ts.net hosts")
	}
	if strings.Contains(u.Path, `\`) {
		return "", errors.New("Mika instance URL path is invalid")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return "", errors.New("Mika instance URL path must not contain dot segments")
		}
	}
	p := strings.TrimRight(u.Path, "/")
	if p == "/mcp" || strings.HasSuffix(p, "/mcp") {
		return "", errors.New("Mika instance URL must use the server root or /api/v1, not /mcp")
	}
	if strings.Contains(p, "/api/v1/") {
		return "", errors.New("Mika instance URL must point to the server root or /api/v1")
	}
	switch {
	case p == "":
		u.Path = "/api/v1"
	case p == "/api/v1" || strings.HasSuffix(p, "/api/v1"):
		u.Path = p
	default:
		u.Path = p + "/api/v1"
	}
	u.RawPath = ""
	return u.String(), nil
}

// isAllowedHTTPHost reports whether plaintext HTTP is permitted for host.
func isAllowedHTTPHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h == "" {
		return false
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		if addr.Zone() != "" {
			if !addr.IsLinkLocalUnicast() {
				return false
			}
			addr = addr.WithZone("")
		}
		addr = addr.Unmap()
		if addr.IsUnspecified() || addr.IsMulticast() {
			return false
		}
		return addr.IsLoopback() || addr.IsPrivate() || cgnatPrefix.Contains(addr) || ulaPrefix.Contains(addr) || addr.IsLinkLocalUnicast()
	}
	if h == "localhost" || h == "host.persona.internal" {
		return true
	}
	if (strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".ts.net")) && validHostname(h) {
		return true
	}
	return false
}

func validHostname(h string) bool {
	if len(h) == 0 || len(h) > 253 || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// maskInstanceURL formats a configured URL without revealing full LAN/Tailscale IPs or subdomains.
func maskInstanceURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "(not configured)"
	}
	host := u.Hostname()
	port := u.Port()
	masked := maskHost(host)
	if strings.Contains(masked, ":") && !strings.HasPrefix(masked, "[") {
		masked = "[" + masked + "]"
	}
	if port != "" {
		masked += ":" + port
	}
	p := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api/v1")
	return u.Scheme + "://" + masked + p
}

func maskHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" || h == "localhost" || h == "host.persona.internal" {
		return h
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		if addr.IsLoopback() {
			return addr.String()
		}
		unmapped := addr.Unmap()
		if unmapped.Is4() {
			b := unmapped.As4()
			return fmt.Sprintf("%d.%d.*.*", b[0], b[1])
		}
		parts := strings.Split(unmapped.WithZone("").String(), ":")
		if len(parts) >= 2 {
			first := parts[0]
			last := parts[len(parts)-1]
			if first == "" {
				first = "0"
			}
			if last == "" {
				last = "0"
			}
			return first + ":***:" + last
		}
		return "***"
	}
	labels := strings.Split(h, ".")
	if len(labels) == 1 {
		return maskLabel(labels[0])
	}
	if len(labels) == 2 {
		return maskLabel(labels[0]) + "." + labels[1]
	}
	return maskLabel(labels[0]) + "." + strings.Join(labels[len(labels)-2:], ".")
}

func maskLabel(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return "***"
	}
	return string(r[:1]) + "***"
}

type client struct {
	base        string
	token       string
	scrubTokens []string
	http        *http.Client
}

func newClient(base, token string, extraScrubTokens ...string) *client {
	transport := &http.Transport{
		Proxy:                 nil, // direct LAN/Tailscale/loopback access without environment proxy interference
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	trimmedToken := strings.TrimSpace(token)
	allTokens := collectScrubTokens(trimmedToken, extraScrubTokens...)
	return &client{
		base:        strings.TrimRight(strings.TrimSpace(base), "/"),
		token:       trimmedToken,
		scrubTokens: allTokens,
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func collectScrubTokens(first string, more ...string) []string {
	seen := make(map[string]struct{}, 1+len(more))
	out := make([]string, 0, 1+len(more))
	add := func(raw string) {
		tok := strings.TrimSpace(raw)
		if tok == "" {
			return
		}
		if _, ok := seen[tok]; ok {
			return
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	add(first)
	for _, tok := range more {
		add(tok)
	}
	// Replace longer tokens first to avoid partial substring collisions.
	slices.SortFunc(out, func(a, b string) int {
		return len(b) - len(a)
	})
	return out
}

func (c *client) configured() bool {
	return c != nil && c.base != "" && c.token != ""
}

func (c *client) do(ctx context.Context, timeout time.Duration, method, path string, query url.Values, in any, expect []int, maxBytes int64, out any) error {
	if !c.configured() {
		return errNotConfigured
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return errors.New(scrubSecrets(err.Error(), c.scrubTokens))
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set(clientHeaderName, clientHeaderValue)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if query != nil {
		req.URL.RawQuery = query.Encode()
	}
	res, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: request timed out", errUnavailable)
		}
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return fmt.Errorf("%w: request canceled", errUnavailable)
		}
		return fmt.Errorf("%w: %s", errUnavailable, sanitizeNetError(err, c.scrubTokens))
	}
	defer res.Body.Close()

	data, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("%w: could not read response body", errUnavailable)
	}
	if int64(len(data)) > maxBytes {
		return errors.New("Mika returned an oversized response")
	}
	if slices.Contains(expect, res.StatusCode) {
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return errors.New("Mika returned invalid JSON")
		}
		return nil
	}
	return c.httpError(res, data)
}

func (c *client) httpError(res *http.Response, body []byte) error {
	if res.StatusCode >= 300 && res.StatusCode <= 399 {
		return errRedirect
	}
	var env apiErrorEnvelope
	_ = json.Unmarshal(body, &env)
	code := clipText(scrubSecrets(strings.TrimSpace(env.Error.Code), c.scrubTokens), 64)
	msg := clipText(scrubSecrets(strings.TrimSpace(env.Error.Message), c.scrubTokens), 300)

	switch res.StatusCode {
	case http.StatusBadRequest:
		return &apiError{Status: res.StatusCode, Code: code, Message: msg, Err: errInvalidArgument}
	case http.StatusUnauthorized:
		return &apiError{Status: res.StatusCode, Code: code, Message: msg, Err: errUnauthorized}
	case http.StatusForbidden:
		return &apiError{Status: res.StatusCode, Code: code, Message: msg, Err: errForbidden}
	case http.StatusNotFound:
		return &apiError{Status: res.StatusCode, Code: code, Message: msg, Err: errNotFound}
	case http.StatusConflict:
		return &apiError{Status: res.StatusCode, Code: code, Message: msg, Err: errConflict}
	case http.StatusGone:
		return &apiError{Status: res.StatusCode, Code: code, Message: msg, Err: errGone}
	case http.StatusTooManyRequests:
		retryAfter := 0
		if raw := strings.TrimSpace(res.Header.Get("Retry-After")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				retryAfter = n
			}
		}
		return &apiError{Status: res.StatusCode, Code: code, Message: msg, RetryAfter: retryAfter, Err: errRateLimited}
	case http.StatusServiceUnavailable:
		return &apiError{Status: res.StatusCode, Code: code, Message: msg, Err: errUnavailable}
	default:
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.StatusCode)
		} else {
			msg = fmt.Sprintf("HTTP %d: %s", res.StatusCode, msg)
		}
		return &apiError{Status: res.StatusCode, Code: code, Message: msg, Err: errUnexpectedHTTP}
	}
}

func sanitizeNetError(err error, tokens []string) string {
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		return clipText(scrubSecrets(uerr.Err.Error(), tokens), 200)
	}
	return clipText(scrubSecrets(err.Error(), tokens), 200)
}

func scrubSecrets(text string, tokens []string) string {
	if text == "" || len(tokens) == 0 {
		return text
	}
	out := text
	for _, tok := range tokens {
		if tok != "" {
			out = strings.ReplaceAll(out, tok, "[redacted]")
		}
	}
	return out
}

func scrubSecret(text, token string) string {
	if token == "" || text == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "[redacted]")
}

func validTaskID(id string) error {
	if !taskIDRe.MatchString(id) {
		return errors.New("task_id must match [A-Za-z0-9_-]{1,128}")
	}
	return nil
}

func validConversationID(id string) error {
	if !convIDRe.MatchString(id) {
		return errors.New("conversation_id must match mcp-<lowercase-uuid>")
	}
	return nil
}

func validRequestUUID(id string) error {
	if !uuidRe.MatchString(id) {
		return errors.New("request_id must be a canonical lowercase UUID")
	}
	return nil
}

func validTextInput(value, name string, maxChars int) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len([]rune(trimmed)) > maxChars {
		return "", fmt.Errorf("%s must be nonempty and at most %d characters", name, maxChars)
	}
	for _, r := range trimmed {
		if r == 0 || (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') {
			return "", fmt.Errorf("%s contains unsupported control characters", name)
		}
	}
	return trimmed, nil
}

func (c *client) discover(ctx context.Context) (discoveryInfo, error) {
	var out discoveryInfo
	err := c.do(ctx, defaultRequestTimeout, http.MethodGet, "", nil, nil, []int{http.StatusOK}, maxJSONBytes, &out)
	return out, err
}

func (c *client) deviceState(ctx context.Context) (deviceResponse, error) {
	var out deviceResponse
	err := c.do(ctx, defaultRequestTimeout, http.MethodGet, "/device", nil, nil, []int{http.StatusOK}, maxJSONBytes, &out)
	return out, err
}

func (c *client) startTask(ctx context.Context, req startTaskRequest) (remoteTask, error) {
	var out remoteTask
	err := c.do(ctx, defaultRequestTimeout, http.MethodPost, "/tasks", nil, req, []int{http.StatusCreated, http.StatusOK}, maxJSONBytes, &out)
	return out, err
}

func (c *client) taskStatus(ctx context.Context, id string) (remoteTask, error) {
	var out remoteTask
	if err := validTaskID(id); err != nil {
		return out, err
	}
	err := c.do(ctx, defaultRequestTimeout, http.MethodGet, "/tasks/"+url.PathEscape(id), nil, nil, []int{http.StatusOK}, maxJSONBytes, &out)
	return out, err
}

func (c *client) waitTask(ctx context.Context, id string, timeoutS int) (remoteTask, error) {
	var out remoteTask
	if err := validTaskID(id); err != nil {
		return out, err
	}
	if timeoutS < minWaitSeconds || timeoutS > maxWaitSeconds {
		return out, fmt.Errorf("timeout_s must be between %d and %d", minWaitSeconds, maxWaitSeconds)
	}
	reqTimeout := time.Duration(timeoutS)*time.Second + waitTimeoutSlack
	err := c.do(ctx, reqTimeout, http.MethodPost, "/tasks/"+url.PathEscape(id)+"/wait", nil, waitTaskRequest{TimeoutS: timeoutS}, []int{http.StatusOK}, maxJSONBytes, &out)
	return out, err
}

func (c *client) listTasks(ctx context.Context, limit int, before string) (taskListResponse, error) {
	var out taskListResponse
	if limit < 1 || limit > maxListLimit {
		return out, fmt.Errorf("limit must be between 1 and %d", maxListLimit)
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if before != "" {
		if len(before) > 256 {
			return out, errors.New("before cursor is too long")
		}
		q.Set("before", before)
	}
	err := c.do(ctx, defaultRequestTimeout, http.MethodGet, "/tasks", q, nil, []int{http.StatusOK}, maxJSONBytes, &out)
	return out, err
}

func (c *client) answerTask(ctx context.Context, id, text string) (answerResponse, error) {
	var out answerResponse
	if err := validTaskID(id); err != nil {
		return out, err
	}
	err := c.do(ctx, defaultRequestTimeout, http.MethodPost, "/tasks/"+url.PathEscape(id)+"/answer", nil, textRequest{Text: text}, []int{http.StatusOK}, maxJSONBytes, &out)
	return out, err
}

func (c *client) steerTask(ctx context.Context, id, text string) (steerResponse, error) {
	var out steerResponse
	if err := validTaskID(id); err != nil {
		return out, err
	}
	err := c.do(ctx, defaultRequestTimeout, http.MethodPost, "/tasks/"+url.PathEscape(id)+"/steer", nil, textRequest{Text: text}, []int{http.StatusAccepted, http.StatusOK}, maxJSONBytes, &out)
	return out, err
}

func (c *client) cancelTask(ctx context.Context, id string) (cancelResponse, error) {
	var out cancelResponse
	if err := validTaskID(id); err != nil {
		return out, err
	}
	// RemoteTaskApi.kt requires Content-Type: application/json and an empty JSON object {}.
	err := c.do(ctx, defaultRequestTimeout, http.MethodPost, "/tasks/"+url.PathEscape(id)+"/cancel", nil, struct{}{}, []int{http.StatusAccepted, http.StatusOK}, maxJSONBytes, &out)
	return out, err
}

func (c *client) exportTask(ctx context.Context, id string) (exportRecord, error) {
	var out exportRecord
	if err := validTaskID(id); err != nil {
		return out, err
	}
	err := c.do(ctx, defaultRequestTimeout, http.MethodGet, "/tasks/"+url.PathEscape(id)+"/export", nil, nil, []int{http.StatusOK}, maxExportBytes, &out)
	return out, err
}

func clipText(text string, limit int) string {
	r := []rune(text)
	if len(r) > limit {
		return string(r[:max(0, limit-1)]) + "…"
	}
	return text
}
