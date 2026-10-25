// Package finish_triage records a finished triage so the next poll skips
// the PR until something changes.
package finish_triage

import (
	"context"

	"github-triage/internal/triage"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Tool returns the finish_triage tool bound to the current run.
func Tool(scope bonnie.ToolScope) kit.Tool {
	type input struct {
		PR      int    `json:"pr" description:"The pull request number."`
		Summary string `json:"summary" description:"One or two sentences on what was decided and done."`
	}
	return kit.NewTool("finish_triage",
		"Record the triage as finished. Refused until every required action is done.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			return triage.ToolResult(triage.Finish(ctx, scope.RunID, in.PR, in.Summary))
		})
}
