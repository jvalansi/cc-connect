package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReauthSession is a scripted ReauthSession for engine-level tests.
type fakeReauthSession struct {
	mu        sync.Mutex
	url       string
	codes     []string
	submitErr error
	cancelled bool
	result    error
	done      chan struct{}
}

func newFakeReauthSession(url string) *fakeReauthSession {
	return &fakeReauthSession{url: url, done: make(chan struct{})}
}

func (f *fakeReauthSession) URL() string { return f.url }

func (f *fakeReauthSession) SubmitCode(code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return f.submitErr
	}
	f.codes = append(f.codes, code)
	return nil
}

func (f *fakeReauthSession) Wait(ctx context.Context) error {
	select {
	case <-f.done:
		return f.result
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeReauthSession) Cancel() {
	f.mu.Lock()
	f.cancelled = true
	f.mu.Unlock()
}

func (f *fakeReauthSession) finish(err error) {
	f.mu.Lock()
	f.result = err
	f.mu.Unlock()
	close(f.done)
}

func (f *fakeReauthSession) submitted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.codes...)
}

func (f *fakeReauthSession) wasCancelled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled
}

// stubReauthAgent is a stubAgent that supports in-chat re-authentication.
type stubReauthAgent struct {
	stubAgent
	session  *fakeReauthSession
	startErr error
	starts   int
	mu       sync.Mutex
}

func (a *stubReauthAgent) IsAuthError(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "api error: 401")
}

func (a *stubReauthAgent) StartReauth(_ context.Context) (ReauthSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.starts++
	if a.startErr != nil {
		return nil, a.startErr
	}
	return a.session, nil
}

func (a *stubReauthAgent) startCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.starts
}

func newReauthTestEngine(t *testing.T, ag Agent) (*Engine, *stubPlatformEngine) {
	t.Helper()
	p := &stubPlatformEngine{n: "stub"}
	e := NewEngine("test", ag, []Platform{p}, t.TempDir()+"/sessions.json", LangEnglish)
	e.SetAdminFrom("admin")
	t.Cleanup(func() { e.cancelReauth() })
	return e, p
}

func authMsg(text string) *Message {
	return &Message{UserID: "admin", Platform: "stub", SessionKey: "chat-1", Content: text}
}

func TestLooksLikeReauthCode(t *testing.T) {
	codes := []string{
		"aBc123_xyz-QRS456tuv#state-9",
		strings.Repeat("a", 20),
		"sk-ant-oat01-abcdefghijklmnop",
	}
	for _, c := range codes {
		if !looksLikeReauthCode(c) {
			t.Errorf("looksLikeReauthCode(%q) = false, want true", c)
		}
	}

	// Ordinary messages must never be swallowed as codes.
	notCodes := []string{
		"",
		"short",
		"please fix the failing test in core/engine.go",
		"yes",
		"run the build again and tell me what breaks",
		strings.Repeat("a", 513),
	}
	for _, c := range notCodes {
		if looksLikeReauthCode(c) {
			t.Errorf("looksLikeReauthCode(%q) = true, want false", c)
		}
	}
}

func TestAuthErrorHint(t *testing.T) {
	ag := &stubReauthAgent{session: newFakeReauthSession("https://claude.ai/oauth/authorize?x=1")}
	e, _ := newReauthTestEngine(t, ag)

	hint, ok := e.authErrorHint(nil, "Failed to authenticate. API Error: 401 OAuth access token has expired. Re-authenticate to continue.")
	if !ok {
		t.Fatal("authErrorHint did not recognize an expired-token error")
	}
	if !strings.Contains(hint, "/auth") {
		t.Errorf("hint %q does not mention /auth", hint)
	}

	if _, ok := e.authErrorHint(nil, "tool exited with code 2"); ok {
		t.Error("authErrorHint matched an unrelated error")
	}
}

func TestAuthErrorHint_AgentWithoutSupport(t *testing.T) {
	e, _ := newReauthTestEngine(t, &stubAgent{})
	if _, ok := e.authErrorHint(nil, "API Error: 401 Invalid authentication credentials"); ok {
		t.Error("authErrorHint should be inert for agents that cannot re-authenticate")
	}
}

func TestCmdAuth_PostsURLAndAcceptsPastedCode(t *testing.T) {
	sess := newFakeReauthSession("https://claude.ai/oauth/authorize?code=1")
	ag := &stubReauthAgent{session: sess}
	e, p := newReauthTestEngine(t, ag)

	e.cmdAuth(p, authMsg("/auth"), nil)

	sent := p.getSent()
	if len(sent) != 1 || !strings.Contains(sent[0], sess.URL()) {
		t.Fatalf("expected the authorization URL to be posted, got %v", sent)
	}

	// The point of the flow: the user answers in the thread, no command needed.
	p.clearSent()
	code := "abcDEF123456789012345#state"
	if !e.handlePendingReauthCode(p, authMsg(code), code) {
		t.Fatal("pasted code was not consumed by the pending flow")
	}
	if got := sess.submitted(); len(got) != 1 || got[0] != code {
		t.Fatalf("submitted codes = %v, want [%s]", got, code)
	}

	sess.finish(nil)
	waitForSent(t, p, "Signed in")
}

func TestHandlePendingReauthCode_IgnoresOrdinaryMessages(t *testing.T) {
	sess := newFakeReauthSession("https://claude.ai/oauth/authorize")
	ag := &stubReauthAgent{session: sess}
	e, p := newReauthTestEngine(t, ag)
	e.cmdAuth(p, authMsg("/auth"), nil)

	prose := "please re-run the tests and paste the output"
	if e.handlePendingReauthCode(p, authMsg(prose), prose) {
		t.Error("a normal message was swallowed as an authorization code")
	}
	if got := sess.submitted(); len(got) != 0 {
		t.Errorf("unexpected submissions: %v", got)
	}
}

func TestHandlePendingReauthCode_OtherUserAndThreadIgnored(t *testing.T) {
	sess := newFakeReauthSession("https://claude.ai/oauth/authorize")
	ag := &stubReauthAgent{session: sess}
	e, p := newReauthTestEngine(t, ag)
	e.cmdAuth(p, authMsg("/auth"), nil)

	code := "abcDEF123456789012345#state"

	other := authMsg(code)
	other.UserID = "someone-else"
	if e.handlePendingReauthCode(p, other, code) {
		t.Error("another user's message was treated as the authorization code")
	}

	elsewhere := authMsg(code)
	elsewhere.SessionKey = "chat-2"
	if e.handlePendingReauthCode(p, elsewhere, code) {
		t.Error("a message in another thread was treated as the authorization code")
	}

	if got := sess.submitted(); len(got) != 0 {
		t.Errorf("unexpected submissions: %v", got)
	}
}

func TestSubmitReauthCode_RejectsNonInitiator(t *testing.T) {
	sess := newFakeReauthSession("https://claude.ai/oauth/authorize")
	ag := &stubReauthAgent{session: sess}
	e, p := newReauthTestEngine(t, ag)
	e.cmdAuth(p, authMsg("/auth"), nil)
	p.clearSent()

	// /auth <code> from a different admin must not log the bot into their account.
	other := authMsg("/auth")
	other.UserID = "admin2"
	e.cmdAuth(p, other, []string{"abcDEF123456789012345#state"})

	if got := sess.submitted(); len(got) != 0 {
		t.Fatalf("code from a non-initiator was submitted: %v", got)
	}
	sent := p.getSent()
	if len(sent) != 1 || !strings.Contains(sent[0], "Only the user who ran /auth") {
		t.Fatalf("expected a rejection notice, got %v", sent)
	}
}

func TestCmdAuth_SupersedesEarlierAttempt(t *testing.T) {
	first := newFakeReauthSession("https://claude.ai/first")
	ag := &stubReauthAgent{session: first}
	e, p := newReauthTestEngine(t, ag)
	e.cmdAuth(p, authMsg("/auth"), nil)

	second := newFakeReauthSession("https://claude.ai/second")
	ag.mu.Lock()
	ag.session = second
	ag.mu.Unlock()
	e.cmdAuth(p, authMsg("/auth"), nil)

	if !first.wasCancelled() {
		t.Error("the superseded attempt was left running")
	}
	if ag.startCount() != 2 {
		t.Errorf("StartReauth called %d times, want 2", ag.startCount())
	}
	if e.reauth.pending == nil || e.reauth.pending.session != second {
		t.Error("the newest attempt is not the pending one")
	}

	// The superseded attempt dying must not contradict the fresh link the
	// user was just handed.
	p.clearSent()
	first.finish(errors.New("killed"))
	time.Sleep(100 * time.Millisecond)
	for _, s := range p.getSent() {
		if strings.Contains(s, "Sign-in failed") {
			t.Errorf("superseded attempt reported a failure: %q", s)
		}
	}
}

func TestCmdAuth_CancelAndNoPending(t *testing.T) {
	sess := newFakeReauthSession("https://claude.ai/oauth/authorize")
	ag := &stubReauthAgent{session: sess}
	e, p := newReauthTestEngine(t, ag)

	e.cmdAuth(p, authMsg("/auth"), []string{"cancel"})
	if sent := p.getSent(); len(sent) != 1 || !strings.Contains(sent[0], "No sign-in is waiting") {
		t.Fatalf("expected a no-pending notice, got %v", sent)
	}

	p.clearSent()
	e.cmdAuth(p, authMsg("/auth"), nil)
	p.clearSent()
	e.cmdAuth(p, authMsg("/auth"), []string{"cancel"})
	if !sess.wasCancelled() {
		t.Error("cancel did not stop the login process")
	}
	if e.reauth.pending != nil {
		t.Error("pending attempt survived cancellation")
	}
}

func TestCmdAuth_ReportsFailure(t *testing.T) {
	sess := newFakeReauthSession("https://claude.ai/oauth/authorize")
	ag := &stubReauthAgent{session: sess}
	e, p := newReauthTestEngine(t, ag)
	e.cmdAuth(p, authMsg("/auth"), nil)
	p.clearSent()

	sess.finish(errors.New("code expired"))
	waitForSent(t, p, "code expired")

	if e.reauth.pending != nil {
		t.Error("a resolved attempt is still pending")
	}
}

func TestCmdAuth_StartFailure(t *testing.T) {
	ag := &stubReauthAgent{startErr: errors.New("claude binary missing")}
	e, p := newReauthTestEngine(t, ag)

	e.cmdAuth(p, authMsg("/auth"), nil)

	sent := p.getSent()
	if len(sent) != 1 || !strings.Contains(sent[0], "claude binary missing") {
		t.Fatalf("expected the start failure to be reported, got %v", sent)
	}
	if e.reauth.pending != nil {
		t.Error("a failed start left a pending attempt behind")
	}
}

func TestCmdAuth_UnsupportedAgent(t *testing.T) {
	e, p := newReauthTestEngine(t, &stubAgent{})
	e.cmdAuth(p, authMsg("/auth"), nil)

	sent := p.getSent()
	if len(sent) != 1 || !strings.Contains(sent[0], "does not support") {
		t.Fatalf("expected an unsupported notice, got %v", sent)
	}
}

// waitForSent waits for a message containing want to reach the platform.
func waitForSent(t *testing.T, p *stubPlatformEngine, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range p.getSent() {
			if strings.Contains(s, want) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no message containing %q was sent; got %v", want, p.getSent())
}
