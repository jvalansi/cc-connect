package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// authErrorMarkers are the substrings the Claude CLI reports when the stored
// credentials are expired, revoked, or otherwise rejected — the cases a user
// can fix by signing in again. Matched case-insensitively against agent error
// output only, never against the model's own text.
//
// Observed in the wild:
//
//	Failed to authenticate. API Error: 401 OAuth access token has expired. Re-authenticate to continue.
//	Failed to authenticate. API Error: 401 Invalid authentication credentials
//	Failed to authenticate: OAuth session expired and could not be refreshed
//	Failed to authenticate: OAuth token revoked. Please log in again or contact your administrator.
//	Authentication required · Sign in again to continue
var authErrorMarkers = []string{
	"api error: 401",
	"oauth token has expired",
	"oauth access token has expired",
	"oauth session expired",
	"oauth token revoked",
	"sign in again to continue",
	"invalid authentication credentials",
	"invalid api key",
	"please run /login",
}

// reauthURLPattern finds the authorization URL the CLI prints for the user.
var reauthURLPattern = regexp.MustCompile(`https://\S+`)

// reauthURLTimeout bounds how long StartReauth waits for the CLI to print the
// authorization URL before giving up on the attempt.
const reauthURLTimeout = 60 * time.Second

// reauthOutputTailLines is how much CLI output is kept to explain a failure.
const reauthOutputTailLines = 8

// IsAuthError reports whether msg is the CLI complaining about credentials
// rather than about the request. Implements core.AgentReauthenticator.
func (a *Agent) IsAuthError(msg string) bool { return isAuthErrorText(msg) }

// isAuthErrorText matches CLI output against the credential-failure markers.
func isAuthErrorText(msg string) bool {
	lower := strings.ToLower(msg)
	for _, marker := range authErrorMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// reauthSession drives one `claude auth login --claudeai` process: the CLI
// prints an authorization URL, waits on stdin for the code the user is shown,
// then exits. Implements core.ReauthSession.
type reauthSession struct {
	url    string
	cancel context.CancelFunc

	mu       sync.Mutex
	stdin    io.WriteCloser
	exited   bool
	tail     []string // trailing CLI output, for failure messages
	stopOnce sync.Once

	done chan struct{} // closed once result is set
	err  error         // valid after done is closed
}

func (s *reauthSession) URL() string { return s.url }

// SubmitCode writes the authorization code to the waiting CLI process.
func (s *reauthSession) SubmitCode(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("claudecode: empty authorization code")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return fmt.Errorf("claudecode: sign-in is no longer waiting for a code")
	}
	if s.stdin == nil {
		return fmt.Errorf("claudecode: sign-in input is closed")
	}
	// The code itself is a single-use secret — never log it.
	if _, err := io.WriteString(s.stdin, code+"\n"); err != nil {
		return fmt.Errorf("claudecode: submit authorization code: %w", err)
	}
	return nil
}

// Wait blocks until the login resolves or ctx is done.
func (s *reauthSession) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		return s.err
	case <-ctx.Done():
		s.Cancel()
		return fmt.Errorf("claudecode: sign-in timed out before it completed")
	}
}

// Cancel kills the login process. Safe to call more than once.
func (s *reauthSession) Cancel() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		if s.stdin != nil {
			_ = s.stdin.Close()
			s.stdin = nil
		}
		s.mu.Unlock()
		s.cancel()
	})
}

// appendTail records a line of CLI output for diagnostics, keeping only the
// last reauthOutputTailLines.
func (s *reauthSession) appendTail(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tail = append(s.tail, line)
	if len(s.tail) > reauthOutputTailLines {
		s.tail = s.tail[len(s.tail)-reauthOutputTailLines:]
	}
}

func (s *reauthSession) tailText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(strings.Join(s.tail, " "))
}

// StartReauth spawns the CLI's subscription login flow and returns once the
// authorization URL is known. Implements core.AgentReauthenticator.
func (a *Agent) StartReauth(ctx context.Context) (core.ReauthSession, error) {
	a.mu.Lock()
	bin := a.cliBin
	opts := a.spawnOpts
	workDir := a.workDir
	a.mu.Unlock()

	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := core.BuildSpawnCommand(cmdCtx, opts, bin, "auth", "login", "--claudeai")
	cmd.Dir = workDir
	cmd.Env = reauthEnv(opts)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claudecode: reauth stdin: %w", err)
	}
	// The CLI prints the URL on stdout and progress on stderr; merge them so
	// one scanner sees whichever stream carries the link.
	outR, outW := io.Pipe()
	cmd.Stdout = outW
	cmd.Stderr = outW

	if err := cmd.Start(); err != nil {
		cancel()
		_ = stdin.Close()
		_ = outW.Close()
		return nil, fmt.Errorf("claudecode: start `%s auth login`: %w", bin, err)
	}

	s := &reauthSession{
		cancel: cancel,
		stdin:  stdin,
		done:   make(chan struct{}),
	}

	urlCh := make(chan string, 1)
	go s.scanOutput(outR, urlCh)

	// Reap the process, then close the pipe so the scanner goroutine ends.
	waitErr := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = outW.Close()
		waitErr <- err
	}()

	select {
	case url := <-urlCh:
		s.url = url
	case err := <-waitErr:
		// Exited before printing a URL — nothing for the user to open.
		s.markExited()
		cancel()
		return nil, fmt.Errorf("claudecode: sign-in exited before showing a link: %w (%s)", err, s.tailText())
	case <-time.After(reauthURLTimeout):
		s.Cancel()
		return nil, fmt.Errorf("claudecode: timed out waiting for the sign-in link")
	case <-cmdCtx.Done():
		s.Cancel()
		return nil, fmt.Errorf("claudecode: sign-in cancelled before it started")
	}

	go s.finish(ctx, waitErr, bin, opts)
	return s, nil
}

// finish waits for the login process and resolves the session's result. The
// CLI's exit status is not trusted on its own: the authoritative answer is
// whether credentials now work, so `auth status` decides.
func (s *reauthSession) finish(ctx context.Context, waitErr <-chan error, bin string, opts core.SpawnOptions) {
	err := <-waitErr
	s.markExited()

	statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	loggedIn := authStatusLoggedIn(statusCtx, bin, opts)
	cancel()

	switch {
	case loggedIn:
		s.err = nil
	case err != nil:
		s.err = fmt.Errorf("%w (%s)", err, s.tailText())
	default:
		detail := s.tailText()
		if detail == "" {
			detail = "the code may have expired or already been used"
		}
		s.err = fmt.Errorf("%s", detail)
	}
	close(s.done)
	s.cancel()
}

func (s *reauthSession) markExited() {
	s.mu.Lock()
	s.exited = true
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	s.mu.Unlock()
}

// scanOutput reads merged CLI output, reporting the first authorization URL.
func (s *reauthSession) scanOutput(r io.Reader, urlCh chan<- string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	sentURL := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		s.appendTail(line)
		if !sentURL {
			if m := reauthURLPattern.FindString(line); m != "" {
				sentURL = true
				urlCh <- strings.TrimRight(m, ".,)")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Debug("claudecode: reauth output scan ended", "error", err)
	}
	// Keep draining: cmd.Wait blocks until the output copier finishes, and
	// io.Pipe writes block until read. Never leave the writer stuck.
	_, _ = io.Copy(io.Discard, r)
}

// reauthEnv builds the environment for the login process. Provider credentials
// are stripped: an API key in the environment makes the CLI use it instead of
// running the subscription sign-in the user asked for.
func reauthEnv(opts core.SpawnOptions) []string {
	env := filterEnv(os.Environ(), "CLAUDECODE")
	env = filterEnv(env, "ANTHROPIC_API_KEY")
	env = filterEnv(env, "ANTHROPIC_AUTH_TOKEN")
	env = append(env, "TERM=dumb")
	return core.FilterEnvForSpawn(env, opts)
}

// authStatusLoggedIn reports whether `claude auth status` says the CLI is
// authenticated. Any failure to run or parse it is treated as not logged in.
func authStatusLoggedIn(ctx context.Context, bin string, opts core.SpawnOptions) bool {
	cmd := core.BuildSpawnCommand(ctx, opts, bin, "auth", "status")
	cmd.Env = reauthEnv(opts)
	// A non-zero exit is itself an answer ("not logged in"), so only give up
	// when the command produced nothing to read.
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		slog.Debug("claudecode: auth status failed to run", "error", err)
		return false
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		// Older CLI builds print prose rather than JSON.
		return strings.Contains(strings.ToLower(string(out)), "logged in")
	}
	return status.LoggedIn
}
