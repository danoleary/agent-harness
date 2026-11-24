package github

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// roundTripFunc is a fake http.RoundTripper: each call takes the next scripted
// reply, so a test drives the transport without the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type reply struct {
	status int
	body   string
	err    error
}

// scripted returns a transport over a fake round-tripper that answers with
// replies in order, and the number of requests it saw.
func scripted(t *testing.T, replies ...reply) (Transport, *int, *[]time.Duration) {
	t.Helper()
	calls := 0
	var slept []time.Duration
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls >= len(replies) {
			t.Fatalf("request %d: no scripted reply left", calls+1)
		}
		rep := replies[calls]
		calls++
		if rep.err != nil {
			return nil, rep.err
		}
		return &http.Response{
			StatusCode: rep.status,
			Body:       io.NopCloser(strings.NewReader(rep.body)),
			Header:     http.Header{},
			Request:    r,
		}, nil
	})
	tr := newTransport("tok", &http.Client{Transport: rt}, func(d time.Duration) { slept = append(slept, d) })
	return tr, &calls, &slept
}

// incidentTimeout is the error the #444 run hit on its review-stage ticket read:
// a TCP read that timed out under GET /repos/…/issues/444.
func incidentTimeout() error {
	return &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ETIMEDOUT}}
}

// The #444 incident: the review stage's first tracker read timed out at the TCP
// layer, and the same read succeeded a second later. One blip must cost a retry,
// not the run.
func TestTransportRetriesAReadThatTimedOutAtTheTransportLayer(t *testing.T) {
	tr, calls, slept := scripted(t,
		reply{err: incidentTimeout()},
		reply{status: 200, body: `{"number":444}`},
	)
	got, err := tr("GET", "/repos/danoleary/agent-harness/issues/444", nil)
	if err != nil {
		t.Fatalf("GET = %v, want it to succeed after one retry", err)
	}
	if string(got) != `{"number":444}` {
		t.Errorf("payload = %s", got)
	}
	if *calls != 2 {
		t.Errorf("requests = %d, want 2 (the timeout, then one retry)", *calls)
	}
	if len(*slept) != 1 || (*slept)[0] <= 0 {
		t.Errorf("backoffs = %v, want one short pause before the retry", *slept)
	}
}

// A request that is not safe to repeat is never retried: a POST that timed out
// may have landed, and a second one would comment or create twice.
func TestTransportDoesNotRetryANonIdempotentRequest(t *testing.T) {
	tr, calls, _ := scripted(t, reply{err: incidentTimeout()})
	if _, err := tr("POST", "/repos/o/r/issues/1/comments", map[string]string{"body": "x"}); err == nil {
		t.Fatal("POST = nil error, want the timeout surfaced")
	}
	if *calls != 1 {
		t.Errorf("requests = %d, want 1", *calls)
	}
}

// A 4xx is GitHub's verdict, not a blip: it surfaces at once.
func TestTransportDoesNotRetryAClientError(t *testing.T) {
	tr, calls, _ := scripted(t, reply{status: 404, body: `{"message":"Not Found"}`})
	_, err := tr("GET", "/repos/o/r/issues/1", nil)
	if !errors.Is(err, errNotFound) {
		t.Errorf("GET = %v, want the 404", err)
	}
	if *calls != 1 {
		t.Errorf("requests = %d, want 1", *calls)
	}
}

// The retries are bounded: a network that stays down fails the call with the
// last transport error rather than spinning.
func TestTransportGivesUpAfterBoundedRetries(t *testing.T) {
	replies := make([]reply, maxRESTAttempts)
	for i := range replies {
		replies[i] = reply{err: incidentTimeout()}
	}
	tr, calls, _ := scripted(t, replies...)
	_, err := tr("GET", "/user", nil)
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("GET = %v, want the last timeout", err)
	}
	if *calls != maxRESTAttempts {
		t.Errorf("requests = %d, want %d", *calls, maxRESTAttempts)
	}
}

// A gateway error that clears on the next attempt is retried too.
func TestTransportRetriesAGatewayError(t *testing.T) {
	tr, calls, _ := scripted(t,
		reply{status: 502, body: "<html>Bad Gateway</html>"},
		reply{status: 200, body: `{}`},
	)
	if _, err := tr("GET", "/user", nil); err != nil {
		t.Fatalf("GET = %v, want success after the 502", err)
	}
	if *calls != 2 {
		t.Errorf("requests = %d, want 2", *calls)
	}
}

// The real transport bounds every request, so a stalled connection fails (and
// is retried) instead of hanging the stage forever: the default client has no
// timeout at all.
func TestNewTransportBoundsEachRequest(t *testing.T) {
	saved := restClient.Transport
	t.Cleanup(func() { restClient.Transport = saved })
	var deadline bool
	restClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, deadline = r.Context().Deadline()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}, Request: r}, nil
	})
	if _, err := NewTransport("tok")("GET", "/user", nil); err != nil {
		t.Fatalf("GET = %v", err)
	}
	if !deadline {
		t.Error("the request carried no deadline, want restTimeout to bound it")
	}
}

func TestShouldRetryREST(t *testing.T) {
	timeoutURL := &url.Error{Op: "Get", URL: githubAPIBase + "/user", Err: incidentTimeout()}
	cases := []struct {
		name   string
		method string
		status int
		err    error
		want   bool
	}{
		{"tcp read timeout", "GET", 0, incidentTimeout(), true},
		{"client timeout wrapped in url.Error", "GET", 0, timeoutURL, true},
		{"connection reset", "GET", 0, &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}, true},
		{"connection refused", "GET", 0, &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}, true},
		{"dns failure", "GET", 0, &url.Error{Op: "Get", URL: githubAPIBase, Err: &net.DNSError{Err: "no such host", Name: "api.github.com"}}, true},
		{"eof", "GET", 0, &url.Error{Op: "Get", URL: githubAPIBase, Err: io.EOF}, true},
		{"unexpected eof reading the body", "GET", 0, io.ErrUnexpectedEOF, true},
		{"502", "GET", 502, nil, true},
		{"503", "GET", 503, nil, true},
		{"504", "GET", 504, nil, true},
		{"500 is a server verdict", "GET", 500, nil, false},
		{"401", "GET", 401, nil, false},
		{"403", "GET", 403, nil, false},
		{"404", "GET", 404, nil, false},
		{"422", "GET", 422, nil, false},
		{"200", "GET", 200, nil, false},
		{"some other error", "GET", 0, errors.New("boom"), false},
		{"a url.Error that is not a timeout", "GET", 0, &url.Error{Op: "Get", URL: githubAPIBase, Err: errors.New("unsupported protocol scheme")}, false},
		{"HEAD timeout", "HEAD", 0, incidentTimeout(), true},
		{"POST timeout", "POST", 0, incidentTimeout(), false},
		{"PATCH timeout", "PATCH", 0, incidentTimeout(), false},
		{"DELETE timeout", "DELETE", 0, incidentTimeout(), false},
		{"POST 503", "POST", 503, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldRetryREST(c.method, c.status, c.err); got != c.want {
				t.Errorf("shouldRetryREST(%s, %d, %v) = %v, want %v", c.method, c.status, c.err, got, c.want)
			}
		})
	}
}
