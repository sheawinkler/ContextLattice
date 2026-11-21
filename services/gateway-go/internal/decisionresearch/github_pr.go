package decisionresearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	githubPRAPIHost      = "api.github.com"
	githubPRTokenEnv     = "CONTEXTLATTICE_GITHUB_TOKEN"
	githubPRMaxResponse  = 1 << 20
	githubPRRequestLimit = 10 * time.Second
	githubPRAPIVersion   = "2022-11-28"
)

var (
	githubPRRepoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	githubPRSHA      = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
)

// PRObservation is bounded metadata proving the configured pull request state
// at its observed revision. It contains no credentials or source excerpts.
type PRObservation struct {
	State       string
	Revision    string
	ContentHash string
	EffectiveAt time.Time
}

// ReadGitHubPR reads one explicitly named GitHub pull request. Private reads
// require CONTEXTLATTICE_GITHUB_TOKEN; the token is used only for fixed-host
// GitHub API GETs and is never included in returned errors or observations.
func ReadGitHubPR(ctx context.Context, repo string, number int, headSHA string) (PRObservation, error) {
	if ctx == nil {
		return PRObservation{}, errors.New("GitHub pull request context is required")
	}
	ctx, cancel := context.WithTimeout(ctx, githubPRRequestLimit)
	defer cancel()
	client := &http.Client{
		Timeout: githubPRRequestLimit,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return readGitHubPR(ctx, repo, number, headSHA, os.Getenv(githubPRTokenEnv), client)
}

func readGitHubPR(ctx context.Context, repo string, number int, headSHA, token string, client *http.Client) (PRObservation, error) {
	if ctx == nil || ctx.Err() != nil {
		if ctx == nil {
			return PRObservation{}, errors.New("GitHub pull request context is required")
		}
		return PRObservation{}, ctx.Err()
	}
	if !validGitHubPRRepo(repo) || number < 1 || !githubPRSHA.MatchString(headSHA) || client == nil {
		return PRObservation{}, errors.New("invalid GitHub pull request coordinates")
	}
	if strings.ContainsAny(token, "\r\n") || len(token) > 4096 {
		return PRObservation{}, errors.New("invalid GitHub credential configuration")
	}
	token = strings.TrimSpace(token)
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	if token != "" {
		var account struct {
			Login string `json:"login"`
		}
		if err := githubPRGetJSON(ctx, &safeClient, token, "https://api.github.com/user", &account); err != nil {
			if ctx.Err() != nil {
				return PRObservation{}, ctx.Err()
			}
			return PRObservation{}, errors.New("GitHub account could not be verified")
		}
		if account.Login == "" {
			return PRObservation{}, errors.New("GitHub account could not be verified")
		}
		var repository struct {
			FullName    string `json:"full_name"`
			HTMLURL     string `json:"html_url"`
			Permissions struct {
				Pull *bool `json:"pull"`
			} `json:"permissions"`
		}
		if err := githubPRGetJSON(ctx, &safeClient, token, "https://api.github.com/repos/"+repo, &repository); err != nil {
			if ctx.Err() != nil {
				return PRObservation{}, ctx.Err()
			}
			return PRObservation{}, errors.New("GitHub pull permission for the configured repository could not be verified")
		}
		if !strings.EqualFold(repository.FullName, repo) || !validCanonicalGitHubRepoURL(repository.HTMLURL, repo) ||
			repository.Permissions.Pull == nil || !*repository.Permissions.Pull {
			return PRObservation{}, errors.New("GitHub pull permission for the configured repository could not be verified")
		}
	}

	var pr githubPRResponse
	endpoint := "https://api.github.com/repos/" + repo + "/pulls/" + strconv.Itoa(number)
	if err := githubPRGetJSON(ctx, &safeClient, token, endpoint, &pr); err != nil {
		return PRObservation{}, err
	}
	return validateGitHubPR(repo, number, headSHA, pr)
}

type githubPRResponse struct {
	Number         int        `json:"number"`
	HTMLURL        string     `json:"html_url"`
	State          string     `json:"state"`
	Draft          bool       `json:"draft"`
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"merged_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	Head           struct {
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

func validateGitHubPR(repo string, number int, expectedHead string, pr githubPRResponse) (PRObservation, error) {
	if pr.Number != number || !validCanonicalGitHubPRURL(pr.HTMLURL, repo, number) ||
		!strings.EqualFold(pr.Base.Repo.FullName, repo) || !validGitHubPRRepo(pr.Head.Repo.FullName) ||
		!githubPRSHA.MatchString(pr.Head.SHA) || !githubPRSHA.MatchString(pr.Base.SHA) ||
		!strings.EqualFold(pr.Head.SHA, expectedHead) || pr.UpdatedAt.IsZero() {
		return PRObservation{}, errors.New("GitHub pull request identity or revision could not be verified")
	}

	state := ""
	effectiveAt := pr.UpdatedAt
	switch {
	case pr.Merged:
		if pr.State != "closed" || pr.Draft || pr.MergedAt == nil || pr.MergedAt.IsZero() || !githubPRSHA.MatchString(pr.MergeCommitSHA) {
			return PRObservation{}, errors.New("GitHub merged pull request proof is incomplete")
		}
		state = "merged"
		effectiveAt = *pr.MergedAt
	case pr.State == "open":
		if pr.MergedAt != nil {
			return PRObservation{}, errors.New("GitHub pull request state is inconsistent")
		}
		if pr.Draft {
			state = "draft"
		} else {
			state = "open"
		}
	case pr.State == "closed":
		if pr.Draft || pr.MergedAt != nil {
			return PRObservation{}, errors.New("GitHub pull request state is inconsistent")
		}
		state = "closed"
	default:
		return PRObservation{}, errors.New("GitHub pull request state is unsupported")
	}

	proof := struct {
		Number         int        `json:"number"`
		URL            string     `json:"url"`
		State          string     `json:"state"`
		Draft          bool       `json:"draft"`
		Merged         bool       `json:"merged"`
		MergedAt       *time.Time `json:"merged_at"`
		UpdatedAt      time.Time  `json:"updated_at"`
		MergeCommitSHA string     `json:"merge_commit_sha"`
		HeadRepo       string     `json:"head_repo"`
		HeadSHA        string     `json:"head_sha"`
		BaseRepo       string     `json:"base_repo"`
		BaseSHA        string     `json:"base_sha"`
	}{pr.Number, pr.HTMLURL, state, pr.Draft, pr.Merged, pr.MergedAt, pr.UpdatedAt,
		strings.ToLower(pr.MergeCommitSHA), strings.ToLower(pr.Head.Repo.FullName), strings.ToLower(pr.Head.SHA),
		strings.ToLower(pr.Base.Repo.FullName), strings.ToLower(pr.Base.SHA)}
	body, err := json.Marshal(proof)
	if err != nil {
		return PRObservation{}, errors.New("GitHub pull request proof could not be encoded")
	}
	hash := sha256.Sum256(body)
	return PRObservation{
		State: state, Revision: strings.ToLower(pr.Head.SHA), ContentHash: hex.EncodeToString(hash[:]), EffectiveAt: effectiveAt,
	}, nil
}

func githubPRGetJSON(ctx context.Context, client *http.Client, token, endpoint string, target any) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host != githubPRAPIHost || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("GitHub request target is not allowed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("GitHub request could not be created")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubPRAPIVersion)
	req.Header.Set("User-Agent", "ContextLattice-decisionresearch")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("GitHub request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub request was denied or unavailable (HTTP %d)", resp.StatusCode)
	}
	if resp.Request != nil && resp.Request.URL != nil && (resp.Request.URL.Scheme != "https" || resp.Request.URL.Host != githubPRAPIHost) {
		return errors.New("GitHub response origin could not be verified")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, githubPRMaxResponse+1))
	if err != nil || len(body) > githubPRMaxResponse {
		return errors.New("GitHub response exceeded the bounded JSON limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(target); err != nil {
		return errors.New("GitHub response JSON is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("GitHub response contains trailing JSON")
	}
	return nil
}

func validGitHubPRRepo(repo string) bool {
	parts := strings.Split(repo, "/")
	return len(parts) == 2 && githubPRRepoPart.MatchString(parts[0]) && githubPRRepoPart.MatchString(parts[1]) &&
		parts[0] != "." && parts[0] != ".." && parts[1] != "." && parts[1] != ".."
}

func validCanonicalGitHubRepoURL(raw, repo string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Host, "github.com") && u.User == nil &&
		u.RawQuery == "" && u.Fragment == "" && strings.EqualFold(strings.TrimSuffix(u.Path, "/"), "/"+repo)
}

func validCanonicalGitHubPRURL(raw, repo string, number int) bool {
	u, err := url.Parse(raw)
	want := "/" + repo + "/pull/" + strconv.Itoa(number)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Host, "github.com") && u.User == nil &&
		u.RawQuery == "" && u.Fragment == "" && strings.EqualFold(u.Path, want)
}
