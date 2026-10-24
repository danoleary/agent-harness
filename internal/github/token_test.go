package github

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A rejected token must read as a credential problem the operator can act on, on
// one line — not a pretty-printed JSON body the loop log splits over five lines
// and an operator reads as "queue empty".
func TestHTTPErrorNamesARejectedTokenOnOneLine(t *testing.T) {
	err := httpError(401, []byte("{\n  \"message\": \"Bad credentials\",\n  \"status\": \"401\"\n}"))
	msg := err.Error()
	if strings.Contains(msg, "\n") {
		t.Errorf("error spans lines: %q", msg)
	}
	for _, want := range []string{"401", "Bad credentials", "GH_TOKEN"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q is missing %q", msg, want)
		}
	}
}

// A non-auth failure keeps GitHub's own message and doesn't blame the token.
func TestHTTPErrorKeepsGitHubsMessageForOtherStatuses(t *testing.T) {
	msg := httpError(422, []byte(`{"message":"Validation Failed"}`)).Error()
	if msg != "GitHub HTTP 422: Validation Failed" {
		t.Errorf("error = %q", msg)
	}
	if strings.Contains(msg, "GH_TOKEN") {
		t.Errorf("a 422 is not a token problem: %q", msg)
	}
}

// A body that isn't GitHub's JSON error shape is still flattened onto one line.
func TestHTTPErrorFlattensANonJSONBody(t *testing.T) {
	msg := httpError(502, []byte("<html>\nBad Gateway\n</html>")).Error()
	if strings.Contains(msg, "\n") || !strings.Contains(msg, "502") {
		t.Errorf("error = %q", msg)
	}
}

func TestCheckTokenProbesTheAuthenticatedUser(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{"GET /user": map[string]any{"login": "bot"}})
	if err := CheckToken(tr); err != nil {
		t.Fatalf("CheckToken() = %v, want nil", err)
	}
	if len(*calls) != 1 || (*calls)[0].path != "/user" {
		t.Errorf("calls = %v, want one GET /user", *calls)
	}
}

func TestCheckTokenSurfacesARejection(t *testing.T) {
	rejected := errors.New("GitHub HTTP 401: Bad credentials")
	tr := func(string, string, any) (json.RawMessage, error) { return nil, rejected }
	err := CheckToken(tr)
	if !errors.Is(err, rejected) || !strings.Contains(err.Error(), "classic PAT with `repo` and `workflow`") {
		t.Errorf("CheckToken() = %v, want it to wrap the rejection and say what a replacement needs", err)
	}
}
