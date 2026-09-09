package core

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// reauthMaxWait bounds how long an attempt stays pending before the engine
// gives up and tears down the underlying login process. The provider's code
// expires well inside this window; the timeout only stops an abandoned
// attempt from holding a process open forever.
const reauthMaxWait = 10 * time.Minute

// reauthCodePattern matches the shape of an authorization code the user pastes
// back: one opaque whitespace-free token. Deliberately strict — a bare message
// is only swallowed as a code when it cannot be ordinary prose.
var reauthCodePattern = regexp.MustCompile(`^[A-Za-z0-9._#~/+=-]{20,512}$`)

// pendingReauth is an in-flight /auth flow. Agent credentials belong to the
// machine rather than to one conversation, so at most one exists at a time;
// the origin fields let the outcome be reported back to the thread that
// started it, even though it resolves asynchronously.
type pendingReauth struct {
	session    ReauthSession
	platform   Platform
	replyCtx   any
	sessionKey string
	userID     string
	cancel     context.CancelFunc
}

// reauthState is embedded in Engine.
type reauthState struct {
	mu      sync.Mutex
	pending *pendingReauth
}

// looksLikeReauthCode reports whether a bare chat message could be an
// authorization code rather than something the user meant for the agent.
func looksLikeReauthCode(s string) bool {
	return reauthCodePattern.MatchString(s)
}

// reauthenticator returns the active agent's re-authentication support, if any.
// Credentials are per-machine, so the default agent answers for every workspace
// even in multi-workspace mode (all workspace agents share its type and config).
func (e *Engine) reauthenticator() (AgentReauthenticator, bool) {
	ra, ok := e.agent.(AgentReauthenticator)
	return ra, ok
}

// authErrorHint returns the re-authentication prompt to show instead of a raw
// provider error, when agent recognizes errMsg as expired or rejected
// credentials. agent may be nil.
func (e *Engine) authErrorHint(agent Agent, errMsg string) (string, bool) {
	if agent == nil {
		agent = e.agent
	}
	ra, ok := agent.(AgentReauthenticator)
	if !ok || !ra.IsAuthError(errMsg) {
		return "", false
	}
	return e.i18n.T(MsgAuthExpired), true
}

// cmdAuth implements /auth: start a re-authentication flow, submit the code
// the user pasted back, or cancel an attempt in progress.
func (e *Engine) cmdAuth(p Platform, msg *Message, args []string) {
	ra, ok := e.reauthenticator()
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgAuthUnsupported))
		return
	}

	arg := strings.TrimSpace(strings.Join(args, " "))
	switch {
	case strings.EqualFold(arg, "cancel"):
		if e.cancelReauth() {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgAuthCancelled))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgAuthNoPending))
		}
	case arg != "":
		if !e.submitReauthCode(p, msg, arg) {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgAuthNoPending))
		}
	default:
		e.startReauth(p, msg, ra)
	}
}

// startReauth spawns the agent's login flow and posts the authorization URL to
// the thread. The outcome is reported later by awaitReauth.
func (e *Engine) startReauth(p Platform, msg *Message, ra AgentReauthenticator) {
	// Only one login can be in flight — a second would race the first for the
	// same credential file. Supersede rather than refuse, so a user who lost
	// the earlier link can always get a fresh one.
	e.cancelReauth()

	ctx, cancel := context.WithTimeout(e.ctx, reauthMaxWait)
	sess, err := ra.StartReauth(ctx)
	if err != nil {
		cancel()
		slog.Error("reauth: failed to start", "error", err, "user_id", msg.UserID)
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgAuthStartFailed, err))
		return
	}

	pr := &pendingReauth{
		session:    sess,
		platform:   p,
		replyCtx:   msg.ReplyCtx,
		sessionKey: msg.SessionKey,
		userID:     msg.UserID,
		cancel:     cancel,
	}
	e.reauth.mu.Lock()
	e.reauth.pending = pr
	e.reauth.mu.Unlock()

	slog.Info("audit: reauth_started", "user_id", msg.UserID, "platform", msg.Platform, "project", e.name)
	e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgAuthStarted, sess.URL()))

	go e.awaitReauth(ctx, pr)
}

// awaitReauth blocks on the login attempt and reports the result to the thread
// that started it.
func (e *Engine) awaitReauth(ctx context.Context, pr *pendingReauth) {
	err := pr.session.Wait(ctx)

	e.reauth.mu.Lock()
	if e.reauth.pending == pr {
		e.reauth.pending = nil
	}
	e.reauth.mu.Unlock()
	pr.cancel()

	if err != nil {
		slog.Warn("reauth: failed", "error", err, "user_id", pr.userID)
		e.send(pr.platform, pr.replyCtx, e.i18n.Tf(MsgAuthFailed, err))
		return
	}
	slog.Info("audit: reauth_succeeded", "user_id", pr.userID, "project", e.name)
	e.send(pr.platform, pr.replyCtx, e.i18n.T(MsgAuthSucceeded))
}

// submitReauthCode hands code to the pending attempt. It returns false when
// there is nothing waiting for a code, so callers can fall through.
func (e *Engine) submitReauthCode(p Platform, msg *Message, code string) bool {
	e.reauth.mu.Lock()
	pr := e.reauth.pending
	e.reauth.mu.Unlock()
	if pr == nil {
		return false
	}
	// Only the user who started the flow may complete it: anyone else
	// submitting a code would be logging the bot into their own account.
	if !strings.EqualFold(pr.userID, msg.UserID) {
		slog.Info("audit: reauth_code_rejected", "user_id", msg.UserID,
			"reason", "not the initiating user", "project", e.name)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgAuthNotInitiator))
		return true
	}
	if err := pr.session.SubmitCode(code); err != nil {
		slog.Error("reauth: submit code failed", "error", err)
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgAuthCodeFailed, err))
		return true
	}
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgAuthCodeReceived))
	return true
}

// cancelReauth aborts any attempt in progress. It reports whether there was one.
func (e *Engine) cancelReauth() bool {
	e.reauth.mu.Lock()
	pr := e.reauth.pending
	e.reauth.pending = nil
	e.reauth.mu.Unlock()
	if pr == nil {
		return false
	}
	pr.session.Cancel()
	pr.cancel()
	return true
}

// handlePendingReauthCode lets the user answer the authorization prompt by
// pasting the code straight into the thread, with no command prefix — the
// point of the flow is that it needs nothing but the messaging app.
func (e *Engine) handlePendingReauthCode(p Platform, msg *Message, content string) bool {
	if strings.HasPrefix(content, "/") {
		return false
	}
	e.reauth.mu.Lock()
	pr := e.reauth.pending
	e.reauth.mu.Unlock()
	if pr == nil {
		return false
	}
	// Scope to the originating thread and user so a code-shaped message
	// elsewhere is still delivered to the agent as an ordinary message.
	if pr.sessionKey != msg.SessionKey || !strings.EqualFold(pr.userID, msg.UserID) {
		return false
	}
	code := strings.TrimSpace(content)
	if !looksLikeReauthCode(code) {
		return false
	}
	return e.submitReauthCode(p, msg, code)
}
