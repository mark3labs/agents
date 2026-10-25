// Package flag_duplicate comments on a PR the plan found to be a likely
// duplicate.
package flag_duplicate

import (
	"context"

	"github-triage/internal/triage"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Tool returns the flag_duplicate tool bound to the current run.
func Tool(scope bonnie.ToolScope) kit.Tool {
	type input struct {
		PR   int    `json:"pr" description:"The pull request number."`
		Note string `json:"note,omitempty" description:"Optional sentence on what the PRs have in common, placed before the standard comment."`
	}
	return kit.NewTool("flag_duplicate",
		"Post the standard possible-duplicate comment naming the plan's duplicate_of PRs. Refused unless the plan has a duplicate_comment.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			return triage.ToolResult(triage.FlagDuplicate(ctx, scope.RunID, in.PR, in.Note))
		})
}
