// Command serves the github-triage agent.
//
// This file is yours; BONNIE never rewrites it. The tree's data lives at its
// default paths — instructions.md is the system prompt, context/ is the
// agent's root for files — and the tools under tools/ are wired by codegen
// into bonnie_gen.go. The triage policy itself lives in internal/triage and
// is configured by triage.yaml.
package main

import (
	"errors"
	"net"
	"net/http"
	"os"

	"github-triage/internal/triage"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/schedule"
)

func main() {
	// The cron and models come from triage.yaml. A config that does not load
	// yet still lets the binary start and print help; the first poll reports
	// the error. Load, not Get: Get caches, and BONNIE has not read .env yet,
	// so its overrides must apply on the later, cached read.
	cron, model, decisionModel := triage.DefaultPollCron, triage.DefaultModel, triage.DefaultDecisionModel
	if c, err := triage.Load(triage.ConfigPath()); err == nil {
		cron, model, decisionModel = c.PollCron, c.Model, c.DecisionModel
	}

	opts := []bonnie.Option{
		bonnie.WithModel(model),
		bonnie.WithDecisionModel(decisionModel),

		// Polling, not the GitHub channel: Prepare diffs the open PRs against
		// the state store and starts one background run per changed PR.
		// Bump Revision when the policy or instructions change meaningfully.
		bonnie.WithSchedule(schedule.Definition{
			Name:     triage.ScheduleName,
			Cron:     cron,
			Revision: "1",
			Prepare:  triage.Prepare,
			CatchUp:  "latest",
			Overlap:  "skip",
		}),

		// A run is done only when its plan is carried out and recorded.
		bonnie.WithCompletionHook(triage.CompletionPolicy()),

		// Background runs have nobody to answer a question, and a waiting run
		// would block every later poll.
		bonnie.WithoutHumanInput(),
	}

	if os.Getenv("TRIAGE_HTTP_API") == "true" {
		// For local work: `bonnie dev`, `chat` and `schedules` need the API.
		// `schedules trigger poll-prs` from this machine runs a poll now;
		// remote callers are refused.
		opts = append(opts, bonnie.WithScheduleTriggerAuthorizer(func(r *http.Request) error {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if ip := net.ParseIP(host); err == nil && ip != nil && ip.IsLoopback() {
				return nil
			}
			return errors.New("schedule triggers are accepted from loopback only")
		}))
	} else {
		// No HTTP API by default: the agent only polls GitHub, so nothing
		// needs to reach it. The schedule clock still runs, and `runs` still
		// reads the journal directly.
		opts = append(opts, bonnie.WithHTTPListenerDisabled())
	}

	bonnie.New(opts...).Serve()
}
