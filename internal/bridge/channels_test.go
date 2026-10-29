package bridge

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Hive-RSI-Lab/hive-slack/internal/slack"
)

// saved is state.json as the module last wrote it (its live state is the
// Run loop's own).
func (r *rig) saved(t *testing.T) State {
	t.Helper()
	var st State
	raw, _ := os.ReadFile(r.b.statePath())
	json.Unmarshal(raw, &st)
	return st
}

// said is what the module said back in the Hive, its replies to commands.
func (r *rig) said(t *testing.T, n int) string {
	t.Helper()
	p := r.hivePosts(t, n)
	return p[n-1].Text
}

// The lead lays the channels out with "slack:" lines in its own chat: the
// module makes a private channel (or takes one already there), invites the
// person, sets its topic, answers in the lead's thread, and routes by the
// map it keeps; nothing of it goes to Slack as a message.
func TestLeadLaysOutChannels(t *testing.T) {
	h := newFakeHive(t)
	for _, p := range []string{"hive-hwde", "hive-fw", "pll"} {
		h.add("project.created", map[string]string{"name": p, "repo": "o/" + p})
	}
	h.add("run.created", map[string]string{"id": "lead-lead-1", "role": "lead", "name": "lead"})
	h.chats["project:hive-fw"], h.chats["project:hive-hwde"] = true, true
	s := newFakeSlack(t)
	s.chans = []slack.Channel{{ID: "CH0", Name: "h0"}, {ID: "COLD", Name: "old-stuff", IsMember: true}}
	r := startWith(t, s, h, Config{People: []string{"UME", "UTOO"}})

	m := h.post("lead", "", "lead-lead-1", "lead", "slack: channel h0-modules hive-hwde, hive-fw")
	reply := r.said(t, 1)
	if p := h.Posts()[0]; p.Chat != "lead" || p.Thread != m.Thread {
		t.Fatalf("the answer went to %+v, not the lead's thread %s", p, m.Thread)
	}
	s.mu.Lock()
	made, invites := append([]string{}, s.made...), append([]string{}, s.invites...)
	s.mu.Unlock()
	if len(made) != 1 || made[0] != "h0-modules private" {
		t.Fatalf("made %v", made)
	}
	mod := r.saved(t).Channels["h0-modules"]
	if mod.ID == "" || len(mod.Holds) != 2 || !strings.Contains(reply, "is made and reaches the leads of hive-fw, hive-hwde") {
		t.Fatalf("state %+v, reply %q", mod, reply)
	}
	if strings.Join(invites, " ") != mod.ID+"/UME "+mod.ID+"/UTOO" {
		t.Fatalf("invited %v", invites)
	}
	s.mu.Lock()
	topic := s.topics[mod.ID]
	s.mu.Unlock()
	if topic != "Talk to the Hive: reaches the leads of hive-fw, hive-hwde" {
		t.Fatalf("topic %q", topic)
	}

	// A channel already there is taken, joined, not made again.
	h.post("lead", "", "lead-lead-1", "lead", "slack: channel #h0 lead")
	if reply := r.said(t, 2); !strings.HasPrefix(reply, "<#CH0> is reaches the lead") {
		t.Fatalf("reply %q", reply)
	}
	s.mu.Lock()
	if len(s.made) != 1 || !s.chans[0].IsMember {
		t.Fatalf("made %v, h0 %+v", s.made, s.chans[0])
	}
	s.mu.Unlock()
	if got := len(r.s.Posts()); got != 0 {
		t.Fatalf("the lead's commands reached Slack: %+v", r.s.Posts())
	}

	// Routing by the map: the lead's channel, a shared channel's "<p>: …",
	// and anything else there to the lead, saying where it came from.
	r.s.say(t, slack.Message{Channel: "CH0", User: "UME", Text: "morning", TS: "1.1"})
	r.s.say(t, slack.Message{Channel: mod.ID, User: "UME", Text: "hive-fw: does it boot?", TS: "1.2"})
	r.s.say(t, slack.Message{Channel: mod.ID, User: "UME", Text: "who owns this?", TS: "1.3"})
	p := r.hivePosts(t, 5)[2:]
	if p[0].Chat != "lead" || p[1].Chat != "project:hive-fw" || p[1].Text != "does it boot?" || p[2].Chat != "lead" || p[2].Text != "(in #h0-modules) who owns this?" {
		t.Fatalf("the Hive got %+v", p)
	}
	// A project lead's own new thread goes to its channel; the lead's to #h0.
	h.post("project:hive-hwde", "", "lead-project-hive-hwde-1", "lead:hive-hwde", "a board is ready")
	h.post("lead", "", "lead-lead-1", "lead", "a question")
	sp := r.slackPosts(t, 2)
	if sp[0].Channel != mod.ID || sp[1].Channel != "CH0" {
		t.Fatalf("Slack got %+v", sp)
	}

	// Moving a project takes it out of its old channel.
	h.post("lead", "", "lead-lead-1", "lead", "slack: channel proj-pll pll hive-fw")
	if reply := r.said(t, 6); !strings.Contains(reply, "hive-fw left #h0-modules") {
		t.Fatalf("reply %q", reply)
	}
	if hs := r.saved(t).Channels["h0-modules"].Holds; len(hs) != 1 || hs[0] != "hive-hwde" {
		t.Fatalf("h0-modules holds %v", hs)
	}

	// A retired project leaves its channel; the channel is archived once
	// nothing is left in it.
	h.add("project.removed", map[string]string{"name": "pll", "why": "done"})
	h.add("project.removed", map[string]string{"name": "hive-hwde", "why": "made by mistake"})
	waitFor(t, "h0-modules archived", func() bool { _, ok := r.saved(t).Channels["h0-modules"]; return !ok })
	s.mu.Lock()
	archived := strings.Join(s.archived, " ")
	s.mu.Unlock()
	if archived != "h0-modules" {
		t.Fatalf("archived %q", archived)
	}
	if hs := r.saved(t).Channels["proj-pll"].Holds; len(hs) != 1 || hs[0] != "hive-fw" {
		t.Fatalf("proj-pll holds %v", hs)
	}

	// And by hand: a channel the module didn't make.
	h.post("lead", "", "lead-lead-1", "lead", "slack: archive old-stuff")
	if reply := r.said(t, 7); reply != "#old-stuff is archived." {
		t.Fatalf("reply %q", reply)
	}
}

// The person can say the same from Slack; a private channel the bot can't
// see is said plainly, and so is an app without the scopes, in one message.
func TestPersonCommandsAndRefusals(t *testing.T) {
	s := newFakeSlack(t)
	s.hidden = []string{"secret"}
	r := startWith(t, s, nil, Config{})
	r.s.say(t, slack.Message{Channel: "D1", ChannelType: "im", User: "UME", Text: "slack: channel proj-pll pll", TS: "1.1"})
	if sp := r.slackPosts(t, 1)[0]; sp.Thread != "1.1" || !strings.Contains(sp.Text, "is made and reaches pll's lead") || !strings.Contains(sp.Text, "named no project pll") {
		t.Fatalf("Slack got %+v", sp)
	}
	r.s.say(t, slack.Message{Channel: "D1", ChannelType: "im", User: "UME", Text: "slack: channel secret", TS: "1.2"})
	if sp := r.slackPosts(t, 2)[1]; !strings.Contains(sp.Text, "/invite @Hive") {
		t.Fatalf("Slack got %+v", sp)
	}
	r.s.say(t, slack.Message{Channel: "D1", ChannelType: "im", User: "UME", Text: "channels", TS: "1.3"})
	if sp := r.slackPosts(t, 3)[2]; !strings.Contains(sp.Text, "reaches pll's lead") {
		t.Fatalf("Slack got %+v", sp)
	}
	if len(r.h.Posts()) != 0 {
		t.Fatalf("commands reached the Hive: %+v", r.h.Posts())
	}
	s.mu.Lock()
	s.noScope = true
	s.mu.Unlock()
	r.s.say(t, slack.Message{Channel: "D1", ChannelType: "im", User: "UME", Text: "slack: channel h0 lead", TS: "1.4"})
	if sp := r.slackPosts(t, 4)[3]; sp.Text != UpdateApp {
		t.Fatalf("Slack got %+v", sp)
	}
}

// Only the lead and the person steer: an agent's "slack:" line is an
// ordinary message, mirrored as any other.
func TestOnlyLeadsSteer(t *testing.T) {
	h := newFakeHive(t)
	h.add("run.created", map[string]string{"id": "w-1-abcdef", "role": "worker"})
	r := start(t, h, Config{Lead: "CLEAD"})
	h.post("agent:w-1-abcdef", "", "w-1-abcdef", "worker abcde", "slack: archive general")
	if sp := r.slackPosts(t, 1)[0]; sp.Channel != "CLEAD" || !strings.Contains(sp.Text, "slack: archive general") {
		t.Fatalf("Slack got %+v", sp)
	}
	time.Sleep(100 * time.Millisecond)
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if len(h.Posts()) != 0 || len(r.s.archived) != 0 {
		t.Fatalf("a worker steered the channels")
	}
}

// An app made from the old manifest is told once what to do.
func TestOldAppTold(t *testing.T) {
	s := newFakeSlack(t)
	s.scopes = "chat:write,channels:history,groups:history,im:history"
	r := startWith(t, s, nil, Config{})
	if miss := r.b.missingScopes(); len(miss) != len(ManageScopes) {
		t.Fatalf("missing %v", miss)
	}
	s.scopes = strings.Join(append([]string{"chat:write"}, ManageScopes...), ",")
	r2 := startWith(t, s, nil, Config{})
	if miss := r2.b.missingScopes(); len(miss) != 0 {
		t.Fatalf("missing %v", miss)
	}
}
