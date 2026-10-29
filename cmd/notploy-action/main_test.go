package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	testAPIKey    = "npk_end_to_end_key"
	testGitHubTok = "ghp_test_status_token"
	testAppID     = "application-1"
	testOldID     = "deployment-1"
	testNewID     = "deployment-2"
	testPublicURL = "https://my-app.example.com"
	testDashboard = "https://notploy.example.com/dashboard/applications/application-1"
	testLogText   = "step 1/3: cloning\nstep 2/3: building\nstep 3/3: build failed"
	testError     = "build failed: Dockerfile not found"
)

// binary builds the action once per test run and returns its path.
var binary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "notploy-action-build")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "notploy-action")
	build := exec.Command("go", "build", "-o", path, ".")
	if output, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w\n%s", err, output)
	}
	return path, nil
})

// scenario scripts a fake Notploy instance for one action run.
type scenario struct {
	// statuses are the statuses reported after the trigger, in order. The last
	// one is repeated, so the wait always terminates.
	statuses []string
	// apiMode is the value of the `api-mode` input; empty means `auto`.
	apiMode string
	// cicdUnavailable makes the instance answer the CI/CD API with the 403 a
	// non-CI/CD token gets, which is what the `auto` fallback reacts to.
	cicdUnavailable bool
	// underscoredAPIKey uses the other spelling of an input name.
	underscoredAPIKey bool
	// githubToken provides the `github-token` input, which turns on the
	// commit status publication.
	githubToken bool
}

func TestActionSucceedsViaTheCicdAPI(t *testing.T) {
	result := runAction(t, scenario{statuses: []string{"running", "done"}})

	if result.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout:\n%s", result.exitCode, result.stdout)
	}
	if !strings.Contains(result.stdout, "Deployment ID: "+testNewID) {
		t.Errorf("stdout does not report the deployment id:\n%s", result.stdout)
	}
	if !strings.Contains(result.stdout, "✓ Deployment successful") {
		t.Errorf("stdout does not report success:\n%s", result.stdout)
	}
	if result.outputs["deployment-id"] != testNewID {
		t.Errorf("deployment-id = %q", result.outputs["deployment-id"])
	}
	if result.outputs["deployment-status"] != "done" {
		t.Errorf("deployment-status = %q", result.outputs["deployment-status"])
	}
	if result.outputs["deployment-url"] != testPublicURL {
		t.Errorf("deployment-url = %q", result.outputs["deployment-url"])
	}
	if result.outputs["deployment-details-url"] != testDashboard {
		t.Errorf("deployment-details-url = %q", result.outputs["deployment-details-url"])
	}
	if !strings.Contains(result.outputs["deployment-logs-url"], "/api/cicd/deployments/"+testNewID+"/logs") {
		t.Errorf("deployment-logs-url = %q", result.outputs["deployment-logs-url"])
	}
	if result.outputs["deployment-environment"] != "production" {
		t.Errorf("deployment-environment = %q", result.outputs["deployment-environment"])
	}
	if result.outputs["deployment-idempotent"] != "false" {
		t.Errorf("deployment-idempotent = %q, want false for a new deployment", result.outputs["deployment-idempotent"])
	}
	if !strings.Contains(result.summary, "## Notploy deployment") {
		t.Errorf("summary is missing its title:\n%s", result.summary)
	}
	if !strings.Contains(result.summary, testNewID) {
		t.Errorf("summary is missing the deployment id:\n%s", result.summary)
	}
	if !strings.Contains(result.summary, "CI/CD API") {
		t.Errorf("summary does not name the API surface:\n%s", result.summary)
	}
	if !result.instance.cicdDeployed || result.instance.legacyDeployed {
		t.Errorf("cicdDeployed = %v, legacyDeployed = %v, want only the CI/CD API",
			result.instance.cicdDeployed, result.instance.legacyDeployed)
	}
	// The summary is what reviewers read: secrets must never reach it.
	if strings.Contains(result.summary, testAPIKey) || strings.Contains(result.summary, testGitHubTok) {
		t.Fatalf("the summary leaked a secret:\n%s", result.summary)
	}
}

func TestActionFallsBackWithoutACicdToken(t *testing.T) {
	result := runAction(t, scenario{
		statuses:        []string{"running", "done"},
		cicdUnavailable: true,
	})

	if result.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout:\n%s", result.exitCode, result.stdout)
	}
	if !result.instance.legacyDeployed {
		t.Fatalf("the classic endpoints were not used:\n%s", result.stdout)
	}
	if result.outputs["deployment-status"] != "done" {
		t.Errorf("deployment-status = %q", result.outputs["deployment-status"])
	}
	// The classic API has no idempotency: an empty value says "not applicable".
	if result.outputs["deployment-idempotent"] != "" {
		t.Errorf("deployment-idempotent = %q, want empty on the classic endpoints", result.outputs["deployment-idempotent"])
	}
	if !strings.Contains(result.summary, "classic API") {
		t.Errorf("summary does not name the API surface:\n%s", result.summary)
	}
	if result.outputs["deployment-url"] != testPublicURL {
		t.Errorf("deployment-url = %q", result.outputs["deployment-url"])
	}
}

func TestActionUsesOnlyTheClassicAPIWhenAskedTo(t *testing.T) {
	result := runAction(t, scenario{
		statuses: []string{"running", "done"},
		apiMode:  "legacy",
	})

	if result.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout:\n%s", result.exitCode, result.stdout)
	}
	if result.instance.cicdDeployed {
		t.Fatal("the CI/CD API was used despite api-mode: legacy")
	}
	if !result.instance.legacyDeployed {
		t.Fatal("the classic endpoints were not used")
	}
	if !strings.Contains(result.stdout, "classic API") {
		t.Errorf("stdout does not report the API mode:\n%s", result.stdout)
	}
}

func TestActionFailsWhenTheDeploymentFails(t *testing.T) {
	result := runAction(t, scenario{statuses: []string{"error"}})

	if result.exitCode == 0 {
		t.Fatalf("exit code = 0, want a failure\nstdout:\n%s", result.stdout)
	}
	if result.outputs["deployment-status"] != "error" {
		t.Errorf("deployment-status = %q", result.outputs["deployment-status"])
	}
	if !strings.Contains(result.stdout, "Notploy deployment failed") {
		t.Errorf("stdout does not report the failure:\n%s", result.stdout)
	}
	// Outputs are published even when the deployment fails, so later steps can
	// link to the failed deployment.
	if result.outputs["deployment-id"] != testNewID {
		t.Errorf("deployment-id = %q", result.outputs["deployment-id"])
	}
	// The failure is only actionable with the deployment's own message and log.
	if !strings.Contains(result.stdout, testError) {
		t.Errorf("stdout does not report the deployment error:\n%s", result.stdout)
	}
	if !strings.Contains(result.stdout, "step 3/3: build failed") {
		t.Errorf("stdout does not contain the log tail:\n%s", result.stdout)
	}
	if !strings.Contains(result.summary, testError) {
		t.Errorf("summary does not contain the deployment error:\n%s", result.summary)
	}
	if !strings.Contains(result.summary, "step 3/3: build failed") {
		t.Errorf("summary does not contain the log excerpt:\n%s", result.summary)
	}
	if !result.instance.cicdLogsRead {
		t.Error("the CI/CD logs endpoint was not used")
	}
}

func TestActionReadsUnderscoredInputNames(t *testing.T) {
	result := runAction(t, scenario{
		statuses:          []string{"done"},
		underscoredAPIKey: true,
	})

	if result.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout:\n%s", result.exitCode, result.stdout)
	}
	if result.outputs["deployment-status"] != "done" {
		t.Errorf("deployment-status = %q", result.outputs["deployment-status"])
	}
}

func TestActionPublishesCommitStatuses(t *testing.T) {
	result := runAction(t, scenario{
		statuses:    []string{"running", "done"},
		githubToken: true,
	})

	if result.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout:\n%s", result.exitCode, result.stdout)
	}
	states := result.instance.statusStates
	if len(states) < 2 {
		t.Fatalf("published commit statuses = %v, want at least a pending and a final one", states)
	}
	if states[len(states)-1] != "success" {
		t.Errorf("last commit status = %q, want success (%v)", states[len(states)-1], states)
	}
	for _, state := range states[:len(states)-1] {
		if state != "pending" {
			t.Errorf("intermediate commit status = %q, want pending (%v)", state, states)
		}
	}
	contexts := map[string]bool{}
	for _, name := range result.instance.statusContexts {
		contexts[name] = true
	}
	if len(contexts) != 1 {
		t.Fatalf("commit status contexts = %v, want exactly one", result.instance.statusContexts)
	}
	if !strings.HasPrefix(result.instance.statusContexts[0], "notploy/") {
		t.Errorf("commit status context = %q, want the notploy/ prefix", result.instance.statusContexts[0])
	}
}

type runResult struct {
	exitCode int
	stdout   string
	outputs  map[string]string
	summary  string
	instance *fakeInstance
}

func runAction(t *testing.T, scenario scenario) runResult {
	t.Helper()

	path, err := binary()
	if err != nil {
		t.Fatalf("build the action: %v", err)
	}

	instance := newFakeInstance(t, scenario)

	workDir := t.TempDir()
	outputPath := filepath.Join(workDir, "outputs")
	summaryPath := filepath.Join(workDir, "summary")

	apiKeyInput := "INPUT_API-KEY"
	if scenario.underscoredAPIKey {
		apiKeyInput = "INPUT_API_KEY"
	}

	command := exec.Command(path)
	command.Env = []string{
		"INPUT_ENDPOINT=" + instance.URL,
		apiKeyInput + "=" + testAPIKey,
		"INPUT_APPLICATION-ID=" + testAppID,
		"INPUT_WAIT=true",
		"INPUT_TIMEOUT=30s",
		"INPUT_POLL-INTERVAL=1s",
		"GITHUB_OUTPUT=" + outputPath,
		"GITHUB_STEP_SUMMARY=" + summaryPath,
		"GITHUB_REPOSITORY=skygenesisenterprise/notploy",
		"GITHUB_REF=refs/heads/main",
		"GITHUB_REF_NAME=main",
		"GITHUB_SHA=0123456789abcdef0123456789abcdef01234567",
		"GITHUB_ACTOR=octocat",
		"GITHUB_EVENT_NAME=push",
		"GITHUB_RUN_ID=123456",
		"GITHUB_RUN_NUMBER=42",
		"GITHUB_RUN_ATTEMPT=1",
		"GITHUB_SERVER_URL=https://github.com",
		"GITHUB_WORKFLOW=Deploy",
		"GITHUB_API_URL=" + instance.URL,
	}
	if scenario.apiMode != "" {
		command.Env = append(command.Env, "INPUT_API-MODE="+scenario.apiMode)
	}
	if scenario.githubToken {
		command.Env = append(command.Env, "INPUT_GITHUB-TOKEN="+testGitHubTok)
	}
	output, err := command.CombinedOutput()

	result := runResult{
		stdout:   string(output),
		outputs:  parseOutputs(t, outputPath),
		summary:  readIfPresent(t, summaryPath),
		instance: instance,
	}
	if err == nil {
		return result
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run the action: %v\noutput:\n%s", err, output)
	}
	result.exitCode = exitErr.ExitCode()
	return result
}

// fakeInstance is a Notploy that speaks both API surfaces, plus the GitHub
// commit status endpoint.
type fakeInstance struct {
	URL string

	mutex sync.Mutex

	// Classic endpoints.
	legacyDeployed bool
	listCalls      int
	deployments    []map[string]any

	// CI/CD API.
	cicdDeployed    bool
	cicdStatusCalls int
	cicdLogsRead    bool
	cicdRequest     map[string]any

	// GitHub commit statuses.
	statusStates   []string
	statusContexts []string
}

func newFakeInstance(t *testing.T, sc scenario) *fakeInstance {
	t.Helper()

	instance := &fakeInstance{}
	mux := http.NewServeMux()

	// --- CI/CD API -----------------------------------------------------------

	mux.HandleFunc("/api/cicd/deploy", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if sc.cicdUnavailable {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"message": "This API key is not a CI/CD token. Create one from the project's CI/CD page.",
				"code":    "FORBIDDEN",
			})
			return
		}
		if r.Header.Get("x-api-key") != testAPIKey {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"message": "This endpoint requires a Notploy CI/CD API token. Send it as `x-api-key`.",
				"code":    "UNAUTHORIZED",
			})
			return
		}

		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		instance.mutex.Lock()
		defer instance.mutex.Unlock()
		instance.cicdDeployed = true
		instance.cicdRequest = body

		writeJSON(w, http.StatusOK, map[string]any{
			"deploymentId":    testNewID,
			"status":          "running",
			"queued":          true,
			"title":           body["title"],
			"description":     body["description"],
			"applicationId":   testAppID,
			"applicationName": "my-app",
			"environmentName": "production",
			"deploymentUrl":   testDashboard,
			"environmentUrl":  testPublicURL,
			"idempotent":      false,
			"message":         "deployment queued",
		})
	})

	mux.HandleFunc("/api/cicd/deployments/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/cicd/deployments/")
		id, action, _ := strings.Cut(rest, "/")

		instance.mutex.Lock()
		defer instance.mutex.Unlock()

		switch {
		case action == "logs" && r.Method == http.MethodGet:
			instance.cicdLogsRead = true
			writeJSON(w, http.StatusOK, map[string]any{
				"deploymentId": id,
				"status":       "error",
				"logs":         testLogText,
				"lineCount":    3,
				"tail":         100,
				"truncated":    false,
			})
		case action == "cancel" && r.Method == http.MethodPost:
			writeJSON(w, http.StatusOK, map[string]any{})
		case action == "":
			status := sc.statuses[len(sc.statuses)-1]
			if instance.cicdStatusCalls < len(sc.statuses) {
				status = sc.statuses[instance.cicdStatusCalls]
			}
			instance.cicdStatusCalls++

			view := map[string]any{
				"deploymentId":    id,
				"status":          status,
				"applicationId":   testAppID,
				"applicationName": "my-app",
				"environmentName": "production",
				"deploymentUrl":   testDashboard,
				"environmentUrl":  testPublicURL,
				"title":           "GitHub Actions deployment",
			}
			if status == "error" {
				view["errorMessage"] = testError
			}
			writeJSON(w, http.StatusOK, view)
		default:
			http.NotFound(w, r)
		}
	})

	// --- Classic endpoints ---------------------------------------------------

	mux.HandleFunc("/api/application.deploy", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("x-api-key") != testAPIKey {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"message": "Authorization not provided",
				"code":    "UNAUTHORIZED",
			})
			return
		}

		var body struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		instance.mutex.Lock()
		defer instance.mutex.Unlock()
		instance.legacyDeployed = true
		// The deployment row appears only after the trigger, carrying the title
		// and description verbatim — which is exactly how the action
		// recognises its own deployment.
		instance.deployments = append([]map[string]any{{
			"deploymentId":  testNewID,
			"title":         body.Title,
			"description":   body.Description,
			"status":        "running",
			"applicationId": testAppID,
			"createdAt":     "2026-09-28T12:00:00.000Z",
		}}, instance.deployments...)
		writeJSON(w, http.StatusOK, map[string]any{})
	})

	mux.HandleFunc("/api/deployment.all", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != testAPIKey {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "nope"})
			return
		}

		instance.mutex.Lock()
		defer instance.mutex.Unlock()

		call := instance.listCalls
		instance.listCalls++

		for _, deployment := range instance.deployments {
			if deployment["deploymentId"] == testNewID {
				deployment["status"] = statusAt(sc.statuses, call-1)
			}
		}
		writeJSON(w, http.StatusOK, instance.deployments)
	})

	mux.HandleFunc("/api/application.one", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"applicationId": testAppID,
			"name":          "my-app",
			"domains": []map[string]any{
				{"host": "my-app.example.com", "https": true, "enabled": true},
			},
		})
	})

	mux.HandleFunc("/api/deployment.readLogs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(testLogText))
	})

	// --- GitHub commit statuses ---------------------------------------------

	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State   string `json:"state"`
			Context string `json:"context"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		instance.mutex.Lock()
		defer instance.mutex.Unlock()
		instance.statusStates = append(instance.statusStates, body.State)
		instance.statusContexts = append(instance.statusContexts, body.Context)

		writeJSON(w, http.StatusCreated, body)
	})

	// The historical deployment the snapshot has to ignore.
	instance.deployments = []map[string]any{{
		"deploymentId":  testOldID,
		"title":         "previous",
		"status":        "done",
		"applicationId": testAppID,
		"createdAt":     "2026-09-27T12:00:00.000Z",
	}}

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	instance.URL = server.URL
	return instance
}

// statusAt indexes the scripted statuses, ignoring the pre-trigger snapshot
// call and repeating the last entry.
func statusAt(statuses []string, pollIndex int) string {
	if len(statuses) == 0 {
		return "done"
	}
	if pollIndex < 0 {
		return statuses[0]
	}
	if pollIndex >= len(statuses) {
		return statuses[len(statuses)-1]
	}
	return statuses[pollIndex]
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func parseOutputs(t *testing.T, path string) map[string]string {
	t.Helper()

	content := readIfPresent(t, path)
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		if line == "" || strings.Contains(line, "<<") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[key] = value
	}
	return values
}

func readIfPresent(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
