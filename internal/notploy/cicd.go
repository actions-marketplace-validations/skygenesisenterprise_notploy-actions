package notploy

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// CI/CD API paths, as declared by the `cicd` tRPC router's OpenAPI metadata
// (`POST /api/cicd/deploy`, `GET /api/cicd/deployments/{deploymentId}`, …).
//
// This is the contract the action prefers: unlike `application.deploy`, it
// returns the id of the deployment it created, accepts the GitHub run metadata
// and is idempotent, so a retried request cannot deploy twice.
const (
	cicdDeployPath  = "cicd/deploy"
	cicdListPath    = "cicd/deployments"
	cicdLogsTail    = 5000
	legacyLogsTail  = 10000
	commitSHAminLen = 7
)

// SourceMetadata is the `source` object `cicd.deploy` requires: it is what
// makes a deployment traceable back to the exact workflow run that asked for
// it, and it is persisted in Notploy's `cicd_deployment` table.
type SourceMetadata struct {
	// Provider is the CI system. Notploy restricts the token's scope to it, so
	// it is always "github" for this action.
	Provider string `json:"provider"`
	// Repository is `owner/name`.
	Repository string `json:"repository"`
	// CommitSHA is the commit being deployed, 7 to 64 hex characters.
	CommitSHA string `json:"commitSha"`
	// Ref is the branch or tag the run was triggered for.
	Ref string `json:"ref,omitempty"`
	// Workflow, RunID, RunURL, RunNumber and Actor identify the run itself.
	Workflow  string `json:"workflow,omitempty"`
	RunID     string `json:"runId,omitempty"`
	RunURL    string `json:"runUrl,omitempty"`
	RunNumber int    `json:"runNumber,omitempty"`
	Actor     string `json:"actor,omitempty"`
}

// CicdDeployRequest is the body of `POST /api/cicd/deploy`.
type CicdDeployRequest struct {
	// Project, Environment and Application accept ids or dashboard names.
	// Application is mandatory; the two others default to the token's scope.
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	Application string `json:"application"`

	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`

	Source SourceMetadata `json:"source"`

	// IdempotencyKey scopes "already recorded" to one workflow run (and attempt).
	// When it is set, a retried request returns the original deployment instead
	// of creating a second one, which is what makes retrying the trigger safe.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

// Validate checks the parts of the request Notploy rejects with a 400, so the
// action can report them before spending a round trip.
//
// Field names match the action inputs, not the JSON keys, because that is what
// a workflow author can act on.
func (r CicdDeployRequest) Validate() error {
	if strings.TrimSpace(r.Application) == "" {
		return fmt.Errorf("application is required")
	}
	if strings.TrimSpace(r.Source.Repository) == "" || len(r.Source.Repository) < 3 {
		return fmt.Errorf("the CI context is incomplete: GITHUB_REPOSITORY is missing (required by the CI/CD API)")
	}
	if !validCommitSHA(r.Source.CommitSHA) {
		return fmt.Errorf("the CI context is incomplete: GITHUB_SHA %q is not a valid commit sha (required by the CI/CD API)", r.Source.CommitSHA)
	}
	if len(r.Source.Ref) > 300 || len(r.Source.Workflow) > 300 || len(r.Source.Actor) > 300 || len(r.Source.RunID) > 100 {
		return fmt.Errorf("the GitHub run context exceeds the field lengths the CI/CD API accepts")
	}
	if r.Source.RunURL != "" {
		parsed, err := url.Parse(r.Source.RunURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || len(r.Source.RunURL) > 500 {
			return fmt.Errorf("the GitHub run URL is not a valid http(s) URL under 500 characters")
		}
	}
	if len(r.Title) > 200 {
		return fmt.Errorf("input `title` must be at most 200 characters, got %d", len(r.Title))
	}
	if len(r.Description) > 2000 {
		return fmt.Errorf("input `description` must be at most 2000 characters, got %d", len(r.Description))
	}
	if len(r.IdempotencyKey) > 300 {
		return fmt.Errorf("input `idempotency-key` must be at most 300 characters, got %d", len(r.IdempotencyKey))
	}
	return nil
}

// validCommitSHA mirrors Notploy's `cicdCommitShaRegex`.
func validCommitSHA(sha string) bool {
	if len(sha) < commitSHAminLen || len(sha) > 64 {
		return false
	}
	for _, r := range sha {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// DeploymentView is the payload returned by `cicd.deploy`, `cicd.status` and
// `cicd.list`.
//
// Notploy's OpenAPI document declares these response bodies as empty objects,
// so the fields are modelled from the server implementation and parsed
// defensively: an unknown or missing field simply keeps its zero value.
type DeploymentView struct {
	DeploymentID string `json:"deploymentId"`
	// Status is one of running, done, error or cancelled.
	Status string `json:"status"`
	// GitHubStatus is the state Notploy maps this deployment to for GitHub
	// Deployments (queued, in_progress, success, failure, error, inactive).
	GitHubStatus string `json:"githubStatus"`
	// Queued is true while the row exists but no worker picked it up yet.
	Queued bool `json:"queued"`

	Title        string `json:"title"`
	Description  string `json:"description"`
	ErrorMessage string `json:"errorMessage"`

	CreatedAt  string `json:"createdAt"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`

	LogsAvailable bool `json:"logsAvailable"`

	ApplicationID   string `json:"applicationId"`
	ProjectID       string `json:"projectId"`
	EnvironmentID   string `json:"environmentId"`
	ProjectName     string `json:"projectName"`
	EnvironmentName string `json:"environmentName"`
	ApplicationName string `json:"applicationName"`

	// DeploymentURL links to the application page of the Notploy dashboard,
	// which is where its deployment logs are shown. Notploy has no
	// per-deployment page yet.
	DeploymentURL string `json:"deploymentUrl"`
	// EnvironmentURL is the public URL of the application, when it has a domain.
	EnvironmentURL string `json:"environmentUrl"`

	Cicd *CicdDeploymentMetadata `json:"cicd"`

	// Idempotent and Message are only present on the deploy response.
	Idempotent bool   `json:"idempotent"`
	Message    string `json:"message"`
}

// CicdDeploymentMetadata is the correlation record Notploy stores next to the
// deployment. It is what lets an operator find the GitHub run from Notploy.
type CicdDeploymentMetadata struct {
	Provider   string `json:"provider"`
	Repository string `json:"repository"`
	CommitSHA  string `json:"commitSha"`
	Ref        string `json:"ref"`
	Workflow   string `json:"workflow"`
	RunID      string `json:"workflowRunId"`
	RunURL     string `json:"workflowRunUrl"`
	RunNumber  int    `json:"runNumber"`
	Actor      string `json:"actor"`
	ExternalID string `json:"externalId"`
}

// IsTerminal reports whether the deployment reached a final state.
func (v DeploymentView) IsTerminal() bool {
	switch v.EffectiveStatus() {
	case StatusDone, StatusError, StatusCancelled:
		return true
	default:
		return false
	}
}

// Failed reports whether the deployment ended unsuccessfully.
func (v DeploymentView) Failed() bool {
	return v.Status == StatusError
}

// EffectiveStatus folds the synthetic `queued` state into the status vocabulary
// so callers only ever deal with one string.
func (v DeploymentView) EffectiveStatus() string {
	if v.Queued && v.Status == StatusRunning {
		return StatusQueued
	}
	if v.Status == "" {
		return StatusRunning
	}
	return v.Status
}

// CicdLogsResult is the payload of `GET /api/cicd/deployments/{id}/logs`.
type CicdLogsResult struct {
	DeploymentID string `json:"deploymentId"`
	Status       string `json:"status"`
	Logs         string `json:"logs"`
	LineCount    int    `json:"lineCount"`
	Tail         int    `json:"tail"`
	Truncated    bool   `json:"truncated"`
}

// CicdDeploy queues a deployment and returns the id of the deployment that was
// created (or of the one already recorded for this idempotency key).
//
// It is retried only when an idempotency key was sent: without one, a request
// whose response was lost would create a second deployment on the next try.
func (c *Client) CicdDeploy(ctx context.Context, request CicdDeployRequest) (*DeploymentView, error) {
	var view DeploymentView
	err := c.do(ctx, request.IdempotencyKey != "", http.MethodPost, cicdDeployPath, nil, request, &view)
	if err != nil {
		return nil, err
	}
	if view.DeploymentID == "" {
		return nil, &APIError{Operation: "cicd.deploy", StatusCode: http.StatusOK, Message: "Notploy did not return a deployment id"}
	}
	return &view, nil
}

// CicdStatus returns the current state of one deployment.
func (c *Client) CicdStatus(ctx context.Context, deploymentID string) (*DeploymentView, error) {
	var view DeploymentView
	path := cicdListPath + "/" + url.PathEscape(deploymentID)
	if err := c.do(ctx, true, http.MethodGet, path, nil, nil, &view); err != nil {
		return nil, err
	}
	return &view, nil
}

// CicdLogs returns the last lines of a deployment's log. The tail is clamped to
// what the API accepts rather than failing the request.
func (c *Client) CicdLogs(ctx context.Context, deploymentID string, tail int) (*CicdLogsResult, error) {
	if tail <= 0 {
		tail = 100
	}
	if tail > cicdLogsTail {
		tail = cicdLogsTail
	}
	query := url.Values{"tail": {strconv.Itoa(tail)}}
	path := cicdListPath + "/" + url.PathEscape(deploymentID) + "/logs"

	var result CicdLogsResult
	if err := c.do(ctx, true, http.MethodGet, path, query, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CicdCancel stops a queued or running deployment. It is never retried: a
// deployment that was already cancelled is reported as a 400, not as success.
func (c *Client) CicdCancel(ctx context.Context, deploymentID string) error {
	path := cicdListPath + "/" + url.PathEscape(deploymentID) + "/cancel"
	return c.do(ctx, false, http.MethodPost, path, nil, nil, nil)
}

// LogsURL returns the absolute URL of the API endpoint that returns a
// deployment's log for the API surface the action ran on (`cicd` selects the
// CI/CD endpoint, anything else the legacy `deployment.readLogs` one).
//
// It is an API endpoint, not a page: opening it in a browser requires
// authenticating with an API key first. The job summary is what a human reads;
// this URL is what a script uses.
func (c *Client) LogsURL(cicd bool, deploymentID string) string {
	if deploymentID == "" {
		return ""
	}
	if cicd {
		return c.operationURL(cicdListPath + "/" + url.PathEscape(deploymentID) + "/logs")
	}
	return c.operationURL("deployment.readLogs") + "?deploymentId=" + url.QueryEscape(deploymentID)
}

// operationURL renders an API operation as an absolute URL.
func (c *Client) operationURL(operation string) string {
	absolute := *c.baseURL
	absolute.Path = strings.TrimRight(absolute.Path, "/") + "/" + operation
	absolute.RawPath = ""
	absolute.RawQuery = ""
	return absolute.String()
}
