package bridge

// The module manages its own Slack channels. The lead (or the person, from
// Slack or the control center) writes a line starting "slack:", and the
// module makes or adopts a private channel, puts projects' leads in it,
// invites the people, archives one no longer wanted, and keeps the map of
// which channel holds which chats in state.json. A channel may hold several
// projects, the lead's chat too. A project the Hive retires leaves its
// channel, and a channel left holding nothing is archived.

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/Hive-RSI-Lab/hive-slack/internal/slack"
)

// Channel is a Slack channel the module manages: its id and the chats it
// holds, "lead" for the lead's and a project's name for its lead's.
type Channel struct {
	ID    string   `json:"id"`
	Holds []string `json:"holds"`
}

// LeadChat is the name a channel's Holds uses for the lead's own chat.
const LeadChat = "lead"

// ManageScopes are the bot scopes managing channels takes; the manifest
// asks for them.
var ManageScopes = []string{"channels:read", "groups:read", "channels:manage", "groups:write", "channels:join", "channels:write.topic", "groups:write.topic"}

// UpdateApp is the one step left for the person when the app lacks them.
const UpdateApp = "The Hive's Slack app can't manage channels yet. Update it from the new manifest (`hive slack manifest` prints it) on the app's App Manifest page at api.slack.com/apps, then reinstall it to the workspace."

// missingScopes are the ManageScopes the bot token doesn't have, as Slack
// last said; none when it hasn't said.
func (b *Bridge) missingScopes() []string {
	if b.Slack.Scopes == "" {
		return nil
	}
	have := map[string]bool{}
	for _, s := range strings.Split(b.Slack.Scopes, ",") {
		have[strings.TrimSpace(s)] = true
	}
	var miss []string
	for _, s := range ManageScopes {
		if !have[s] {
			miss = append(miss, s)
		}
	}
	return miss
}

// leadChannel is the channel the lead's chat is in: the managed one, else
// slack.json's lead.
func (b *Bridge) leadChannel() string {
	if name := b.holder(LeadChat); name != "" {
		return b.st.Channels[name].ID
	}
	return b.Cfg.Lead
}

// projectChannel is the channel a project's lead is in: the managed one,
// else the one slack.json or its side file names.
func (b *Bridge) projectChannel(p string) string {
	if name := b.holder(p); name != "" {
		return b.st.Channels[name].ID
	}
	return b.projects[p].Channel
}

// holder is the name of the managed channel holding chat, or "".
func (b *Bridge) holder(chat string) string {
	for name, c := range b.st.Channels {
		for _, h := range c.Holds {
			if h == chat {
				return name
			}
		}
	}
	return ""
}

// byID is the managed channel with Slack id id.
func (b *Bridge) byID(id string) (string, Channel, bool) {
	for name, c := range b.st.Channels {
		if c.ID == id {
			return name, c, true
		}
	}
	return "", Channel{}, false
}

// known says whether p is a project: one the journal named, or one a
// channel holds.
func (b *Bridge) known(p string) bool {
	if b.st.Projects[p] || b.holder(p) != "" {
		return true
	}
	_, ok := b.projects[p]
	return ok
}

var shortTo = regexp.MustCompile(`(?is)^([a-z0-9][a-z0-9_.-]*)\s*:\s*(.+)$`)

// routeManaged picks the chat a new conversation in a managed channel goes
// to. A channel holding one chat reaches it; in a shared one, "<project>: …"
// reaches that project's lead and anything else the lead (or the lead's
// chat, when the channel holds it).
func (b *Bridge) routeManaged(name string, c Channel, text string) (string, string) {
	if len(c.Holds) == 1 {
		return chatOf(c.Holds[0]), text
	}
	if g := shortTo.FindStringSubmatch(text); g != nil {
		for _, h := range c.Holds {
			if strings.EqualFold(h, g[1]) {
				return chatOf(h), strings.TrimSpace(g[2])
			}
		}
	}
	for _, h := range c.Holds {
		if h == LeadChat {
			return "lead", text
		}
	}
	return "lead", "(in #" + name + ") " + text
}

func chatOf(hold string) string {
	if hold == LeadChat {
		return "lead"
	}
	return "project:" + hold
}

// Command reads a "slack: …" line; ok is false for any other text.
func Command(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if len(t) < 6 || !strings.EqualFold(t[:6], "slack:") {
		return "", false
	}
	return strings.TrimSpace(t[6:]), true
}

const usage = "`slack: channel <name> [lead] [project …]` makes the private channel #name (or takes the one there), puts the lead's and those projects' leads in it and invites you; " +
	"`slack: drop <project …>` takes projects out of their channel; `slack: archive <name>` archives #name; `slack: channels` lists them."

var chanName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,79}$`)

// command runs one "slack: …" line and says what came of it, in Slack's markup.
func (b *Bridge) command(ctx context.Context, line string) string {
	f := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\n' })
	if len(f) == 0 {
		return usage
	}
	for i := range f {
		f[i] = strings.ToLower(strings.TrimPrefix(f[i], "#"))
	}
	defer b.save()
	switch f[0] {
	case "channel":
		if len(f) < 2 {
			return usage
		}
		return b.cmdChannel(ctx, f[1], f[2:])
	case "drop":
		if len(f) < 2 {
			return usage
		}
		var out []string
		for _, p := range f[1:] {
			if name := b.unhold(p); name != "" {
				out = append(out, fmt.Sprintf("%s left #%s.", p, name))
				b.topic(ctx, name)
			} else {
				out = append(out, p+" is in no channel.")
			}
		}
		return strings.Join(out, " ")
	case "archive":
		if len(f) != 2 {
			return usage
		}
		return b.cmdArchive(ctx, f[1])
	case "channels", "list":
		return b.channels()
	}
	return usage
}

func (b *Bridge) cmdChannel(ctx context.Context, name string, holds []string) string {
	if !chanName.MatchString(name) {
		return fmt.Sprintf("%q can't be a channel's name: lowercase letters, digits, - and _ only.", name)
	}
	id, made, err := b.ensure(ctx, name)
	if err != nil {
		return b.said(err)
	}
	c := b.st.Channels[name]
	c.ID = id
	b.st.Channels[name] = c
	var moved, unknown []string
	for _, h := range holds {
		if from := b.holder(h); from != "" && from != name {
			b.unhold(h)
			b.topic(ctx, from)
			moved = append(moved, fmt.Sprintf("%s left #%s", h, from))
		}
		if b.holder(h) == name {
			continue
		}
		if h != LeadChat && !b.known(h) {
			unknown = append(unknown, h)
		}
		c = b.st.Channels[name]
		c.Holds = append(c.Holds, h)
		b.st.Channels[name] = c
	}
	var warn []string
	for _, u := range b.invitees(name) {
		if err := b.Slack.Invite(ctx, id, u); err != nil {
			warn = append(warn, fmt.Sprintf("inviting <@%s>: %s", u, b.said(err)))
		}
	}
	b.topic(ctx, name)
	verb := "is"
	if made {
		verb = "is made and"
	}
	s := fmt.Sprintf("<#%s> %s %s.", id, verb, b.holdsText(name))
	if len(moved) > 0 {
		s += " " + strings.Join(moved, "; ") + "."
	}
	if len(unknown) > 0 {
		s += " The Hive has named no project " + strings.Join(unknown, ", ") + " (yet); its messages wait until it does."
	}
	if len(warn) > 0 {
		s += " " + strings.Join(warn, "; ")
	}
	return s
}

// ensure finds the channel called name, or makes it private, and puts the
// bot in it.
func (b *Bridge) ensure(ctx context.Context, name string) (id string, made bool, err error) {
	chs, err := b.Slack.Channels(ctx)
	if err != nil {
		return "", false, err
	}
	for _, ch := range chs {
		if ch.Name != name {
			continue
		}
		if !ch.IsMember {
			if err := b.Slack.Join(ctx, ch.ID); err != nil {
				return "", false, err
			}
		}
		return ch.ID, false, nil
	}
	id, err = b.Slack.Create(ctx, name, false)
	if slack.Code(err) == "name_taken" {
		return "", false, fmt.Errorf("#%s is there, but I can't see it: it is private and I'm not in it, or archived. `/invite @Hive` in it (unarchive it first if need be), then say this again", name)
	}
	return id, err == nil, err
}

func (b *Bridge) cmdArchive(ctx context.Context, name string) string {
	id := b.st.Channels[name].ID
	if id == "" {
		chs, err := b.Slack.Channels(ctx)
		if err != nil {
			return b.said(err)
		}
		for _, ch := range chs {
			if ch.Name == name {
				id = ch.ID
				if !ch.IsMember {
					if err := b.Slack.Join(ctx, id); err != nil {
						return b.said(err)
					}
				}
			}
		}
	}
	if id == "" {
		return fmt.Sprintf("I can't see #%s: there is none, or it is private and I'm not in it (`/invite @Hive` there first).", name)
	}
	if err := b.Slack.Archive(ctx, id); err != nil {
		return b.said(err)
	}
	s := "#" + name + " is archived."
	if hs := b.st.Channels[name].Holds; len(hs) > 0 {
		s += " " + strings.Join(hs, ", ") + " now reach Slack only by `to <project>: …` until a channel holds them again."
	}
	delete(b.st.Channels, name)
	return s
}

// retire takes a project the Hive removed out of its channel, and archives
// the channel when that leaves it holding nothing.
func (b *Bridge) retire(ctx context.Context, p string) {
	name := b.unhold(p)
	if name == "" {
		return
	}
	if len(b.st.Channels[name].Holds) > 0 {
		b.topic(ctx, name)
		return
	}
	text := fmt.Sprintf("archived #%s: its project %s was retired", name, p)
	if err := b.Slack.Archive(ctx, b.st.Channels[name].ID); err != nil {
		text = fmt.Sprintf("couldn't archive #%s after project %s was retired: %s", name, p, b.said(err))
	} else {
		delete(b.st.Channels, name)
	}
	b.logf("%s", text)
	if b.Hive != nil {
		b.Hive.Report(ctx, map[string]any{"kind": "channel", "text": text})
	}
}

// unhold takes chat out of its channel and returns that channel's name.
func (b *Bridge) unhold(chat string) string {
	name := b.holder(chat)
	if name == "" {
		return ""
	}
	c := b.st.Channels[name]
	var keep []string
	for _, h := range c.Holds {
		if h != chat {
			keep = append(keep, h)
		}
	}
	c.Holds = keep
	b.st.Channels[name] = c
	return name
}

// invitees are who a channel is for: the people, and the own users of the
// projects it holds.
func (b *Bridge) invitees(name string) []string {
	us := append([]string{}, b.Cfg.People...)
	for _, h := range b.st.Channels[name].Holds {
		for _, u := range b.projects[h].People {
			if !slices.Contains(us, u) {
				us = append(us, u)
			}
		}
	}
	return us
}

// topic sets a managed channel's topic to who it reaches.
func (b *Bridge) topic(ctx context.Context, name string) {
	c, ok := b.st.Channels[name]
	if !ok || c.ID == "" {
		return
	}
	t := "Talk to the Hive: " + b.holdsText(name)
	if err := b.Slack.Topic(ctx, c.ID, t); err != nil {
		b.logf("topic of #%s: %v", name, err)
	}
}

// holdsText says whom a channel reaches.
func (b *Bridge) holdsText(name string) string {
	var who []string
	lead := false
	for _, h := range b.st.Channels[name].Holds {
		if h == LeadChat {
			lead = true
		} else {
			who = append(who, h)
		}
	}
	sort.Strings(who)
	switch {
	case lead && len(who) > 0:
		return "reaches the lead and the leads of " + strings.Join(who, ", ")
	case lead:
		return "reaches the lead"
	case len(who) == 1:
		return "reaches " + who[0] + "'s lead"
	case len(who) > 1:
		return "reaches the leads of " + strings.Join(who, ", ")
	}
	return "reaches no one yet"
}

// channels lists the managed channels, and slack.json's own.
func (b *Bridge) channels() string {
	var lines []string
	for name, c := range b.st.Channels {
		lines = append(lines, fmt.Sprintf("<#%s> %s", c.ID, b.holdsText(name)))
	}
	sort.Strings(lines)
	if b.Cfg.Lead != "" && b.holder(LeadChat) == "" {
		lines = append(lines, fmt.Sprintf("<#%s> reaches the lead (slack.json)", b.Cfg.Lead))
	}
	var ps []string
	for p, pr := range b.projects {
		if b.holder(p) == "" && pr.Channel != "" {
			ps = append(ps, fmt.Sprintf("<#%s> reaches %s's lead (slack.json or its side file)", pr.Channel, p))
		}
	}
	sort.Strings(ps)
	lines = append(lines, ps...)
	if len(lines) == 0 {
		return "No channels yet: a DM with me reaches the lead. " + usage
	}
	return strings.Join(lines, "\n")
}

// said is an error as the person or the lead reads it.
func (b *Bridge) said(err error) string {
	if slack.Code(err) == "missing_scope" {
		return UpdateApp
	}
	return Escape(err.Error())
}
