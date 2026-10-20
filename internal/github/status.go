package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Commit status states accepted by the GitHub commit status API. GitHub's
// Deployments API knows more states (notably `inactive`), but a commit status
// only has these four, which is what this action publishes.
const (
	StatusPending = "pending"
	StatusSuccess = "success"
	StatusFailure = "failure"
	StatusError   = "error"
)

const (
	defaultAPIURL        = "https://api.github.com"
	statusTimeout        = 30 * time.Second
	statusBodyLimit      = 8 << 10
	maxStatusDescription = 140
	gitHubAPIVersion     = "2022-11-28"
)

// CommitStatus is one state published against a commit.
//
// Description and TargetURL are the only free-form fields; both are truncated
// to what GitHub accepts rather than rejected by it.
type CommitStatus struct {
	State       string
	Description string
	TargetURL   string
	Context     string
}

// StatusPublisher publishes commit statuses. A nil publisher (no token
// configured) publishes nothing, so callers never have to branch on the input.
type StatusPublisher struct {
	token  string
	apiURL string
	client *http.Client
}

// NewStatusPublisher returns a publisher for the GitHub instance of the
// workflow (`GITHUB_API_URL`, so GitHub Enterprise works too), or nil when no
// token was provided.
func NewStatusPublisher(getenv Getenv, token string) *StatusPublisher {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	apiURL := strings.TrimRight(strings.TrimSpace(getenv("GITHUB_API_URL")), "/")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	return &StatusPublisher{
		token:  token,
		apiURL: apiURL,
		client: &http.Client{Timeout: statusTimeout},
	}
}

// Publish creates a commit status. It never returns an error that could leak
// the token, and it is a no-op on a nil publisher.
func (p *StatusPublisher) Publish(ctx context.Context, repository, sha string, status CommitStatus) error {
	if p == nil || repository == "" || sha == "" || status.Context == "" {
		return nil
	}

	body, err := json.Marshal(map[string]string{
		"state":       status.State,
		"description": truncate(status.Description, maxStatusDescription),
		"target_url":  truncate(status.TargetURL, 2048),
		"context":     status.Context,
	})
	if err != nil {
		return fmt.Errorf("encode commit status: %w", err)
	}

	endpoint := fmt.Sprintf("%s/repos/%s/statuses/%s", p.apiURL, escapeRepository(repository), url.PathEscape(sha))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build commit status request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Api-Version", gitHubAPIVersion)
	request.Header.Set("User-Agent", "notploy-action")

	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("publish commit status: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, statusBodyLimit))
		return fmt.Errorf("publish commit status failed with HTTP %d: %s",
			response.StatusCode, p.redact(strings.Join(strings.Fields(string(snippet)), " ")))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, statusBodyLimit))
	return nil
}

// redact makes sure the token never reaches a log line, whatever GitHub echoed
// back.
func (p *StatusPublisher) redact(text string) string {
	if p.token == "" {
		return text
	}
	return strings.ReplaceAll(text, p.token, "[redacted]")
}

// escapeRepository escapes both halves of `owner/name` for a URL path.
func escapeRepository(repository string) string {
	parts := strings.Split(repository, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// truncate shortens a value to the given limit, cutting at a rune boundary.
func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}
