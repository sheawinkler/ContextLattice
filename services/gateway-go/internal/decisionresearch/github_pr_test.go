package decisionresearch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type githubPRRoundTripper func(*http.Request) (*http.Response, error)

func (f githubPRRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func githubPRTestResponse(req *http.Request, status int, value any) *http.Response {
	body, _ := json.Marshal(value)
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Request:    req,
	}
}

func validGitHubPRFixture() githubPRResponse {
	updated := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	merged := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	pr := githubPRResponse{
		Number:         42,
		HTMLURL:        "https://github.com/acme/engine/pull/42",
		State:          "closed",
		Merged:         true,
		MergedAt:       &merged,
		UpdatedAt:      updated,
		MergeCommitSHA: strings.Repeat("c", 40),
	}
	pr.Head.SHA = strings.Repeat("a", 40)
	pr.Head.Repo.FullName = "contributor/engine"
	pr.Base.SHA = strings.Repeat("b", 40)
	pr.Base.Repo.FullName = "acme/engine"
	return pr
}

func TestReadGitHubPRUsesConfiguredForkHeadAndReturnsBoundedProof(t *testing.T) {
	pr := validGitHubPRFixture()
	client := &http.Client{Transport: githubPRRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != "https://api.github.com/repos/acme/engine/pulls/42" {
			t.Fatalf("unexpected request target: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatal("anonymous request unexpectedly carried authorization")
		}
		return githubPRTestResponse(req, http.StatusOK, pr), nil
	})}

	got, err := readGitHubPR(context.Background(), "acme/engine", 42, strings.Repeat("a", 40), "", client)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "merged" || got.Revision != strings.Repeat("a", 40) || got.EffectiveAt != *pr.MergedAt || len(got.ContentHash) != 64 {
		t.Fatalf("unexpected observation: %+v", got)
	}
	if got.Revision == pr.Base.SHA {
		t.Fatal("observation used the base SHA instead of the fork head SHA")
	}
}

func TestReadGitHubPRRejectsBaseSHAAsExpectedHead(t *testing.T) {
	pr := validGitHubPRFixture()
	client := &http.Client{Transport: githubPRRoundTripper(func(req *http.Request) (*http.Response, error) {
		return githubPRTestResponse(req, http.StatusOK, pr), nil
	})}
	if _, err := readGitHubPR(context.Background(), "acme/engine", 42, pr.Base.SHA, "", client); err == nil {
		t.Fatal("base SHA was accepted as the configured PR head")
	}
}

func TestReadGitHubPRAuthenticatedChecksAccountAndPullPermission(t *testing.T) {
	pr := validGitHubPRFixture()
	pull := true
	responses := map[string]any{
		"/user": map[string]string{"login": "operator"},
		"/repos/acme/engine": map[string]any{
			"full_name": "acme/engine", "html_url": "https://github.com/acme/engine",
			"permissions": map[string]bool{"pull": pull},
		},
		"/repos/acme/engine/pulls/42": pr,
	}
	var requested []string
	client := &http.Client{Transport: githubPRRoundTripper(func(req *http.Request) (*http.Response, error) {
		requested = append(requested, req.URL.Path)
		if req.URL.Scheme != "https" || req.URL.Host != githubPRAPIHost || req.Method != http.MethodGet {
			t.Fatalf("request escaped fixed read-only API target: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer test-secret" {
			t.Fatal("expected explicit feature credential on GitHub GET")
		}
		return githubPRTestResponse(req, http.StatusOK, responses[req.URL.Path]), nil
	})}
	if _, err := readGitHubPR(context.Background(), "acme/engine", 42, pr.Head.SHA, "test-secret", client); err != nil {
		t.Fatal(err)
	}
	want := []string{"/user", "/repos/acme/engine", "/repos/acme/engine/pulls/42"}
	if strings.Join(requested, ",") != strings.Join(want, ",") {
		t.Fatalf("GET sequence = %v, want %v", requested, want)
	}
}

func TestReadGitHubPRDoesNotFetchPRWithoutPullPermission(t *testing.T) {
	responses := map[string]any{
		"/user": map[string]string{"login": "operator"},
		"/repos/acme/engine": map[string]any{
			"full_name": "acme/engine", "html_url": "https://github.com/acme/engine",
			"permissions": map[string]bool{"pull": false},
		},
	}
	var requested []string
	client := &http.Client{Transport: githubPRRoundTripper(func(req *http.Request) (*http.Response, error) {
		requested = append(requested, req.URL.Path)
		return githubPRTestResponse(req, http.StatusOK, responses[req.URL.Path]), nil
	})}
	_, err := readGitHubPR(context.Background(), "acme/engine", 42, strings.Repeat("a", 40), "test-secret", client)
	if err == nil || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("permission failure was missing or exposed credential: %v", err)
	}
	if strings.Join(requested, ",") != "/user,/repos/acme/engine" {
		t.Fatalf("PR was fetched without pull permission proof: %v", requested)
	}
}

func TestValidateGitHubPRRejectsMismatchedIdentityAndMergedProof(t *testing.T) {
	base := validGitHubPRFixture()
	tests := []struct {
		name string
		edit func(*githubPRResponse)
	}{
		{"wrong number", func(pr *githubPRResponse) { pr.Number++ }},
		{"wrong URL", func(pr *githubPRResponse) { pr.HTMLURL = "https://github.com/acme/other/pull/42" }},
		{"wrong base repository", func(pr *githubPRResponse) { pr.Base.Repo.FullName = "other/engine" }},
		{"head mismatch", func(pr *githubPRResponse) { pr.Head.SHA = strings.Repeat("d", 40) }},
		{"missing merged timestamp", func(pr *githubPRResponse) { pr.MergedAt = nil }},
		{"missing merge commit", func(pr *githubPRResponse) { pr.MergeCommitSHA = "" }},
		{"merged state not closed", func(pr *githubPRResponse) { pr.State = "open" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := base
			tt.edit(&pr)
			if _, err := validateGitHubPR("acme/engine", 42, base.Head.SHA, pr); err == nil {
				t.Fatal("invalid PR proof was accepted")
			}
		})
	}
}

func TestReadGitHubPRRejectsRedirectAndBoundsResponse(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		client := &http.Client{Transport: githubPRRoundTripper(func(req *http.Request) (*http.Response, error) {
			resp := githubPRTestResponse(req, http.StatusFound, nil)
			resp.Header.Set("Location", "https://attacker.example/collect")
			return resp, nil
		})}
		_, err := readGitHubPR(context.Background(), "acme/engine", 42, strings.Repeat("a", 40), "test-secret", client)
		if err == nil || strings.Contains(err.Error(), "test-secret") {
			t.Fatalf("redirect failure was missing or leaked credential: %v", err)
		}
	})
	t.Run("oversized response", func(t *testing.T) {
		client := &http.Client{Transport: githubPRRoundTripper(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(strings.Repeat("x", githubPRMaxResponse+1))), Request: req,
			}, nil
		})}
		_, err := readGitHubPR(context.Background(), "acme/engine", 42, strings.Repeat("a", 40), "", client)
		if err == nil {
			t.Fatal("oversized response was accepted")
		}
	})
}

func TestReadGitHubPRReturnsCancellationWithoutTransportCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: githubPRRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("transport called for canceled request")
		return nil, errors.New("unreachable")
	})}
	_, err := readGitHubPR(ctx, "acme/engine", 42, strings.Repeat("a", 40), "", client)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestReadGitHubPRRejectsInvalidCoordinates(t *testing.T) {
	client := &http.Client{Transport: githubPRRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("transport called for invalid coordinates")
		return nil, errors.New("unreachable")
	})}
	for _, tc := range []struct {
		repo string
		num  int
		sha  string
	}{
		{"acme/../engine", 42, strings.Repeat("a", 40)},
		{"acme/engine", 0, strings.Repeat("a", 40)},
		{"acme/engine", 42, "not-a-sha"},
	} {
		if _, err := readGitHubPR(context.Background(), tc.repo, tc.num, tc.sha, "", client); err == nil {
			t.Fatalf("invalid coordinates accepted: %+v", tc)
		}
	}
}
