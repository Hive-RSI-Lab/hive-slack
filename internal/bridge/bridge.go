// Package bridge carries the person's Slack messages into the Hive's chats
// and the Hive's answers back into the same Slack threads.
//
// In: a message from one of the configured people in the lead's channel or a
// DM with the app goes to the lead's chat; one in a project's channel goes to
// that project's lead; "to <agent>: …" anywhere goes to that running agent.
// A project's own people (slack.json's project entry, or the project's
// <name>.dispatch.json side file) reach only that project's lead, only from
// its channel, and the lead reads who wrote it.
// The Hive posts it tagged with the module's name, so the lead reads it as
// "Module slack: …". A Slack thread is one Hive thread: replies in it follow
// it, and the Hive's answers in that thread come back as replies in it.
//
// Out: the journal's chat.posted lines. One in a Hive thread that started in
// Slack is posted in that Slack thread; one in a thread that didn't (the
// lead's own message to the person, an agent writing to the person) is posted
// as a new message in the chat's channel when mirror is on, and that message
// becomes the thread's home in Slack; the Hive's own notifications are posted
// only in threads Slack is already in.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Hive-RSI-Lab/hive-slack/internal/hive"
	"github.com/Hive-RSI-Lab/hive-slack/internal/slack"
)

// Config is slack.json in the module's HOME: the person's tokens and which
// channel reaches which chat. HOME is .hive/modules/slack/, which every
// agent's view of the Hive has covered, so no agent reads it.
type Config struct {
	AppToken string             `json:"app_token"` // xapp-…, with connections:write
	BotToken string             `json:"bot_token"` // xoxb-…
	People   []string           `json:"people"`    // Slack user ids whose messages are carried in; no one else's are
	Lead     string             `json:"lead"`      // channel id for the lead; a DM with the app reaches the lead too
	Projects map[string]Project `json:"projects"`  // project name -> its lead's channel, and who else may write there
	Dispatch string             `json:"dispatch"`  // folder of <project>.dispatch.json side files; the workspace's projects/ by default
	Mirror   *bool              `json:"mirror"`    // post the Hive's own new threads to Slack; on unless false
	SlackAPI string             `json:"slack_api,omitempty"`
}

// Project is a project's Slack channel and the Slack users, besides the
// global people, who may write to its lead there. In slack.json it is a
// channel id ("C…") or {"channel": "C…", "people": ["U…"]}.
type Project struct {
	Channel string   `json:"channel"`
	People  []string `json:"people,omitempty"`
}

func (p *Project) UnmarshalJSON(raw []byte) error {
	var ch string
	if json.Unmarshal(raw, &ch) == nil {
		*p = Project{Channel: ch}
		return nil
	}
	type plain Project
	return json.Unmarshal(raw, (*plain)(p))
}

// LoadConfig reads and checks slack.json.
func LoadConfig(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("%s: %v", path, err)
	}
	var miss []string
	if !strings.HasPrefix(c.AppToken, "xapp-") {
		miss = append(miss, `app_token (the app-level token, "xapp-…")`)
	}
	if !strings.HasPrefix(c.BotToken, "xoxb-") {
		miss = append(miss, `bot_token (the bot token, "xoxb-…")`)
	}
	if len(miss) > 0 {
		return c, fmt.Errorf("%s needs %s", path, strings.Join(miss, ", "))
	}
	return c, nil
}

func (c Config) mirror() bool { return c.Mirror == nil || *c.Mirror }

// Link is where a Slack thread goes in the Hive.
type Link struct {
	Chat   string `json:"chat"`
	Thread string `json:"thread"`
}

// Run is a run the module has seen in the journal and not seen end.
type Run struct {
	ID      string `json:"id"`
	Role    string `json:"role,omitempty"`
	Name    string `json:"name,omitempty"`
	Project string `json:"project,omitempty"`
	Status  string `json:"status,omitempty"`
}

// State is state.json in HOME: the journal cursor and the thread map, so a
// restart picks up where it left off.
type State struct {
	After   int64             `json:"after"`
	Caught  bool              `json:"caught"`  // read through the journal once, posting nothing
	Threads map[string]Link   `json:"threads"` // "<channel>/<thread ts>" -> Hive chat and thread
	Homes   map[string]string `json:"homes"`   // "<chat>/<thread>" -> "<channel>/<thread ts>"
	Chats   map[string]string `json:"chats"`   // agent chat -> its latest Slack thread
	LeadDM  string            `json:"lead_dm,omitempty"`
	Runs    map[string]Run    `json:"runs"`
	Seen    []string          `json:"seen"` // Slack messages already carried in, newest last
}

// Bridge is the module's program.
type Bridge struct {
	Cfg   Config
	Slack *slack.Client
	Hive  *hive.Client
	Name  string // the module's name, as its posts are tagged
	Home  string // where state.json is kept
	Poll  time.Duration
	Log   func(format string, a ...any)

	st       State
	me       string // the bot's Slack user id
	misses   int
	projects map[string]Project // slack.json's projects over the side files'
	loaded   time.Time
	told     string
	names    map[string]string // Slack user id -> the name the lead reads
}

// New sets a bridge up from its config and HOME, reading state.json.
func New(cfg Config, h *hive.Client, name, home string) *Bridge {
	b := &Bridge{Cfg: cfg, Hive: h, Name: name, Home: home, Poll: time.Second}
	b.Slack = &slack.Client{API: cfg.SlackAPI, AppToken: cfg.AppToken, BotToken: cfg.BotToken}
	b.names = map[string]string{}
	b.LoadProjects()
	raw, err := os.ReadFile(b.statePath())
	if err == nil {
		json.Unmarshal(raw, &b.st)
	}
	b.init()
	return b
}

func (b *Bridge) init() {
	if b.st.Threads == nil {
		b.st.Threads = map[string]Link{}
	}
	if b.st.Homes == nil {
		b.st.Homes = map[string]string{}
	}
	if b.st.Chats == nil {
		b.st.Chats = map[string]string{}
	}
	if b.st.Runs == nil {
		b.st.Runs = map[string]Run{}
	}
}

func (b *Bridge) statePath() string { return filepath.Join(b.Home, "state.json") }

func (b *Bridge) logf(format string, a ...any) {
	if b.Log != nil {
		b.Log(format, a...)
	}
}

func (b *Bridge) save() {
	raw, _ := json.MarshalIndent(b.st, "", " ")
	tmp := b.statePath() + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		os.Rename(tmp, b.statePath())
	}
}

// State is a copy of what the bridge keeps (tests read it).
func (b *Bridge) State() State { return b.st }

// Start learns the bot's own id and, the first time, reads through the
// journal so far without posting any of it: Slack hears from the Hive from
// the moment the module is installed, not its history.
func (b *Bridge) Start(ctx context.Context) error {
	me, team, err := b.Slack.Whoami(ctx)
	if err != nil {
		return err
	}
	b.me = me
	b.logf("connected to Slack workspace %s as %s", team, me)
	b.LoadProjects()
	if !b.st.Caught {
		for {
			evs, err := b.Hive.Events(ctx, b.st.After)
			if err != nil {
				return err
			}
			for _, e := range evs {
				b.st.After = e.Seq
				b.track(e)
			}
			if len(evs) < 2000 {
				break
			}
		}
		b.st.Caught = true
		b.save()
	}
	return nil
}

// Run carries messages both ways until ctx ends or the Hive stops answering.
func (b *Bridge) Run(ctx context.Context) error {
	msgs := make(chan slack.Message, 64)
	go func() {
		wait := time.Second
		for ctx.Err() == nil {
			began := time.Now()
			err := b.Slack.Listen(ctx, func(m slack.Message) {
				select {
				case msgs <- m:
				case <-ctx.Done():
				}
			})
			if ctx.Err() != nil {
				return
			}
			if time.Since(began) > time.Minute {
				wait = time.Second
			}
			if !errors.Is(err, slack.ErrDisconnect) {
				b.logf("slack connection ended: %v; again in %s", err, wait)
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
			if wait < 2*time.Minute {
				wait *= 2
			}
		}
	}()
	tick := time.NewTicker(b.Poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m := <-msgs:
			b.FromSlack(ctx, m)
		case <-tick.C:
			if time.Since(b.loaded) > 30*time.Second {
				b.LoadProjects() // the export may have added or changed a side file
			}
			if err := b.PollHive(ctx); err != nil {
				return err
			}
		}
	}
}

// PollHive reads the journal on from the cursor and carries each chat line out.
func (b *Bridge) PollHive(ctx context.Context) error {
	evs, err := b.Hive.Events(ctx, b.st.After)
	if err != nil {
		// The daemon is gone or restarting; it starts this program again
		// when it is back, so give up after half a minute of silence.
		if b.misses++; b.misses*int(b.Poll/time.Millisecond) > 30000 {
			return fmt.Errorf("the Hive has not answered for 30s: %v", err)
		}
		return nil
	}
	b.misses = 0
	for _, e := range evs {
		b.track(e)
		if e.Kind == "chat.posted" {
			b.fromHive(ctx, e)
		}
		b.st.After = e.Seq
	}
	if len(evs) > 0 {
		b.save()
	}
	return nil
}

// track keeps the list of runs "to <agent>: …" can reach.
func (b *Bridge) track(e hive.Event) {
	var d struct {
		ID, Role, Name, Project, Status string
	}
	switch e.Kind {
	case "run.created":
		json.Unmarshal(e.Data, &d)
		if d.ID != "" {
			b.st.Runs[d.ID] = Run{ID: d.ID, Role: d.Role, Name: d.Name, Project: d.Project, Status: "queued"}
		}
	case "run.status":
		json.Unmarshal(e.Data, &d)
		switch d.Status {
		case "done", "blocked", "stopped":
			delete(b.st.Runs, d.ID)
			b.forget("agent:" + d.ID)
		default:
			r, ok := b.st.Runs[d.ID]
			if !ok {
				r = Run{ID: d.ID}
			}
			r.Status = d.Status
			b.st.Runs[d.ID] = r
		}
	}
}

// forget drops an ended agent's threads from the map.
func (b *Bridge) forget(chat string) {
	delete(b.st.Chats, chat)
	for k, l := range b.st.Threads {
		if l.Chat == chat {
			delete(b.st.Threads, k)
		}
	}
	for k := range b.st.Homes {
		if strings.HasPrefix(k, chat+"/") {
			delete(b.st.Homes, k)
		}
	}
}

func key(a, b string) string { return a + "/" + b }

func split(k string) (string, string) {
	a, b, _ := strings.Cut(k, "/")
	return a, b
}

// FromSlack carries one Slack message into the Hive.
func (b *Bridge) FromSlack(ctx context.Context, m slack.Message) {
	if m.Subtype != "" || m.BotID != "" || m.User == "" || m.User == b.me {
		return // edits, joins, bots and the app's own posts
	}
	// One of the global people steers everything; a project's own people
	// reach that project's lead from its channel, and nothing else.
	own := ""
	if !b.person(m.User) {
		if own = b.projectOf(m.User, m.Channel); own == "" {
			return
		}
	}
	id := key(m.Channel, m.TS)
	for _, s := range b.st.Seen {
		if s == id {
			return // Slack sent it again
		}
	}
	b.st.Seen = append(b.st.Seen, id)
	if len(b.st.Seen) > 200 {
		b.st.Seen = b.st.Seen[len(b.st.Seen)-200:]
	}
	defer b.save()
	if m.ChannelType == "im" {
		b.st.LeadDM = m.Channel
	}
	root := m.ThreadTS
	if root == "" {
		root = m.TS
	}
	text := Clean(m.Text, b.me)
	if text == "" {
		return
	}
	link, known := b.st.Threads[key(m.Channel, root)]
	if own != "" {
		if known && link.Chat != "project:"+own {
			b.reply(ctx, m.Channel, root, "This thread isn't with "+Escape(own)+"'s lead, so I didn't carry that in. Write in the channel to reach "+Escape(own)+"'s lead.")
			return
		}
		if !known && strings.EqualFold(text, "help") {
			b.reply(ctx, m.Channel, root, "Write here to reach "+Escape(own)+"'s lead in the Hive; its answer comes back in your thread. Reply in a thread to keep talking in it.")
			return
		}
		if !known {
			link = Link{Chat: "project:" + own}
		}
		text = b.signed(ctx, m.User, text)
	} else if !known {
		switch strings.ToLower(text) {
		case "help":
			b.reply(ctx, m.Channel, root, b.help())
			return
		case "agents":
			b.reply(ctx, m.Channel, root, b.agents())
			return
		}
		chat, rest, err := b.route(m, text)
		if err != nil {
			b.reply(ctx, m.Channel, root, Escape(err.Error()))
			return
		}
		if chat == "" {
			return // a channel the module wasn't told about
		}
		link, text = Link{Chat: chat}, rest
	}
	msg, err := b.Hive.Post(ctx, link.Chat, link.Thread, text)
	if err != nil {
		b.reply(ctx, m.Channel, root, "The Hive didn't take that: "+Escape(err.Error())+". Send it again once that's sorted.")
		return
	}
	link.Thread = msg.Thread
	b.st.Threads[key(m.Channel, root)] = link
	if _, ok := b.st.Homes[key(link.Chat, link.Thread)]; !ok {
		b.st.Homes[key(link.Chat, link.Thread)] = key(m.Channel, root)
	}
	if strings.HasPrefix(link.Chat, "agent:") {
		b.st.Chats[link.Chat] = key(m.Channel, root)
	}
}

func (b *Bridge) person(user string) bool {
	return slices.Contains(b.Cfg.People, user)
}

// projectOf is the project whose channel this is and whose own people
// include user, or "".
func (b *Bridge) projectOf(user, channel string) string {
	for name, p := range b.projects {
		if p.Channel == channel && slices.Contains(p.People, user) {
			return name
		}
	}
	return ""
}

// signed names who wrote text for the lead, as Slack knows them.
func (b *Bridge) signed(ctx context.Context, user, text string) string {
	name, ok := b.names[user]
	if !ok {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		n, err := b.Slack.UserName(c, user)
		cancel()
		if err != nil {
			b.logf("slack users.info %s: %v", user, err)
		} else {
			b.names[user] = n // a failure is asked again next time
		}
		name = n
	}
	if name == "" {
		return "From Slack user " + user + ": " + text
	}
	return "From " + name + " (Slack " + user + "): " + text
}

// Projects is each project's channel and own people, as the bridge has them.
func (b *Bridge) Projects() map[string]Project { return b.projects }

// LoadProjects reads the side files in Cfg.Dispatch and lays slack.json's
// projects over them: slack.json's channel wins, and the people of both may
// write.
func (b *Bridge) LoadProjects() {
	b.loaded = time.Now()
	ps := map[string]Project{}
	var bad []string
	if b.Cfg.Dispatch != "" {
		ps, bad = ReadDispatch(b.Cfg.Dispatch)
	}
	for name, p := range b.Cfg.Projects {
		q := ps[name]
		if p.Channel != "" {
			q.Channel = p.Channel
		}
		for _, u := range p.People {
			if !slices.Contains(q.People, u) {
				q.People = append(q.People, u)
			}
		}
		ps[name] = q
	}
	for name, p := range ps {
		if p.Channel == "" {
			delete(ps, name)
		}
	}
	b.projects = ps
	var names []string
	for name, p := range ps {
		names = append(names, fmt.Sprintf("%s=%s(%d)", name, p.Channel, len(p.People)))
	}
	sort.Strings(names)
	sort.Strings(bad)
	if msg := strings.Join(names, " ") + " " + strings.Join(bad, "; "); msg != b.told {
		b.told = msg
		b.logf("projects in Slack: %s", strings.Join(names, " "))
		for _, s := range bad {
			b.logf("side file skipped: %s", s)
		}
	}
}

// ReadDispatch reads <project>.dispatch.json in dir: each export's side file
// names its project's Slack channel and the Slack users allowed in it. It
// reads the fields one-hive's export may use: "channel" (or "channel_id",
// "slack_channel", "slack_channel_id") and "allowed_users" (or "users",
// "people", "slack_users", "slack_allowed_users"), at the top or under
// "slack"; a user is an id ("U…") or an object with "id" (or "user_id",
// "slack_id"). Symlinks and files over 1 MB are skipped, and so are files
// that don't read; those come back in bad.
func ReadDispatch(dir string) (map[string]Project, []string) {
	ps := map[string]Project{}
	var bad []string
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ps, nil
	}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".dispatch.json")
		if !ok || name == "" || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() > 1<<20 {
			bad = append(bad, e.Name()+": over 1 MB")
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			bad = append(bad, e.Name()+": "+err.Error())
			continue
		}
		p, err := parseDispatch(raw)
		if err != nil {
			bad = append(bad, e.Name()+": "+err.Error())
			continue
		}
		if p.Channel == "" {
			bad = append(bad, e.Name()+": no Slack channel")
			continue
		}
		ps[name] = p
	}
	return ps, bad
}

func parseDispatch(raw []byte) (Project, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return Project{}, err
	}
	var p Project
	look := func(m map[string]json.RawMessage) {
		for _, k := range []string{"channel", "channel_id", "slack_channel", "slack_channel_id"} {
			var ch string
			if p.Channel == "" && json.Unmarshal(m[k], &ch) == nil {
				p.Channel = strings.TrimSpace(ch)
			}
		}
		for _, k := range []string{"allowed_users", "users", "people", "slack_users", "slack_allowed_users"} {
			var us []json.RawMessage
			if json.Unmarshal(m[k], &us) != nil {
				continue
			}
			for _, u := range us {
				if id := userID(u); id != "" && !slices.Contains(p.People, id) {
					p.People = append(p.People, id)
				}
			}
		}
	}
	var sub map[string]json.RawMessage
	if json.Unmarshal(top["slack"], &sub) == nil {
		look(sub)
	}
	look(top)
	return p, nil
}

func userID(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var o map[string]any
	json.Unmarshal(raw, &o)
	for _, k := range []string{"id", "user_id", "slack_id"} {
		if s, ok := o[k].(string); ok && s != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

var toRe = regexp.MustCompile(`(?is)^to\s+([^\s:]+)\s*:\s*(.+)$`)

// route picks the chat a new Slack conversation goes to, and the text to send.
func (b *Bridge) route(m slack.Message, text string) (string, string, error) {
	if g := toRe.FindStringSubmatch(text); g != nil {
		chat, err := b.resolve(g[1])
		return chat, strings.TrimSpace(g[2]), err
	}
	if m.ChannelType == "im" || (b.Cfg.Lead != "" && m.Channel == b.Cfg.Lead) {
		return "lead", text, nil
	}
	for p, pr := range b.projects {
		if pr.Channel == m.Channel {
			return "project:" + p, text, nil
		}
	}
	return "", "", nil
}

// resolve finds who "to <who>: …" means: the lead, a project's lead, or a
// running agent by id, the id's end, or its name.
func (b *Bridge) resolve(who string) (string, error) {
	lw := strings.ToLower(who)
	if lw == "lead" {
		return "lead", nil
	}
	if p, ok := strings.CutPrefix(lw, "lead:"); ok {
		lw = p
	}
	for p := range b.projects {
		if strings.ToLower(p) == lw {
			return "project:" + p, nil
		}
	}
	var hits []string
	for _, r := range b.st.Runs {
		if r.ID == who || strings.EqualFold(r.Name, who) {
			return "agent:" + r.ID, nil
		}
		if len(who) >= 4 && strings.Contains(r.ID, who) {
			hits = append(hits, r.ID)
		}
	}
	if len(hits) == 1 {
		return "agent:" + hits[0], nil
	}
	if len(hits) > 1 {
		sort.Strings(hits)
		return "", fmt.Errorf("%q is more than one running agent: %s", who, strings.Join(hits, ", "))
	}
	return "", fmt.Errorf("no running agent is %q. Send `agents` for the list.", who)
}

func (b *Bridge) help() string {
	lead := "a DM with me"
	if b.Cfg.Lead != "" {
		lead += " or <#" + b.Cfg.Lead + ">"
	}
	var ps []string
	for p, pr := range b.projects {
		ps = append(ps, fmt.Sprintf("<#%s> reaches %s's lead", pr.Channel, p))
	}
	sort.Strings(ps)
	s := "Write in " + lead + " to reach the Hive's lead."
	if len(ps) > 0 {
		s += " " + strings.Join(ps, "; ") + "."
	}
	return s + "\n`to <agent>: …` reaches a running agent (its id, the end of its id, or its name); `to <project>: …` a project's lead. `agents` lists who is running. Reply in a thread to keep talking in it."
}

func (b *Bridge) agents() string {
	var rs []Run
	for _, r := range b.st.Runs {
		rs = append(rs, r)
	}
	if len(rs) == 0 {
		return "No agent is running."
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
	var lines []string
	for _, r := range rs {
		s := "`" + r.ID + "` " + r.Role
		if r.Name != "" {
			s += " " + r.Name
		}
		if r.Project != "" {
			s += " · " + r.Project
		}
		if r.Status != "" {
			s += " · " + r.Status
		}
		lines = append(lines, s)
	}
	if len(lines) > 60 {
		lines = append(lines[:60], fmt.Sprintf("and %d more", len(rs)-60))
	}
	return strings.Join(lines, "\n")
}

// reply answers in Slack with the module's own text, already in Slack's markup.
func (b *Bridge) reply(ctx context.Context, channel, root, text string) {
	if _, err := b.Slack.Post(ctx, channel, root, text); err != nil {
		b.logf("slack post: %v", err)
	}
}

// fromHive carries one chat.posted line out to Slack.
func (b *Bridge) fromHive(ctx context.Context, e hive.Event) {
	var m hive.Message
	if json.Unmarshal(e.Data, &m) != nil || m.Text == "" {
		return
	}
	if m.From == "person" || m.From == b.Name {
		return // what the person or this module wrote; Slack has it or it came from there
	}
	home, ok := b.st.Homes[key(m.Chat, m.Thread)]
	if !ok && strings.HasPrefix(m.Chat, "agent:") {
		// An agent's message to the person starts a thread of its own; it
		// goes where the person last wrote to that agent.
		home, ok = b.st.Chats[m.Chat]
	}
	text := Escape(m.Text)
	if m.Name != "" && m.From != "hive" {
		text = "*" + Escape(m.Name) + "*: " + text
	}
	if ok {
		ch, ts := split(home)
		if _, err := b.Slack.Post(ctx, ch, ts, text); err != nil {
			b.logf("slack post: %v", err)
		}
		return
	}
	if !b.Cfg.mirror() || m.From == "hive" {
		// The Hive's notifications reach Slack only in a thread Slack is in.
		return
	}
	ch := b.channelFor(m.Chat)
	if ch == "" {
		return
	}
	ts, err := b.Slack.Post(ctx, ch, "", text)
	if err != nil {
		b.logf("slack post: %v", err)
		return
	}
	b.st.Threads[key(ch, ts)] = Link{Chat: m.Chat, Thread: m.Thread}
	b.st.Homes[key(m.Chat, m.Thread)] = key(ch, ts)
	if strings.HasPrefix(m.Chat, "agent:") {
		b.st.Chats[m.Chat] = key(ch, ts)
	}
}

// channelFor is the Slack channel a chat's new threads go to.
func (b *Bridge) channelFor(chat string) string {
	lead := b.Cfg.Lead
	if lead == "" {
		lead = b.st.LeadDM
	}
	switch {
	case chat == "lead":
		return lead
	case strings.HasPrefix(chat, "project:"):
		return b.projects[strings.TrimPrefix(chat, "project:")].Channel
	case strings.HasPrefix(chat, "agent:"):
		return lead // an agent that writes to the person unasked
	}
	return ""
}

var (
	mention = regexp.MustCompile(`<@([A-Z0-9]+)(\|[^>]*)?>`)
	labeled = regexp.MustCompile(`<((?:https?|mailto):[^|>]+)\|([^>]+)>`)
	bare    = regexp.MustCompile(`<((?:https?|mailto):[^>]+)>`)
	chanRef = regexp.MustCompile(`<#[A-Z0-9]+\|([^>]*)>`)
)

// Clean turns Slack's markup back into the person's words: the mention of
// the app goes, links show their address, and &amp; &lt; &gt; are unescaped.
func Clean(text, me string) string {
	text = mention.ReplaceAllStringFunc(text, func(s string) string {
		if g := mention.FindStringSubmatch(s); g[1] == me {
			return ""
		}
		return s
	})
	text = labeled.ReplaceAllString(text, "$2 ($1)")
	text = bare.ReplaceAllString(text, "$1")
	text = chanRef.ReplaceAllString(text, "#$1")
	text = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(text)
	return strings.TrimSpace(text)
}

// maxText keeps a post under Slack's 40,000-character limit.
const maxText = 39000

var bold = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)

// Escape makes the Hive's text safe for Slack: &, < and > escaped, markdown's
// **bold** made Slack's *bold*, and clipped to what one post holds.
func Escape(text string) string {
	text = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
	text = bold.ReplaceAllString(text, "*$1*")
	if utf8.RuneCountInString(text) > maxText {
		r := []rune(text)
		text = string(r[:maxText]) + "… (clipped; the rest is in the Hive's chat)"
	}
	return text
}
