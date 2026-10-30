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

- **In.** A message from one of the global `people` (slack.json lists their
  Slack member ids) goes to:
  - the **lead** when it's a DM with the app or in the `lead` channel;
  - a **project's lead** when it's in that project's channel, or starts
    `to <project>: …`;
  - a **running agent** when it starts `to <agent>: …`. The agent can be
    named by its run id, the end of its id (`to abcde: …`) or its name.
    `agents` lists who is running.
  A reply in a Slack thread goes on in the same Hive thread, whatever it says.
  `help` says all of this in Slack.
- **A project's own people.** Each project can also name Slack users of its
  own, who may write in that project's channel (see
  [Projects' channels and people](#projects-channels-and-people)). A message
  from one of them in that channel goes to that project's lead, and only
  there: `to …:` and `agents` are passed to the lead as they are, a DM or
  another channel is dropped, and a reply in a thread that isn't with that
  project's lead gets a one-line refusal. The lead reads who wrote it:
  `From Ann Lee (Slack U…): …` (the name is from Slack's `users.info`; without
  the `users:read` scope it is `From Slack user U…: …`). The lead's answer comes
  back in the same Slack thread.
- **Out.** The module reads the journal's `chat.posted` lines. An answer in a
  thread that started in Slack is posted in that Slack thread. When `mirror` is
  on (the default), something the Hive starts itself is posted as a new message
  in the chat's channel: the lead writing to you, or an agent messaging you.
  The Hive's own notifications ("Merged: …") reach Slack only in a thread
  Slack is already in. Lead's posts go in the `lead` channel, or
  your DM with the app once you've written there. Replying to that message in
  Slack continues the Hive's thread. Only what is posted after the module
  starts goes to Slack, not the history.
- **Channels.** The module makes and keeps its own channels (below); in a
  channel shared by several projects, `<project>: …` reaches that project's
  lead, and anything else the lead (or the lead's chat, when the channel holds
  it).
- **State.** `state.json` beside slack.json holds the journal cursor, the
  thread map, the runs it has seen and the channel map, so a restart goes on
  where it stopped.

## Channels: the Hive keeps them

The lead lays out the Slack, at once and with no file to edit: a line in its
own chat (`hive agent message --to person --text "slack: …"`) that starts
`slack:` is a command to this module, done there and answered in the same
thread, not sent to Slack. A project's lead can do the same in its chat, and
you can write the same line in Slack or in the control center. No one else
steers it: an agent's `slack:` line is an ordinary message.

- `slack: channel <name> [lead] [project …]` makes the private channel
  `#name`, or takes the one of that name already there (joining a public
  one), invites the people slack.json names, puts the lead's chat (`lead`) and
  those projects' leads in it, and sets its topic to whom it reaches. A
  project already in another channel moves. A channel may hold several
  projects.
- `slack: drop <project …>` takes projects out of their channel.
- `slack: archive <name>` archives `#name`, the module's or any other it can
  see.
- `slack: channels` (or `channels` in Slack) lists the channels and whom each
  reaches.

When the Hive retires a project (`project.removed` in the journal), the project
leaves its channel, and a channel left holding nothing is archived.

A private channel made by someone else is one the app can't see until it is
invited: `/invite @Hive` there first, and the module says so when it meets
one. The `lead` and `projects` fields of slack.json still name channels made by
hand; a channel the module keeps takes precedence.

## Set it up (your steps)

The Slack app and its tokens are yours. The Hive never makes them.

1. **Make the app.** Go to <https://api.slack.com/apps>, then **Create New App**,
   **From a manifest**, and pick your workspace. Paste
   [`slack-app.json`](../slack-app.json) into the JSON tab (`hive slack manifest`
   prints it too) and create the app. The manifest turns on Socket Mode and asks
   for these bot scopes:
   - `chat:write`: post the answers;
   - `channels:history`, `groups:history`, `im:history`: read the messages in
     its channels and your DM with it;
   - `channels:read`, `groups:read`, `channels:manage`, `groups:write`,
     `channels:join`, `channels:write.topic`, `groups:write.topic`: make,
     find, join, archive and title its channels, and invite you to them;
   - `users:read`: name who wrote a project user's message for the lead
     (optional: without it the lead reads the Slack id).

   **An app made from an older manifest** lacks the channel scopes; the
   module reports that once when it starts and in answer to any `slack:`
   command. The one step: on the app's **App Manifest** page at
   <https://api.slack.com/apps>, paste the new manifest (`hive slack manifest`),
   save, then **Install App**, **Reinstall to Workspace**. The tokens stay the
   same, so slack.json doesn't change.
   It subscribes to the `message.channels`, `message.groups` and `message.im`
   events.
2. **Install it.** Under **Install App**, click **Install to Workspace**, then
   copy the **Bot User OAuth Token** (`xoxb-…`).
3. **App-level token.** Under **Basic Information**, go to **App-Level Tokens**
   and click **Generate Token and Scopes**. Name it `socket` and give it the
   scope `connections:write`, then copy the token (`xapp-…`).
4. **Your member id.** In Slack, open your profile, then **⋮**, then **Copy
   member ID** (`U…`).
5. **Channels: nothing to do.** A DM with the app always reaches the lead,
   and the Hive makes its own channels and invites you (see Channels above).
6. **Write slack.json** in the module's own HOME (`hive slack where` prints
   the path; it is `<workspace>/.hive/modules/slack/slack.json`):

   ```json
   {
     "app_token": "xapp-…",
     "bot_token": "xoxb-…",
     "people": ["U…"]
   }
   ```

   That is all it needs. A project's own Slack users (who reach only its
   lead, from its channel) go in `projects`, as
   `"projects": {"box": {"people": ["U…"]}}`, or in its dispatch side file;
   a `channel` there names a channel made by hand, and one the module keeps
   for the project takes precedence. `"mirror": false` keeps the Hive's own
   new threads out of Slack. Then run
   `chmod 600` on it (the module sets that mode too when it starts).
7. **Check it.** `hive slack check` reads slack.json, opens a Socket Mode
   connection with the app token and asks Slack who the bot token is.
8. **Grant it the Hive.** In the control center's Modules screen, grant the
   module `*`: the lead's chat belongs to no project, and only a `*` grant
   reaches it. A grant of some projects reaches only those projects' leads and
   agents.

`hive slack check` also lists each project's channel and its own people, as
the program reads them from slack.json and the side files.

The program waits until slack.json exists, and reports once (`module.reported`,
kind `waiting`) what is missing, so it can be installed before the app is made.
Once Slack's Socket Mode says hello it reports kind `connected`, with
`workspace` and `bot_user`, so the Hive sees it is live. A connection Slack
refreshes, or one that drops and is back within a minute, is only logged; if
Slack stays out of reach for a minute it reports `disconnected` (with the
error) and, when it is back, `reconnected` (with `down_seconds`).

## Projects' channels and people

A project's Slack channel and the users allowed in it come from either place,
per project:

- **slack.json's `projects`.** `"name": "C…"` is just the channel (only the
  global `people` write there); `"name": {"channel": "C…", "people": ["U…"]}`
  lets those users write there too.
- **The project's side file**, `<name>.dispatch.json`, as one-hive's export
  writes it. The program reads every `*.dispatch.json` in the `dispatch`
  folder. That folder is the workspace's `projects/` by default, where an
  exported project's side file sits beside its clone (`projects/<name>/`). It
  is `<HOME>/../../../projects`, as the module's HOME is
  `<workspace>/.hive/modules/slack`. Set `"dispatch": "/some/folder"` in
  slack.json to read them elsewhere (a relative path is from HOME). The file
  is read again every 30 seconds, so a new or changed export takes effect
  without a restart. The program reads the channel from `channel`,
  `channel_id`, `slack_channel` or `slack_channel_id`, and the users from
  `allowed_users`, `users`, `people`, `slack_users` or `slack_allowed_users`,
  either at the top of the file or under `"slack"`. A user is a member id
  (`"U…"`) or an object with `id`, `user_id` or `slack_id`. For example:

  ```json
  {"slack": {"channel": "C0123", "allowed_users": ["U0456", "U0789"]}}
  ```

  A side file with no channel, one that doesn't parse, a symlink or a file
  over 1 MB is skipped, and the module's log says so. Channels must be ids
  (`C…`/`G…`), not names.

When both name a project, slack.json's channel wins and the people of both may
write. The global `people` reach every project's channel, as before.

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
- **Only the people named steer.** A message from anyone not in `people` is
  dropped, unless they are one of a project's own people writing in that
  project's channel, and then it reaches only that project's lead, signed with
  who they are. Bots' posts, edits and the app's own posts are dropped too.
  A side file grants Slack users a project lead's ear, so the `dispatch`
  folder should be one only you and the export write to. Everyone in a channel you
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
and back, threads, project leads, a project's own people (from side files and
slack.json), reaching an agent, mirroring, who is let in, refusals, restarts,
reading side files, the channels the lead lays out (made, taken, moved,
archived, a retired project's), an app without the scopes, and the WebSocket
framing.
