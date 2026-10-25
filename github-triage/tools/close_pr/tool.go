// Package close_pr closes a PR when the plan calls for it.
package close_pr

import (
	"context"

	"github-triage/internal/triage"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Tool returns the close_pr tool bound to the current run.
func Tool(scope bonnie.ToolScope) kit.Tool {
	type input struct {
		PR     int    `json:"pr" description:"The pull request number."`
		Reason string `json:"reason" description:"The plan's close value: promotional or no_linked_issue."`
		Note   string `json:"note,omitempty" description:"Optional short, polite sentence or two placed before the standard close comment. Leave empty for promotional PRs."`
	}
	return kit.NewTool("close_pr",
		"Close the PR with the standard comment for its reason. Refused unless the plan's close field matches reason.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			return triage.ToolResult(triage.ClosePR(ctx, scope.RunID, in.PR, in.Reason, in.Note))
		})
}
