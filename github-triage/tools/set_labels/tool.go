// Package set_labels applies the plan's labels plus the agent's type and
// area choices.
package set_labels

import (
	"context"

	"github-triage/internal/triage"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Tool returns the set_labels tool bound to the current run.
func Tool(scope bonnie.ToolScope) kit.Tool {
	type input struct {
		PR    int      `json:"pr" description:"The pull request number."`
		Type  string   `json:"type,omitempty" description:"One of type_label_choices. Empty uses proposed_type."`
		Areas []string `json:"areas,omitempty" description:"Area labels from area_label_choices that fit the change. May be empty."`
	}
	return kit.NewTool("set_labels",
		"Set the PR's labels: the plan's host_labels, one type label and any area labels. Removes managed labels "+
			"that no longer apply; never touches labels outside the configured set.",
		func(ctx context.Context, in input) (kit.ToolOutput, error) {
			return triage.ToolResult(triage.SetLabels(ctx, scope.RunID, in.PR, in.Type, in.Areas))
		})
}
