// Package inspect_pr reads a pull request, runs the triage policy and its
// decisions, and returns the plan the other tools enforce.
package inspect_pr

import (
	"context"

	"github-triage/internal/triage"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Tool returns the inspect_pr tool bound to the current run.
func Tool(scope bonnie.ToolScope) kit.Tool {
	type input struct {
		PR int `json:"pr" description:"The pull request number."`
	}
	return kit.NewTool("inspect_pr",
		"Read a pull request, run the triage policy and decisions, and return the plan: the labels to set, "+
			"whether to close it, which CI runs to approve, whether to nudge the author, and the required_actions. "+
			"Call this first, and again if a later tool says the PR changed.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			if in.PR <= 0 {
				return kit.ErrorResult("pr must be a positive number"), nil
			}
			return triage.ToolResult(triage.Inspect(ctx, scope, in.PR))
		})
}
