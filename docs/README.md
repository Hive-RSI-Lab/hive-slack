# slack: talk to the Hive from Slack

A door, like the Hive's own chat: what you write in Slack reaches the Hive's
lead, a project's lead or any running agent, and their answers come back in the
same Slack thread. A Slack thread is one chat thread in the Hive. You can start a
conversation in Slack and go on with it in the control center, or the other
way round.

It is an optional module, not one of the defaults.

## How it works

```
 Slack                               this machine
 ─────                               ────────────
 you write in a DM / #channel ─┐
                               │  Socket Mode: a WebSocket the module opens
                               └▶ outbound to Slack (no port opens here)
                                    module program (hooks: channel)
                                      POST /api/chat {chat, thread, text}
                                        → the lead reads "Module slack: …"
                                      GET /api/events: chat.posted
 answer, in the same thread  ◀──────  chat.postMessage (bot token)
```

- **In.** A message from you (and only you: slack.json lists the Slack member
  ids it carries in) goes to:
  - the **lead** when it's a DM with the app or in the `lead` channel;
  - a **project's lead** when it's in that project's channel, or starts
    `to <project>: …`;
  - a **running agent** when it starts `to <agent>: …`. The agent can be
    named by its run id, the end of its id (`to abcde: …`) or its name.
    `agents` lists who is running.
  A reply in a Slack thread goes on in the same Hive thread, whatever it says.
  `help` says all of this in Slack.
- **Out.** The module reads the journal's `chat.posted` lines. An answer in a
  thread that started in Slack is posted in that Slack thread. When `mirror` is
  on (the default), something the Hive starts itself is posted as a new message
  in the chat's channel: the lead writing to you, or an agent messaging you.
  The Hive's own notifications ("Merged: …") reach Slack only in a thread
  Slack is already in. Lead's posts go in the `lead` channel, or
  your DM with the app once you've written there. Replying to that message in
  Slack continues the Hive's thread. Only what is posted after the module
  starts goes to Slack, not the history.
- **State.** `state.json` beside slack.json holds the journal cursor, the
  thread map and the runs it has seen, so a restart goes on where it stopped.

## Set it up (your steps)

The Slack app and its tokens are yours. The Hive never makes them.

1. **Make the app.** Go to <https://api.slack.com/apps>, then **Create New App**,
   **From a manifest**, and pick your workspace. Paste
   [`slack-app.json`](../slack-app.json) into the JSON tab (`hive slack manifest`
   prints it too) and create the app. The manifest turns on Socket Mode and asks
   for these bot scopes:
   - `chat:write`: post the answers;
   - `channels:history`, `groups:history`, `im:history`: read the messages in
     the channels you invite it to and your DM with it.
   It subscribes to the `message.channels`, `message.groups` and `message.im`
   events.
2. **Install it.** Under **Install App**, click **Install to Workspace**, then
   copy the **Bot User OAuth Token** (`xoxb-…`).
3. **App-level token.** Under **Basic Information**, go to **App-Level Tokens**
   and click **Generate Token and Scopes**. Name it `socket` and give it the
   scope `connections:write`, then copy the token (`xapp-…`).
4. **Your member id.** In Slack, open your profile, then **⋮**, then **Copy
   member ID** (`U…`).
5. **Channels (optional).** A DM with the app always reaches the lead. For a
   channel instead, or one per project lead, make the channel, run
   `/invite @Hive` in it, and copy its id from the bottom of the channel's
   details (`C…`).
6. **Write slack.json** in the module's own HOME (`hive slack where` prints
   the path; it is `<workspace>/.hive/modules/slack/slack.json`):

   ```json
   {
     "app_token": "xapp-…",
     "bot_token": "xoxb-…",
     "people": ["U…"],
     "lead": "C…",
     "projects": {"hive": "C…", "hive-hwde": "C…"},
     "mirror": true
   }
   ```

   Only `app_token`, `bot_token` and `people` are needed. Then run
   `chmod 600` on it (the module sets that mode too when it starts).
7. **Check it.** `hive slack check` reads slack.json, opens a Socket Mode
   connection with the app token and asks Slack who the bot token is.
8. **Grant it the Hive.** In the control center's Modules screen, grant the
   module `*`: the lead's chat belongs to no project, and only a `*` grant
   reaches it. A grant of some projects reaches only those projects' leads and
   agents.

The program waits until slack.json exists, and reports once (`module.reported`,
kind `waiting`) what is missing, so it can be installed before the app is made.

## What it holds, and who can read it

- **The tokens are read only by this program.** slack.json lives in the
  module's HOME, `.hive/modules/slack/`, a folder that is covered in every
  agent's view of the Hive (`/hive/state`), so no agent's sandbox reads it. The
  tokens are never in the program's environment or its arguments, never sent
  to the Hive's API and never written to its log. As with any module program,
  it runs as you, outside the sandbox (docs/architecture.md, "Trust"). Its
  review is what vouches for it.
- **No port opens.** Socket Mode is a WebSocket the module dials out to
  Slack (through `HTTPS_PROXY` if one is set). Slack never connects in.
- **Only you steer.** A message from anyone not in `people` is dropped, and so
  are bots' posts, edits and the app's own posts. Everyone in a channel you
  invite the app to can read the Hive's answers there, so use a private
  channel or the DM for anything private.
- **Its reach in the Hive** is the `channel` hook: `/api/chat`,
  `/api/events`, `/api/run/stop` and `/api/task/stop` (it uses the first two),
  limited to the projects you grant it.

## Commands

- `hive slack`: the program the daemon runs.
- `hive slack check`: test slack.json against Slack.
- `hive slack manifest`: print the Slack app manifest.
- `hive slack where`: print where slack.json goes.

## Tests

`go test ./...` runs against a fake Slack (its Web API and a Socket Mode
WebSocket on localhost) and a fake Hive API. The tests cover DMs to the lead
and back, threads, project leads, reaching an agent, mirroring, who is let in,
refusals, restarts, and the WebSocket framing.
