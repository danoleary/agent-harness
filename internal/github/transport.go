package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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
