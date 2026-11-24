package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/danoleary/agent-harness/internal/proc"
)

const githubAPIBase = "https://api.github.com"

// restTimeout bounds one REST request, headers and body, so a stalled
// connection fails (and is retried) instead of hanging the Stage: the default
// client has no timeout at all.
const restTimeout = 60 * time.Second

// restClient is the client [NewTransport] sends through.
var restClient = &http.Client{Timeout: restTimeout}

// maxRESTAttempts bounds one call's attempts (the first plus its retries), and
// restBackoff is the pause before retry n (1-based), so a network that stays
// down fails the call in seconds rather than spinning (#444).
const maxRESTAttempts = 3

func restBackoff(n int) time.Duration { return time.Duration(n) * time.Second }

// NewTransport is the real GitHub REST transport — the one piece that holds the
// host-only GH_TOKEN (ADR-0001: the credential stays on the host, never crossing
// the sandbox boundary). It reuses the harness's existing GitHub access. Kept thin
// and out of the unit suite; the Client logic is exercised with a fake transport.
// A read that fails at the transport layer is retried ([shouldRetryREST]), so one
// network blip does not fail a whole Stage (#444).
func NewTransport(token string) Transport {
	return newTransport(token, restClient, time.Sleep)
}

func newTransport(token string, client *http.Client, sleep func(time.Duration)) Transport {
	return func(method, path string, body any) (json.RawMessage, error) {
		var b []byte
		if body != nil {
			var err error
			if b, err = json.Marshal(body); err != nil {
				return nil, err
			}
		}
		for attempt := 1; ; attempt++ {
			status, payload, err := restAttempt(client, token, method, path, b)
			if err == nil && status >= 200 && status < 300 {
				return payload, nil
			}
			if attempt >= maxRESTAttempts || !shouldRetryREST(method, status, err) {
				if err != nil {
					return nil, err
				}
				return nil, httpError(status, payload)
			}
			sleep(restBackoff(attempt))
		}
	}
}

// restAttempt sends one request and reads its whole body. A non-nil error means
// no complete answer arrived; status is 0 when no response did.
func restAttempt(client *http.Client, token, method, path string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, githubAPIBase+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("accept", "application/vnd.github+json")
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("x-github-api-version", "2022-11-28")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}

	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()

	payload, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, nil, err
	}
	return res.StatusCode, payload, nil
}

// shouldRetryREST is the retry policy for one failed REST attempt: retry only a
// request that is safe to repeat (GET, HEAD), and only when GitHub never gave a
// verdict — a transport error (a timeout, a reset or refused connection, a DNS
// failure, a connection cut short at EOF) or a gateway error (502/503/504).
// A 4xx is a verdict and never retried. A POST, PATCH or DELETE that timed out
// may have landed, so repeating it could comment twice or fail on its own effect.
// It mirrors the CI poller's transient class (internal/ci isTransientErr, #194).
func shouldRetryREST(method string, status int, err error) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	if err == nil {
		switch status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// errNotFound marks a 404, so a caller can tell a reference that no longer
// resolves from a read that failed.
var errNotFound = errors.New("not found")

// httpError renders a non-2xx response as ONE line: GitHub's own `message` when
// the body is its JSON error shape, else the body with its newlines folded. The
// raw pretty-printed body used to spill over five log lines, and a 401 in the loop
// log read as noise ahead of "queue empty" rather than as a dead token.
func httpError(status int, payload []byte) error {
	var body struct {
		Message string `json:"message"`
	}
	msg := strings.Join(strings.Fields(string(payload)), " ")
	if json.Unmarshal(payload, &body) == nil && body.Message != "" {
		msg = body.Message
	}
	if status == http.StatusUnauthorized {
		return fmt.Errorf("GitHub HTTP %d: %s — GH_TOKEN was rejected (expired or revoked?)", status, msg)
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("GitHub HTTP %d: %s: %w", status, msg, errNotFound)
	}
	return fmt.Errorf("GitHub HTTP %d: %s", status, msg)
}

// CheckToken makes one cheap authenticated read (GET /user) so a dead GH_TOKEN is
// caught when the daemon starts, not discovered minutes later as a log full of
// failed polls.
func CheckToken(tr Transport) error {
	if _, err := tr("GET", "/user", nil); err != nil {
		return fmt.Errorf("startup GitHub check: %w; a replacement must be a classic PAT with `repo` and `workflow` scopes", err)
	}
	return nil
}

// ghAPITimeout bounds one `gh api` call, so a stalled network or a blocking gh auth
// prompt can never hang the PR loop's poll (the BEH-386 hang class).
const ghAPITimeout = 2 * time.Minute

// NewGhTransport is the GitHub REST transport over `gh api`, run in the Consumer's
// checkout (dir). gh fills the `{owner}`/`{repo}` placeholders in a path from that
// checkout's origin, so the PR loop ([Pulls]) needs no repo setting: it works the
// repository the harness already pushes to, with the GH_TOKEN gh reads from the
// host environment (ADR-0002). Each call is bounded (internal/proc). Kept thin and
// out of the unit suite, like [NewTransport].
func NewGhTransport(dir string) Transport {
	return func(method, path string, body any) (json.RawMessage, error) {
		args := []string{"api", "--method", method, strings.TrimPrefix(path, "/")}
		var stdin []byte
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			args = append(args, "--input", "-")
			stdin = b
		}
		out, _, err := proc.OutputInDirWithInput(ghAPITimeout, dir, stdin, "gh", args...)
		if err != nil {
			return nil, fmt.Errorf("gh api %s %s: %w", method, path, err)
		}
		if len(bytes.TrimSpace(out)) == 0 {
			return json.RawMessage("null"), nil
		}
		return out, nil
	}
}
