package maintenance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type coreRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn coreRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestPrepareUpstreamReleaseParsing(t *testing.T) {
	for _, tc := range []struct {
		label, body, prefix, want string
		status                    int
		invalid                   bool
	}{
		{"release", `{"tag_name":"v1.10.0","other":true}`, "v", "1.10.0", 200, false},
		{"literal-prefix", `{"tag_name":"release.1.2"}`, "release.", "1.2", 200, false},
		{"empty-prefix", `{"tag_name":"1.2"}`, "", "1.2", 200, false},
		{"wrong-prefix", `{"tag_name":"v1.2"}`, "release.", "", 200, true},
		{"wrong-version", `{"tag_name":"v1.2-rc1"}`, "v", "", 200, true},
		{"missing-tag", `{"name":"v1.2"}`, "v", "", 200, true},
		{"null-tag", `{"tag_name":null}`, "v", "", 200, true},
		{"numeric-tag", `{"tag_name":123}`, "v", "", 200, true},
		{"malformed", `{"tag_name":`, "v", "", 200, true},
		{"trailing-json", `{"tag_name":"v1.2"}{}`, "v", "", 200, true},
		{"not-found", `{"tag_name":"v1.2"}`, "v", "", 404, true},
		{"server-error", `{"tag_name":"v1.2"}`, "v", "", 500, true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: coreRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				if request.URL.String() != "https://api.github.com/repos/owner/repository/releases/latest" || request.Method != http.MethodGet {
					t.Fatalf("unexpected upstream request: %s %s", request.Method, request.URL)
				}
				if request.Header.Get("Authorization") != "" {
					t.Fatal("upstream request contains authorization")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header), Request: request}, nil
			})}
			service := &Service{client: client}
			version, err := service.latestVersion(context.Background(), Package{UpstreamRepo: "owner/repository", TagPrefix: tc.prefix})
			if requests != 1 {
				t.Fatalf("upstream requests = %d", requests)
			}
			if tc.invalid {
				if err == nil {
					t.Fatalf("accepted invalid release: %s", tc.body)
				}
			} else if err != nil || version != tc.want {
				t.Fatalf("release = %q, %v", version, err)
			}
		})
	}
}

func TestPrepareUpstreamRequestContextAndFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service := &Service{client: &http.Client{Transport: coreRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Context().Err() != context.Canceled {
			t.Fatal("upstream request lost cancellation")
		}
		return nil, request.Context().Err()
	})}}
	if _, err := service.latestVersion(ctx, Package{UpstreamRepo: "owner/repo"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("upstream cancellation: %v", err)
	}
}
