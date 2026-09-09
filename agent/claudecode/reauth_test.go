package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestIsAuthError(t *testing.T) {
	a := &Agent{}

	// Verbatim from a hosted instance whose subscription token lapsed.
	authErrors := []string{
		"Failed to authenticate. API Error: 401 OAuth access token has expired. Re-authenticate to continue.",
		"Failed to authenticate. API Error: 401 Invalid authentication credentials",
		"Invalid API key · Please run /login",
		"API Error: 401 {\"type\":\"error\"}",
	}
	for _, msg := range authErrors {
		if !a.IsAuthError(msg) {
			t.Errorf("IsAuthError(%q) = false, want true", msg)
		}
	}

	notAuthErrors := []string{
		"API Error: 500 Internal server error",
		"API Error: 429 rate limit exceeded",
		"read stdout: file already closed",
		"Session not found",
		"",
	}
	for _, msg := range notAuthErrors {
		if a.IsAuthError(msg) {
			t.Errorf("IsAuthError(%q) = true, want false", msg)
		}
	}
}

func TestHandleResult_AuthFailureBecomesErrorEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cs := &claudeSession{events: make(chan core.Event, 8), ctx: ctx}
	cs.sessionID.Store("test-session")
	cs.alive.Store(true)

	cs.handleResult(map[string]any{
		"type":       "result",
		"subtype":    "error_during_execution",
		"is_error":   true,
		"result":     "Failed to authenticate. API Error: 401 OAuth access token has expired. Re-authenticate to continue.",
		"session_id": "test-session",
	})

	evt := <-cs.events
	if evt.Type != core.EventError {
		t.Fatalf("event type = %v, want EventError", evt.Type)
	}
	if evt.Error == nil || !strings.Contains(evt.Error.Error(), "401") {
		t.Fatalf("error = %v, want the CLI auth failure", evt.Error)
	}
}

func TestHandleResult_NonAuthErrorStaysAResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cs := &claudeSession{events: make(chan core.Event, 8), ctx: ctx}
	cs.sessionID.Store("test-session")
	cs.alive.Store(true)

	// The agent reporting someone else's 401 in its answer must not be
	// mistaken for the CLI's own credentials failing.
	cs.handleResult(map[string]any{
		"type":       "result",
		"result":     "The Stripe call returned API Error: 401, so the key is wrong.",
		"session_id": "test-session",
	})

	evt := <-cs.events
	if evt.Type != core.EventResult {
		t.Fatalf("event type = %v, want EventResult", evt.Type)
	}
}

func TestHandleResult_ErrorWithoutAuthMarkerStaysAResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cs := &claudeSession{events: make(chan core.Event, 8), ctx: ctx}
	cs.sessionID.Store("test-session")
	cs.alive.Store(true)

	cs.handleResult(map[string]any{
		"type":     "result",
		"is_error": true,
		"result":   "Execution failed: tool exited with code 2",
	})

	evt := <-cs.events
	if evt.Type != core.EventResult {
		t.Fatalf("event type = %v, want EventResult", evt.Type)
	}
}

// fakeClaudeBin writes a stand-in for the Claude CLI that implements just
// enough of `auth login` and `auth status` to drive the flow end to end.
func fakeClaudeBin(t *testing.T, stateDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := `#!/bin/sh
if [ "$2" = "status" ]; then
  if [ -f "` + stateDir + `/logged_in" ]; then
    echo '{"loggedIn":true,"authMethod":"claude.ai"}'
  else
    echo '{"loggedIn":false}'
  fi
  exit 0
fi
echo "Browser didn't open? Use the url below to sign in:"
echo "https://claude.ai/oauth/authorize?code=true&state=abc123"
echo "Paste code here if prompted >"
read code
if [ "$code" = "good-code" ]; then
  touch "` + stateDir + `/logged_in"
  echo "Login successful"
  exit 0
fi
echo "OAuth error: invalid_grant"
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

func TestStartReauth_SuccessfulLogin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI is a POSIX shell script")
	}
	stateDir := t.TempDir()
	a := &Agent{cliBin: fakeClaudeBin(t, stateDir), workDir: t.TempDir()}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := a.StartReauth(ctx)
	if err != nil {
		t.Fatalf("StartReauth: %v", err)
	}
	defer sess.Cancel()

	if !strings.HasPrefix(sess.URL(), "https://claude.ai/oauth/authorize") {
		t.Fatalf("URL = %q, want the authorization link", sess.URL())
	}

	if err := sess.SubmitCode("good-code"); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	if err := sess.Wait(ctx); err != nil {
		t.Fatalf("Wait = %v, want nil after a successful login", err)
	}
}

func TestStartReauth_RejectedCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI is a POSIX shell script")
	}
	stateDir := t.TempDir()
	a := &Agent{cliBin: fakeClaudeBin(t, stateDir), workDir: t.TempDir()}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := a.StartReauth(ctx)
	if err != nil {
		t.Fatalf("StartReauth: %v", err)
	}
	defer sess.Cancel()

	if err := sess.SubmitCode("stale-code"); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}
	err = sess.Wait(ctx)
	if err == nil {
		t.Fatal("Wait = nil, want a failure for a rejected code")
	}
	// The CLI's own explanation is what makes the failure actionable.
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error = %v, want it to carry the CLI output", err)
	}
}

func TestStartReauth_CancelStopsTheProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI is a POSIX shell script")
	}
	stateDir := t.TempDir()
	a := &Agent{cliBin: fakeClaudeBin(t, stateDir), workDir: t.TempDir()}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := a.StartReauth(ctx)
	if err != nil {
		t.Fatalf("StartReauth: %v", err)
	}
	sess.Cancel()

	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	if err := sess.Wait(waitCtx); err == nil {
		t.Fatal("Wait = nil after Cancel, want a failure")
	}
	if err := sess.SubmitCode("good-code"); err == nil {
		t.Error("SubmitCode succeeded after Cancel")
	}
}

func TestStartReauth_MissingBinary(t *testing.T) {
	a := &Agent{cliBin: filepath.Join(t.TempDir(), "does-not-exist"), workDir: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := a.StartReauth(ctx); err == nil {
		t.Fatal("StartReauth succeeded with a missing CLI binary")
	}
}

func TestReauthEnv_StripsProviderCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-leak")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-should-not-leak")
	t.Setenv("CLAUDECODE", "1")

	for _, kv := range reauthEnv(core.SpawnOptions{}) {
		switch {
		case strings.HasPrefix(kv, "ANTHROPIC_API_KEY="),
			strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN="),
			strings.HasPrefix(kv, "CLAUDECODE="):
			t.Errorf("reauth environment leaks %q", kv)
		}
	}
}
