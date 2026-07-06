package jira

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// NewTransport is the real Jira Cloud REST transport — the one piece that holds
// the host-only credential (ADR-0001: base URL + email + API token stay on the
// host, never crossing the sandbox boundary). Jira Cloud authenticates with HTTP
// Basic auth over `email:api_token` (not the password). Kept thin and out of the
// unit suite; the Client logic is exercised with a fake transport.
func NewTransport(baseURL, email, apiToken string) Transport {
	base := strings.TrimRight(baseURL, "/")
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+apiToken))
	return func(method, path string, body any) (json.RawMessage, error) {
		var reader io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			reader = bytes.NewReader(b)
		}

		req, err := http.NewRequest(method, base+path, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("accept", "application/json")
		req.Header.Set("authorization", auth)
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
			return nil, fmt.Errorf("Jira HTTP %d: %s", res.StatusCode, string(payload))
		}
		return payload, nil
	}
}
