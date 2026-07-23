package sink

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LogFunc is optional structured logger (printf style).
type LogFunc func(format string, args ...any)

// Config configures auto-upload to local/remote grok2api instances.
//
// Typical VPS layout:
//   - jiujiu (SSO only):   http://127.0.0.1:8000  Authorization: Bearer <ADMIN_PASSWORD>
//   - chenyme (SSO+Build): http://127.0.0.1:8001  admin login → multipart import
type Config struct {
	// jiujiu / classic grok2api — SSO only
	JiujiuEnabled bool
	JiujiuBase    string // e.g. http://127.0.0.1:8000
	JiujiuToken   string // admin password used as Bearer token
	JiujiuPool    string // default basic

	// chenyme grok2api — SSO (web) + Build OAuth JSON
	ChenymeEnabled     bool
	ChenymeBase        string // e.g. http://127.0.0.1:8001
	ChenymeUser        string
	ChenymePassword    string
	ChenymeUploadSSO   bool // POST /api/admin/v1/accounts/web/import
	ChenymeUploadBuild bool // POST /api/admin/v1/accounts/import (CPA/xai JSON)
}

func (c Config) AnyEnabled() bool {
	return c.JiujiuEnabled || c.ChenymeEnabled
}

// Uploader pushes SSO and/or Build OAuth files to configured grok2api sinks.
type Uploader struct {
	cfg    Config
	client *http.Client
	logf   LogFunc

	mu          sync.Mutex
	chenymeTok  string
	chenymeExp  time.Time
}

func New(cfg Config, logf LogFunc) *Uploader {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if cfg.JiujiuPool == "" {
		cfg.JiujiuPool = "basic"
	}
	if cfg.ChenymeEnabled {
		if !cfg.ChenymeUploadSSO && !cfg.ChenymeUploadBuild {
			// default: both when chenyme enabled
			cfg.ChenymeUploadSSO = true
			cfg.ChenymeUploadBuild = true
		}
	}
	return &Uploader{
		cfg: cfg,
		client: &http.Client{
			Timeout:   90 * time.Second,
			Transport: &http.Transport{Proxy: nil}, // local sinks — never use WARP
		},
		logf: logf,
	}
}

func (u *Uploader) Enabled() bool { return u != nil && u.cfg.AnyEnabled() }

// OnSSO is called right after registration succeeds (have SSO JWT).
func (u *Uploader) OnSSO(email, sso string) {
	if u == nil || !u.Enabled() || strings.TrimSpace(sso) == "" {
		return
	}
	if u.cfg.JiujiuEnabled {
		if err := u.uploadJiujiuSSO(sso); err != nil {
			u.logf("[g2a/jiujiu] SSO upload fail %s: %v", email, err)
		} else {
			u.logf("[g2a/jiujiu] SSO uploaded %s", email)
		}
	}
	if u.cfg.ChenymeEnabled && u.cfg.ChenymeUploadSSO {
		if err := u.uploadChenymeWebSSO(sso); err != nil {
			u.logf("[g2a/chenyme] web SSO import fail %s: %v", email, err)
		} else {
			u.logf("[g2a/chenyme] web SSO imported %s", email)
		}
	}
}

// OnBuildOAuth is called after CPA/xai JSON is written (Build OAuth credentials).
func (u *Uploader) OnBuildOAuth(email, filename string, rawJSON []byte) {
	if u == nil || !u.Enabled() || len(rawJSON) == 0 {
		return
	}
	if u.cfg.ChenymeEnabled && u.cfg.ChenymeUploadBuild {
		if filename == "" {
			filename = "xai.json"
		}
		if err := u.uploadChenymeBuild(filename, rawJSON); err != nil {
			u.logf("[g2a/chenyme] Build OAuth import fail %s: %v", email, err)
		} else {
			u.logf("[g2a/chenyme] Build OAuth imported %s (%s)", email, filename)
		}
	}
}

func (u *Uploader) uploadJiujiuSSO(sso string) error {
	base := strings.TrimRight(strings.TrimSpace(u.cfg.JiujiuBase), "/")
	if base == "" {
		return fmt.Errorf("jiujiu base empty")
	}
	if strings.TrimSpace(u.cfg.JiujiuToken) == "" {
		return fmt.Errorf("jiujiu admin token empty")
	}
	payload, _ := json.Marshal(map[string]any{
		"tokens": []string{sso},
		"pool":   u.cfg.JiujiuPool,
		"tags":   []string{"grok-register"},
	})
	req, err := http.NewRequest(http.MethodPost, base+"/admin/api/tokens/add", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+u.cfg.JiujiuToken)
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("http=%d body=%s", resp.StatusCode, truncate(string(body), 160))
	}
	// expect {"status":"success",...}
	var doc map[string]any
	_ = json.Unmarshal(body, &doc)
	if st, _ := doc["status"].(string); st != "" && st != "success" {
		return fmt.Errorf("status=%s body=%s", st, truncate(string(body), 160))
	}
	return nil
}

func (u *Uploader) chenymeLogin() (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.chenymeTok != "" && time.Now().Before(u.chenymeExp) {
		return u.chenymeTok, nil
	}
	base := strings.TrimRight(strings.TrimSpace(u.cfg.ChenymeBase), "/")
	if base == "" {
		return "", fmt.Errorf("chenyme base empty")
	}
	if u.cfg.ChenymeUser == "" || u.cfg.ChenymePassword == "" {
		return "", fmt.Errorf("chenyme user/password empty")
	}
	payload, _ := json.Marshal(map[string]string{
		"username": u.cfg.ChenymeUser,
		"password": u.cfg.ChenymePassword,
	})
	req, err := http.NewRequest(http.MethodPost, base+"/api/admin/v1/auth/login", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := u.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("login http=%d body=%s", resp.StatusCode, truncate(string(body), 160))
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", err
	}
	tok := extractNestedString(doc, "data", "tokens", "accessToken")
	if tok == "" {
		tok = extractNestedString(doc, "data", "accessToken")
	}
	if tok == "" {
		tok = extractNestedString(doc, "accessToken")
	}
	if tok == "" {
		return "", fmt.Errorf("login: no accessToken in response")
	}
	u.chenymeTok = tok
	// access token TTL is short (~15m); refresh a bit early
	u.chenymeExp = time.Now().Add(12 * time.Minute)
	return tok, nil
}

func (u *Uploader) uploadChenymeWebSSO(sso string) error {
	return u.chenymeMultipart("/api/admin/v1/accounts/web/import", "sso.txt", "text/plain", []byte(sso))
}

func (u *Uploader) uploadChenymeBuild(filename string, raw []byte) error {
	return u.chenymeMultipart("/api/admin/v1/accounts/import", filename, "application/json", raw)
}

func (u *Uploader) chenymeMultipart(path, filename, contentType string, content []byte) error {
	tok, err := u.chenymeLogin()
	if err != nil {
		return err
	}
	base := strings.TrimRight(strings.TrimSpace(u.cfg.ChenymeBase), "/")
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(content); err != nil {
		return err
	}
	_ = w.Close()

	req, err := http.NewRequest(http.MethodPost, base+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "text/event-stream, application/json")
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode == 401 {
		// force re-login once
		u.mu.Lock()
		u.chenymeTok = ""
		u.mu.Unlock()
		tok2, err := u.chenymeLogin()
		if err != nil {
			return err
		}
		var buf2 bytes.Buffer
		w2 := multipart.NewWriter(&buf2)
		part2, _ := w2.CreateFormFile("file", filename)
		_, _ = part2.Write(content)
		_ = w2.Close()
		req2, _ := http.NewRequest(http.MethodPost, base+path, &buf2)
		req2.Header.Set("Content-Type", w2.FormDataContentType())
		req2.Header.Set("Authorization", "Bearer "+tok2)
		req2.Header.Set("Accept", "text/event-stream, application/json")
		resp2, err := u.client.Do(req2)
		if err != nil {
			return err
		}
		defer resp2.Body.Close()
		body, _ = io.ReadAll(io.LimitReader(resp2.Body, 2<<20))
		if resp2.StatusCode >= 300 {
			return fmt.Errorf("http=%d body=%s", resp2.StatusCode, truncate(string(body), 200))
		}
		return parseSSEComplete(body)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("http=%d body=%s", resp.StatusCode, truncate(string(body), 200))
	}
	return parseSSEComplete(body)
}

func parseSSEComplete(body []byte) error {
	// SSE: event: complete\ndata: {"created":1,...}
	text := string(body)
	if strings.Contains(text, "authImportFailed") || strings.Contains(text, "\"event\":\"error\"") {
		return fmt.Errorf("import failed: %s", truncate(text, 200))
	}
	// success if complete event or empty-ish success JSON
	if strings.Contains(text, "event: complete") || strings.Contains(text, `"created"`) || strings.Contains(text, `"updated"`) {
		return nil
	}
	// some versions may return plain JSON
	var doc map[string]any
	if json.Unmarshal(body, &doc) == nil {
		if _, ok := doc["error"]; ok {
			return fmt.Errorf("import error: %s", truncate(text, 200))
		}
		return nil
	}
	// accept 200 with connected stream even if parsing is odd
	if strings.Contains(text, ": connected") {
		return nil
	}
	return nil
}

func extractNestedString(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
