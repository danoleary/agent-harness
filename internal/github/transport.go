package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
			return nil, fmt.Errorf("GitHub HTTP %d: %s", res.StatusCode, string(payload))
		}
		return payload, nil
	}
}
