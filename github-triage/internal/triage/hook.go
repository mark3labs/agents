package triage

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/bonnie"
)

// CompletionPolicy keeps a triage run going until its plan is carried out:
// inspect_pr was called, every required action is recorded, and
// finish_triage ran. A PR that turned out closed or missing ends without a
// plan; the hook lets that through.
func CompletionPolicy() bonnie.CompletionPolicy {
	return bonnie.CompletionPolicy{
		MaxContinuations: 3,
		NewHook: func(_ context.Context, scope bonnie.RunScope) (bonnie.CompletionHook, error) {
			return func(ctx context.Context, _ bonnie.CompletionCandidate) (bonnie.CompletionFeedback, error) {
				c, err := Get()
				if err != nil {
					return bonnie.CompletionFeedback{}, nil
				}
				st, err := OpenStore(c)
				if err != nil {
					return bonnie.CompletionFeedback{}, err
				}
				p, err := st.Plan(ctx, scope.RunID)
				if err != nil || p == nil {
					return bonnie.CompletionFeedback{}, err
				}
				missing, err := Missing(ctx, st, scope.RunID, p)
				if err != nil {
					return bonnie.CompletionFeedback{}, err
				}
				acts, err := st.Actions(ctx, scope.RunID)
				if err != nil {
					return bonnie.CompletionFeedback{}, err
				}
				if !acts[ActionFinish] {
					missing = append(missing, ActionFinish)
				}
				if len(missing) == 0 {
					return bonnie.CompletionFeedback{}, nil
				}
				return bonnie.CompletionFeedback{ContinueWith: fmt.Sprintf(
					"The triage of PR #%d is not finished. Still required: %s. Call the matching tools, then finish_triage.",
					p.Number, strings.Join(missing, ", "))}, nil
			}, nil
		},
	}
}
