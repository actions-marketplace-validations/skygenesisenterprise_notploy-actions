package github

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	summaryEnv = "GITHUB_STEP_SUMMARY"

	// summaryExcerptLines and summaryExcerptBytes bound the log excerpt put in
	// the job summary: a summary is a glanceable artifact, not a log store.
	summaryExcerptLines = 60
	summaryExcerptBytes = 8 << 10
)

// Summary is the data rendered into the job summary.
//
// Every field is either public repository metadata or a Notploy identifier;
// credentials are never part of it.
type Summary struct {
	Application string
	Project     string
	Environment string

	Repository string
	Commit     string
	CommitURL  string
	Ref        string
	Actor      string
	Workflow   string
	RunNumber  string
	RunURL     string

	// Mode is the API surface that answered: `cicd` (CI/CD API) or `legacy`.
	Mode string

	DeploymentID  string
	DeploymentURL string
	LogsURL       string
	Status        string
	// URL is the public URL of the application.
	URL string
	// Reused reports that the CI/CD API returned an already recorded
	// deployment instead of creating one (idempotent replay).
	Reused bool

	Duration   time.Duration
	Error      string
	LogExcerpt string
}

// Markdown renders the summary as a table, plus the failure details when there
// are any.
func (s Summary) Markdown() string {
	var b strings.Builder
	b.WriteString("## Notploy deployment\n\n")
	b.WriteString("| Field | Value |\n")
	b.WriteString("| --- | --- |\n")

	writeRow(&b, "Application", s.Application)
	writeRow(&b, "Project", s.Project)
	writeRow(&b, "Environment", s.Environment)

	commit := s.Commit
	if s.CommitURL != "" && s.Commit != "" {
		commit = fmt.Sprintf("[%s](%s)", escapeCell(s.Commit), s.CommitURL)
	}
	writeRow(&b, "Commit", commit)

	writeRow(&b, "Repository", s.Repository)
	writeRow(&b, "Ref", s.Ref)
	writeRow(&b, "Triggered by", s.Actor)
	if workflow := s.workflowLabel(); workflow != "" {
		writeRow(&b, "Workflow", workflow)
	}
	writeRow(&b, "API", modeLabel(s.Mode))

	deployment := s.DeploymentID
	if deployment != "" && s.DeploymentURL != "" {
		deployment = fmt.Sprintf("[%s](%s)", escapeCell(s.DeploymentID), s.DeploymentURL)
	}
	writeRow(&b, "Deployment", deployment)
	writeRow(&b, "Status", statusLabel(s.Status))
	if s.URL != "" {
		writeRow(&b, "App URL", s.URL)
	}
	if s.LogsURL != "" {
		writeRow(&b, "Logs", fmt.Sprintf("[API](%s)", s.LogsURL))
	}
	if s.Duration > 0 {
		writeRow(&b, "Duration", s.Duration.Round(time.Second).String())
	}

	b.WriteString("\n")
	if s.Reused {
		b.WriteString("> **Note:** this run reused the deployment already recorded for it (idempotent replay), it did not queue a new one.\n\n")
	}
	if s.Error != "" {
		b.WriteString("**Error:** ")
		b.WriteString(escapeCell(s.Error))
		b.WriteString("\n\n")
	}
	if excerpt := s.excerpt(); excerpt != "" {
		b.WriteString("<details>\n<summary>Last lines of the deployment log</summary>\n\n")
		b.WriteString(fence(excerpt) + "log\n")
		b.WriteString(excerpt)
		b.WriteString("\n" + fence(excerpt) + "\n\n</details>\n")
	}
	return b.String()
}

// excerpt bounds the log excerpt to what belongs in a summary.
func (s Summary) excerpt() string {
	excerpt := strings.TrimRight(s.LogExcerpt, "\n")
	if excerpt == "" {
		return ""
	}
	lines := strings.Split(excerpt, "\n")
	if len(lines) > summaryExcerptLines {
		lines = lines[len(lines)-summaryExcerptLines:]
		excerpt = strings.Join(lines, "\n")
	}
	if len(excerpt) > summaryExcerptBytes {
		excerpt = "…" + excerpt[len(excerpt)-summaryExcerptBytes:]
		// Drop the possibly torn first line.
		if index := strings.IndexByte(excerpt, '\n'); index >= 0 {
			excerpt = excerpt[index+1:]
		}
	}
	return excerpt
}

// fence picks a markdown fence longer than any backtick run in the content, so
// a log line cannot close the code block early.
func fence(content string) string {
	longest, current := 0, 0
	for _, r := range content {
		if r == '`' {
			current++
			if current > longest {
				longest = current
			}
		} else {
			current = 0
		}
	}
	return strings.Repeat("`", max(longest+1, 3))
}

func (s Summary) workflowLabel() string {
	label := ""
	switch {
	case s.Workflow != "" && s.RunNumber != "":
		label = s.Workflow + " #" + s.RunNumber
	case s.Workflow != "":
		label = s.Workflow
	case s.RunNumber != "":
		label = "#" + s.RunNumber
	}
	if label == "" {
		return ""
	}
	if s.RunURL != "" {
		return fmt.Sprintf("[%s](%s)", escapeCell(label), s.RunURL)
	}
	return label
}

func writeRow(b *strings.Builder, field, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(b, "| %s | %s |\n", escapeCell(field), escapeCell(value))
}

// escapeCell keeps user-controlled values from breaking out of the table cell.
func escapeCell(value string) string {
	replacer := strings.NewReplacer(
		"|", "\\|",
		"\\", "\\\\",
		"\r\n", " ",
		"\r", " ",
		"\n", " ",
	)
	return strings.TrimSpace(replacer.Replace(value))
}

// modeLabel names the API surface for humans.
func modeLabel(mode string) string {
	switch mode {
	case "cicd":
		return "CI/CD API"
	case "legacy":
		return "classic API"
	default:
		return mode
	}
}

// statusLabel renders a Notploy deployment status for humans.
func statusLabel(status string) string {
	switch status {
	case "done":
		return "✅ success"
	case "error":
		return "❌ failed"
	case "cancelled":
		return "⚠️ cancelled"
	case "queued":
		return "🕓 queued"
	case "running":
		return "⏳ running"
	case "":
		return ""
	case "triggered":
		return "🚀 triggered"
	default:
		return status
	}
}

// WriteSummary appends markdown to the job summary ($GITHUB_STEP_SUMMARY).
// It is a no-op outside of GitHub Actions.
func WriteSummary(getenv Getenv, markdown string) error {
	path := getenv(summaryEnv)
	if path == "" || strings.TrimSpace(markdown) == "" {
		return nil
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", summaryEnv, err)
	}
	defer file.Close()

	if _, err := io.WriteString(file, markdown); err != nil {
		return fmt.Errorf("write %s: %w", summaryEnv, err)
	}
	return nil
}
