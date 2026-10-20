package deployment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/skygenesisenterprise/notploy-actions/internal/notploy"
)

// ambiguousError reports that the deployment created by this run cannot be told
// apart from at least one other deployment started at the same time with the
// same title and description. The action never guesses in that situation: a
// wrong id would report another deployment's failure as this run's.
type ambiguousError struct {
	count int
}

func (e *ambiguousError) Error() string {
	return fmt.Sprintf("%d deployments started since the first listing share this title and description", e.count)
}

// runLegacy triggers a deployment through `application.deploy` and follows it
// through `deployment.all`.
//
// Unlike the CI/CD API, these endpoints do not return the id of the deployment
// they create and are not idempotent: the run therefore snapshots the existing
// deployments first and then looks for the one that appeared — and that one must
// also carry this run's title and description, so a concurrent deployment is
// never mistaken for this one.
func (s *Service) runLegacy(ctx context.Context, params Params, started time.Time) (*Result, error) {
	runCtx, cancel := resolveTimeout(ctx, params)

	known, err := s.snapshot(runCtx, params.ApplicationID)
	if err != nil {
		cancel()
		if params.Wait {
			return nil, fmt.Errorf("list existing deployments: %w", err)
		}
		s.log.Warningf("Could not list existing deployments, the deployment id will not be reported: %v", err)
		known = nil
	}

	s.log.Infof("Triggering deployment for application %s through the classic API", params.ApplicationID)
	if err := s.client.TriggerDeployment(runCtx, params.ApplicationID, params.Title, params.Description); err != nil {
		cancel()
		return nil, fmt.Errorf("trigger deployment: %w", err)
	}

	result := &Result{
		Mode:          ModeLegacy,
		ApplicationID: params.ApplicationID,
		Status:        StatusTriggered,
		LogsURL:       s.client.LogsURL(false, ""),
	}
	// One best-effort lookup for both the public URL and the application name:
	// it must not block the workflow, and a missing domain is not a failure.
	result.URL, result.ApplicationName = s.applicationInfo(ctx, params.ApplicationID)

	s.emit(ctx, params, StatusEvent{Status: StatusTriggered, ApplicationName: result.ApplicationName})

	if !params.Wait {
		if known != nil {
			deployment, findErr := s.findNewDeployment(runCtx, params, known)
			if findErr != nil {
				s.log.Warningf("Could not identify this run's deployment: %v", findErr)
			} else if deployment != nil {
				applyLegacyDeployment(result, deployment)
				result.LogsURL = s.client.LogsURL(false, deployment.DeploymentID)
			}
		}
		result.Duration = time.Since(started)
		cancel()
		return result, nil
	}

	deployment, waitErr := s.waitLegacy(runCtx, params, known, result)
	cancel()

	if deployment != nil {
		applyLegacyDeployment(result, deployment)
		result.LogsURL = s.client.LogsURL(false, deployment.DeploymentID)
	}
	result.Duration = time.Since(started)

	switch {
	case waitErr == nil:
		switch {
		case deployment.Failed():
			s.collectLogs(ctx, params, result, false)
			message := strings.TrimSpace(result.Error)
			if message == "" {
				message = "the deployment failed without an error message"
			}
			return result, fmt.Errorf("deployment %s failed: %s", deployment.DeploymentID, message)
		case deployment.Status == notploy.StatusCancelled:
			s.log.Warningf("Deployment %s was cancelled in Notploy", deployment.DeploymentID)
			s.collectLogs(ctx, params, result, false)
			return result, nil
		default:
			return result, nil
		}
	case errors.Is(waitErr, context.DeadlineExceeded):
		if params.CancelOnTimeout {
			s.log.Warningf("The classic API cannot cancel a deployment; deployment %s keeps running in Notploy", result.DeploymentID)
		}
		return result, fmt.Errorf("deployment did not reach a final state within %s", params.Timeout)
	case errors.Is(waitErr, context.Canceled):
		return result, fmt.Errorf("deployment wait was cancelled: %w", waitErr)
	default:
		return result, waitErr
	}
}

// snapshot records the deployments that already exist, so the one created by
// this run can be told apart from its predecessors and from deployments started
// by someone else.
func (s *Service) snapshot(ctx context.Context, applicationID string) (map[string]struct{}, error) {
	deployments, err := s.client.ListDeployments(ctx, applicationID)
	if err != nil {
		return nil, err
	}

	known := make(map[string]struct{}, len(deployments))
	for _, deployment := range deployments {
		known[deployment.DeploymentID] = struct{}{}
	}
	return known, nil
}

// findNewDeployment returns the deployment this run created, or nil while it
// does not exist yet.
//
// The candidate must be unknown *and* carry the exact title and description the
// run sent: `application.deploy` stores both verbatim, which is the only
// correlation available on this API surface. Zero candidates means "not created
// yet"; several candidates mean "cannot be told apart", which is reported as an
// error instead of a guess.
func (s *Service) findNewDeployment(ctx context.Context, params Params, known map[string]struct{}) (*notploy.Deployment, error) {
	deployments, err := s.client.ListDeployments(ctx, params.ApplicationID)
	if err != nil {
		return nil, err
	}

	expectedTitle := legacyTitle(params.Title)
	matched := make([]notploy.Deployment, 0, 1)
	// `deployment.all` is ordered newest first.
	for i := range deployments {
		if _, seen := known[deployments[i].DeploymentID]; seen {
			continue
		}
		if deployments[i].Title != expectedTitle || deployments[i].Description != params.Description {
			continue
		}
		matched = append(matched, deployments[i])
	}

	switch len(matched) {
	case 0:
		return nil, nil
	case 1:
		return &matched[0], nil
	default:
		return nil, &ambiguousError{count: len(matched)}
	}
}

// legacyTitle mirrors what the server stores for the title: the router
// substitutes `Manual deployment` when the action sends none.
func legacyTitle(title string) string {
	if title == "" {
		return "Manual deployment"
	}
	return title
}

// waitLegacy polls until the deployment this run created is terminal, the
// context is done or the instance stops answering.
func (s *Service) waitLegacy(ctx context.Context, params Params, known map[string]struct{}, result *Result) (*notploy.Deployment, error) {
	ticker := time.NewTicker(params.PollInterval)
	defer ticker.Stop()

	var (
		latest          *notploy.Deployment
		consecutiveErr  int
		warnedAmbiguity bool
		lastReported    string
	)

	for {
		deployment, err := s.findNewDeployment(ctx, params, known)
		switch {
		case err != nil:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return latest, ctxErr
			}
			var ambiguous *ambiguousError
			if errors.As(err, &ambiguous) {
				if !warnedAmbiguity {
					s.log.Warningf("%v; the deployment id of this run cannot be reported, and the wait may time out", ambiguous)
					warnedAmbiguity = true
				}
				break
			}
			consecutiveErr++
			s.log.Warningf("Could not read the deployment status (attempt %d/%d): %v", consecutiveErr, maxConsecutiveErrors, err)
			if consecutiveErr >= maxConsecutiveErrors {
				return latest, fmt.Errorf("gave up after %d failed status lookups: %w", consecutiveErr, err)
			}
		case deployment == nil:
			consecutiveErr = 0
		default:
			consecutiveErr = 0
			if deployment.Status != lastReported {
				if lastReported == "" {
					s.log.Infof("Deployment %s is %s", deployment.DeploymentID, deployment.Status)
				} else {
					s.log.Infof("Deployment %s is now %s", deployment.DeploymentID, deployment.Status)
				}
				lastReported = deployment.Status
				s.emit(ctx, params, StatusEvent{
					DeploymentID:    deployment.DeploymentID,
					Status:          deployment.Status,
					ApplicationName: result.ApplicationName,
				})
			}
			latest = deployment
			if deployment.IsTerminal() {
				return deployment, nil
			}
		}

		select {
		case <-ctx.Done():
			return latest, ctx.Err()
		case <-ticker.C:
		}
	}
}

// applicationInfo returns the application's public URL and name in one call.
func (s *Service) applicationInfo(ctx context.Context, applicationID string) (url, name string) {
	lookupCtx, cancel := context.WithTimeout(ctx, urlLookupTimeout)
	defer cancel()

	application, err := s.client.GetApplication(lookupCtx, applicationID)
	if err != nil {
		s.log.Warningf("Could not resolve the application URL: %v", err)
		return "", ""
	}
	return application.PublicURL(), application.Name
}

func applyLegacyDeployment(result *Result, deployment *notploy.Deployment) {
	if deployment == nil {
		return
	}
	result.DeploymentID = deployment.DeploymentID
	if deployment.Status != "" {
		result.Status = deployment.Status
	}
	if deployment.Failed() {
		result.Error = strings.TrimSpace(deref(deployment.ErrorMessage))
	}
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
