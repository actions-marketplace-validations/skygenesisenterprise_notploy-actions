package deployment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/skygenesisenterprise/notploy-actions/internal/notploy"
)

// runCicd triggers a deployment through the CI/CD API and follows it.
//
// A non-nil Result with a non-nil error means the deployment was created but
// could not be followed to completion; a nil Result means nothing was
// deployed and the caller may consider the mode unavailable.
func (s *Service) runCicd(ctx context.Context, params Params, started time.Time) (*Result, error) {
	request := notploy.CicdDeployRequest{
		Project:        params.Project,
		Environment:    params.Environment,
		Application:    params.ApplicationID,
		Title:          params.Title,
		Description:    params.Description,
		Source:         params.Source,
		IdempotencyKey: params.IdempotencyKey,
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}

	s.log.Infof("Requesting a deployment of application %s through the Notploy CI/CD API", params.ApplicationID)
	view, err := s.client.CicdDeploy(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("trigger deployment: %w", err)
	}

	result := &Result{
		Mode:         ModeCicd,
		Idempotent:   view.Idempotent,
		DeploymentID: view.DeploymentID,
	}
	s.applyCicdView(result, view)
	result.LogsURL = s.client.LogsURL(true, view.DeploymentID)

	switch {
	case view.Idempotent:
		s.log.Infof("Notploy already recorded deployment %s for this run; not queueing another one", view.DeploymentID)
	case view.Message != "":
		s.log.Infof("Deployment %s queued: %s", view.DeploymentID, strings.TrimSpace(view.Message))
	default:
		s.log.Infof("Deployment %s queued", view.DeploymentID)
	}
	s.emit(ctx, params, StatusEvent{
		DeploymentID:    view.DeploymentID,
		Status:          view.EffectiveStatus(),
		ApplicationName: view.ApplicationName,
		URL:             view.DeploymentURL,
	})

	if !params.Wait {
		result.Duration = time.Since(started)
		result.URL = firstNonEmpty(result.URL, s.resolvePublicURL(ctx, view))
		return result, nil
	}

	runCtx, cancel := resolveTimeout(ctx, params)
	latest, waitErr := s.waitCicd(runCtx, params, result)
	cancel()

	if latest != nil {
		s.applyCicdView(result, latest)
	}
	result.Duration = time.Since(started)
	// Resolve the URL with the caller's context: runCtx may already be expired.
	result.URL = firstNonEmpty(result.URL, s.resolvePublicURL(ctx, latest))

	switch {
	case waitErr == nil:
		return s.settleCicd(ctx, params, result)
	case errors.Is(waitErr, context.DeadlineExceeded):
		if params.CancelOnTimeout {
			s.bestEffortCancel(result.DeploymentID)
		} else {
			s.log.Warningf("Deployment %s keeps running in Notploy; set `cancel-on-timeout: true` to stop it when the wait gives up", result.DeploymentID)
		}
		return result, fmt.Errorf("deployment %s did not reach a final state within %s", result.DeploymentID, params.Timeout)
	case errors.Is(waitErr, context.Canceled):
		// The job itself was cancelled. The server-side deployment only stops
		// when the user asked for that behaviour explicitly.
		if params.CancelOnTimeout {
			s.bestEffortCancel(result.DeploymentID)
		}
		return result, fmt.Errorf("deployment wait was cancelled: %w", waitErr)
	default:
		return result, waitErr
	}
}

// waitCicd polls the deployment until it is terminal, the context is done or
// the instance stops answering. The result is updated on every observation, so
// the caller keeps the last known state even when the wait is abandoned.
func (s *Service) waitCicd(ctx context.Context, params Params, result *Result) (*notploy.DeploymentView, error) {
	ticker := time.NewTicker(params.PollInterval)
	defer ticker.Stop()

	var (
		latest         *notploy.DeploymentView
		consecutiveErr int
		// The state reported right after the trigger is not reported again.
		lastReported string
	)
	if result != nil {
		lastReported = result.Status
	}

	for {
		view, err := s.client.CicdStatus(ctx, result.DeploymentID)
		switch {
		case err != nil:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return latest, ctxErr
			}
			consecutiveErr++
			s.log.Warningf("Could not read the deployment status (attempt %d/%d): %v", consecutiveErr, maxConsecutiveErrors, err)
			if consecutiveErr >= maxConsecutiveErrors {
				return latest, fmt.Errorf("gave up after %d failed status lookups: %w", consecutiveErr, err)
			}
		default:
			consecutiveErr = 0
			s.applyCicdView(result, view)
			if status := view.EffectiveStatus(); status != lastReported {
				if lastReported != "" {
					s.log.Infof("Deployment %s is now %s", view.DeploymentID, status)
				} else {
					s.log.Infof("Deployment %s is %s", view.DeploymentID, status)
				}
				lastReported = status
				s.emit(ctx, params, StatusEvent{
					DeploymentID:    view.DeploymentID,
					Status:          status,
					ApplicationName: view.ApplicationName,
					URL:             view.DeploymentURL,
				})
			}
			latest = view
			if view.IsTerminal() {
				return view, nil
			}
		}

		select {
		case <-ctx.Done():
			return latest, ctx.Err()
		case <-ticker.C:
		}
	}
}

// settleCicd translates a terminal state into the action's outcome: success,
// documented warning (cancelled) or failure with the deployment's own message
// and the tail of its log.
func (s *Service) settleCicd(ctx context.Context, params Params, result *Result) (*Result, error) {
	switch result.Status {
	case notploy.StatusDone, "":
		return result, nil
	case notploy.StatusCancelled:
		s.log.Warningf("Deployment %s was cancelled in Notploy", result.DeploymentID)
		s.collectLogs(ctx, params, result, true)
		return result, nil
	default:
		s.collectLogs(ctx, params, result, true)
		message := strings.TrimSpace(result.Error)
		if message == "" {
			message = "the deployment failed without an error message"
		} else {
			result.Error = message
		}
		return result, fmt.Errorf("deployment %s failed: %s", result.DeploymentID, message)
	}
}

// applyCicdView copies the fields a view contributes into the result, keeping
// what it does not carry (the resolved public URL, the log excerpt) intact.
func (s *Service) applyCicdView(result *Result, view *notploy.DeploymentView) {
	if view == nil {
		return
	}
	if view.DeploymentID != "" {
		result.DeploymentID = view.DeploymentID
	}
	if status := view.EffectiveStatus(); status != "" {
		result.Status = status
	}
	result.ApplicationID = firstNonEmpty(view.ApplicationID, result.ApplicationID)
	result.ApplicationName = firstNonEmpty(view.ApplicationName, result.ApplicationName)
	result.ProjectName = firstNonEmpty(view.ProjectName, result.ProjectName)
	result.EnvironmentName = firstNonEmpty(view.EnvironmentName, result.EnvironmentName)
	result.DetailsURL = firstNonEmpty(view.DeploymentURL, result.DetailsURL)
	result.Error = strings.TrimSpace(view.ErrorMessage)
	if view.EnvironmentURL != "" && result.URL == "" {
		result.URL = view.EnvironmentURL
	}
}

// resolvePublicURL prefers the URL Notploy reports for the environment, and
// falls back to reading the application's domains. Both are best effort: a
// missing domain must not fail a deployment.
func (s *Service) resolvePublicURL(ctx context.Context, view *notploy.DeploymentView) string {
	if view != nil && view.EnvironmentURL != "" {
		return view.EnvironmentURL
	}
	if view != nil && view.ApplicationID != "" {
		return s.publicURL(ctx, view.ApplicationID)
	}
	return ""
}

// publicURL resolves the application's public URL from `application.one`.
func (s *Service) publicURL(ctx context.Context, applicationID string) string {
	lookupCtx, cancel := context.WithTimeout(ctx, urlLookupTimeout)
	defer cancel()

	application, err := s.client.GetApplication(lookupCtx, applicationID)
	if err != nil {
		s.log.Warningf("Could not resolve the application URL: %v", err)
		return ""
	}
	return application.PublicURL()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
