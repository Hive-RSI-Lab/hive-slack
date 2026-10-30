package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hive-RSI-Lab/hive-slack/internal/hive"
	"github.com/Hive-RSI-Lab/hive-slack/internal/slack"
	"github.com/Hive-RSI-Lab/hive-slack/internal/ws"
)

// fakeSlack is Slack's Web API and a Socket Mode WebSocket, on localhost.
type fakeSlack struct {
	srv   *httptest.Server
	mu    sync.Mutex
	posts []slackPost
	acks  []string
	conn  net.Conn
	n     int
	opens int
	// conversations.*: the channels the bot can see, names taken by ones it
	// can't, what it made, invited, archived and set as topics, and the bot
	// token's scopes ("" says none).
	chans    []slack.Channel
	hidden   []string
	made     []string
	invites  []string
	archived []string
	topics   map[string]string
	scopes   string
	noScope  bool // conversations.* answer missing_scope
	shut     bool // apps.connections.open refuses, as when Slack is out of reach
}

type slackPost struct{ Channel, Thread, Text, TS string }

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{topics: map[string]string{}}
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request, want string) bool {
		if r.Header.Get("Authorization") != "Bearer "+want {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
			return false
		}
		return true
	}
	mux.HandleFunc("/api/auth.test", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r, "xoxb-bot") {
			f.mu.Lock()
			if f.scopes != "" {
				w.Header().Set("X-OAuth-Scopes", f.scopes)
			}
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user_id": "UBOT", "team": "Lab"})
		}
	})
	mux.HandleFunc("/api/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r, "xapp-app") {
			f.mu.Lock()
			f.opens++
			shut := f.shut
			f.mu.Unlock()
			if shut {
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "service_unavailable"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/socket?ticket=1"})
		}
	})
	mux.HandleFunc("/api/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r, "xoxb-bot") {
			return
		}
		var b struct{ Channel, Text, Thread_TS string }
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.n++
		ts := fmt.Sprintf("2000.%04d", f.n)
		f.posts = append(f.posts, slackPost{b.Channel, b.Thread_TS, b.Text, ts})
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": ts})
	})
	conv := func(method string, do func(r *http.Request) map[string]any) {
		mux.HandleFunc("/api/conversations."+method, func(w http.ResponseWriter, r *http.Request) {
			if !auth(w, r, "xoxb-bot") {
				return
			}
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_form_data"})
				return
			}
			r.ParseForm()
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.noScope {
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "missing_scope", "needed": "channels:read"})
				return
			}
			out := do(r)
			if out["error"] == nil {
				out["ok"] = true
			}
			json.NewEncoder(w).Encode(out)
		})
	}
	find := func(id string) *slack.Channel {
		for i := range f.chans {
			if f.chans[i].ID == id {
				return &f.chans[i]
			}
		}
		return nil
	}
	conv("list", func(r *http.Request) map[string]any {
		var out []slack.Channel
		for _, c := range f.chans {
			if !c.IsArchived && (!c.IsPrivate || c.IsMember) {
				out = append(out, c)
			}
		}
		return map[string]any{"channels": out}
	})
	conv("create", func(r *http.Request) map[string]any {
		name := r.Form.Get("name")
		for _, c := range f.chans {
			if c.Name == name {
				return map[string]any{"error": "name_taken"}
			}
		}
		for _, h := range f.hidden {
			if h == name {
				return map[string]any{"error": "name_taken"}
			}
		}
		c := slack.Channel{ID: fmt.Sprintf("C%d", len(f.chans)+100), Name: name, IsPrivate: r.Form.Get("is_private") == "true", IsMember: true}
		f.chans = append(f.chans, c)
		f.made = append(f.made, name+map[bool]string{true: " private", false: " public"}[c.IsPrivate])
		return map[string]any{"channel": c}
	})
	conv("join", func(r *http.Request) map[string]any {
		c := find(r.Form.Get("channel"))
		if c == nil || c.IsPrivate {
			return map[string]any{"error": "method_not_supported_for_channel_type"}
		}
		c.IsMember = true
		return map[string]any{}
	})
	conv("invite", func(r *http.Request) map[string]any {
		if c := find(r.Form.Get("channel")); c == nil || !c.IsMember {
			return map[string]any{"error": "not_in_channel"}
		}
		f.invites = append(f.invites, r.Form.Get("channel")+"/"+r.Form.Get("users"))
		return map[string]any{}
	})
	conv("archive", func(r *http.Request) map[string]any {
		c := find(r.Form.Get("channel"))
		if c == nil || !c.IsMember {
			return map[string]any{"error": "not_in_channel"}
		}
		c.IsArchived = true
		f.archived = append(f.archived, c.Name)
		return map[string]any{}
	})
	conv("setTopic", func(r *http.Request) map[string]any {
		f.topics[r.Form.Get("channel")] = r.Form.Get("topic")
		return map[string]any{}
	})
	mux.HandleFunc("/api/users.info", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r, "xoxb-bot") {
			return
		}
		if r.URL.Query().Get("user") != "UANN" {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "missing_scope"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"name": "ann", "profile": map[string]string{"display_name": "Ann Lee"}}})
	})
	mux.HandleFunc("/socket", func(w http.ResponseWriter, r *http.Request) {
		conn, rw, _ := w.(http.Hijacker).Hijack()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + ws.Accept(r.Header.Get("Sec-Websocket-Key")) + "\r\n\r\n")
		rw.Flush()
		ws.WriteFrame(conn, 1, []byte(`{"type":"hello"}`), nil)
		f.mu.Lock()
		f.conn = conn
		f.mu.Unlock()
		br := bufio.NewReader(rw)
		for {
			_, op, p, err := ws.ReadFrame(br)
			if err != nil || op == 8 {
				conn.Close()
				return
			}
			var ack struct{ Envelope_ID string }
			if op == 1 && json.Unmarshal(p, &ack) == nil {
				f.mu.Lock()
				f.acks = append(f.acks, ack.Envelope_ID)
				f.mu.Unlock()
			}
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

var envN int

// say sends a message event over the socket, as Slack does.
func (f *fakeSlack) say(t *testing.T, m slack.Message) {
	t.Helper()
	waitFor(t, "a socket", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.conn != nil })
	m.Type = "message"
	envN++
	raw, _ := json.Marshal(map[string]any{"envelope_id": "env" + strconv.Itoa(envN), "type": "events_api",
		"payload": map[string]any{"event_id": "Ev" + strconv.Itoa(envN), "event": m}})
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ws.WriteFrame(f.conn, 1, raw, nil); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeSlack) Posts() []slackPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]slackPost{}, f.posts...)
}

// fakeHive is the daemon's API as a channel module sees it.
type fakeHive struct {
	srv     *httptest.Server
	mu      sync.Mutex
	events  []hive.Event
	posts   []hive.Message
	chats   map[string]bool
	reports []map[string]any
}

func newFakeHive(t *testing.T) *fakeHive {
	h := &fakeHive{chats: map[string]bool{"lead": true, "project:hive": true}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	mux.HandleFunc("/api/module/report", func(w http.ResponseWriter, r *http.Request) {
		var f map[string]any
		json.NewDecoder(r.Body).Decode(&f)
		h.mu.Lock()
		h.reports = append(h.reports, f)
		h.mu.Unlock()
		w.Write([]byte(`"ok"`))
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		h.mu.Lock()
		out := []hive.Event{}
		for _, e := range h.events {
			if e.Seq > after && len(out) < 2000 {
				out = append(out, e)
			}
		}
		h.mu.Unlock()
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Chat, Thread, Text string }
		json.NewDecoder(r.Body).Decode(&b)
		h.mu.Lock()
		ok := h.chats[b.Chat]
		h.mu.Unlock()
		if !ok && !strings.HasPrefix(b.Chat, "agent:") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		m := h.post(b.Chat, b.Thread, "slack", "slack", b.Text)
		h.mu.Lock()
		h.posts = append(h.posts, m)
		h.mu.Unlock()
		json.NewEncoder(w).Encode(m)
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHive) add(kind string, data any) {
	raw, _ := json.Marshal(data)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, hive.Event{Seq: int64(len(h.events) + 1), Kind: kind, Data: raw})
}

// post is a line in a chat, journaled as chat.posted.
func (h *fakeHive) post(chat, thread, from, name, text string) hive.Message {
	h.mu.Lock()
	id := fmt.Sprintf("m%d", len(h.events)+1)
	h.mu.Unlock()
	if thread == "" {
		thread = id
	}
	m := hive.Message{ID: id, Chat: chat, Thread: thread, From: from, Name: name, Text: text}
	h.add("chat.posted", m)
	return m
}

func (h *fakeHive) Reports() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any{}, h.reports...)
}

func (h *fakeHive) Posts() []hive.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hive.Message{}, h.posts...)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("no %s within 30s", what)
}

type rig struct {
	s      *fakeSlack
	h      *fakeHive
	b      *Bridge
	cancel func()
	done   chan error
}

func start(t *testing.T, h *fakeHive, cfg Config, set ...func(*Bridge)) *rig {
	t.Helper()
	return startWith(t, newFakeSlack(t), h, cfg, set...)
}

func startWith(t *testing.T, s *fakeSlack, h *fakeHive, cfg Config, set ...func(*Bridge)) *rig {
	t.Helper()
	if h == nil {
		h = newFakeHive(t)
	}
	cfg.AppToken, cfg.BotToken, cfg.SlackAPI = "xapp-app", "xoxb-bot", s.srv.URL+"/api/"
	if cfg.People == nil {
		cfg.People = []string{"UME"}
	}
	b := New(cfg, &hive.Client{API: h.srv.URL, Token: "tok"}, "slack", t.TempDir())
	b.Poll = 20 * time.Millisecond
	for _, f := range set {
		f(b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r := &rig{s: s, h: h, b: b, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- b.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-r.done })
	return r
}

func (r *rig) hivePosts(t *testing.T, n int) []hive.Message {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d posts in the Hive", n), func() bool { return len(r.h.Posts()) >= n })
	return r.h.Posts()
}

func (r *rig) slackPosts(t *testing.T, n int) []slackPost {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d posts in Slack", n), func() bool { return len(r.s.Posts()) >= n })
	return r.s.Posts()
}

// A DM reaches the lead, the lead's answer comes back in the DM's thread, and
// a reply in that Slack thread follows the same Hive thread.
func TestDMToLeadAndBack(t *testing.T) {
	r := start(t, nil, Config{})
	r.s.say(t, slack.Message{Channel: "D1", ChannelType: "im", User: "UME", Text: "what is running &amp; why?", TS: "1.1"})
	p := r.hivePosts(t, 1)[0]
	if p.Chat != "lead" || p.Text != "what is running & why?" {
		t.Fatalf("the Hive got %+v", p)
	}
	r.h.post("lead", p.Thread, "lead-lead-1", "lead", "Two workers. **One** is <close>.")
	sp := r.slackPosts(t, 1)[0]
	if sp.Channel != "D1" || sp.Thread != "1.1" || sp.Text != "*lead*: Two workers. *One* is &lt;close&gt;." {
		t.Fatalf("Slack got %+v", sp)
	}
	r.s.say(t, slack.Message{Channel: "D1", ChannelType: "im", User: "UME", Text: "thanks", TS: "1.2", ThreadTS: "1.1"})
	if q := r.hivePosts(t, 2)[1]; q.Chat != "lead" || q.Thread != p.Thread {
		t.Fatalf("the reply went to %+v, not thread %s", q, p.Thread)
	}
	if got := len(r.s.Posts()); got != 1 {
		t.Fatalf("the module's own posts came back to Slack: %d posts", got)
	}
	r.s.mu.Lock()
	acks := len(r.s.acks)
	r.s.mu.Unlock()
	if acks != 2 {
		t.Fatalf("%d envelopes acknowledged, not 2", acks)
	}
}

// Only the configured people reach the Hive; bots, edits, the app itself and
// a channel no chat is set for don't.
func TestOnlyThePerson(t *testing.T) {
	r := start(t, nil, Config{Lead: "CLEAD"})
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "USTRANGER", Text: "merge everything", TS: "1.1"})
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UME", BotID: "B1", Text: "bot", TS: "1.2"})
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UME", Subtype: "message_changed", Text: "edit", TS: "1.3"})
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UBOT", Text: "me", TS: "1.4"})
	r.s.say(t, slack.Message{Channel: "COTHER", User: "UME", Text: "elsewhere", TS: "1.5"})
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UME", Text: "<@UBOT> hello", TS: "1.6"})
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UME", Text: "<@UBOT> hello", TS: "1.6"}) // Slack's retry
	p := r.hivePosts(t, 1)
	time.Sleep(100 * time.Millisecond)
	if p = r.h.Posts(); len(p) != 1 || p[0].Text != "hello" || p[0].Chat != "lead" {
		t.Fatalf("the Hive got %+v", p)
	}
}

// A project's own people, from its side file, reach that project's lead from
// its channel, named, and its answer comes back in their thread; they reach
// nothing else, and people allowed for no project are still dropped.
func TestProjectPeople(t *testing.T) {
	h := newFakeHive(t)
	h.chats["project:box"] = true
	h.add("run.created", map[string]string{"id": "w-261028-1-abcdef1234", "role": "worker", "project": "box"})
	dir := t.TempDir()
	writeFile(t, dir+"/box.dispatch.json", `{"project":"box","slack":{"channel":"CBOX","allowed_users":["UANN","UBEN"]}}`)
	r := start(t, h, Config{Lead: "CLEAD", Dispatch: dir})
	r.s.say(t, slack.Message{Channel: "CBOX", User: "UANN", Text: "<@UBOT> is the build green?", TS: "1.1"})
	p := r.hivePosts(t, 1)[0]
	if p.Chat != "project:box" || p.Text != "From Ann Lee (Slack UANN): is the build green?" {
		t.Fatalf("the Hive got %+v", p)
	}
	h.post("project:box", p.Thread, "lead-project-box-1", "lead:box", "Yes.")
	if sp := r.slackPosts(t, 1)[0]; sp.Channel != "CBOX" || sp.Thread != "1.1" || sp.Text != "*lead:box*: Yes." {
		t.Fatalf("Slack got %+v", sp)
	}
	// Ann's reply follows the thread; Ben, whose name Slack won't give, is named by id.
	r.s.say(t, slack.Message{Channel: "CBOX", User: "UANN", Text: "thanks", TS: "1.2", ThreadTS: "1.1"})
	r.s.say(t, slack.Message{Channel: "CBOX", User: "UBEN", Text: "to abcdef: stop", TS: "1.3"})
	ps := r.hivePosts(t, 3)
	if ps[1].Chat != "project:box" || ps[1].Thread != p.Thread || ps[1].Text != "From Ann Lee (Slack UANN): thanks" {
		t.Fatalf("Ann's reply went in as %+v", ps[1])
	}
	if ps[2].Chat != "project:box" || ps[2].Text != "From Slack user UBEN: to abcdef: stop" {
		t.Fatalf("Ben's message went in as %+v", ps[2])
	}
	// Not their channel, not someone allowed, not their thread: dropped.
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UANN", Text: "lead?", TS: "1.4"})
	r.s.say(t, slack.Message{Channel: "D9", ChannelType: "im", User: "UANN", Text: "dm", TS: "1.5"})
	r.s.say(t, slack.Message{Channel: "CBOX", User: "USTRANGER", Text: "merge it", TS: "1.6"})
	// The person still reaches everything, unnamed, from the project's channel too.
	r.s.say(t, slack.Message{Channel: "CBOX", User: "UME", Text: "to abcdef: how far?", TS: "1.7"})
	ps = r.hivePosts(t, 4)
	if ps[3].Chat != "agent:w-261028-1-abcdef1234" || ps[3].Text != "how far?" {
		t.Fatalf("the person's message went in as %+v", ps[3])
	}
	r.s.say(t, slack.Message{Channel: "CBOX", User: "UANN", Text: "me too", TS: "1.8", ThreadTS: "1.7"})
	if sp := r.slackPosts(t, 2)[1]; sp.Thread != "1.7" || !strings.Contains(sp.Text, "isn't with box's lead") {
		t.Fatalf("Slack got %+v", sp)
	}
	time.Sleep(100 * time.Millisecond)
	if got := r.h.Posts(); len(got) != 4 {
		t.Fatalf("dropped messages reached the Hive: %+v", got[4:])
	}
}

// slack.json's projects give a project's people too, and its channel wins
// over the side file's; a side file the export changes is read again.
func TestProjectsFromSlackJSON(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/box.dispatch.json", `{"slack_channel":"COLD","allowed_users":[{"id":"UANN","name":"Ann"}]}`)
	writeFile(t, dir+"/nochan.dispatch.json", `{"allowed_users":["UX"]}`)
	writeFile(t, dir+"/broken.dispatch.json", `{`)
	b := New(Config{People: []string{"UME"}, Dispatch: dir, Projects: map[string]Project{"box": {Channel: "CBOX", People: []string{"UCAL"}}, "deck": {Channel: "CDECK"}}}, nil, "slack", t.TempDir())
	ps := b.Projects()
	if len(ps) != 2 || ps["box"].Channel != "CBOX" || strings.Join(ps["box"].People, ",") != "UANN,UCAL" || ps["deck"].Channel != "CDECK" {
		t.Fatalf("projects are %+v", ps)
	}
	if b.projectOf("UCAL", "CBOX") != "box" || b.projectOf("UANN", "COLD") != "" || b.projectOf("UANN", "CDECK") != "" {
		t.Fatal("a project's people reach the wrong channel")
	}
	writeFile(t, dir+"/new.dispatch.json", `{"channel_id":"CNEW","users":["UNEW"]}`)
	b.LoadProjects()
	if b.projectOf("UNEW", "CNEW") != "new" {
		t.Fatalf("a new side file isn't read: %+v", b.Projects())
	}
}

func TestReadDispatch(t *testing.T) {
	for in, want := range map[string]Project{
		`{"slack":{"channel":"C1","allowed_users":["U1","U2"]}}`:              {Channel: "C1", People: []string{"U1", "U2"}},
		`{"slack_channel":"C1","slack_users":[{"user_id":"U1"},{"x":1}]}`:     {Channel: "C1", People: []string{"U1"}},
		`{"slack":{"channel_id":" C1 "},"people":["U1"],"channel":"C2"}`:      {Channel: "C1", People: []string{"U1"}},
		`{"id":"box","slack":{"slack_channel_id":"C1","people":["U1","U1"]}}`: {Channel: "C1", People: []string{"U1"}},
	} {
		got, err := parseDispatch([]byte(in))
		if err != nil || got.Channel != want.Channel || strings.Join(got.People, ",") != strings.Join(want.People, ",") {
			t.Errorf("parseDispatch(%s) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	dir := t.TempDir()
	writeFile(t, dir+"/real.dispatch.json", `{"channel":"C1"}`)
	os.Symlink(dir+"/real.dispatch.json", dir+"/link.dispatch.json")
	writeFile(t, dir+"/other.json", `{"channel":"C2"}`)
	ps, bad := ReadDispatch(dir)
	if len(ps) != 1 || ps["real"].Channel != "C1" || len(bad) != 0 {
		t.Fatalf("ReadDispatch = %+v, %v", ps, bad)
	}
}

// A project's channel reaches that project's lead; "to <project>: …" does
// from anywhere.
func TestProjectLead(t *testing.T) {
	r := start(t, nil, Config{Projects: map[string]Project{"hive": {Channel: "CHIVE"}}})
	r.s.say(t, slack.Message{Channel: "CHIVE", User: "UME", Text: "status?", TS: "1.1"})
	r.s.say(t, slack.Message{Channel: "D1", ChannelType: "im", User: "UME", Text: "to hive: and this", TS: "1.2"})
	p := r.hivePosts(t, 2)
	if p[0].Chat != "project:hive" || p[1].Chat != "project:hive" || p[1].Text != "and this" {
		t.Fatalf("the Hive got %+v", p)
	}
	r.h.post("project:hive", p[1].Thread, "lead-project-hive-1", "lead:hive", "fine")
	if sp := r.slackPosts(t, 1)[0]; sp.Channel != "D1" || sp.Thread != "1.2" {
		t.Fatalf("the answer went to %+v", sp)
	}
}

// "to <agent>: …" reaches a running agent, and what it says back comes into
// that Slack thread, though the Hive starts a thread of its own for it.
func TestReachAnAgent(t *testing.T) {
	h := newFakeHive(t)
	h.add("run.created", map[string]string{"id": "w-261028-1-abcdef1234", "role": "worker", "project": "hive"})
	h.add("run.created", map[string]string{"id": "w-261028-2-0000000000", "role": "worker", "project": "deck"})
	h.add("run.status", map[string]string{"id": "w-261028-2-0000000000", "status": "done"})
	r := start(t, h, Config{Lead: "CLEAD"})
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UME", Text: "agents", TS: "1.1"})
	sp := r.slackPosts(t, 1)[0]
	if sp.Thread != "1.1" || !strings.Contains(sp.Text, "w-261028-1-abcdef1234") || strings.Contains(sp.Text, "0000000000") {
		t.Fatalf("agents answered %+v", sp)
	}
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UME", Text: "to nobody: hi", TS: "1.2"})
	if sp := r.slackPosts(t, 2)[1]; !strings.Contains(sp.Text, "no running agent") {
		t.Fatalf("an unknown agent answered %+v", sp)
	}
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UME", Text: "to abcdef: how far along?", TS: "1.3"})
	p := r.hivePosts(t, 1)[0]
	if p.Chat != "agent:w-261028-1-abcdef1234" || p.Text != "how far along?" {
		t.Fatalf("the Hive got %+v", p)
	}
	h.post(p.Chat, "", "w-261028-1-abcdef1234", "worker abcde", "halfway")
	if sp := r.slackPosts(t, 3)[2]; sp.Channel != "CLEAD" || sp.Thread != "1.3" || sp.Text != "*worker abcde*: halfway" {
		t.Fatalf("the agent's answer went to %+v", sp)
	}
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UME", Text: "go on", TS: "1.4", ThreadTS: "1.3"})
	if q := r.hivePosts(t, 2)[1]; q.Chat != p.Chat {
		t.Fatalf("the reply went to %+v", q)
	}
	h.add("run.status", map[string]string{"id": "w-261028-1-abcdef1234", "status": "done"})
	waitFor(t, "the module to read the run's end", func() bool {
		_, running := r.saved(t).Runs["w-261028-1-abcdef1234"]
		return !running
	})
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UME", Text: "still there?", TS: "1.5", ThreadTS: "1.3"})
	if q := r.hivePosts(t, 3)[2]; q.Chat != "lead" {
		t.Fatalf("a thread with an ended agent still went to %+v", q)
	}
}

// What the Hive starts (the lead writing to the person unasked) is posted in
// the lead's channel, and a reply in Slack continues that Hive thread; with
// mirror off it isn't posted. History from before the module started isn't.
func TestMirror(t *testing.T) {
	h := newFakeHive(t)
	h.post("lead", "", "lead-lead-0", "lead", "old news")
	r := start(t, h, Config{Lead: "CLEAD"})
	m := h.post("lead", "", "lead-lead-1", "lead", "A question for you")
	sp := r.slackPosts(t, 1)
	if len(sp) != 1 || sp[0].Channel != "CLEAD" || sp[0].Thread != "" || !strings.Contains(sp[0].Text, "A question") {
		t.Fatalf("Slack got %+v", sp)
	}
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UME", Text: "yes", TS: "1.1", ThreadTS: sp[0].TS})
	if p := r.hivePosts(t, 1)[0]; p.Chat != "lead" || p.Thread != m.Thread {
		t.Fatalf("the reply went to %+v, not thread %s", p, m.Thread)
	}
	h.post("lead", m.Thread, "hive", "hive", "Merged: x")
	if got := r.slackPosts(t, 2)[1]; got.Thread != sp[0].TS || got.Text != "Merged: x" {
		t.Fatalf("Slack got %+v", got)
	}

	h.post("lead", "", "hive", "hive", "Merged: a thread Slack isn't in")
	time.Sleep(150 * time.Millisecond)
	if got := r.s.Posts(); len(got) != 2 {
		t.Fatalf("a notification in a new thread reached Slack: %+v", got[2:])
	}

	off := false
	r2 := start(t, newFakeHive(t), Config{Lead: "CLEAD", Mirror: &off})
	r2.h.post("lead", "", "lead-lead-1", "lead", "unasked")
	time.Sleep(150 * time.Millisecond)
	if got := r2.s.Posts(); len(got) != 0 {
		t.Fatalf("mirror off posted %+v", got)
	}
}

// A Hive that refuses the post (a chat the module isn't granted) says so in
// the Slack thread.
func TestRefusalSaid(t *testing.T) {
	r := start(t, nil, Config{Projects: map[string]Project{"secret": {Channel: "CSEC"}}})
	r.s.say(t, slack.Message{Channel: "CSEC", User: "UME", Text: "hi", TS: "1.1"})
	if sp := r.slackPosts(t, 1)[0]; sp.Thread != "1.1" || !strings.Contains(sp.Text, "didn't take that: not found") {
		t.Fatalf("Slack got %+v", sp)
	}
}

// drop ends the live socket from Slack's side.
func (f *fakeSlack) drop(t *testing.T) {
	t.Helper()
	waitFor(t, "a socket", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.conn != nil })
	f.mu.Lock()
	f.conn.Close()
	f.conn = nil
	f.mu.Unlock()
}

// The Hive hears when the module is live, with the workspace and bot user;
// a connection that drops and comes straight back is not reported, one out
// of reach for longer is, and so is its return.
func TestReportsConnection(t *testing.T) {
	r := start(t, nil, Config{}, func(b *Bridge) { b.Retry, b.ReportAfter = 10*time.Millisecond, 300*time.Millisecond })
	waitFor(t, "a connected report", func() bool { return len(r.h.Reports()) >= 1 })
	if c := r.h.Reports()[0]; c["kind"] != "connected" || c["workspace"] != "Lab" || c["bot_user"] != "UBOT" || !strings.Contains(c["text"].(string), "Lab") {
		t.Fatalf("the first report is %v", c)
	}

	r.s.drop(t)
	waitFor(t, "a new socket", func() bool { r.s.mu.Lock(); defer r.s.mu.Unlock(); return r.s.opens >= 2 && r.s.conn != nil })
	time.Sleep(50 * time.Millisecond)
	if got := r.h.Reports(); len(got) != 1 {
		t.Fatalf("a quick reconnect was reported: %v", got[1:])
	}

	r.s.mu.Lock()
	r.s.shut = true
	r.s.mu.Unlock()
	r.s.drop(t)
	waitFor(t, "a disconnected report", func() bool { return len(r.h.Reports()) >= 2 })
	if d := r.h.Reports()[1]; d["kind"] != "disconnected" || d["workspace"] != "Lab" || !strings.Contains(d["text"].(string), "service_unavailable") {
		t.Fatalf("the outage was reported as %v", d)
	}
	r.s.mu.Lock()
	r.s.shut = false
	r.s.mu.Unlock()
	waitFor(t, "a reconnected report", func() bool { return len(r.h.Reports()) >= 3 })
	if c := r.h.Reports()[2]; c["kind"] != "reconnected" || c["bot_user"] != "UBOT" || len(r.h.Reports()) != 3 {
		t.Fatalf("the return was reported as %v", r.h.Reports()[2:])
	}
	r.s.say(t, slack.Message{Channel: "D1", ChannelType: "im", User: "UME", Text: "back?", TS: "1.1"})
	if p := r.hivePosts(t, 1)[0]; p.Text != "back?" {
		t.Fatalf("after the outage the Hive got %+v", p)
	}
}

// A restart reads state.json and goes on from the cursor and thread map.
func TestRestartKeepsThreads(t *testing.T) {
	h := newFakeHive(t)
	s := newFakeSlack(t)
	home := t.TempDir()
	cfg := Config{AppToken: "xapp-app", BotToken: "xoxb-bot", SlackAPI: s.srv.URL + "/api/", People: []string{"UME"}}
	b := New(cfg, &hive.Client{API: h.srv.URL, Token: "tok"}, "slack", home)
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	b.FromSlack(ctx, slack.Message{Channel: "D1", ChannelType: "im", User: "UME", Text: "hi", TS: "1.1"})
	p := h.Posts()[0]
	h.post("lead", p.Thread, "lead-1", "lead", "hello")
	b2 := New(cfg, &hive.Client{API: h.srv.URL, Token: "tok"}, "slack", home)
	if err := b2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b2.PollHive(ctx); err != nil {
		t.Fatal(err)
	}
	if sp := s.Posts(); len(sp) != 1 || sp[0].Thread != "1.1" {
		t.Fatalf("after a restart Slack got %+v", sp)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/slack.json"
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("no file loaded")
	}
	writeFile(t, path, `{"app_token":"xoxb-wrong","bot_token":"xoxb-1"}`)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "app_token") {
		t.Fatalf("a bad config read as %v", err)
	}
	writeFile(t, path, `{"app_token":"xapp-1","bot_token":"xoxb-1","people":["U1"]}`)
	if c, err := LoadConfig(path); err != nil || !c.mirror() {
		t.Fatalf("a good config read as %v", err)
	}
	writeFile(t, path, `{"app_token":"xapp-1","bot_token":"xoxb-1","projects":{"a":"CA","b":{"channel":"CB","people":["UB"]}}}`)
	c, err := LoadConfig(path)
	if err != nil || c.Projects["a"].Channel != "CA" || c.Projects["b"].Channel != "CB" || len(c.Projects["b"].People) != 1 {
		t.Fatalf("projects read as %+v, %v", c.Projects, err)
	}
}

func TestCleanAndEscape(t *testing.T) {
	for in, want := range map[string]string{
		"<@UBOT> see <https://x.io/a|the doc> and <https://y.io>": "see the doc (https://x.io/a) and https://y.io",
		"ask <@UOTHER> in <#C1|general> &lt;now&gt;":              "ask <@UOTHER> in #general <now>",
	} {
		if got := Clean(in, "UBOT"); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Escape("a < b & **c**"); got != "a &lt; b &amp; *c*" {
		t.Errorf("Escape = %q", got)
	}
	if got := Escape(strings.Repeat("é", maxText+5)); !strings.HasSuffix(got, "chat)") {
		t.Error("a long post isn't clipped")
	}
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}
