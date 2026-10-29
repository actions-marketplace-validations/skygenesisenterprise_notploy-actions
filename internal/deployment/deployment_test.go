package deployment

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/skygenesisenterprise/notploy-actions/internal/notploy"
)

type listResult struct {
	deployments []notploy.Deployment
	err         error
}

type deployResult struct {
	view *notploy.DeploymentView
	err  error
}

type statusResult struct {
	view *notploy.DeploymentView
	err  error
}

// fakeClient replays scripted answers of both API surfaces. Once a script is
// exhausted its last entry is repeated, which is how a test expresses "and then
// it stays like that".
//
// By default the CI/CD API answers like an instance without the CI/CD router
// for a classic API key, so a test that does not script it exercises the
// `auto` fallback — and a test that scripts it exercises the preferred path.
type fakeClient struct {
	// Classic endpoints.
	lists          []listResult
	listCalls      int
	triggerErr     error
	triggerCalls   int
	lastTrigger    triggerCall
	application    *notploy.Application
	applicationErr error
	logs           string
	logsErr        error
	logCalls       int
	lastLogsTail   int

	// CI/CD API.
	cicdDeploys       []deployResult
	cicdDeployCalls   int
	lastDeployRequest notploy.CicdDeployRequest
	cicdStatuses      []statusResult
	cicdStatusCalls   int
	cicdLogs          *notploy.CicdLogsResult
	cicdLogsErr       error
	cicdLogsCalls     int
	cicdCancelErr     error
	cicdCancelCalls   int
	lastCancelledID   string
}

type triggerCall struct {
	applicationID string
	title         string
	description   string
}

// notCicdToken is how a Notploy instance answers `cicd.deploy` for a key that
// is a plain API key: the only 403 that may fall back to the classic endpoints.
var notCicdToken = &notploy.APIError{
	Operation:  "cicd.deploy",
	StatusCode: 403,
	Message:    "This API key is not a CI/CD token. Create one from the project's CI/CD page.",
}

func (f *fakeClient) TriggerDeployment(_ context.Context, applicationID, title, description string) error {
	f.triggerCalls++
	f.lastTrigger = triggerCall{applicationID: applicationID, title: title, description: description}
	return f.triggerErr
}

func (f *fakeClient) ListDeployments(_ context.Context, _ string) ([]notploy.Deployment, error) {
	if len(f.lists) == 0 {
		return nil, nil
	}
	index := f.listCalls
	f.listCalls++
	if index >= len(f.lists) {
		index = len(f.lists) - 1
	}
	result := f.lists[index]
	return result.deployments, result.err
}

func (f *fakeClient) GetApplication(_ context.Context, _ string) (*notploy.Application, error) {
	if f.applicationErr != nil {
		return nil, f.applicationErr
	}
	if f.application == nil {
		return &notploy.Application{}, nil
	}
	return f.application, nil
}

func (f *fakeClient) ReadDeploymentLogs(_ context.Context, _ string, tail int) (string, error) {
	f.logCalls++
	f.lastLogsTail = tail
	return f.logs, f.logsErr
}

func (f *fakeClient) CicdDeploy(_ context.Context, request notploy.CicdDeployRequest) (*notploy.DeploymentView, error) {
	f.cicdDeployCalls++
	f.lastDeployRequest = request
	if len(f.cicdDeploys) == 0 {
		return nil, notCicdToken
	}
	index := f.cicdDeployCalls - 1
	if index >= len(f.cicdDeploys) {
		index = len(f.cicdDeploys) - 1
	}
	result := f.cicdDeploys[index]
	return result.view, result.err
}

func (f *fakeClient) CicdStatus(_ context.Context, _ string) (*notploy.DeploymentView, error) {
	if len(f.cicdStatuses) == 0 {
		return nil, errors.New("no cicd status scripted")
	}
	index := f.cicdStatusCalls
	f.cicdStatusCalls++
	if index >= len(f.cicdStatuses) {
		index = len(f.cicdStatuses) - 1
	}
	result := f.cicdStatuses[index]
	return result.view, result.err
}

func (f *fakeClient) CicdLogs(_ context.Context, _ string, tail int) (*notploy.CicdLogsResult, error) {
	f.cicdLogsCalls++
	f.lastLogsTail = tail
	return f.cicdLogs, f.cicdLogsErr
}

func (f *fakeClient) CicdCancel(_ context.Context, id string) error {
	f.cicdCancelCalls++
	f.lastCancelledID = id
	return f.cicdCancelErr
}

func (f *fakeClient) LogsURL(cicd bool, deploymentID string) string {
	if deploymentID == "" {
		return ""
	}
	if cicd {
		return "https://notploy.example.com/api/cicd/deployments/" + deploymentID + "/logs"
	}
	return "https://notploy.example.com/api/deployment.readLogs?deploymentId=" + deploymentID
}

// deployment builds a deployment stored the way the classic API stores it for
// an action run with no explicit title.
func deployment(id, status string) notploy.Deployment {
	return notploy.Deployment{DeploymentID: id, Status: status, Title: "Manual deployment"}
}

func titled(id, status, title, description string) notploy.Deployment {
	return notploy.Deployment{DeploymentID: id, Status: status, Title: title, Description: description}
}

func cicdView(id, status string) *notploy.DeploymentView {
	return &notploy.DeploymentView{
		DeploymentID:  id,
		Status:        status,
		ApplicationID: "application-1",
		DeploymentURL: "https://notploy.example.com/dashboard/applications/application-1",
	}
}

// recorder captures the status events a run reports.
type recorder struct {
	events []StatusEvent
}

func (r *recorder) attach(params *Params) {
	params.OnStatus = func(_ context.Context, event StatusEvent) {
		r.events = append(r.events, event)
	}
}

func (r *recorder) statuses() []string {
	statuses := make([]string, 0, len(r.events))
	for _, event := range r.events {
		statuses = append(statuses, event.Status)
	}
	return statuses
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunRequiresAnApplication(t *testing.T) {
	if _, err := New(&fakeClient{}, nil).Run(context.Background(), Params{Mode: ModeLegacy}); err == nil {
		t.Fatal("Run() succeeded, want an error for a missing application id")
	}
}

func TestRunRejectsAnUnknownMode(t *testing.T) {
	_, err := New(&fakeClient{}, nil).Run(context.Background(), Params{
		Mode:          "telerun",
		ApplicationID: "application-1",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown api mode") {
		t.Fatalf("error = %v, want an unknown mode error", err)
	}
}

func TestRunWithoutWaitReturnsAfterTheTrigger(t *testing.T) {
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{deployment("old", notploy.StatusDone)}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusRunning), deployment("old", notploy.StatusDone)}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          false,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if client.triggerCalls != 1 {
		t.Fatalf("triggerCalls = %d", client.triggerCalls)
	}
	if result.DeploymentID != "new" {
		t.Errorf("DeploymentID = %q", result.DeploymentID)
	}
	if result.Status != notploy.StatusRunning {
		t.Errorf("Status = %q", result.Status)
	}
	if result.Mode != ModeLegacy {
		t.Errorf("Mode = %q", result.Mode)
	}
}

func TestRunWithoutWaitStillTriggersWhenTheSnapshotFails(t *testing.T) {
	client := &fakeClient{
		lists: []listResult{{err: errors.New("deployment.all is not permitted")}},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          false,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if client.triggerCalls != 1 {
		t.Fatalf("triggerCalls = %d, want the deployment to be triggered anyway", client.triggerCalls)
	}
	if result.Status != StatusTriggered {
		t.Errorf("Status = %q, want %q", result.Status, StatusTriggered)
	}
	if result.DeploymentID != "" {
		t.Errorf("DeploymentID = %q, want empty", result.DeploymentID)
	}
}

func TestRunFailsWhenTheSnapshotFailsAndItMustWait(t *testing.T) {
	client := &fakeClient{lists: []listResult{{err: errors.New("deployment.all is not permitted")}}}

	_, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "list existing deployments") {
		t.Fatalf("error = %q", err)
	}
	if client.triggerCalls != 0 {
		t.Fatalf("triggerCalls = %d, want 0", client.triggerCalls)
	}
}

func TestRunWaitsForSuccess(t *testing.T) {
	old := deployment("old", notploy.StatusDone)
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{old}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusRunning), old}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusRunning), old}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusDone), old}},
		},
	}
	var events recorder

	params := Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       2 * time.Second,
		PollInterval:  time.Millisecond,
	}
	events.attach(&params)

	result, err := New(client, nil).Run(context.Background(), params)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != notploy.StatusDone {
		t.Errorf("Status = %q", result.Status)
	}
	if result.DeploymentID != "new" {
		t.Errorf("DeploymentID = %q", result.DeploymentID)
	}
	if result.Duration <= 0 {
		t.Error("Duration was not measured")
	}
	if want := []string{StatusTriggered, notploy.StatusRunning, notploy.StatusDone}; !equalStrings(events.statuses(), want) {
		t.Errorf("events = %v, want %v", events.statuses(), want)
	}
}

func TestRunFailsWhenTheDeploymentFails(t *testing.T) {
	message := "build failed: Dockerfile not found"
	failed := notploy.Deployment{
		DeploymentID: "new",
		Status:       notploy.StatusError,
		Title:        "Manual deployment",
		ErrorMessage: &message,
	}
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{deployment("old", notploy.StatusDone)}},
			{deployments: []notploy.Deployment{failed}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Fatalf("error = %q", err)
	}
	if result == nil {
		t.Fatal("Run() returned no result on failure")
	}
	if result.Status != notploy.StatusError {
		t.Errorf("Status = %q", result.Status)
	}
	if result.Error != message {
		t.Errorf("Error = %q, want %q", result.Error, message)
	}
	if !result.Failed() {
		t.Error("Failed() = false, want true")
	}
}

func TestRunSucceedsWhenTheDeploymentIsCancelled(t *testing.T) {
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{deployment("old", notploy.StatusDone)}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusCancelled)}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil for a cancelled deployment", err)
	}
	if result.Status != notploy.StatusCancelled {
		t.Errorf("Status = %q", result.Status)
	}
	if !result.Cancelled() {
		t.Error("Cancelled() = false, want true")
	}
}

// The classic API gives no id: the run may only claim the deployment that
// carries its own title and description, so a deployment someone else started
// in between is never mistaken for this one.
func TestRunIgnoresAConcurrentDeploymentWithAnotherTitle(t *testing.T) {
	old := deployment("old", notploy.StatusDone)
	other := titled("other", notploy.StatusRunning, "Deploy api", "someone else")
	ours := titled("ours", notploy.StatusDone, "Deploy web", "this run")

	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{old}},
			{deployments: []notploy.Deployment{other, ours, old}},
			{deployments: []notploy.Deployment{other, ours, old}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Title:         "Deploy web",
		Description:   "this run",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.DeploymentID != "ours" {
		t.Fatalf("DeploymentID = %q, want the deployment matching this run", result.DeploymentID)
	}
}

// Without a match the run must keep waiting rather than claim a deployment that
// is not its own — even when that means timing out.
func TestRunDoesNotClaimADeploymentWithAnotherTitle(t *testing.T) {
	old := deployment("old", notploy.StatusDone)
	other := titled("other", notploy.StatusRunning, "Deploy api", "someone else")

	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{old}},
			{deployments: []notploy.Deployment{other, old}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Title:         "Deploy web",
		Description:   "this run",
		Wait:          true,
		Timeout:       40 * time.Millisecond,
		PollInterval:  5 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want a timeout")
	}
	if !strings.Contains(err.Error(), "did not reach a final state within") {
		t.Fatalf("error = %q", err)
	}
	if result == nil || result.DeploymentID != "" {
		t.Fatalf("result = %+v, want no deployment id claimed", result)
	}
}

// When several indistinguishable candidates appear the action reports the
// ambiguity instead of guessing which one belongs to this run.
func TestRunWarnsWhenTheDeploymentIsAmbiguous(t *testing.T) {
	old := deployment("old", notploy.StatusDone)
	first := titled("first", notploy.StatusRunning, "Deploy web", "same")
	second := titled("second", notploy.StatusRunning, "Deploy web", "same")

	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{old}},
			{deployments: []notploy.Deployment{first, second, old}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Title:         "Deploy web",
		Description:   "same",
		Wait:          true,
		Timeout:       40 * time.Millisecond,
		PollInterval:  5 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want a timeout")
	}
	if result == nil || result.DeploymentID != "" {
		t.Fatalf("result = %+v, want no deployment id claimed", result)
	}
}

// The server substitutes `Manual deployment` when no title is sent; correlation
// must mirror that, otherwise an action run with no title never matches.
func TestRunCorrelatesAnUntitledDeployment(t *testing.T) {
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{}},
			{deployments: []notploy.Deployment{titled("new", notploy.StatusDone, "Manual deployment", "")}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.DeploymentID != "new" {
		t.Fatalf("DeploymentID = %q", result.DeploymentID)
	}
}

func TestRunTimesOut(t *testing.T) {
	old := deployment("old", notploy.StatusDone)
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{old}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusRunning), old}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       40 * time.Millisecond,
		PollInterval:  5 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want a timeout")
	}
	if !strings.Contains(err.Error(), "did not reach a final state within") {
		t.Fatalf("error = %q", err)
	}
	// The caller still learns what the last observed state was.
	if result == nil || result.DeploymentID != "new" || result.Status != notploy.StatusRunning {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunStopsWhenTheContextIsCancelled(t *testing.T) {
	old := deployment("old", notploy.StatusDone)
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{old}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusRunning), old}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	result, err := New(client, nil).Run(ctx, Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Minute,
		PollInterval:  5 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want a cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if result == nil {
		t.Fatal("Run() returned no result on cancellation")
	}
}

func TestRunReportsATriggerFailure(t *testing.T) {
	client := &fakeClient{triggerErr: errors.New("boom")}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "trigger deployment") {
		t.Fatalf("error = %q", err)
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil", result)
	}
}

func TestRunPassesTheGitHubMetadataToNotploy(t *testing.T) {
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{titled("old", notploy.StatusDone, "Deploy #42", "event push")}},
			{deployments: []notploy.Deployment{titled("new", notploy.StatusDone, "Deploy #42", "event push")}},
		},
	}

	if _, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Title:         "Deploy #42",
		Description:   "event push",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if client.lastTrigger != (triggerCall{applicationID: "application-1", title: "Deploy #42", description: "event push"}) {
		t.Fatalf("trigger = %+v", client.lastTrigger)
	}
}

func TestRunToleratesTransientStatusLookupFailures(t *testing.T) {
	old := deployment("old", notploy.StatusDone)
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{old}},
			{err: errors.New("connection reset")},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusRunning), old}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusDone), old}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       2 * time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != notploy.StatusDone {
		t.Fatalf("Status = %q", result.Status)
	}
}

func TestRunGivesUpAfterRepeatedStatusLookupFailures(t *testing.T) {
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{deployment("old", notploy.StatusDone)}},
			{err: errors.New("connection reset")},
		},
	}

	_, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       5 * time.Second,
		PollInterval:  time.Millisecond,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("error = %q", err)
	}
}

func TestRunResolvesTheApplicationURLAndName(t *testing.T) {
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{deployment("old", notploy.StatusDone)}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusDone)}},
		},
		application: &notploy.Application{
			ApplicationID: "application-1",
			Name:          "my-app",
			Domains:       []notploy.Domain{{Host: "my-app.example.com", HTTPS: true, Enabled: boolPtr(true)}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.URL != "https://my-app.example.com" {
		t.Fatalf("URL = %q", result.URL)
	}
	if result.ApplicationName != "my-app" {
		t.Fatalf("ApplicationName = %q", result.ApplicationName)
	}
}

func TestRunSurvivesAnUnresolvableApplicationURL(t *testing.T) {
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{deployment("old", notploy.StatusDone)}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusDone)}},
		},
		applicationErr: errors.New("application.one is not permitted"),
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want the URL lookup to be best effort", err)
	}
	if result.URL != "" {
		t.Fatalf("URL = %q, want empty", result.URL)
	}
}

func TestRunUsesADefaultPollInterval(t *testing.T) {
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{deployment("old", notploy.StatusDone)}},
		},
	}

	// PollInterval is left at zero on purpose: the service must not spin.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	result, err := New(client, nil).Run(ctx, Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Minute,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want the wait to be interrupted")
	}
	// A zero interval would have hammered the fake; a default one keeps the
	// call count low.
	if client.listCalls > 5 {
		t.Fatalf("listCalls = %d, want the default poll interval to be used", client.listCalls)
	}
	if result == nil {
		t.Fatal("Run() returned no result")
	}
}

func TestRunCollectsTheLogOfAFailedDeployment(t *testing.T) {
	message := "build failed"
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{}},
			{deployments: []notploy.Deployment{{DeploymentID: "new", Status: notploy.StatusError, Title: "Manual deployment", ErrorMessage: &message}}},
		},
		logs: "step 1/3: cloning\nstep 2/3: building\nstep 3/3: boom",
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
		LogTail:       50,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want an error")
	}
	if client.logCalls != 1 {
		t.Fatalf("logCalls = %d, want the log of a failed deployment to be read", client.logCalls)
	}
	if !strings.Contains(result.LogExcerpt, "step 3/3: boom") {
		t.Fatalf("LogExcerpt = %q", result.LogExcerpt)
	}
	if result.LogsURL == "" {
		t.Error("LogsURL is empty")
	}
}

func TestRunSkipsTheLogWhenTheDeploymentSucceeds(t *testing.T) {
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusDone)}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeLegacy,
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
		LogTail:       50,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if client.logCalls != 0 {
		t.Fatalf("logCalls = %d, want 0 on success", client.logCalls)
	}
	if result.LogExcerpt != "" {
		t.Fatalf("LogExcerpt = %q, want empty", result.LogExcerpt)
	}
}

func validSource() notploy.SourceMetadata {
	return notploy.SourceMetadata{
		Provider:   "github",
		Repository: "skygenesisenterprise/notploy-actions",
		CommitSHA:  "abcdef1",
		Ref:        "refs/heads/main",
		Workflow:   "Deploy",
		RunID:      "12345",
		RunNumber:  42,
		Actor:      "octocat",
	}
}

// --- CI/CD API ---------------------------------------------------------------

func TestRunUsesTheCicdAPIWhenItIsAvailable(t *testing.T) {
	client := &fakeClient{
		cicdDeploys: []deployResult{{view: cicdView("dpl_1", notploy.StatusRunning)}},
		cicdStatuses: []statusResult{
			{view: cicdView("dpl_1", notploy.StatusRunning)},
			{view: cicdView("dpl_1", notploy.StatusDone)},
		},
	}
	var events recorder

	params := Params{
		Mode:          ModeCicd,
		Source:        validSource(),
		ApplicationID: "application-1",
		Title:         "Deploy web",
		Description:   "this run",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	}
	events.attach(&params)

	result, err := New(client, nil).Run(context.Background(), params)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Mode != ModeCicd {
		t.Errorf("Mode = %q", result.Mode)
	}
	if result.DeploymentID != "dpl_1" {
		t.Errorf("DeploymentID = %q", result.DeploymentID)
	}
	if result.Status != notploy.StatusDone {
		t.Errorf("Status = %q", result.Status)
	}
	if result.DetailsURL == "" {
		t.Error("DetailsURL is empty")
	}
	if client.triggerCalls != 0 || client.listCalls != 0 {
		t.Fatalf("the classic endpoints were used (trigger=%d, list=%d)", client.triggerCalls, client.listCalls)
	}
	if want := []string{notploy.StatusRunning, notploy.StatusDone}; !equalStrings(events.statuses(), want) {
		t.Errorf("events = %v, want %v", events.statuses(), want)
	}
}

func TestRunSendsTheRunMetadataToTheCicdAPI(t *testing.T) {
	client := &fakeClient{
		cicdDeploys:  []deployResult{{view: cicdView("dpl_1", notploy.StatusDone)}},
		cicdStatuses: []statusResult{{view: cicdView("dpl_1", notploy.StatusDone)}},
	}

	_, err := New(client, nil).Run(context.Background(), Params{
		Mode:           ModeCicd,
		ApplicationID:  "application-1",
		Project:        "project-1",
		Environment:    "production",
		Title:          "Deploy web",
		Description:    "this run",
		IdempotencyKey: "gha-12345-1",
		Source: notploy.SourceMetadata{
			Provider:   "github",
			Repository: "skygenesisenterprise/notploy-actions",
			CommitSHA:  "abcdef1",
			Ref:        "refs/heads/main",
			Workflow:   "Deploy",
			RunID:      "12345",
			RunNumber:  42,
			Actor:      "octocat",
		},
		Wait:         true,
		Timeout:      time.Second,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	request := client.lastDeployRequest
	if request.Project != "project-1" || request.Environment != "production" {
		t.Errorf("target = %q/%q", request.Project, request.Environment)
	}
	if request.IdempotencyKey != "gha-12345-1" {
		t.Errorf("IdempotencyKey = %q", request.IdempotencyKey)
	}
	if request.Source.RunID != "12345" || request.Source.CommitSHA != "abcdef1" {
		t.Errorf("source = %+v", request.Source)
	}
}

func TestRunReportsAnIdempotentReplay(t *testing.T) {
	view := cicdView("dpl_1", notploy.StatusDone)
	view.Idempotent = true
	view.Message = "already recorded"
	client := &fakeClient{
		cicdDeploys:  []deployResult{{view: view}},
		cicdStatuses: []statusResult{{view: view}},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeCicd,
		Source:        validSource(),
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Idempotent {
		t.Error("Idempotent = false, want true")
	}
}

func TestRunFailsWhenTheCicdDeploymentFails(t *testing.T) {
	view := cicdView("dpl_1", notploy.StatusError)
	view.ErrorMessage = "build failed: Dockerfile not found"
	client := &fakeClient{
		cicdDeploys:  []deployResult{{view: view}},
		cicdStatuses: []statusResult{{view: view}},
		cicdLogs:     &notploy.CicdLogsResult{DeploymentID: "dpl_1", Logs: "step 1: boom"},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeCicd,
		Source:        validSource(),
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
		LogTail:       100,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "build failed") {
		t.Fatalf("error = %q", err)
	}
	if client.cicdLogsCalls != 1 {
		t.Fatalf("cicdLogsCalls = %d, want the CI/CD log endpoint to be used", client.cicdLogsCalls)
	}
	if !strings.Contains(result.LogExcerpt, "step 1: boom") {
		t.Fatalf("LogExcerpt = %q", result.LogExcerpt)
	}
}

func TestRunSucceedsWhenTheCicdDeploymentIsCancelled(t *testing.T) {
	view := cicdView("dpl_1", notploy.StatusCancelled)
	client := &fakeClient{
		cicdDeploys:  []deployResult{{view: view}},
		cicdStatuses: []statusResult{{view: view}},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeCicd,
		Source:        validSource(),
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil for a cancelled deployment", err)
	}
	if !result.Cancelled() {
		t.Fatalf("Cancelled() = false, status %q", result.Status)
	}
}

func TestRunCancelsTheDeploymentWhenTheWaitGivesUp(t *testing.T) {
	client := &fakeClient{
		cicdDeploys:  []deployResult{{view: cicdView("dpl_1", notploy.StatusRunning)}},
		cicdStatuses: []statusResult{{view: cicdView("dpl_1", notploy.StatusRunning)}},
	}

	_, err := New(client, nil).Run(context.Background(), Params{
		Mode:            ModeCicd,
		Source:          validSource(),
		ApplicationID:   "application-1",
		Wait:            true,
		Timeout:         40 * time.Millisecond,
		PollInterval:    5 * time.Millisecond,
		CancelOnTimeout: true,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want a timeout")
	}
	if client.cicdCancelCalls != 1 {
		t.Fatalf("cicdCancelCalls = %d, want 1", client.cicdCancelCalls)
	}
	if client.lastCancelledID != "dpl_1" {
		t.Fatalf("cancelled = %q", client.lastCancelledID)
	}
}

func TestRunLeavesTheDeploymentRunningWithoutCancelOnTimeout(t *testing.T) {
	client := &fakeClient{
		cicdDeploys:  []deployResult{{view: cicdView("dpl_1", notploy.StatusRunning)}},
		cicdStatuses: []statusResult{{view: cicdView("dpl_1", notploy.StatusRunning)}},
	}

	_, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeCicd,
		Source:        validSource(),
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       40 * time.Millisecond,
		PollInterval:  5 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want a timeout")
	}
	if client.cicdCancelCalls != 0 {
		t.Fatalf("cicdCancelCalls = %d, want 0 without cancel-on-timeout", client.cicdCancelCalls)
	}
}

// In `cicd` mode there is no fallback: a credential without CI/CD access is an
// error the user must see.
func TestRunInCicdModeDoesNotFallBack(t *testing.T) {
	client := &fakeClient{}

	_, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeCicd,
		Source:        validSource(),
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want an error")
	}
	if client.triggerCalls != 0 {
		t.Fatalf("triggerCalls = %d, want 0", client.triggerCalls)
	}
}

// `auto` mode falls back to the classic endpoints exactly when Notploy says the
// credential has no CI/CD API — and only before anything was deployed.
func TestRunFallsBackToTheClassicAPIWhenThereIsNoCicdToken(t *testing.T) {
	client := &fakeClient{
		lists: []listResult{
			{deployments: []notploy.Deployment{}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusDone)}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeAuto,
		Source:        validSource(),
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Mode != ModeLegacy {
		t.Fatalf("Mode = %q, want the classic endpoints", result.Mode)
	}
	if client.triggerCalls != 1 {
		t.Fatalf("triggerCalls = %d", client.triggerCalls)
	}
}

func TestRunFallsBackWhenTheInstanceHasNoCicdRouter(t *testing.T) {
	client := &fakeClient{
		cicdDeploys: []deployResult{{
			err: &notploy.APIError{Operation: "cicd.deploy", StatusCode: 404, Message: "Not found"},
		}},
		lists: []listResult{
			{deployments: []notploy.Deployment{}},
			{deployments: []notploy.Deployment{deployment("new", notploy.StatusDone)}},
		},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeAuto,
		Source:        validSource(),
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Mode != ModeLegacy {
		t.Fatalf("Mode = %q", result.Mode)
	}
}

// A rejected request is a real error: falling back would hide a typo'd target
// or a malformed payload behind a different API surface.
func TestRunDoesNotFallBackOnARejectedRequest(t *testing.T) {
	client := &fakeClient{
		cicdDeploys: []deployResult{{
			err: &notploy.APIError{Operation: "cicd.deploy", StatusCode: 400, Message: "application not found"},
		}},
	}

	_, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeAuto,
		Source:        validSource(),
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       time.Second,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want an error")
	}
	if client.triggerCalls != 0 {
		t.Fatalf("triggerCalls = %d, want 0: a rejected request must not fall back", client.triggerCalls)
	}
}

// Once a deployment exists on the CI/CD API, an error while following it must
// never restart the same intent on the classic endpoints.
func TestRunDoesNotFallBackAfterTheDeploymentWasCreated(t *testing.T) {
	client := &fakeClient{
		cicdDeploys:  []deployResult{{view: cicdView("dpl_1", notploy.StatusRunning)}},
		cicdStatuses: []statusResult{{err: errors.New("connection reset")}},
	}

	result, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeAuto,
		Source:        validSource(),
		ApplicationID: "application-1",
		Wait:          true,
		Timeout:       5 * time.Second,
		PollInterval:  time.Millisecond,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("error = %q", err)
	}
	if client.triggerCalls != 0 {
		t.Fatalf("triggerCalls = %d, want 0: the deployment already exists", client.triggerCalls)
	}
	if result == nil || result.DeploymentID != "dpl_1" {
		t.Fatalf("result = %+v", result)
	}
}

// An incomplete CI context (running outside GitHub Actions) is reported, not
// papered over by the classic endpoints: the user asked for the CI/CD contract.
func TestRunInAutoModeReportsAnIncompleteContext(t *testing.T) {
	client := &fakeClient{
		cicdDeploys: []deployResult{{view: cicdView("dpl_1", notploy.StatusDone)}},
	}

	_, err := New(client, nil).Run(context.Background(), Params{
		Mode:          ModeAuto,
		ApplicationID: "application-1",
		Wait:          false,
	})
	if err == nil {
		t.Fatal("Run() succeeded, want an error for the missing CI context")
	}
	if !strings.Contains(err.Error(), "GITHUB_REPOSITORY") {
		t.Fatalf("error = %q", err)
	}
	if client.triggerCalls != 0 {
		t.Fatalf("triggerCalls = %d, want 0", client.triggerCalls)
	}
}

func boolPtr(value bool) *bool {
	return &value
}
