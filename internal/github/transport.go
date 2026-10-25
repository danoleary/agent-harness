package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/proc"
)

const githubAPIBase = "https://api.github.com"

// NewTransport is the real GitHub REST transport — the one piece that holds the
// host-only GH_TOKEN (ADR-0001: the credential stays on the host, never crossing
// the sandbox boundary). It reuses the harness's existing GitHub access. Kept thin
// and out of the unit suite; the Client logic is exercised with a fake transport.
func NewTransport(token string) Transport {
	return func(method, path string, body any) (json.RawMessage, error) {
		var reader io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			reader = bytes.NewReader(b)
		}

		req, err := http.NewRequest(method, githubAPIBase+path, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("accept", "application/vnd.github+json")
		req.Header.Set("authorization", "Bearer "+token)
		req.Header.Set("x-github-api-version", "2022-11-28")
		if body != nil {
			req.Header.Set("content-type", "application/json")
		}

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()

		payload, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return nil, httpError(res.StatusCode, payload)
		}
		return payload, nil
	}
}

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
