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
	shut  bool   // apps.connections.open refuses, as when Slack is out of reach
	fail  int    // chat.postMessage fails this many more posts (-1: all of them)
	code  string // with this error ("" for a 500 with no body)
	tries int    // chat.postMessage calls, failed ones too
}

type slackPost struct{ Channel, Thread, Text, TS string }

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{}
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
		f.tries++
		if f.fail != 0 {
			if f.fail > 0 {
				f.fail--
			}
			code := f.code
			f.mu.Unlock()
			if code == "" {
				w.WriteHeader(500)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code})
			return
		}
		f.n++
		ts := fmt.Sprintf("2000.%04d", f.n)
		f.posts = append(f.posts, slackPost{b.Channel, b.Thread_TS, b.Text, ts})
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": ts})
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
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("no %s within 5s", what)
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
	s := newFakeSlack(t)
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

// A project's channel reaches that project's lead; "to <project>: …" does
// from anywhere.
func TestProjectLead(t *testing.T) {
	r := start(t, nil, Config{Projects: map[string]string{"hive": "CHIVE"}})
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
	h.post("lead", "", "lead-lead-1", "lead", "after the end")
	r.slackPosts(t, 4) // the module has read the end, which came before this
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
	r := start(t, nil, Config{Projects: map[string]string{"secret": "CSEC"}})
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
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "app_token") || !strings.Contains(err.Error(), "people") {
		t.Fatalf("a bad config read as %v", err)
	}
	writeFile(t, path, `{"app_token":"xapp-1","bot_token":"xoxb-1","people":["U1"]}`)
	if c, err := LoadConfig(path); err != nil || !c.mirror() {
		t.Fatalf("a good config read as %v", err)
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

func (f *fakeSlack) failing(n int, code string) {
	f.mu.Lock()
	f.fail, f.code = n, code
	f.mu.Unlock()
}

// A Slack that fails posts and then recovers gets each of the Hive's lines
// once, in order, in its thread: a mirrored thread's first line and the
// reply that follows it, both held until Slack takes them.
func TestRetriesFailedPosts(t *testing.T) {
	r := start(t, nil, Config{Lead: "CLEAD"}, func(b *Bridge) { b.PostRetry = 20 * time.Millisecond })
	r.s.failing(3, "")
	m := r.h.post("lead", "", "lead-lead-1", "lead", "first")
	r.h.post("lead", m.Thread, "lead-lead-1", "lead", "second")
	r.h.post("lead", m.Thread, "hive", "hive", "third")
	sp := r.slackPosts(t, 3)
	time.Sleep(100 * time.Millisecond)
	sp = r.s.Posts()
	if len(sp) != 3 || !strings.HasSuffix(sp[0].Text, "first") || sp[0].Thread != "" ||
		!strings.HasSuffix(sp[1].Text, "second") || sp[1].Thread != sp[0].TS ||
		sp[2].Text != "third" || sp[2].Thread != sp[0].TS {
		t.Fatalf("Slack got %+v", sp)
	}
	r.s.mu.Lock()
	tries := r.s.tries
	r.s.mu.Unlock()
	if tries != 6 {
		t.Fatalf("%d tries for 3 failures and 3 posts", tries)
	}
	for _, rep := range r.h.Reports() {
		if rep["kind"] == "undelivered" {
			t.Fatalf("a delivered line was reported: %v", rep)
		}
	}
}

// A line Slack still refuses after GiveUp is reported to the Hive as
// undelivered, naming its chat and thread, and the next line goes on.
func TestReportsUndelivered(t *testing.T) {
	r := start(t, nil, Config{Lead: "CLEAD"}, func(b *Bridge) { b.PostRetry, b.GiveUp = 10*time.Millisecond, 150*time.Millisecond })
	r.s.say(t, slack.Message{Channel: "CLEAD", User: "UME", Text: "hi", TS: "1.1"})
	p := r.hivePosts(t, 1)[0]
	r.s.failing(-1, "service_unavailable")
	r.h.post("lead", p.Thread, "lead-lead-1", "lead", "lost")
	var u map[string]any
	waitFor(t, "an undelivered report", func() bool {
		for _, rep := range r.h.Reports() {
			if rep["kind"] == "undelivered" {
				u = rep
				return true
			}
		}
		return false
	})
	if u["chat"] != "lead" || u["thread"] != p.Thread || u["excerpt"] != "lost" ||
		!strings.Contains(u["text"].(string), "service_unavailable") || !strings.Contains(u["text"].(string), p.Thread) {
		t.Fatalf("reported %v", u)
	}
	r.s.failing(0, "")
	r.h.post("lead", p.Thread, "lead-lead-1", "lead", "kept")
	sp := r.slackPosts(t, 1)
	if len(sp) != 1 || !strings.HasSuffix(sp[0].Text, "kept") || sp[0].Thread != "1.1" {
		t.Fatalf("Slack got %+v", sp)
	}
}

// A refusal no retry gets past (the channel is gone) is reported at once.
func TestPermanentRefusalReported(t *testing.T) {
	r := start(t, nil, Config{Lead: "CGONE"}, func(b *Bridge) { b.GiveUp = time.Hour })
	r.s.failing(-1, "channel_not_found")
	r.h.post("lead", "", "lead-lead-1", "lead", "anyone?")
	waitFor(t, "an undelivered report", func() bool {
		for _, rep := range r.h.Reports() {
			if rep["kind"] == "undelivered" && rep["chat"] == "lead" && strings.Contains(rep["error"].(string), "channel_not_found") {
				return true
			}
		}
		return false
	})
	r.s.mu.Lock()
	tries := r.s.tries
	r.s.mu.Unlock()
	if tries != 1 {
		t.Fatalf("a permanent refusal was tried %d times", tries)
	}
}

// Lines still in the outbox when the module stops are posted after a restart.
func TestOutboxSurvivesRestart(t *testing.T) {
	h := newFakeHive(t)
	s := newFakeSlack(t)
	home := t.TempDir()
	cfg := Config{AppToken: "xapp-app", BotToken: "xoxb-bot", SlackAPI: s.srv.URL + "/api/", People: []string{"UME"}, Lead: "CLEAD"}
	b := New(cfg, &hive.Client{API: h.srv.URL, Token: "tok"}, "slack", home)
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s.failing(-1, "")
	h.post("lead", "", "lead-lead-1", "lead", "before the restart")
	if err := b.PollHive(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(b.State().Outbox); n != 1 {
		t.Fatalf("the outbox holds %d lines", n)
	}
	s.failing(0, "")
	b2 := New(cfg, &hive.Client{API: h.srv.URL, Token: "tok"}, "slack", home)
	if err := b2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	b2.st.Outbox[0].Next = time.Time{} // don't wait out the first retry
	if err := b2.PollHive(ctx); err != nil {
		t.Fatal(err)
	}
	if sp := s.Posts(); len(sp) != 1 || !strings.HasSuffix(sp[0].Text, "before the restart") {
		t.Fatalf("after a restart Slack got %+v", sp)
	}
}
