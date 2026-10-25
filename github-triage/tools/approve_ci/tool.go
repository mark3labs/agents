// Package approve_ci approves fork workflow runs the plan judged safe.
package approve_ci

import (
	"context"

	"github-triage/internal/triage"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Tool returns the approve_ci tool bound to the current run.
func Tool(scope bonnie.ToolScope) kit.Tool {
	type input struct {
		PR int `json:"pr" description:"The pull request number."`
	}
	return kit.NewTool("approve_ci",
		"Approve the workflow runs listed in the plan's approve_ci_runs. Refused when the list is empty or new commits arrived.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			return triage.ToolResult(triage.ApproveCI(ctx, scope.RunID, in.PR))
		})
}
