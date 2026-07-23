package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/grok-free-register/grok-reg/internal/clearance"
	"github.com/grok-free-register/grok-reg/internal/config"
	"github.com/grok-free-register/grok-reg/internal/cpa"
	"github.com/grok-free-register/grok-reg/internal/email"
	"github.com/grok-free-register/grok-reg/internal/home"
	"github.com/grok-free-register/grok-reg/internal/inventory"
	"github.com/grok-free-register/grok-reg/internal/logx"
	"github.com/grok-free-register/grok-reg/internal/oauth"
	"github.com/grok-free-register/grok-reg/internal/protocol"
	"github.com/grok-free-register/grok-reg/internal/sink"
	"github.com/grok-free-register/grok-reg/internal/state"
	"github.com/grok-free-register/grok-reg/internal/turnstile"
)

type QItem struct {
	Email    string
	Password string
	Code     string
	Handle   email.Handle
}

type SSOJob struct {
	Email    string
	Password string
	SSO      string
}

type Options struct {
	Cfg    config.Config
	Paths  home.Paths
	Run    home.RunDirs
	Target int
	Log    *logx.Logger
	Store  *state.Store
}

type Engine struct {
	opt Options

	cm       *clearance.Manager
	xai      *protocol.Client
	mail     *email.Provider
	turn     turnstile.Provider
	oauth    *oauth.Client
	inv      *inventory.Inventory[string, QItem]
	phys     *inventory.Semaphore
	qPending *inventory.Semaphore

	oauthCh  chan SSOJob
	uploader *cpa.Uploader
	g2a      *sink.Uploader

	// scfg is the live signup config (Action ID / site key / state tree).
	// xAI rotates Next.js Server Action IDs on deploy; workers re-read this
	// under RLock and cWorker hot-reloads it after "Server action not found".
	scfgMu          sync.RWMutex
	scfg            protocol.SignupConfig
	scfgRefreshMu   sync.Mutex
	scfgLastRefresh time.Time
	actionStaleN    atomic.Int64 // consecutive action-stale signup failures

	done     atomic.Int64 // CPA successes (counts toward -t)
	reserved atomic.Int64 // in-flight accounts (email→register→oauth→probe)
	ssoN     atomic.Int64
	oaN      atomic.Int64
	fail     atomic.Int64

	start   time.Time
	wgReg   sync.WaitGroup // S/P/C
	wgOAuth sync.WaitGroup
	wgAux   sync.WaitGroup // status ticker etc
}

// Min interval between automatic signup-config refreshes (FlareSolverr is expensive).
const signupConfigRefreshMinInterval = 90 * time.Second

// softSeatCap limits in-flight accounts (especially important for infinite mode).
func (e *Engine) softSeatCap() int {
	soft := e.opt.Cfg.TurnstileWorkers
	if soft < 1 {
		soft = 1
	}
	if soft > 4 {
		soft = 4
	}
	if e.opt.Cfg.PhysicalCap > 0 && e.opt.Cfg.PhysicalCap < soft {
		soft = e.opt.Cfg.PhysicalCap
	}
	return soft
}

// remainingCapacity = seats still allowed to start new accounts.
// Finite: target - done - reserved.
// Infinite (target<=0): softSeatCap - reserved (keeps pipeline moving without flooding).
func (e *Engine) remainingCapacity() int {
	if config.IsInfiniteTarget(e.opt.Target) {
		n := e.softSeatCap() - int(e.reserved.Load())
		if n < 0 {
			return 0
		}
		return n
	}
	n := e.opt.Target - int(e.done.Load()) - int(e.reserved.Load())
	if n < 0 {
		return 0
	}
	return n
}

func (e *Engine) getSignupConfig() protocol.SignupConfig {
	e.scfgMu.RLock()
	defer e.scfgMu.RUnlock()
	return e.scfg
}

func (e *Engine) setSignupConfig(cfg protocol.SignupConfig) {
	e.scfgMu.Lock()
	e.scfg = cfg
	e.scfgMu.Unlock()
}

// isStaleActionError detects xAI rotating Next.js Server Action IDs.
// Typical: signup http=404 body=Server action not found.
func isStaleActionError(err error, body string) bool {
	var b strings.Builder
	if err != nil {
		b.WriteString(err.Error())
		b.WriteByte(' ')
	}
	b.WriteString(body)
	s := strings.ToLower(b.String())
	if strings.Contains(s, "server action not found") {
		return true
	}
	// Defensive: 404 on Next-Action with empty/unknown body after deploy.
	if strings.Contains(s, "http=404") && (strings.Contains(s, "action") || strings.Contains(s, "not found")) {
		return true
	}
	return false
}

// maybeRefreshSignupConfig re-scrapes site key / Action ID / state tree.
// Triggered when signup fails with stale Server Action (xAI front-end deploy).
// Single-flight + min interval so multi-thread C workers don't stampede FlareSolverr.
func (e *Engine) maybeRefreshSignupConfig(reason string) bool {
	e.scfgRefreshMu.Lock()
	defer e.scfgRefreshMu.Unlock()

	if since := time.Since(e.scfgLastRefresh); since < signupConfigRefreshMinInterval && !e.scfgLastRefresh.IsZero() {
		e.opt.Log.Warnf("signup config refresh skipped (cooldown %.0fs left, reason=%s)",
			(signupConfigRefreshMinInterval - since).Seconds(), reason)
		return false
	}
	e.scfgLastRefresh = time.Now()

	old := e.getSignupConfig()
	e.opt.Log.Warnf("刷新注册配置 (reason=%s old_action=%s...)", reason, trim(old.ActionID, 12))
	_ = e.opt.Store.Set(func(s *state.Snapshot) {
		s.Phase = state.PhaseRegister
		s.PhaseDetail = "刷新注册配置 (Action ID)"
	})

	cfg, err := e.xai.FetchConfig()
	if err != nil {
		e.opt.Log.Warnf("signup config refresh failed: %v", err)
		return false
	}
	e.setSignupConfig(cfg)
	e.actionStaleN.Store(0)
	if cfg.ActionID != old.ActionID || cfg.StateTree != old.StateTree || cfg.SiteKey != old.SiteKey {
		e.opt.Log.OKf("注册配置已更新 ACTION_ID=%s... (was %s...)",
			trim(cfg.ActionID, 12), trim(old.ActionID, 12))
	} else {
		e.opt.Log.Infof("注册配置重抓完成，Action ID 未变 ACTION_ID=%s...", trim(cfg.ActionID, 12))
	}
	return true
}

// tryReserve claims one pipeline seat for a new account attempt.
func (e *Engine) tryReserve() bool {
	for {
		d := e.done.Load()
		r := e.reserved.Load()
		if config.IsInfiniteTarget(e.opt.Target) {
			if int(r) >= e.softSeatCap() {
				return false
			}
		} else if d+r >= int64(e.opt.Target) {
			return false
		}
		if e.reserved.CompareAndSwap(r, r+1) {
			return true
		}
	}
}

func (e *Engine) releaseReserve() {
	for {
		r := e.reserved.Load()
		if r <= 0 {
			return
		}
		if e.reserved.CompareAndSwap(r, r-1) {
			return
		}
	}
}

// tryComplete moves a reserved seat into done. Returns (newDone, ok).
// ok=false means target already met (caller should discard extra success).
// Infinite mode always accepts.
func (e *Engine) tryComplete() (int64, bool) {
	if config.IsInfiniteTarget(e.opt.Target) {
		e.releaseReserve()
		return e.done.Add(1), true
	}
	for {
		d := e.done.Load()
		if d >= int64(e.opt.Target) {
			e.releaseReserve()
			return d, false
		}
		if e.done.CompareAndSwap(d, d+1) {
			e.releaseReserve()
			return d + 1, true
		}
	}
}

func Run(ctx context.Context, opt Options) error {
	e := &Engine{
		opt:     opt,
		oauthCh: make(chan SSOJob, 64),
		start:   time.Now(),
	}
	return e.run(ctx)
}

func (e *Engine) run(ctx context.Context) error {
	cfg := e.opt.Cfg
	log := e.opt.Log
	st := e.opt.Store

	config.ApplyProxyEnv(cfg)

	sWorkers, pWorkers, cWorkers, oauthWorkers, physCap := deriveWorkers(cfg)
	e.phys = inventory.NewSemaphore(physCap)
	// Pending email codes in flight: cap by target so target=5 doesn't open 12 boxes.
	qPend := cfg.Target
	if qPend <= 0 {
		// infinite mode: small pending email/code budget
		qPend = e.softSeatCap() + 1
		if qPend < 2 {
			qPend = 2
		}
	}
	if qPend > 6 {
		qPend = 6
	}
	if qPend < 2 {
		qPend = 2
	}
	e.qPending = inventory.NewSemaphore(qPend)
	tSlots, qSlots := 4, 4
	if cfg.Target > 0 && cfg.Target < 4 {
		tSlots, qSlots = cfg.Target, cfg.Target
	}
	if config.IsInfiniteTarget(cfg.Target) {
		tSlots, qSlots = e.softSeatCap(), e.softSeatCap()
	}
	e.inv = inventory.New[string, QItem](tSlots, qSlots)
	log.Infof("workers S=%d P=%d C=%d OAuth=%d phys=%d q_pending=%d", sWorkers, pWorkers, cWorkers, oauthWorkers, physCap, qPend)

	_ = st.Set(func(s *state.Snapshot) {
		s.Status = state.StatusRunning
		s.RunID = e.opt.Run.RunID
		s.Target = e.opt.Target
		s.Done = 0
		s.Phase = state.PhaseClearance
		s.PhaseDetail = "清障预热中"
		s.Workers = state.Workers{S: sWorkers, P: pWorkers, C: cWorkers, OAuth: oauthWorkers}
		s.PID = os.Getpid()
		s.StartedAt = e.start.UTC().Format(time.RFC3339)
		s.LogPath = e.opt.Run.LogPath
		s.OutputDir = e.opt.Run.Root
		s.Error = ""
	})

	// Clearance
	if cfg.ClearanceEnabled {
		e.cm = clearance.NewManager(cfg.FlareSolverrURL, cfg.ClearanceProxy, cfg.ClearanceURLs)
		msg, err := e.cm.Prewarm()
		if err != nil {
			log.Warnf("clearance: %v (%s)", err, msg)
		} else {
			log.Infof("[clearance] %s", msg)
		}
	} else {
		log.Info("[clearance] 未启用")
	}

	var err error
	e.xai, err = protocol.NewClient(cfg.RegisterProxy, e.cm)
	if err != nil {
		return err
	}
	e.mail = email.New(email.Config{
		Mode:                   cfg.EmailMode,
		Domain:                 cfg.EmailDomain,
		API:                    cfg.EmailAPI,
		LOLRetries:             cfg.TempmailLOLRetries,
		LOLIntervalMS:          cfg.TempmailLOLIntervalMS,
		TestmailAPIKey:         cfg.TestmailAPIKey,
		TestmailNamespace:      cfg.TestmailNamespace,
		TestmailDomain:         cfg.TestmailDomain,
		CloudmailAdminEmail:    cfg.CloudmailAdminEmail,
		CloudmailAdminPassword: cfg.CloudmailAdminPassword,
		CloudmailProxy:         cfg.CloudmailProxy,
	})
	switch cfg.EmailMode {
	case config.EmailTestmail:
		log.Infof("Email mode=testmail namespace=%s domain=%s", cfg.TestmailNamespace, cfg.TestmailDomain)
	case config.EmailCloudmail:
		px := cfg.CloudmailProxy
		if px == "" {
			px = "(inherit HTTP_PROXY)"
		}
		log.Infof("Email mode=cloudmail api=%s domain=%s admin=%s proxy=%s", cfg.EmailAPI, cfg.EmailDomain, cfg.CloudmailAdminEmail, px)
	default:
		log.Infof("Email mode=%s", cfg.EmailMode)
	}
	e.turn = turnstile.New(turnstile.Options{
		Provider: cfg.TurnstileProvider,
		LiteURL:  cfg.LiteSolverURL,
		Proxy:    cfg.RegisterProxy,
		Clear:    e.cm,
		Workers:  sWorkers, // parallel S = pool slots
	})
	if c, ok := e.turn.(turnstile.Closer); ok {
		defer c.Close()
	}
	log.Infof("Turnstile provider=%s workers=%d (pool → one-shot mint → chromedp)", e.turn.Name(), sWorkers)
	log.Infof("Turnstile mint: python=%s pool=%s script=%s", turnstile.DetectedPython(), turnstile.DetectedPoolScript(), turnstile.DetectedScript())
	e.uploader = cpa.NewUploader(cpa.UploadConfig{
		Enabled:      cfg.CPAUploadEnabled,
		BaseURL:      cfg.CPAManagementBase,
		Key:          cfg.CPAManagementKey,
		TimeoutSec:   cfg.CPAUploadTimeoutSec,
		Retries:      cfg.CPAUploadRetries,
		NameTemplate: cfg.CPAUploadNameTemplate,
		Verify:       cfg.CPAUploadVerify,
		Mode:         cfg.CPAUploadMode,
	}, func(f string, a ...any) {
		log.Infof(f, a...)
	})
	if e.uploader.Enabled() {
		log.Infof("CPA upload enabled base=%s", cfg.CPAManagementBase)
	}
	e.g2a = sink.New(sink.Config{
		JiujiuEnabled:      cfg.G2AJiujiuEnabled,
		JiujiuBase:         cfg.G2AJiujiuBase,
		JiujiuToken:        cfg.G2AJiujiuToken,
		JiujiuPool:         cfg.G2AJiujiuPool,
		ChenymeEnabled:     cfg.G2AChenymeEnabled,
		ChenymeBase:        cfg.G2AChenymeBase,
		ChenymeUser:        cfg.G2AChenymeUser,
		ChenymePassword:    cfg.G2AChenymePassword,
		ChenymeUploadSSO:   cfg.G2AChenymeUploadSSO,
		ChenymeUploadBuild: cfg.G2AChenymeUploadBuild,
	}, func(f string, a ...any) {
		log.Infof(f, a...)
	})
	if e.g2a.Enabled() {
		log.Infof("g2a sink: jiujiu=%v(%s) chenyme=%v(%s sso=%v build=%v)",
			cfg.G2AJiujiuEnabled, cfg.G2AJiujiuBase,
			cfg.G2AChenymeEnabled, cfg.G2AChenymeBase,
			cfg.G2AChenymeUploadSSO, cfg.G2AChenymeUploadBuild)
	}
	e.oauth, err = oauth.NewClient(cfg.RegisterProxy, e.cm, time.Duration(cfg.OAuthRetrySec)*time.Second)
	if err != nil {
		return err
	}

	_ = st.Set(func(s *state.Snapshot) {
		s.Phase = state.PhaseRegister
		s.PhaseDetail = "获取注册配置"
	})
	log.Info("Fetching signup config...")
	scfg, err := e.xai.FetchConfig()
	if err != nil {
		_ = st.Set(func(s *state.Snapshot) {
			s.Status = state.StatusError
			s.Error = err.Error()
			s.PhaseDetail = "配置获取失败"
		})
		return fmt.Errorf("config fetch: %w", err)
	}
	e.setSignupConfig(scfg)
	e.scfgLastRefresh = time.Now()
	log.Infof("SITE_KEY=%s ACTION_ID=%s...", scfg.SiteKey, trim(scfg.ActionID, 12))
	if config.IsInfiniteTarget(e.opt.Target) {
		log.OKf("注册服务已启动 | 目标 无限 | run=%s", e.opt.Run.RunID)
	} else {
		log.OKf("注册服务已启动 | 目标 %d | run=%s", e.opt.Target, e.opt.Run.RunID)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// signal
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-sigCh:
			log.Warn("收到停止信号，正在退出...")
			cancel()
		case <-ctx.Done():
		}
	}()

	// status ticker
	e.wgAux.Add(1)
	go func() {
		defer e.wgAux.Done()
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				e.refreshState()
			}
		}
	}()

	for i := 0; i < sWorkers; i++ {
		e.wgReg.Add(1)
		go e.sWorker(ctx, i)
	}
	for i := 0; i < pWorkers; i++ {
		e.wgReg.Add(1)
		go e.pWorker(ctx, i)
	}
	for i := 0; i < cWorkers; i++ {
		e.wgReg.Add(1)
		go e.cWorker(ctx, i)
	}
	for i := 0; i < oauthWorkers; i++ {
		e.wgOAuth.Add(1)
		go e.oauthWorker(ctx, i)
	}

	// wait until target or cancel
	for {
		if !config.IsInfiniteTarget(e.opt.Target) && int(e.done.Load()) >= e.opt.Target {
			log.OKf("已达目标 %d，停止", e.opt.Target)
			cancel()
			break
		}
		select {
		case <-ctx.Done():
			goto shutdown
		case <-time.After(500 * time.Millisecond):
		}
	}
shutdown:
	// 1) stop S/P/C producers (ctx canceled)
	// 2) wait register workers so no more sends to oauthCh
	// 3) close oauthCh so OAuth workers exit range
	waitGroupTimeout(&e.wgReg, 15*time.Second, log, "register workers")
	close(e.oauthCh)
	waitGroupTimeout(&e.wgOAuth, 30*time.Second, log, "oauth workers")
	waitGroupTimeout(&e.wgAux, 3*time.Second, log, "aux")

	_ = st.Set(func(s *state.Snapshot) {
		if s.Status != state.StatusError {
			s.Status = state.StatusStopped
		}
		s.Phase = state.PhaseIdle
		if config.IsInfiniteTarget(e.opt.Target) {
			s.PhaseDetail = fmt.Sprintf("运行中(无限) 成功=%d", e.done.Load())
		} else {
			s.PhaseDetail = fmt.Sprintf("完成 %d/%d", e.done.Load(), e.opt.Target)
		}
		s.Done = int(e.done.Load())
		s.SSOCount = int(e.ssoN.Load())
		s.OAuthCount = int(e.oaN.Load())
		s.FailCount = int(e.fail.Load())
		s.PID = 0
	})
	log.Infof("结束 done=%d sso=%d oauth=%d fail=%d", e.done.Load(), e.ssoN.Load(), e.oaN.Load(), e.fail.Load())
	return nil
}

func (e *Engine) refreshState() {
	elapsed := time.Since(e.start).Minutes()
	rate := 0.0
	if elapsed > 0 {
		rate = float64(e.done.Load()) / elapsed
	}
	t, q := e.inv.Depths()
	_ = e.opt.Store.Set(func(s *state.Snapshot) {
		s.Done = int(e.done.Load())
		s.SSOCount = int(e.ssoN.Load())
		s.OAuthCount = int(e.oaN.Load())
		s.FailCount = int(e.fail.Load())
		s.RatePerMin = rate
		if s.Phase == state.PhaseRegister || s.Phase == "" {
			if config.IsInfiniteTarget(e.opt.Target) {
				s.PhaseDetail = fmt.Sprintf("无限注册 T=%d Q=%d done=%d inflight=%d", t, q, e.done.Load(), e.reserved.Load())
			} else {
				s.PhaseDetail = fmt.Sprintf("注册中 T=%d Q=%d done=%d/%d inflight=%d", t, q, e.done.Load(), e.opt.Target, e.reserved.Load())
			}
		}
	})
}

func waitGroupTimeout(wg *sync.WaitGroup, d time.Duration, log *logx.Logger, name string) {
	ch := make(chan struct{})
	go func() {
		wg.Wait()
		close(ch)
	}()
	select {
	case <-ch:
	case <-time.After(d):
		log.Warnf("%s 退出超时", name)
	}
}

func (e *Engine) sWorker(ctx context.Context, id int) {
	defer e.wgReg.Done()
	log := e.opt.Log
	pageURL := protocol.SiteURL + "/sign-up"
	for {
		if !config.IsInfiniteTarget(e.opt.Target) && int(e.done.Load()) >= e.opt.Target {
			return
		}
		// Token demand:
		// - remainingCapacity: seats not yet reserved (future accounts)
		// - qDepth: reserved seats already holding email/code, still need a Turnstile token
		// Without counting qDepth, P can reserve first and starve S forever (deadlock).
		tDepth, qDepth := e.inv.Depths()
		need := e.remainingCapacity() + qDepth
		if need < 1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		// Don't mint far ahead of demand.
		if tDepth >= need {
			select {
			case <-ctx.Done():
				return
			case <-time.After(400 * time.Millisecond):
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		siteKey := e.getSignupConfig().SiteKey
		if siteKey == "" {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		if err := e.phys.Acquire(ctx); err != nil {
			return
		}
		tok, err := e.turn.Solve(ctx, siteKey, pageURL)
		e.phys.Release()
		if err != nil {
			log.Warnf("[S%d] turnstile: %v", id, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if err := e.inv.PutT(ctx, tok, 5*time.Minute); err != nil {
			return
		}
		log.Infof("[S%d] token ok (len=%d)", id, len(tok))
	}
}

func (e *Engine) pWorker(ctx context.Context, id int) {
	defer e.wgReg.Done()
	log := e.opt.Log
	for {
		if !config.IsInfiniteTarget(e.opt.Target) && int(e.done.Load()) >= e.opt.Target {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		// Global seat: done + reserved <= target (not per-worker).
		if e.remainingCapacity() <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			if !config.IsInfiniteTarget(e.opt.Target) && int(e.done.Load()) >= e.opt.Target {
				return
			}
			continue
		}
		_, qDepth := e.inv.Depths()
		qCap := e.remainingCapacity()
		if qCap > 4 {
			qCap = 4
		}
		if qCap < 1 {
			qCap = 1
		}
		if qDepth >= qCap {
			select {
			case <-ctx.Done():
				return
			case <-time.After(800 * time.Millisecond):
			}
			continue
		}

		// Reserve seat BEFORE creating email so multi-P cannot overshoot -t.
		if !e.tryReserve() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
			continue
		}

		if err := e.qPending.Acquire(ctx); err != nil {
			e.releaseReserve()
			return
		}
		h, err := e.mail.Create()
		if err != nil {
			e.qPending.Release()
			e.releaseReserve()
			log.Debugf("[P%d] create email: %v", id, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		if err := e.xai.CreateEmailCode(h.Email); err != nil {
			e.qPending.Release()
			e.releaseReserve()
			log.Debugf("[P%d] create code %s: %v", id, h.Email, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		code, err := e.mail.PollCode(h, 90*time.Second)
		if err != nil {
			e.qPending.Release()
			e.releaseReserve()
			log.Debugf("[P%d] poll code: %v", id, err)
			continue
		}
		item := QItem{Email: h.Email, Password: h.Password, Code: code, Handle: h}
		// Q must outlive Turnstile mint (often 1–3+ min under CF). Short TTL
		// drops verified email/code while S is still minting → stuck T-only queue.
		if err := e.inv.PutQ(ctx, item, 15*time.Minute); err != nil {
			e.qPending.Release()
			e.releaseReserve()
			return
		}
		e.qPending.Release()
		// seat stays reserved until signup fail / oauth fail / CPA success
		log.Debugf("[P%d] Q ready %s (reserved=%d done=%d/%d)", id, h.Email, e.reserved.Load(), e.done.Load(), e.opt.Target)
	}
}

func (e *Engine) cWorker(ctx context.Context, id int) {
	defer e.wgReg.Done()
	log := e.opt.Log
	for {
		if !config.IsInfiniteTarget(e.opt.Target) && int(e.done.Load()) >= e.opt.Target {
			return
		}
		pair, err := e.inv.ClaimPair(ctx)
		if err != nil {
			return
		}
		token := pair.T.Value
		q := pair.Q.Value
		_ = e.opt.Store.Set(func(s *state.Snapshot) {
			s.Phase = state.PhaseRegister
			s.PhaseDetail = fmt.Sprintf("正在注册 %s", q.Email)
		})
		log.Startf("开始注册 %s", q.Email)

		e.xai.ClearAuthCookies()
		if err := e.xai.VerifyEmailCode(q.Email, q.Code); err != nil {
			log.Warnf("verify fail %s: %v", q.Email, err)
			pair.Release()
			e.fail.Add(1)
			e.releaseReserve()
			continue
		}
		scfg := e.getSignupConfig()
		body := protocol.BuildSignupBody(q.Email, q.Password, q.Code, token)
		text, sso, err := e.xai.SignupServerAction(body, scfg.ActionID, scfg.StateTree)
		if sso == "" {
			sso = protocol.ExtractSSOFromText(text)
		}
		pair.Release()
		if err != nil || sso == "" {
			preview := text
			if len(preview) > 180 {
				preview = preview[:180]
			}
			log.Warnf("signup fail %s: err=%v sso=%v body=%q", q.Email, err, sso != "", preview)
			// xAI front-end deploy invalidates Next-Action hashes → 404 "Server action not found".
			// Hot-reload signup config so the next attempt uses the new Action ID (no restart).
			if isStaleActionError(err, text) {
				n := e.actionStaleN.Add(1)
				log.Warnf("检测到过期 Server Action (连续 %d 次)，尝试自动刷新配置", n)
				e.maybeRefreshSignupConfig(fmt.Sprintf("stale_action_n=%d", n))
			} else {
				e.actionStaleN.Store(0)
			}
			e.fail.Add(1)
			e.releaseReserve() // free seat for another attempt
			continue
		}
		e.actionStaleN.Store(0)

		// ensure run dirs exist (first credential)
		accPath := filepath.Join(e.opt.Run.SSO, "accounts.txt")
		if err := cpa.AppendSSO(accPath, q.Email, q.Password, sso); err != nil {
			log.Warnf("write sso: %v", err)
		}
		_ = cpa.AppendAuthSession(filepath.Join(e.opt.Run.SSO, "auth-sessions.jsonl"), q.Email, sso)
		n := e.ssoN.Add(1)
		log.OKf("注册成功 #%d %s", n, q.Email)
		// Auto-upload SSO to grok2api sinks (jiujiu + chenyme web)
		if e.g2a != nil && e.g2a.Enabled() {
			email, ssoCopy := q.Email, sso
			g2a := e.g2a
			go func() {
				defer func() { _ = recover() }()
				g2a.OnSSO(email, ssoCopy)
			}()
		}

		job := SSOJob{Email: q.Email, Password: q.Password, SSO: sso}
		select {
		case <-ctx.Done():
			e.releaseReserve()
			return
		case e.oauthCh <- job:
		default:
			select {
			case <-ctx.Done():
				e.releaseReserve()
				return
			case e.oauthCh <- job:
			}
		}
	}
}

func (e *Engine) oauthWorker(ctx context.Context, id int) {
	defer e.wgOAuth.Done()
	log := e.opt.Log
	minInterval := time.Duration(e.opt.Cfg.OAuthMinIntervalSec * float64(time.Second))
	if minInterval <= 0 {
		minInterval = 10 * time.Second
	}
	var last time.Time
	for job := range e.oauthCh {
		// Soft-stop: still drain with seat accounting, but skip work past target.
		if !config.IsInfiniteTarget(e.opt.Target) && int(e.done.Load()) >= e.opt.Target {
			e.releaseReserve()
			continue
		}
		if !last.IsZero() {
			if d := time.Until(last.Add(minInterval)); d > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(d):
				}
			}
		}
		last = time.Now()
		_ = e.opt.Store.Set(func(s *state.Snapshot) {
			s.Phase = state.PhaseOAuth
			s.PhaseDetail = fmt.Sprintf("正在 OAuth (%s)", job.Email)
		})
		log.Startf("OAuth %s", job.Email)
		cred, err := e.oauth.Exchange(ctx, job.SSO)
		if err != nil {
			log.Warnf("OAuth fail %s: %v", job.Email, err)
			e.fail.Add(1)
			e.releaseReserve()
			continue
		}
		e.oaN.Add(1)
		doc := cpa.FromCredential(cred, job.Email)
		_ = e.opt.Store.Set(func(s *state.Snapshot) {
			s.Phase = state.PhaseProbe
			s.PhaseDetail = fmt.Sprintf("探活 %s", job.Email)
		})
		if e.opt.Cfg.ProbeEnabled {
			if err := cpa.Probe(doc, e.opt.Cfg.RegisterProxy); err != nil {
				log.Warnf("探活失败 %s: %v", job.Email, err)
				path, _ := cpa.WriteAtomic(e.opt.Run.Discarded, doc, cpa.DefaultSecret())
				_ = path
				e.fail.Add(1)
				e.releaseReserve()
				continue
			}
		}
		// Atomic complete: prevents multi-OAuth overshoot of -t.
		d, ok := e.tryComplete()
		if !ok {
			// Target already filled by another worker — keep file in discarded.
			path, _ := cpa.WriteAtomic(e.opt.Run.Discarded, doc, cpa.DefaultSecret())
			log.Warnf("已达目标，额外号移入 discarded: %s (%s)", job.Email, filepath.Base(path))
			continue
		}
		path, err := cpa.WriteAtomic(e.opt.Run.CPA, doc, cpa.DefaultSecret())
		if err != nil {
			log.Warnf("写 CPA 失败: %v", err)
			// seat already converted to done; count as fail but don't re-open flood
			e.fail.Add(1)
			continue
		}
		if e.uploader != nil && e.uploader.Enabled() {
			up := e.uploader
			docCopy := doc
			go func() {
				defer func() { _ = recover() }()
				_ = up.UploadDocument(docCopy)
			}()
		}
		// Upload Build OAuth (CPA JSON) to chenyme before counting complete —
		// do it inline so process exit cannot race with async import.
		if e.g2a != nil && e.g2a.Enabled() {
			if raw, rerr := os.ReadFile(path); rerr == nil {
				e.g2a.OnBuildOAuth(job.Email, filepath.Base(path), raw)
			} else {
				log.Warnf("read CPA for g2a upload: %v", rerr)
			}
		}
		if config.IsInfiniteTarget(e.opt.Target) {
			log.OKf("CPA 就绪 #%d (无限) %s -> %s", d, job.Email, filepath.Base(path))
		} else {
			log.OKf("CPA 就绪 #%d/%d %s -> %s", d, e.opt.Target, job.Email, filepath.Base(path))
		}
		e.refreshState()
	}
}

func deriveWorkers(cfg config.Config) (s, p, c, oa, phys int) {
	phys = cfg.PhysicalCap
	if phys <= 0 {
		cpus := runtime.NumCPU()
		phys = cpus
		if phys > 4 {
			phys = 4
		}
		if phys < 2 {
			phys = 2
		}
	}
	// Browser Turnstile: parallel slots from runtime --thread (not config.env).
	prov := strings.ToLower(strings.TrimSpace(cfg.TurnstileProvider))
	if prov == "" || prov == "browser" || prov == "local" || prov == "playwright" || prov == "pool" {
		s = cfg.TurnstileWorkers
		if s <= 0 {
			s = 2
		}
		if s > 8 {
			s = 8
		}
		if s < 1 {
			s = 1
		}
		// phys caps concurrent browser mints (= pool slots)
		if cfg.PhysicalCap > 0 && cfg.PhysicalCap < s {
			s = cfg.PhysicalCap
		}
		phys = s
	} else {
		s = phys
		if cfg.TurnstileWorkers > 0 {
			s = cfg.TurnstileWorkers
		}
	}
	// P workers: don't spawn 8 when target is 5 (was flooding tempmail).
	// Infinite (target<=0): keep P=1 (or soft seat) so email creation stays single-lane.
	target := cfg.Target
	if config.IsInfiniteTarget(target) {
		p = 1
		if cfg.TurnstileWorkers > 1 {
			// still single-thread friendly when --thread 1
			p = 1
		}
		c = 1
		oa = 2
	} else {
		p = target
		if p > 4 {
			p = 4
		}
		if p < 1 {
			p = 1
		}
		c = 2
		if target < 2 {
			c = 1
		}
		oa = 2
	}
	if s < 1 {
		s = 1
	}
	return
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
