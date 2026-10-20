// Command notploy-action is the entry point of the Notploy GitHub Action.
//
// It only wires packages together: read the action inputs, build a Notploy
// client, run the deployment lifecycle and publish the results back to the
// runner. All behaviour lives in internal/.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/skygenesisenterprise/notploy-actions/internal/config"
	"github.com/skygenesisenterprise/notploy-actions/internal/deployment"
	"github.com/skygenesisenterprise/notploy-actions/internal/github"
	"github.com/skygenesisenterprise/notploy-actions/internal/notploy"
	"github.com/skygenesisenterprise/notploy-actions/internal/version"
)

func main() {
	os.Exit(run())
}

func run() int {
	getenv := os.Getenv
	log := github.NewLogger(os.Stdout)

	cfg, err := config.Load(getenv)
	if err != nil {
		log.Errorf("Invalid configuration: %v", err)
		return 1
	}
	// Register the secrets with the runner before anything else can print them.
	log.Mask(cfg.APIKey)
	log.Mask(cfg.GitHubToken)

	gh := github.LoadContext(getenv)
	if gh.Repository == "" {
		log.Warningf("GITHUB_REPOSITORY is not set; is this running inside GitHub Actions?")
	}

	client, err := notploy.NewClient(notploy.Options{
		Endpoint:  cfg.Endpoint,
		APIKey:    cfg.APIKey,
		UserAgent: "notploy-action/" + version.Version,
	})
	if err != nil {
		log.Errorf("Invalid Notploy endpoint: %v", err)
		return 1
	}

	// Cancel the wait cleanly when the job is cancelled.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mode := string(cfg.APIMode)
	if !cfg.Metadata && mode != string(config.ModeLegacy) {
		// The CI/CD API requires the run metadata; without it there is nothing
		// to send and nothing to correlate on.
		mode = string(config.ModeLegacy)
		log.Infof("GitHub run metadata is disabled; using the classic API")
	}

	reportHeader(log, gh, cfg, mode)

	statusPublisher := github.NewStatusPublisher(getenv, cfg.GitHubToken)
	// The status context is resolved once — from the first event that carries
	// the application's name as Notploy knows it — and then reused, so the
	// commit never shows two contexts for one deployment.
	var (
		statusContext  string
		lastPublishKey string
	)
	publish := func(cbCtx context.Context, state, description, targetURL string) {
		if statusPublisher == nil || statusContext == "" {
			return
		}
		// The last event and the final report often describe the same state:
		// publish it once.
		key := state + "\x00" + description + "\x00" + targetURL
		if key == lastPublishKey {
			return
		}
		lastPublishKey = key
		err := statusPublisher.Publish(cbCtx, gh.Repository, gh.Sha, github.CommitStatus{
			State:       state,
			Description: description,
			TargetURL:   targetURL,
			Context:     statusContext,
		})
		if err != nil {
			log.Warningf("Could not publish the commit status: %v", err)
		}
	}

	params := deployment.Params{
		Mode:            mode,
		ApplicationID:   cfg.ApplicationID,
		Project:         cfg.Project,
		Environment:     cfg.Environment,
		Title:           firstNonEmpty(cfg.Title, gh.DeploymentTitle()),
		Description:     firstNonEmpty(cfg.Description, gh.DeploymentDescription()),
		IdempotencyKey:  firstNonEmpty(cfg.IdempotencyKey, gh.DefaultIdempotencyKey()),
		Wait:            cfg.Wait,
		Timeout:         cfg.Timeout,
		PollInterval:    cfg.PollInterval,
		CancelOnTimeout: cfg.CancelOnTimeout,
		LogTail:         cfg.LogTail,
	}
	if cfg.Metadata {
		params.Source = sourceFrom(gh)
	}
	params.OnStatus = func(cbCtx context.Context, event deployment.StatusEvent) {
		if statusContext == "" {
			statusContext = firstNonEmpty(cfg.GitHubStatusContext, "notploy/"+firstNonEmpty(event.ApplicationName, cfg.ApplicationID))
		}
		publish(
			cbCtx,
			statusState(event.Status),
			"Notploy: "+statusDescription(event.Status),
			firstNonEmpty(event.URL, gh.RunURL()),
		)
	}

	service := deployment.New(client, log)
	result, runErr := service.Run(ctx, params)

	if result != nil && result.Status != "" {
		// The final state is published with the same context as the events, so
		// one deployment always maps to exactly one commit status.
		publish(
			context.Background(),
			statusState(result.Status),
			"Notploy: "+statusDescription(result.Status),
			firstNonEmpty(result.DetailsURL, result.URL, gh.RunURL()),
		)
	}

	if result != nil {
		if err := github.WriteOutputs(getenv, outputs(result, cfg)); err != nil {
			log.Warningf("Could not write the step outputs: %v", err)
		}
		if err := github.WriteSummary(getenv, summary(gh, cfg, result)); err != nil {
			log.Warningf("Could not write the job summary: %v", err)
		}
	}

	if runErr != nil {
		log.Errorf("Notploy deployment failed: %v", runErr)
		reportHint(log, runErr)
		return 1
	}

	reportSuccess(log, result)
	return 0
}

// sourceFrom renders the GitHub run context as the `source` object the CI/CD
// API persists next to the deployment.
func sourceFrom(gh github.Context) notploy.SourceMetadata {
	return notploy.SourceMetadata{
		Provider:   "github",
		Repository: gh.Repository,
		CommitSHA:  gh.Sha,
		Ref:        gh.Ref,
		Workflow:   gh.Workflow,
		RunID:      gh.RunID,
		RunURL:     gh.RunURL(),
		RunNumber:  gh.RunNumberValue(),
		Actor:      gh.Actor,
	}
}

// statusState maps a Notploy status to the four states a commit status accepts.
// Anything unknown stays pending rather than claiming success or failure.
func statusState(status string) string {
	switch status {
	case notploy.StatusDone:
		return github.StatusSuccess
	case notploy.StatusError:
		return github.StatusFailure
	case notploy.StatusCancelled:
		return github.StatusError
	default:
		return github.StatusPending
	}
}

func statusDescription(status string) string {
	switch status {
	case deployment.StatusTriggered:
		return "deployment triggered"
	case notploy.StatusQueued:
		return "deployment queued"
	case notploy.StatusRunning:
		return "deployment in progress"
	case notploy.StatusDone:
		return "deployment succeeded"
	case notploy.StatusError:
		return "deployment failed"
	case notploy.StatusCancelled:
		return "deployment cancelled"
	default:
		return status
	}
}

func reportHeader(log *github.Logger, gh github.Context, cfg *config.Config, mode string) {
	log.Infof("Notploy")
	log.Infof("%s", strings.Repeat("-", 28))
	log.Infof("Endpoint:    %s", cfg.Endpoint)
	log.Infof("Application: %s", cfg.ApplicationID)
	if cfg.Project != "" {
		log.Infof("Project:     %s", cfg.Project)
	}
	if cfg.Environment != "" {
		log.Infof("Environment: %s", cfg.Environment)
	}
	log.Infof("API mode:    %s", mode)
	if gh.Repository != "" {
		log.Infof("Repository:  %s", gh.Repository)
	}
	if gh.ShortSha() != "" {
		log.Infof("Commit:      %s", gh.ShortSha())
	}
	if gh.Ref != "" {
		log.Infof("Ref:         %s", gh.Ref)
	}
	if gh.Actor != "" {
		log.Infof("Actor:       %s", gh.Actor)
	}
	if gh.EventName != "" {
		log.Infof("Event:       %s", gh.EventName)
	}
	if cfg.Wait {
		log.Infof("Timeout:     %s", cfg.Timeout)
		log.Infof("Poll:        %s", cfg.PollInterval)
	} else {
		log.Infof("Wait:        disabled")
	}
	log.Infof("")
}

func outputs(result *deployment.Result, cfg *config.Config) map[string]string {
	values := map[string]string{
		"deployment-id":          result.DeploymentID,
		"deployment-status":      result.Status,
		"deployment-url":         result.URL,
		"deployment-details-url": result.DetailsURL,
		"deployment-logs-url":    result.LogsURL,
		"deployment-environment": firstNonEmpty(result.EnvironmentName, cfg.Environment),
	}
	if result.Mode == deployment.ModeCicd {
		// Only the CI/CD API can replay a deployment: an empty value on the
		// classic endpoints says "not applicable", not "false".
		values["deployment-idempotent"] = boolValue(result.Idempotent)
	} else {
		values["deployment-idempotent"] = ""
	}
	return values
}

func summary(gh github.Context, cfg *config.Config, result *deployment.Result) string {
	return github.Summary{
		Application:   firstNonEmpty(result.ApplicationName, cfg.ApplicationID),
		Project:       firstNonEmpty(result.ProjectName, cfg.Project),
		Environment:   firstNonEmpty(result.EnvironmentName, cfg.Environment),
		Repository:    gh.Repository,
		Commit:        gh.Sha,
		CommitURL:     gh.CommitURL(),
		Ref:           gh.Ref,
		Actor:         gh.Actor,
		Workflow:      gh.Workflow,
		RunNumber:     gh.RunNumber,
		RunURL:        gh.RunURL(),
		Mode:          result.Mode,
		DeploymentID:  result.DeploymentID,
		DeploymentURL: result.DetailsURL,
		LogsURL:       result.LogsURL,
		Status:        result.Status,
		URL:           result.URL,
		Reused:        result.Idempotent,
		Duration:      result.Duration,
		Error:         result.Error,
		LogExcerpt:    result.LogExcerpt,
	}.Markdown()
}

func reportSuccess(log *github.Logger, result *deployment.Result) {
	if result == nil {
		return
	}

	if result.DeploymentID != "" {
		log.Infof("Deployment ID: %s", result.DeploymentID)
	}
	log.Infof("Status: %s", result.Status)
	if result.URL != "" {
		log.Infof("URL: %s", result.URL)
	}
	if result.DetailsURL != "" {
		log.Infof("Details: %s", result.DetailsURL)
	}
	if result.Duration > 0 {
		log.Infof("Duration: %s", result.Duration.Round(time.Second))
	}

	switch {
	case result.Status == notploy.StatusCancelled:
		log.Warningf("Deployment was cancelled in Notploy")
	case result.Status == deployment.StatusTriggered:
		log.Infof("🚀 Deployment triggered (not waiting for it to finish)")
	case result.Status == notploy.StatusDone:
		log.Infof("✓ Deployment successful")
	default:
		log.Infof("Deployment finished with status %s", result.Status)
	}
}

func reportHint(log *github.Logger, err error) {
	var apiErr *notploy.APIError
	if !errors.As(err, &apiErr) {
		return
	}
	if hint := apiErr.Hint(); hint != "" {
		log.Infof("Hint: %s", hint)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func boolValue(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
