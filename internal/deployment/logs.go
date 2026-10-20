package deployment

import (
	"context"
	"strings"

	"github.com/skygenesisenterprise/notploy-actions/internal/notploy"
)

// collectLogs reads the tail of a failed or cancelled deployment's log, prints
// it and keeps it for the job summary.
//
// Each line is printed with a two-space prefix for three reasons: it reads as a
// quotation, it keeps the line from starting with `::` (which the runner would
// otherwise interpret as a workflow command), and it makes the excerpt easy to
// spot in a long log. Fetching the log is best effort — an instance that hides
// it must not turn a diagnosed failure into an internal error.
func (s *Service) collectLogs(ctx context.Context, params Params, result *Result, cicd bool) {
	if params.LogTail <= 0 || result == nil || result.DeploymentID == "" {
		return
	}

	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), logsTimeout)
	defer cancel()

	var (
		text string
		err  error
	)
	if cicd {
		var logs *notploy.CicdLogsResult
		logs, err = s.client.CicdLogs(logCtx, result.DeploymentID, params.LogTail)
		if err == nil && logs != nil {
			text = logs.Logs
		}
	} else {
		text, err = s.client.ReadDeploymentLogs(logCtx, result.DeploymentID, params.LogTail)
	}
	if err != nil {
		s.log.Warningf("Could not read the deployment log: %v", err)
		return
	}

	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return
	}
	result.LogExcerpt = text

	lines := strings.Split(text, "\n")
	s.log.Infof("Last lines of deployment %s:", result.DeploymentID)
	for _, line := range lines {
		s.log.Infof("  %s", strings.TrimRight(line, "\r"))
	}
}
