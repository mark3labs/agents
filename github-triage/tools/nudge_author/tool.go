// Package nudge_author posts one reminder to an author the PR waits on.
package nudge_author

import (
	"context"

	"github-triage/internal/triage"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Tool returns the nudge_author tool bound to the current run.
func Tool(scope bonnie.ToolScope) kit.Tool {
	type input struct {
		PR      int    `json:"pr" description:"The pull request number."`
		Message string `json:"message" description:"A short, friendly reminder that names what the maintainers are waiting for. The author is mentioned automatically."`
	}
	return kit.NewTool("nudge_author",
		"Post one reminder to the PR author. Refused unless the plan's nudge is true.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			return triage.ToolResult(triage.Nudge(ctx, scope.RunID, in.PR, in.Message))
		})
}
