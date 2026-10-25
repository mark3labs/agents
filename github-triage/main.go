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

	"github-triage/internal/triage"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/schedule"
)

func main() {
	// The cron comes from triage.yaml. A config that does not load yet still
	// lets the binary start and print help; the first poll reports the error.
	cron := triage.DefaultPollCron
	if c, err := triage.Get(); err == nil {
		cron = c.PollCron
	}

	bonnie.New(
		bonnie.WithModel("openrouter/deepseek/deepseek-v4.1-flash-1017"),
		bonnie.WithDecisionModel("openrouter/typesafe/jev-1.14"),

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

		// `bonnie schedules trigger poll-prs` from this machine runs a poll
		// now. Remote callers are refused.
		bonnie.WithScheduleTriggerAuthorizer(func(r *http.Request) error {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if ip := net.ParseIP(host); err == nil && ip != nil && ip.IsLoopback() {
				return nil
			}
			return errors.New("schedule triggers are accepted from loopback only")
		}),
	).Serve()
}
