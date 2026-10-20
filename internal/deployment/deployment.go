// Package deployment implements the Notploy deployment lifecycle the action
// exposes: trigger, optionally wait for the result, collect logs, report.
//
// Nothing here reads environment variables or knows about GitHub Actions. The
// caller supplies every parameter and the logger, which is what keeps the
// lifecycle testable and reusable outside of a runner.
//
// Two API surfaces are supported and the service decides which one runs:
//
//   - the CI/CD API, which returns the created deployment's id and is
//     idempotent — preferred whenever the credential allows it;
//   - the classic endpoints, kept for API keys that are not CI/CD tokens and
//     for instances that do not expose the CI/CD router.
package deployment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/skygenesisenterprise/notploy-actions/internal/notploy"
)

const (
	// StatusTriggered is reported when `wait` is disabled and the deployment
	// has not been observed yet.
	StatusTriggered = "triggered"

	// API modes accepted in Params.Mode. They mirror the `api-mode` input.
	ModeAuto   = "auto"
	ModeCicd   = "cicd"
	ModeLegacy = "legacy"

	// defaultPollInterval is a safety net for callers that pass no interval.
	defaultPollInterval = 5 * time.Second
	// defaultTimeout bounds the wait when the caller passes none.
	defaultTimeout = 30 * time.Minute
	// maxConsecutiveErrors is how many broken status lookups in a row are
	// tolerated before the wait is abandoned.
	maxConsecutiveErrors = 5
	// urlLookupTimeout bounds the best-effort call that resolves the
	// application's public URL.
	urlLookupTimeout = 10 * time.Second
	// logsTimeout bounds the log fetch at the end of a deployment.
	logsTimeout = 15 * time.Second
	// statusEventTimeout bounds the publication of a commit status, which must
	// not be tied to the (possibly already cancelled) deployment context.
	statusEventTimeout = 15 * time.Second
)

// Client is the slice of the Notploy client the service needs. Both API
// surfaces are part of it; a run only calls the methods of the surface it
// resolved to.
type Client interface {
	// Classic endpoints.
	TriggerDeployment(ctx context.Context, applicationID, title, description string) error
	ListDeployments(ctx context.Context, applicationID string) ([]notploy.Deployment, error)
	GetApplication(ctx context.Context, applicationID string) (*notploy.Application, error)
	ReadDeploymentLogs(ctx context.Context, deploymentID string, tail int) (string, error)

	// CI/CD API.
	CicdDeploy(ctx context.Context, request notploy.CicdDeployRequest) (*notploy.DeploymentView, error)
	CicdStatus(ctx context.Context, deploymentID string) (*notploy.DeploymentView, error)
	CicdLogs(ctx context.Context, deploymentID string, tail int) (*notploy.CicdLogsResult, error)
	CicdCancel(ctx context.Context, deploymentID string) error

	// LogsURL renders the log endpoint of one of the two surfaces.
	LogsURL(cicd bool, deploymentID string) string
}

// Logger is what the service reports through. `github.Logger` implements it.
type Logger interface {
	Infof(format string, args ...any)
	Warningf(format string, args ...any)
}

// StatusEvent reports a deployment state change to the caller, which turns it
// into a commit status. It carries no credential and only values a workflow may
// print.
type StatusEvent struct {
	DeploymentID string
	// Status is a Notploy status (`queued`, `running`, `done`, `error`,
	// `cancelled`) or `triggered` when the deployment was not observed.
	Status string
	// ApplicationName is the name Notploy knows the application by, when the
	// API surface reports it; it lets the caller build a readable status
	// context before the run ends.
	ApplicationName string
	// URL links to the Notploy dashboard page of the deployment when there is
	// one; it may be empty.
	URL string
}

// Params describes one deployment request.
type Params struct {
	// Mode is one of `auto`, `cicd`, `legacy`.
	Mode string

	// ApplicationID is the deployment target: an id or the name shown in the
	// Notploy dashboard. Both API surfaces accept it as such.
	ApplicationID string
	// Project and Environment optionally narrow the CI/CD target.
	Project     string
	Environment string

	Title       string
	Description string
	// Source is the CI run metadata `cicd.deploy` requires. It is ignored by
	// the classic endpoints.
	Source notploy.SourceMetadata
	// IdempotencyKey, when set, makes the CI/CD trigger replayable and makes
	// one workflow attempt equal one deployment. The classic endpoints have no
	// such concept and are never retried.
	IdempotencyKey string

	// Wait blocks until the deployment reaches a final state.
	Wait bool
	// Timeout bounds the total wait, including the trigger request.
	Timeout time.Duration
	// PollInterval is the delay between two status lookups.
	PollInterval time.Duration
	// CancelOnTimeout asks Notploy to stop the deployment when the wait gives
	// up, instead of leaving it running without anyone following it.
	CancelOnTimeout bool

	// LogTail is how many log lines of a failed or cancelled deployment are
	// read. Zero disables reading logs.
	LogTail int

	// OnStatus, when set, is called once after the deployment was accepted and
	// again on every observed state change. Callback errors are logged, never
	// fatal: a commit status that cannot be published must not fail a
	// deployment that already succeeded.
	OnStatus func(ctx context.Context, event StatusEvent)
}

// Result is what the caller publishes as action outputs and as the job summary.
type Result struct {
	// Mode is the API surface that answered: `cicd` or `legacy`.
	Mode string

	ApplicationID   string
	ApplicationName string
	ProjectName     string
	EnvironmentName string

	DeploymentID string
	// Status is the last observed Notploy status, or `triggered`.
	Status string
	// Error is the deployment's own error message when it failed.
	Error string

	// URL is the public URL of the application.
	URL string
	// DetailsURL links to the Notploy dashboard page of the application (Notploy
	// has no per-deployment page yet).
	DetailsURL string
	// LogsURL is the API endpoint returning the deployment's log.
	LogsURL string

	// Idempotent reports that the CI/CD API returned an existing deployment
	// instead of creating one.
	Idempotent bool
	// LogExcerpt is the tail of a failed deployment's log, for the summary.
	LogExcerpt string

	Duration time.Duration
}

// Failed reports whether the deployment ended in a state that must fail the job.
// A cancellation does not: someone deliberately stopped it, and the action
// reports it as a warning instead of a failure.
func (r *Result) Failed() bool {
	return r != nil && r.Status == notploy.StatusError
}

// Cancelled reports whether the deployment ended cancelled.
func (r *Result) Cancelled() bool {
	return r != nil && r.Status == notploy.StatusCancelled
}

// Service triggers and follows deployments.
type Service struct {
	client Client
	log    Logger
}

// New returns a service. A nil logger is replaced by a no-op one.
func New(client Client, log Logger) *Service {
	if log == nil {
		log = discardLogger{}
	}
	return &Service{client: client, log: log}
}

// Run triggers a deployment and, when Params.Wait is set, follows it to a final
// state.
//
// The returned Result is non-nil as soon as a deployment was created, even when
// the returned error is non-nil, so the caller can still publish the deployment
// id and status it managed to observe.
//
// In `auto` mode the CI/CD API is tried first and the classic endpoints are
// only used when Notploy itself answered that it has no CI/CD API for this
// endpoint or credential — and only before anything was deployed, so a failure
// halfway through never restarts the same intent on another API surface.
func (s *Service) Run(ctx context.Context, params Params) (*Result, error) {
	if strings.TrimSpace(params.ApplicationID) == "" {
		return nil, errors.New("an application id or name is required")
	}
	switch params.Mode {
	case "":
		params.Mode = ModeAuto
	case ModeAuto, ModeCicd, ModeLegacy:
	default:
		return nil, fmt.Errorf("unknown api mode %q (expected %q, %q or %q)", params.Mode, ModeAuto, ModeCicd, ModeLegacy)
	}
	if params.PollInterval <= 0 {
		params.PollInterval = defaultPollInterval
	}
	if params.Wait && params.Timeout <= 0 {
		params.Timeout = defaultTimeout
	}

	started := time.Now()

	if params.Mode == ModeAuto || params.Mode == ModeCicd {
		result, err := s.runCicd(ctx, params, started)
		switch {
		case err == nil:
			return result, nil
		case params.Mode == ModeCicd:
			return result, err
		case result != nil:
			// Something was already deployed on the CI/CD API: retrying the same
			// intent on the classic endpoints would deploy twice.
			return result, err
		case !notploy.IsCicdUnavailable(err):
			return nil, err
		}
		s.log.Warningf("The CI/CD API is unavailable: %v", err)
		s.log.Warningf("Falling back to the classic endpoints (set `api-mode: cicd` to fail instead of falling back)")
	}

	return s.runLegacy(ctx, params, started)
}

// resolveTimeout derives the wait context, cancelled when the parent is.
func resolveTimeout(ctx context.Context, params Params) (context.Context, context.CancelFunc) {
	if !params.Wait {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, params.Timeout)
}

// emit forwards a state change to the caller's callback. The callback runs with
// a detached context: a commit status is worth publishing precisely when the
// deployment context is already gone.
func (s *Service) emit(ctx context.Context, params Params, event StatusEvent) {
	if params.OnStatus == nil {
		return
	}
	emitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusEventTimeout)
	defer cancel()
	params.OnStatus(emitCtx, event)
}

// bestEffortCancel stops a deployment nobody will follow anymore. It runs on a
// detached context with its own deadline.
func (s *Service) bestEffortCancel(deploymentID string) {
	if deploymentID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), logsTimeout)
	defer cancel()
	if err := s.client.CicdCancel(ctx, deploymentID); err != nil {
		s.log.Warningf("Could not cancel deployment %s: %v", deploymentID, err)
		return
	}
	s.log.Warningf("Cancelled deployment %s in Notploy", deploymentID)
}

// discardLogger is used when the caller provides no logger.
type discardLogger struct{}

func (discardLogger) Infof(string, ...any)    {}
func (discardLogger) Warningf(string, ...any) {}
