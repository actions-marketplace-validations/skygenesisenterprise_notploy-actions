// Package config turns the GitHub Actions inputs into a validated, typed
// configuration. This is the only place that maps runner inputs to domain
// values; every other package receives plain values.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/skygenesisenterprise/notploy-actions/internal/github"
)

const (
	// DefaultTimeout is used when the `timeout` input is not set.
	DefaultTimeout = 30 * time.Minute
	// DefaultPollInterval is used when the `poll-interval` input is not set.
	DefaultPollInterval = 5 * time.Second
	// MinPollInterval protects the Notploy instance from a tight polling loop.
	MinPollInterval = time.Second
	// DefaultLogTail is how many log lines are fetched for a failed deployment.
	DefaultLogTail = 100
	// MaxLogTail is the largest tail both Notploy log endpoints accept; the
	// CI/CD API caps at 5000.
	MaxLogTail = 5000
	// MaxTitleLength and MaxDescriptionLength mirror the CI/CD API schemas so a
	// too-long value is rejected before the request instead of by a 400.
	MaxTitleLength       = 200
	MaxDescriptionLength = 2000
	// MaxIdempotencyKeyLength mirrors the CI/CD API schema.
	MaxIdempotencyKeyLength = 300
	// MaxTargetNameLength bounds the `project` and `environment` inputs.
	MaxTargetNameLength = 200
)

// APIMode selects which Notploy API surface the action talks to.
type APIMode string

const (
	// ModeAuto prefers the CI/CD API and falls back to the classic endpoints
	// when the credential or the instance does not expose it.
	ModeAuto APIMode = "auto"
	// ModeCicd requires the CI/CD API and fails when it is unavailable.
	ModeCicd APIMode = "cicd"
	// ModeLegacy only uses `application.deploy` and `deployment.all`.
	ModeLegacy APIMode = "legacy"
)

// Config is the fully validated configuration of one action run.
type Config struct {
	Endpoint      string
	APIKey        string
	ApplicationID string

	// Project and Environment optionally narrow the CI/CD target; both accept
	// an id or the name shown in the Notploy dashboard.
	Project     string
	Environment string
	APIMode     APIMode

	// Metadata reports whether the GitHub run context is sent to Notploy. It
	// is required by the CI/CD API, so disabling it forces the legacy mode.
	Metadata bool

	// Title and Description override the ones derived from the run context.
	Title       string
	Description string
	// IdempotencyKey overrides the key derived from the run id and attempt.
	IdempotencyKey string

	Wait            bool
	Timeout         time.Duration
	PollInterval    time.Duration
	CancelOnTimeout bool

	// LogTail is how many log lines of a failed deployment are read. Zero
	// disables reading logs entirely.
	LogTail int

	// GitHubToken, when set, publishes a commit status for the deployment. It
	// is a secret: it is masked and never written to outputs or summaries.
	GitHubToken string
	// GitHubStatusContext overrides the status context (default
	// `notploy/<application>`).
	GitHubStatusContext string
}

// Load reads the action inputs from the environment and validates them.
//
// Error messages never include a secret value, so a misconfigured credential
// cannot leak into the workflow log. Non-secret values are quoted because a
// typo the user cannot see is a typo they cannot fix.
func Load(getenv github.Getenv) (*Config, error) {
	cfg := &Config{
		Endpoint:      strings.TrimSpace(github.ReadInput(getenv, "endpoint")),
		APIKey:        strings.TrimSpace(github.ReadInput(getenv, "api-key")),
		ApplicationID: strings.TrimSpace(github.ReadInput(getenv, "application-id")),
		Project:       strings.TrimSpace(github.ReadInput(getenv, "project")),
		Environment:   strings.TrimSpace(github.ReadInput(getenv, "environment")),
		APIMode:       ModeAuto,
		Metadata:      true,
		Wait:          true,
		Timeout:       DefaultTimeout,
		PollInterval:  DefaultPollInterval,
		LogTail:       DefaultLogTail,
	}

	missing := make([]string, 0, 3)
	if cfg.Endpoint == "" {
		missing = append(missing, "endpoint")
	}
	if cfg.APIKey == "" {
		missing = append(missing, "api-key")
	}
	if cfg.ApplicationID == "" {
		missing = append(missing, "application-id")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required input(s): %s", strings.Join(missing, ", "))
	}

	var err error
	if cfg.APIMode, err = parseAPIMode(getenv); err != nil {
		return nil, err
	}
	if cfg.Metadata, err = parseBool(getenv, "metadata", cfg.Metadata); err != nil {
		return nil, err
	}
	if cfg.Wait, err = parseBool(getenv, "wait", cfg.Wait); err != nil {
		return nil, err
	}
	if cfg.CancelOnTimeout, err = parseBool(getenv, "cancel-on-timeout", cfg.CancelOnTimeout); err != nil {
		return nil, err
	}

	if cfg.Timeout, err = parseDuration(getenv, "timeout", cfg.Timeout); err != nil {
		return nil, err
	}
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("input `timeout` must be greater than zero")
	}
	if cfg.PollInterval, err = parseDuration(getenv, "poll-interval", cfg.PollInterval); err != nil {
		return nil, err
	}
	if cfg.PollInterval < MinPollInterval {
		return nil, fmt.Errorf("input `poll-interval` must be at least %s", MinPollInterval)
	}

	if cfg.LogTail, err = parseBoundedInt(getenv, "log-tail", cfg.LogTail, 0, MaxLogTail); err != nil {
		return nil, err
	}

	cfg.Title = strings.TrimSpace(github.ReadInput(getenv, "title"))
	if len(cfg.Title) > MaxTitleLength {
		return nil, fmt.Errorf("input `title` must be at most %d characters, got %d", MaxTitleLength, len(cfg.Title))
	}
	cfg.Description = strings.TrimSpace(github.ReadInput(getenv, "description"))
	if len(cfg.Description) > MaxDescriptionLength {
		return nil, fmt.Errorf("input `description` must be at most %d characters, got %d", MaxDescriptionLength, len(cfg.Description))
	}
	cfg.IdempotencyKey = strings.TrimSpace(github.ReadInput(getenv, "idempotency-key"))
	if len(cfg.IdempotencyKey) > MaxIdempotencyKeyLength {
		return nil, fmt.Errorf("input `idempotency-key` must be at most %d characters, got %d", MaxIdempotencyKeyLength, len(cfg.IdempotencyKey))
	}

	if len(cfg.Project) > MaxTargetNameLength {
		return nil, fmt.Errorf("input `project` must be at most %d characters, got %d", MaxTargetNameLength, len(cfg.Project))
	}
	if len(cfg.Environment) > MaxTargetNameLength {
		return nil, fmt.Errorf("input `environment` must be at most %d characters, got %d", MaxTargetNameLength, len(cfg.Environment))
	}

	// The token is read as a secret: it must never appear in an error.
	cfg.GitHubToken = strings.TrimSpace(github.ReadInput(getenv, "github-token"))
	cfg.GitHubStatusContext = strings.TrimSpace(github.ReadInput(getenv, "github-status-context"))
	if cfg.GitHubStatusContext != "" && len(cfg.GitHubStatusContext) > 100 {
		return nil, fmt.Errorf("input `github-status-context` must be at most 100 characters, got %d", len(cfg.GitHubStatusContext))
	}

	if cfg.APIMode == ModeCicd && !cfg.Metadata {
		return nil, fmt.Errorf("input `api-mode: cicd` requires the GitHub run metadata; keep `metadata: true` or set `api-mode: legacy`")
	}

	return cfg, nil
}

func parseAPIMode(getenv github.Getenv) (APIMode, error) {
	raw := strings.TrimSpace(github.ReadInput(getenv, "api-mode"))
	if raw == "" {
		return ModeAuto, nil
	}
	switch APIMode(strings.ToLower(raw)) {
	case ModeAuto, ModeCicd, ModeLegacy:
		return APIMode(strings.ToLower(raw)), nil
	default:
		return "", fmt.Errorf("input `api-mode` must be one of `auto`, `cicd` or `legacy`, got %q", raw)
	}
}

func parseBool(getenv github.Getenv, name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(github.ReadInput(getenv, name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("input `%s` must be a boolean such as `true` or `false`, got %q", name, raw)
	}
	return value, nil
}

func parseDuration(getenv github.Getenv, name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(github.ReadInput(getenv, name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("input `%s` must be a duration such as `30m` or `5s`, got %q", name, raw)
	}
	return value, nil
}

func parseBoundedInt(getenv github.Getenv, name string, fallback, min, max int) (int, error) {
	raw := strings.TrimSpace(github.ReadInput(getenv, name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("input `%s` must be a number between %d and %d, got %q", name, min, max, raw)
	}
	return value, nil
}
