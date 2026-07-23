package protocol

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/grok-free-register/grok-reg/internal/clearance"
)

const (
	SiteURL              = "https://accounts.x.ai"
	ConnectCreate        = SiteURL + "/auth_mgmt.AuthManagement/CreateEmailValidationCode"
	ConnectVerify        = SiteURL + "/auth_mgmt.AuthManagement/VerifyEmailValidationCode"
	SignupURLGrok        = SiteURL + "/sign-up?redirect=grok-com"
	DefaultUserAgent     = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
)

var (
	siteKeyRe  = regexp.MustCompile(`0x4AAAAAAA[a-zA-Z0-9_-]+`)
	jsSrcRe    = regexp.MustCompile(`src="(/_next/static/[^"]+\.js)"`)
	hex40Re    = regexp.MustCompile(`[a-fA-F0-9]{40,50}`)
	flightRe   = regexp.MustCompile(`self\.__next_f\.push\(\[1,"(.*?)"\]\)`)
)

type SignupConfig struct {
	SiteKey   string
	ActionID  string
	StateTree string
	Source    string
}

type Client struct {
	http    *http.Client
	proxy   string
	clear   *clearance.Manager
	ua      string
	mu      sync.Mutex
	cfg     SignupConfig
}

func NewClient(proxy string, cm *clearance.Manager) (*Client, error) {
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(u)
	}
	c := &Client{
		http: &http.Client{
			Timeout:   45 * time.Second,
			Jar:       jar,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 8 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
		proxy: proxy,
		clear: cm,
		ua:    DefaultUserAgent,
	}
	if cm != nil {
		c.ua = cm.UserAgent()
		c.applyClearanceCookies()
	}
	return c, nil
}

func (c *Client) applyClearanceCookies() {
	if c.clear == nil {
		return
	}
	b := c.clear.Get()
	u, _ := url.Parse(SiteURL)
	var cookies []*http.Cookie
	for _, ck := range b.Cookies {
		cookies = append(cookies, &http.Cookie{
			Name:   ck.Name,
			Value:  ck.Value,
			Domain: ck.Domain,
			Path:   ck.Path,
		})
	}
	if len(cookies) > 0 {
		c.http.Jar.SetCookies(u, cookies)
	}
	if b.UserAgent != "" {
		c.ua = b.UserAgent
	}
}

func (c *Client) Config() SignupConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

func (c *Client) FetchConfig() (SignupConfig, error) {
	// Cloudflare path MUST go through WARP+Privoxy+FlareSolverr.
	// cf_clearance is browser-fingerprint-bound: replaying cookies with Go's plain
	// HTTP client almost always yields 403. Prefer FlareSolverr HTML for site key /
	// state tree, then resolve Next.js Action ID from chunk JS (via FS, then proxy).
	var html string
	cfg := SignupConfig{}
	if c.clear != nil {
		res, err := c.clear.Solve(SignupURLGrok)
		if err != nil {
			return SignupConfig{}, fmt.Errorf("flaresolverr signup: %w", err)
		}
		// Refresh cookies/UA from the same browser session that produced HTML.
		if len(res.Cookies) > 0 || res.UserAgent != "" {
			c.mu.Lock()
			// apply into clearance bundle via local jar
			c.mu.Unlock()
			u, _ := url.Parse(SiteURL)
			var cookies []*http.Cookie
			for _, ck := range res.Cookies {
				cookies = append(cookies, &http.Cookie{
					Name: ck.Name, Value: ck.Value, Domain: ck.Domain, Path: ck.Path,
				})
			}
			if len(cookies) > 0 {
				c.http.Jar.SetCookies(u, cookies)
			}
			if res.UserAgent != "" {
				c.ua = res.UserAgent
			}
		}
		html = res.HTML
		cfg.Source = fmt.Sprintf("flaresolverr status=%d html=%d", res.Status, len(html))
		if res.Status != 200 || html == "" || isCloudflare(res.Status, html, http.Header{}) {
			return cfg, fmt.Errorf("signup page blocked via flaresolverr status=%d", res.Status)
		}
	} else {
		// Fallback: direct HTTP (usually fails under CF without clearance stack)
		c.applyClearanceCookies()
		req, err := http.NewRequest(http.MethodGet, SignupURLGrok, nil)
		if err != nil {
			return SignupConfig{}, err
		}
		c.setBrowserHeaders(req)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		req.Header.Set("Referer", "https://grok.com/")
		resp, err := c.http.Do(req)
		if err != nil {
			return SignupConfig{}, err
		}
		defer resp.Body.Close()
		html, err = readBody(resp)
		if err != nil {
			return SignupConfig{}, err
		}
		cfg.Source = fmt.Sprintf("http status=%d", resp.StatusCode)
		if resp.StatusCode != 200 || isCloudflare(resp.StatusCode, html, resp.Header) {
			cfg.Source += " (blocked_or_empty)"
			return cfg, fmt.Errorf("signup page blocked status=%d", resp.StatusCode)
		}
	}

	if m := siteKeyRe.FindString(html); m != "" {
		cfg.SiteKey = m
	}
	cfg.StateTree = scrapeStateTree(html)
	// Action ID lives in Next.js chunk JS. Static assets usually pass via WARP
	// proxy without needing a full FlareSolverr browser session per file.
	jsURLs := unique(jsSrcRe.FindAllStringSubmatch(html, -1))
	// Scan all chunks (typically ~40). Keep each fetch short; stop on first action hash.
	probed := 0
	var lastJSErr error
	for _, path := range jsURLs {
		if cfg.ActionID != "" {
			break
		}
		probed++
		js, err := c.fetchJS(path)
		if err != nil {
			lastJSErr = err
			continue
		}
		if js == "" {
			continue
		}
		// Prefer chunks that look like signup server-action modules.
		interesting := strings.Contains(js, "createUser") ||
			strings.Contains(js, "registerUser") ||
			strings.Contains(js, "emailValidation") ||
			strings.Contains(js, "createEmailValidation") ||
			strings.Contains(js, "createUserAndSession") ||
			strings.Contains(js, "signUp")
		if !interesting {
			continue
		}
		if hexes := hex40Re.FindAllString(js, -1); len(hexes) > 0 {
			cfg.ActionID = hexes[0]
			break
		}
	}
	_ = lastJSErr
	if cfg.SiteKey == "" || cfg.ActionID == "" || cfg.StateTree == "" {
		return cfg, fmt.Errorf("config incomplete site_key=%v action=%v state=%v source=%s js_probed=%d", cfg.SiteKey != "", cfg.ActionID != "", cfg.StateTree != "", cfg.Source, probed)
	}
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
	return cfg, nil
}

func (c *Client) fetchJS(path string) (string, error) {
	// Fetch JS over REGISTER_PROXY (WARP/Privoxy). Do NOT use FlareSolverr here:
	// each FS solve starts a browser and was causing multi-minute hangs.
	// Ensure clearance cookies/UA from prewarm/FS are attached.
	c.applyClearanceCookies()
	req, err := http.NewRequest(http.MethodGet, SiteURL+path, nil)
	if err != nil {
		return "", err
	}
	c.setBrowserHeaders(req)
	req.Header.Set("Referer", SignupURLGrok)
	req.Header.Set("Accept", "*/*")
	// Short timeout per chunk so a bad asset can't hang the whole config phase.
	client := *c.http
	client.Timeout = 12 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("js http=%d", resp.StatusCode)
	}
	body, err := readBody(resp)
	if err != nil {
		return "", err
	}
	// Cloudflare challenge HTML is not useful JS.
	if isCloudflare(resp.StatusCode, body, resp.Header) || strings.Contains(strings.ToLower(body), "just a moment") {
		return "", fmt.Errorf("js blocked by cloudflare")
	}
	return body, nil
}

func (c *Client) CreateEmailCode(email string) error {
	inner := pbStr(1, email)
	frame := grpcWebFrame(inner)
	req, err := http.NewRequest(http.MethodPost, ConnectCreate, bytes.NewReader(frame))
	if err != nil {
		return err
	}
	c.setGRPCHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	st := resp.Header.Get("grpc-status")
	if st == "" {
		st = "0"
	}
	if resp.StatusCode != 200 || (st != "0" && st != "") {
		return fmt.Errorf("create email http=%d grpc=%s", resp.StatusCode, st)
	}
	return nil
}

func (c *Client) VerifyEmailCode(email, code string) error {
	inner := append(pbStr(1, email), pbStr(2, code)...)
	frame := grpcWebFrame(inner)
	req, err := http.NewRequest(http.MethodPost, ConnectVerify, bytes.NewReader(frame))
	if err != nil {
		return err
	}
	c.setGRPCHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	st := resp.Header.Get("grpc-status")
	if st == "" {
		st = "0"
	}
	if resp.StatusCode != 200 || (st != "0" && st != "") {
		return fmt.Errorf("verify email http=%d grpc=%s", resp.StatusCode, st)
	}
	return nil
}

// SignupServerAction posts Next.js server action body; returns response text and SSO cookie if set.
func (c *Client) SignupServerAction(body []byte, actionID, stateTree string) (string, string, error) {
	// POST must match scraped state tree (redirect=grok-com).
	req, err := http.NewRequest(http.MethodPost, SignupURLGrok, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	c.setBrowserHeaders(req)
	req.Header.Set("Accept", "text/x-component")
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Next-Action", actionID)
	req.Header.Set("Next-Router-State-Tree", stateTree)
	req.Header.Set("Origin", SiteURL)
	req.Header.Set("Referer", SignupURLGrok)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	text, _ := readBody(resp)

	// 1) Direct Set-Cookie sso on the action response (session JWT only).
	sso := sessionSSOFromCookies(resp.Cookies())
	// 2) Follow set-cookie hop chain from RSC body (required path for most accounts).
	if !isSessionSSO(sso) {
		for _, hop := range expandSSOHopURLs(extractAllSetCookieURLs(text)) {
			if v, err := c.followSSOHop(hop); err == nil && isSessionSSO(v) {
				sso = v
				break
			}
		}
	}
	// 3) Jar after hops (accounts.x.ai / .x.ai).
	if !isSessionSSO(sso) {
		sso = c.jarSSO()
	}
	// 4) Explicit sso=JWT in body (not hop config JWT).
	if !isSessionSSO(sso) {
		if m := ExtractSSOFromText(text); isSessionSSO(m) {
			sso = m
		}
	}
	if !isSessionSSO(sso) {
		sso = ""
	}
	if resp.StatusCode >= 400 {
		return text, sso, fmt.Errorf("signup http=%d body=%s", resp.StatusCode, truncate(text, 200))
	}
	return text, sso, nil
}

// followSSOHop walks set-cookie redirects manually so intermediate Set-Cookie is kept.
func (c *Client) followSSOHop(start string) (string, error) {
	hops := expandSSOHopURLs([]string{start})
	seen := map[string]struct{}{}
	for i := 0; i < len(hops) && i < 10; i++ {
		hop := hops[i]
		if hop == "" {
			continue
		}
		if _, ok := seen[hop]; ok {
			continue
		}
		seen[hop] = struct{}{}

		req, err := http.NewRequest(http.MethodGet, hop, nil)
		if err != nil {
			continue
		}
		c.setBrowserHeaders(req)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Referer", SiteURL+"/")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Upgrade-Insecure-Requests", "1")

		// Manual redirect walk — auto-follow can drop intermediate sso cookies.
		client := *c.http
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := readBody(resp)
		_ = resp.Body.Close()

		if v := sessionSSOFromCookies(resp.Cookies()); isSessionSSO(v) {
			return v, nil
		}
		if v := ExtractSSOFromText(body); isSessionSSO(v) {
			return v, nil
		}
		if v := c.jarSSO(); isSessionSSO(v) {
			return v, nil
		}

		loc := resp.Header.Get("Location")
		if loc == "" {
			continue
		}
		if strings.HasPrefix(loc, "/") {
			if strings.Contains(hop, "grokusercontent") {
				loc = "https://auth.grokusercontent.com" + loc
			} else {
				loc = SiteURL + loc
			}
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && strings.HasPrefix(loc, "http") {
			if _, ok := seen[loc]; !ok {
				hops = append(hops, expandSSOHopURLs([]string{loc})...)
			}
		}
	}
	if v := c.jarSSO(); isSessionSSO(v) {
		return v, nil
	}
	return "", nil
}

func (c *Client) jarSSO() string {
	for _, host := range []string{SiteURL, "https://x.ai", "https://auth.x.ai", "https://grok.com"} {
		u, _ := url.Parse(host)
		for _, ck := range c.http.Jar.Cookies(u) {
			if ck.Name == "sso" && isSessionSSO(ck.Value) {
				return ck.Value
			}
		}
	}
	return ""
}

func sessionSSOFromCookies(cks []*http.Cookie) string {
	for _, sc := range cks {
		if sc.Name == "sso" && isSessionSSO(sc.Value) {
			return sc.Value
		}
	}
	return ""
}

// isSessionSSO rejects hop-config JWTs (config.token / success_url) used only for set-cookie URLs.
func isSessionSSO(tok string) bool {
	if tok == "" || !strings.HasPrefix(tok, "eyJ") || strings.Count(tok, ".") != 2 {
		return false
	}
	// Hop config JWT payload contains success_url / config.token — not a session.
	payload := jwtPayloadMap(tok)
	if payload == nil {
		// still accept long eyJ tokens if we can't decode (rare)
		return len(tok) > 80
	}
	if cfg, ok := payload["config"].(map[string]any); ok {
		if _, ok := cfg["success_url"]; ok {
			return false
		}
		if _, ok := cfg["token"]; ok {
			return false
		}
	}
	if _, ok := payload["success_url"]; ok {
		return false
	}
	// Real session tokens typically carry sub / sid / session-ish claims or are long opaque JWTs.
	return len(tok) > 40
}

func jwtPayloadMap(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

var (
	setCookieURLRe = regexp.MustCompile(
		`https?://[^\s"'<>\\]+set-cookie/?\?q=eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+`,
	)
	setCookieRelRe = regexp.MustCompile(
		`(/[A-Za-z0-9_./-]*set-cookie/?\?q=eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)`,
	)
	jwtRe = regexp.MustCompile(`eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+`)
)

func normalizeRSC(text string) string {
	t := text
	t = strings.ReplaceAll(t, `\u0026`, "&")
	t = strings.ReplaceAll(t, `\u003d`, "=")
	t = strings.ReplaceAll(t, `\u002F`, "/")
	t = strings.ReplaceAll(t, `\/`, `/`)
	return t
}

func extractAllSetCookieURLs(text string) []string {
	body := normalizeRSC(text)
	var found []string
	seen := map[string]struct{}{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" {
			return
		}
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		found = append(found, u)
	}
	for _, m := range setCookieURLRe.FindAllString(body, -1) {
		add(m)
	}
	for _, m := range setCookieRelRe.FindAllStringSubmatch(body, -1) {
		if len(m) > 1 {
			add(SiteURL + m[1])
		}
	}
	if len(found) == 0 {
		// reconstruct hop from bare JWT near set-cookie marker
		if idx := strings.Index(strings.ToLower(body), "set-cookie"); idx >= 0 {
			window := body[idx:]
			if len(window) > 400 {
				window = window[:400]
			}
			if j := jwtRe.FindString(window); j != "" {
				add("https://auth.grokusercontent.com/set-cookie?q=" + j)
			}
		}
	}
	return found
}

func expandSSOHopURLs(urls []string) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(u string) {
		if u == "" {
			return
		}
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	for _, u := range urls {
		add(u)
		jwt := jwtFromSetCookieURL(u)
		if jwt == "" {
			continue
		}
		if payload := jwtPayloadMap(jwt); payload != nil {
			if cfg, ok := payload["config"].(map[string]any); ok {
				if s, ok := cfg["success_url"].(string); ok && strings.HasPrefix(s, "https://") {
					add(s)
					if strings.Contains(s, "set-cookie") && !strings.Contains(s, "q=") {
						add(strings.TrimRight(s, "/") + "?q=" + jwt)
					}
				}
			}
			if s, ok := payload["success_url"].(string); ok && strings.HasPrefix(s, "https://") {
				add(s)
			}
		}
		add("https://auth.grokusercontent.com/set-cookie?q=" + jwt)
	}
	return out
}

func jwtFromSetCookieURL(u string) string {
	raw, err := url.QueryUnescape(u)
	if err != nil {
		raw = u
	}
	// ?q=eyJ...
	if i := strings.Index(raw, "q="); i >= 0 {
		rest := raw[i+2:]
		if j := strings.IndexAny(rest, "&\"' "); j >= 0 {
			rest = rest[:j]
		}
		if strings.HasPrefix(rest, "eyJ") {
			return rest
		}
	}
	return jwtRe.FindString(raw)
}

func (c *Client) ClearAuthCookies() {
	u, _ := url.Parse(SiteURL)
	var keep []*http.Cookie
	for _, ck := range c.http.Jar.Cookies(u) {
		ln := strings.ToLower(ck.Name)
		if ln == "sso" || ln == "sso-rw" {
			continue
		}
		keep = append(keep, ck)
	}
	// Reset jar for host by setting empty — cookiejar doesn't delete easily;
	// re-apply clearance only.
	jar, _ := cookiejar.New(nil)
	c.http.Jar = jar
	c.applyClearanceCookies()
	_ = keep
}

func (c *Client) setBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Google Chrome";v="146", "Not_A Brand";v="99"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Linux"`)
	if h := c.clearCookieHeader(); h != "" && req.Header.Get("Cookie") == "" {
		req.Header.Set("Cookie", h)
	}
}

func (c *Client) setGRPCHeaders(req *http.Request) {
	c.setBrowserHeaders(req)
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	req.Header.Set("X-Grpc-Web", "1")
	req.Header.Set("X-User-Agent", "connect-es/2.1.1")
	req.Header.Set("Origin", SiteURL)
	req.Header.Set("Referer", SignupURLGrok)
	req.Header.Set("Accept", "*/*")
}

func (c *Client) clearCookieHeader() string {
	if c.clear == nil {
		return ""
	}
	return c.clear.CookieHeader()
}

var givenNames = []string{
	"James", "John", "Robert", "Michael", "William", "David", "Richard", "Joseph", "Thomas", "Charles",
}
var familyNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis", "Rodriguez", "Martinez",
}

// BuildSignupBody matches grok_register/http_protocol.server_action_register lite shape.
//
//	[{ emailValidationCode, createUserAndSessionRequest, turnstileToken,
//	   conversionId, castleRequestToken },
//	 { client:"$T", meta:"$undefined", mutationKey:"$undefined" }]
func BuildSignupBody(email, password, code, turnstileToken string) []byte {
	given := givenNames[mrand.Intn(len(givenNames))]
	family := familyNames[mrand.Intn(len(familyNames))]
	payload := []any{
		map[string]any{
			"emailValidationCode": code,
			"createUserAndSessionRequest": map[string]any{
				"email":              email,
				"givenName":          given,
				"familyName":         family,
				"clearTextPassword":  password,
				"tosAcceptedVersion": "$undefined",
			},
			"turnstileToken":     turnstileToken,
			"conversionId":       randomUUID(),
			"castleRequestToken": "",
		},
		map[string]any{
			"client":      "$T",
			"meta":        "$undefined",
			"mutationKey": "$undefined",
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return []byte("[]")
	}
	return raw
}

func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

func pbStr(field int, s string) []byte {
	tag := byte(field<<3 | 2)
	b := []byte(s)
	out := []byte{tag}
	out = append(out, pbVarint(len(b))...)
	out = append(out, b...)
	return out
}

func pbVarint(n int) []byte {
	var parts []byte
	for n > 0x7f {
		parts = append(parts, byte(n&0x7f)|0x80)
		n >>= 7
	}
	parts = append(parts, byte(n))
	return parts
}

func grpcWebFrame(inner []byte) []byte {
	frame := make([]byte, 5+len(inner))
	frame[0] = 0
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(inner)))
	copy(frame[5:], inner)
	return frame
}

func scrapeStateTree(html string) string {
	chunks := flightRe.FindAllStringSubmatch(html, -1)
	for _, ch := range chunks {
		if len(ch) < 2 {
			continue
		}
		decoded := strings.ReplaceAll(ch[1], `\"`, `"`)
		if !strings.Contains(decoded, "sign-up") {
			continue
		}
		idx := strings.Index(decoded, `"f":[[[`)
		if idx < 0 {
			continue
		}
		fStart := idx + 5
		end := strings.Index(decoded[fStart:], `"$undefined"`)
		if end < 0 {
			continue
		}
		raw := decoded[fStart : fStart+end]
		raw = strings.ReplaceAll(raw, `\\"`, `"`)
		raw = strings.ReplaceAll(raw, `\`, "")
		return url.QueryEscape(raw)
	}
	return ""
}

func isCloudflare(status int, body string, h http.Header) bool {
	if status == 403 || status == 503 {
		low := strings.ToLower(body)
		if strings.Contains(low, "cf-") || strings.Contains(low, "cloudflare") || strings.Contains(low, "just a moment") {
			return true
		}
	}
	if strings.Contains(strings.ToLower(h.Get("Server")), "cloudflare") && status >= 400 {
		return true
	}
	return false
}

func readBody(resp *http.Response) (string, error) {
	var r io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err == nil {
			defer gz.Close()
			r = gz
		}
	}
	b, err := io.ReadAll(io.LimitReader(r, 8<<20))
	return string(b), err
}

func unique(matches [][]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		if _, ok := seen[m[1]]; ok {
			continue
		}
		seen[m[1]] = struct{}{}
		out = append(out, m[1])
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ExtractSSOFromText finds an embedded sso=JWT (session) in RSC/HTML body.
func ExtractSSOFromText(text string) string {
	body := normalizeRSC(text)
	// sso=eyJ...
	reNamed := regexp.MustCompile(`(?i)(?:^|[;,\s'"\\])sso=(eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)`)
	if m := reNamed.FindStringSubmatch(body); len(m) > 1 && isSessionSSO(m[1]) {
		return m[1]
	}
	// near session/sso markers
	reNear := regexp.MustCompile(`(?i)(?:sso|session)[^e]{0,40}(eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)`)
	if m := reNear.FindStringSubmatch(body); len(m) > 1 && isSessionSSO(m[1]) {
		return m[1]
	}
	for _, m := range jwtRe.FindAllString(body, -1) {
		if isSessionSSO(m) {
			return m
		}
	}
	return ""
}
