package clearance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

type Bundle struct {
	Cookies   []Cookie
	UserAgent string
	UpdatedAt time.Time
}

type Manager struct {
	FSURL   string
	Proxy   string // inside FS network, e.g. http://privoxy:8118
	URLs    []string
	Timeout time.Duration

	mu     sync.RWMutex
	bundle Bundle
}

func NewManager(fsURL, proxy, urlsCSV string) *Manager {
	var urls []string
	for _, u := range strings.Split(urlsCSV, ",") {
		u = strings.TrimSpace(u)
		if u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		urls = []string{"https://accounts.x.ai", "https://x.ai", "https://auth.x.ai"}
	}
	return &Manager{
		FSURL:   strings.TrimRight(fsURL, "/"),
		Proxy:   proxy,
		URLs:    urls,
		Timeout: 90 * time.Second,
	}
}

func (m *Manager) Get() Bundle {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.bundle
}

func (m *Manager) CookieHeader() string {
	b := m.Get()
	parts := make([]string, 0, len(b.Cookies))
	for _, c := range b.Cookies {
		if c.Name == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func (m *Manager) UserAgent() string {
	ua := m.Get().UserAgent
	if ua == "" {
		return "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	}
	return ua
}

type fsRequest struct {
	Cmd        string `json:"cmd"`
	URL        string `json:"url"`
	MaxTimeout int    `json:"maxTimeout"`
	Proxy      struct {
		URL string `json:"url,omitempty"`
	} `json:"proxy,omitempty"`
}

type fsResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Solution *struct {
		URL       string              `json:"url"`
		Status    int                 `json:"status"`
		Cookies   []map[string]any    `json:"cookies"`
		UserAgent string              `json:"userAgent"`
		Response  string              `json:"response"`
	} `json:"solution"`
}

func (m *Manager) Prewarm() (string, error) {
	client := &http.Client{Timeout: m.Timeout}
	var okN, failN int
	var parts []string
	var allCookies []Cookie
	ua := ""

	for _, u := range m.URLs {
		start := time.Now()
		res, err := m.solveOne(client, u)
		elapsed := time.Since(start).Seconds()
		if err != nil {
			failN++
			parts = append(parts, fmt.Sprintf("%s:ERR", hostOf(u)))
			continue
		}
		okN++
		cf := "cf-"
		for _, c := range res.Cookies {
			if c.Name == "cf_clearance" {
				cf = "cf+"
				break
			}
		}
		parts = append(parts, fmt.Sprintf("%s:%.1fs/%s", hostOf(u), elapsed, cf))
		allCookies = mergeCookies(allCookies, res.Cookies)
		if res.UserAgent != "" {
			ua = res.UserAgent
		}
		_ = res.Status
	}

	m.mu.Lock()
	m.bundle = Bundle{Cookies: allCookies, UserAgent: ua, UpdatedAt: time.Now()}
	m.mu.Unlock()

	msg := fmt.Sprintf("预热完成 cookies=%d fs=%s | %s", len(allCookies), m.FSURL, strings.Join(parts, " "))
	if okN == 0 {
		return msg, fmt.Errorf("clearance prewarm failed all hosts")
	}
	return msg, nil
}

// SolveResult is one FlareSolverr request.get outcome (cookies + UA + HTML body).
// HTML is the reliable source for signup config: cf_clearance is browser-bound and
// cannot be safely replayed by Go's plain HTTP client.
type SolveResult struct {
	Cookies   []Cookie
	UserAgent string
	Status    int
	HTML      string
}

func (m *Manager) Solve(target string) (SolveResult, error) {
	client := &http.Client{Timeout: m.Timeout}
	return m.solveOne(client, target)
}

func (m *Manager) solveOne(client *http.Client, target string) (SolveResult, error) {
	reqBody := fsRequest{
		Cmd:        "request.get",
		URL:        target,
		MaxTimeout: int(m.Timeout.Milliseconds()),
	}
	if m.Proxy != "" {
		reqBody.Proxy.URL = m.Proxy
	}
	raw, _ := json.Marshal(reqBody)
	httpReq, err := http.NewRequest(http.MethodPost, m.FSURL+"/v1", bytes.NewReader(raw))
	if err != nil {
		return SolveResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return SolveResult{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var fs fsResponse
	if err := json.Unmarshal(body, &fs); err != nil {
		return SolveResult{}, fmt.Errorf("fs json: %w", err)
	}
	if fs.Status != "ok" || fs.Solution == nil {
		return SolveResult{}, fmt.Errorf("fs status=%s msg=%s", fs.Status, fs.Message)
	}
	var cookies []Cookie
	for _, c := range fs.Solution.Cookies {
		name, _ := c["name"].(string)
		val, _ := c["value"].(string)
		dom, _ := c["domain"].(string)
		path, _ := c["path"].(string)
		if name == "" {
			continue
		}
		// Skip account SSO from clearance dumps.
		ln := strings.ToLower(name)
		if ln == "sso" || ln == "sso-rw" {
			continue
		}
		cookies = append(cookies, Cookie{Name: name, Value: val, Domain: dom, Path: path})
	}
	html := ""
	if fs.Solution != nil {
		html = fs.Solution.Response
	}
	return SolveResult{
		Cookies:   cookies,
		UserAgent: fs.Solution.UserAgent,
		Status:    fs.Solution.Status,
		HTML:      html,
	}, nil
}

func mergeCookies(base, add []Cookie) []Cookie {
	idx := map[string]int{}
	out := append([]Cookie{}, base...)
	for i, c := range out {
		idx[c.Name] = i
	}
	for _, c := range add {
		if i, ok := idx[c.Name]; ok {
			out[i] = c
		} else {
			idx[c.Name] = len(out)
			out = append(out, c)
		}
	}
	return out
}

func hostOf(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}
