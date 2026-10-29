package notploy

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// errorResponse is the public error body Notploy returns for a failed request.
type errorResponse struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Issues  []struct {
		Message string `json:"message"`
	} `json:"issues"`
}

// APIError is returned when Notploy answers with a non-2xx status code.
//
// It only ever carries data that is safe to print: the operation, the status
// code and the public message from the JSON body. Request headers — including
// the API key — are never part of it.
type APIError struct {
	Operation  string
	StatusCode int
	Code       string
	Message    string
	// RetryAfter is the bounded `Retry-After` hint of a 429/503 response.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = strings.TrimSpace(http.StatusText(e.StatusCode))
	}
	if message == "" {
		message = "no error message returned"
	}
	if e.Code != "" {
		message = fmt.Sprintf("%s [%s]", message, e.Code)
	}
	return fmt.Sprintf("%s failed with HTTP %d: %s", e.Operation, e.StatusCode, message)
}

// IsUnauthorized reports whether the credential was rejected.
func (e *APIError) IsUnauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// IsNotFound reports whether the target does not exist.
func (e *APIError) IsNotFound() bool {
	return e.StatusCode == http.StatusNotFound
}

// IsRateLimited reports whether the instance asked the caller to slow down.
func (e *APIError) IsRateLimited() bool {
	return e.StatusCode == http.StatusTooManyRequests
}

// IsRetryable reports whether repeating the request may succeed.
func (e *APIError) IsRetryable() bool {
	return e.IsRateLimited() || e.StatusCode >= http.StatusInternalServerError
}

// mentions reports whether the server message contains a phrase, ignoring case.
// The exact wording matters here: the action must only fall back to another API
// when Notploy itself said the capability is missing.
func (e *APIError) mentions(phrases ...string) bool {
	message := strings.ToLower(e.Message)
	for _, phrase := range phrases {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

// IsCicdUnavailable reports whether a failed CI/CD request means "this instance
// or this credential has no CI/CD API", which is the only situation in which the
// action may retry the same intent on the legacy endpoints.
//
// Any other failure — a revoked key, a target outside the token's scope, a
// malformed request — is a real error that must reach the user instead of being
// papered over by a different API surface.
func IsCicdUnavailable(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return apiErr.mentions(
			"notploy cicd api token",
			"not a ci/cd token",
			"not allowed to",
		)
	case http.StatusNotFound:
		// An instance without the `cicd` router answers the OpenAPI adapter's
		// "Not found" for the unknown route; a *known* route reports which
		// object was missing ("Deployment not found", "Project not found").
		message := strings.TrimSpace(apiErr.Message)
		return strings.EqualFold(message, "Not found") || strings.EqualFold(message, "Not Found")
	default:
		return false
	}
}

// Hint returns an actionable explanation for the most common failure modes, or
// "" when there is nothing specific to add.
func (e *APIError) Hint() string {
	switch {
	case e.mentions("not a ci/cd token"):
		return "This API key is not a Notploy CI/CD token. Create one from the project's CI/CD page in Notploy, or set the `api-mode` input to `legacy` to use the classic endpoints."
	case e.mentions("not allowed to"):
		return "The CI/CD token exists but its scope does not grant this action; edit the token's actions on the project's CI/CD page in Notploy."
	case e.mentions("notploy cicd api token"):
		return "The CI/CD API needs its own credential: create a CI/CD token from the project's CI/CD page in Notploy and pass it as `api-key`."
	case e.StatusCode == http.StatusBadRequest:
		return "Notploy rejected the request; check that `application-id` is a valid application id, and `project`/`environment` if you set them."
	case e.StatusCode == http.StatusUnauthorized:
		return "Notploy rejected the API key; check the `api-key` input (NOTPLOY_API_KEY)."
	case e.StatusCode == http.StatusForbidden:
		return "The API key is valid but not allowed to deploy this application; check its permissions."
	case e.StatusCode == http.StatusNotFound:
		return "Not found; check the `endpoint` input and `application-id`, and that the instance exposes this endpoint."
	case e.StatusCode == http.StatusConflict:
		return "Notploy reported a conflict; the application may already be deploying."
	case e.StatusCode == http.StatusTooManyRequests:
		return "Notploy is rate limiting this API key; retry later or increase `poll-interval`."
	case e.StatusCode >= http.StatusInternalServerError:
		return "Notploy returned a server error; retry, and check the instance logs if it persists."
	default:
		return ""
	}
}
