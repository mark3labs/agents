# agents

A collection of useful agents built with [BONNIE](https://go-bonnie.dev), the
Go framework for durable agent runs. Each agent lives in its own directory
with its own README, configuration and build.

| Agent | What it does |
| --- | --- |
| [github-triage](github-triage) | Polls a GitHub repository and triages its pull requests: labels them, closes promotional ones, tracks whose turn it is, approves safe CI runs and flags duplicates. |

## Getting started

Install Go and the [BONNIE CLI](https://go-bonnie.dev/installation), then
follow the README in the agent's directory.

## License

[MIT](LICENSE)
