# hive-slack

The Slack module: talk to the Hive's lead, project leads and any running agent from Slack, as a door like the chat module. Slack threads are chat threads, the answers come back in the thread you wrote in, and it connects over Socket Mode (outbound only, no open port).

- How it works and how to make the Slack app: [docs/README.md](docs/README.md)
- The Slack app's manifest: [slack-app.json](slack-app.json)
- Tests: `go test ./...` (a fake Slack and a fake Hive API)
