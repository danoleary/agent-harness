package linear

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const linearGraphQLURL = "https://api.linear.app/graphql"

// NewTransport is the real Linear GraphQL transport — the one piece that holds
// the host-only LINEAR_API_KEY (ADR-0001). Linear authenticates with the raw key
// in the Authorization header (no "Bearer" prefix). Kept thin and out of the
// unit suite; the Client logic is exercised with a fake transport.
func NewTransport(apiKey string) Transport {
	return func(query string, variables map[string]any) (json.RawMessage, error) {
		body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequest(http.MethodPost, linearGraphQLURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("authorization", apiKey)

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
			return nil, fmt.Errorf("Linear HTTP %d: %s", res.StatusCode, string(payload))
		}

		var parsed struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(payload, &parsed); err != nil {
			return nil, err
		}
		if len(parsed.Errors) > 0 {
			msgs := make([]string, len(parsed.Errors))
			for i, e := range parsed.Errors {
				msgs[i] = e.Message
			}
			return nil, fmt.Errorf("Linear GraphQL error: %s", strings.Join(msgs, "; "))
		}
		return parsed.Data, nil
	}
}
