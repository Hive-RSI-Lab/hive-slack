// Package slack is the slice of Slack the module uses: Socket Mode for the
// person's messages in (an outbound WebSocket, so no port opens on this
// machine), and chat.postMessage for the Hive's answers out.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Hive-RSI-Lab/hive-slack/internal/ws"
)

// Client talks to one Slack app.
type Client struct {
	API      string // Web API base, "https://slack.com/api/" by default
	AppToken string // xapp-…: opens Socket Mode connections
	BotToken string // xoxb-…: posts messages
	HTTP     *http.Client
	// Scopes is the bot token's scopes, as Slack last said (X-OAuth-Scopes).
	Scopes string
	// Proxy picks the proxy for the WebSocket (http.ProxyFromEnvironment);
	// nil dials directly.
	Proxy func(*http.Request) (*url.URL, error)
}

// Message is a message event Slack sends: the person writing in a channel,
// a DM or a thread.
type Message struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"` // channel | group | im | mpim
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
}

func (c *Client) base() string {
	if c.API != "" {
		return strings.TrimSuffix(c.API, "/") + "/"
	}
	return "https://slack.com/api/"
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// call POSTs a Web API method and decodes its answer into out; Slack's
// {"ok": false, "error": …} comes back as an *Error. A url.Values body is
// sent as a form (the read methods take no JSON), anything else as JSON.
func (c *Client) call(ctx context.Context, token, method string, body, out any) error {
	var raw []byte
	ctype := "application/json; charset=utf-8"
	if f, ok := body.(url.Values); ok {
		raw, ctype = []byte(f.Encode()), "application/x-www-form-urlencoded"
	} else {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+method, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", ctype)
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimited{After: resp.Header.Get("Retry-After")}
	}
	if sc := resp.Header.Get("X-OAuth-Scopes"); sc != "" && token == c.BotToken {
		c.Scopes = sc
	}
	var res struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Needed string `json:"needed"`
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return err
	}
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		return fmt.Errorf("slack %s: %s: %v", method, resp.Status, err)
	}
	if !res.OK {
		return &Error{Method: method, Code: res.Error, Needed: res.Needed}
	}
	if out != nil {
		return json.Unmarshal(buf.Bytes(), out)
	}
	return nil
}

// Error is a Web API method's refusal: Code is Slack's error string
// ("missing_scope", "name_taken", …) and Needed the scope it lacked.
type Error struct{ Method, Code, Needed string }

func (e *Error) Error() string {
	if e.Needed != "" {
		return fmt.Sprintf("slack %s: %s (needs %s)", e.Method, e.Code, e.Needed)
	}
	return fmt.Sprintf("slack %s: %s", e.Method, e.Code)
}

// Code is Slack's error string for err, or "" when it isn't Slack's refusal.
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// RateLimited is Slack's 429.
type RateLimited struct{ After string }

func (r *RateLimited) Error() string { return "slack: rate limited, retry after " + r.After + "s" }

// Whoami is the bot's own user id (auth.test), so its own posts and the
// mention of it are told apart from the person's words.
func (c *Client) Whoami(ctx context.Context) (user, team string, err error) {
	var out struct {
		UserID string `json:"user_id"`
		Team   string `json:"team"`
	}
	err = c.call(ctx, c.BotToken, "auth.test", map[string]string{}, &out)
	return out.UserID, out.Team, err
}

// Post writes text in channel, in the thread whose root is threadTS ("" for
// a new message), and returns the new message's ts.
func (c *Client) Post(ctx context.Context, channel, threadTS, text string) (string, error) {
	body := map[string]any{"channel": channel, "text": text, "unfurl_links": false, "unfurl_media": false}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	var out struct {
		TS string `json:"ts"`
	}
	err := c.call(ctx, c.BotToken, "chat.postMessage", body, &out)
	return out.TS, err
}

// Open asks for a Socket Mode address with the app-level token.
func (c *Client) Open(ctx context.Context) (string, error) {
	var open struct {
		URL string `json:"url"`
	}
	err := c.call(ctx, c.AppToken, "apps.connections.open", map[string]string{}, &open)
	return open.URL, err
}

// ErrDisconnect is Slack asking for a fresh connection (a refresh, or the
// app being turned off); the caller opens a new one.
var ErrDisconnect = errors.New("slack: socket mode disconnect")

// Listen opens one Socket Mode connection and hands each message event to
// handle until the connection ends; every envelope is acknowledged first,
// as Slack retries one it doesn't hear back about within 3 seconds.
func (c *Client) Listen(ctx context.Context, handle func(Message)) error {
	u, err := c.Open(ctx)
	if err != nil {
		return err
	}
	conn, err := ws.Dial(ctx, u, c.Proxy)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			case <-stop:
				return
			case <-t.C:
				conn.Ping()
			}
		}
	}()
	for {
		// Slack pings and our pings' pongs arrive well within this.
		conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		raw, err := conn.Read()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		var env struct {
			EnvelopeID string `json:"envelope_id"`
			Type       string `json:"type"`
			Reason     string `json:"reason"`
			Payload    struct {
				EventID string  `json:"event_id"`
				Event   Message `json:"event"`
			} `json:"payload"`
		}
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		if env.EnvelopeID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			if err := conn.WriteText(ack); err != nil {
				return err
			}
		}
		switch env.Type {
		case "disconnect":
			return fmt.Errorf("%w (%s)", ErrDisconnect, env.Reason)
		case "events_api":
			if env.Payload.Event.Type == "message" {
				handle(env.Payload.Event)
			}
		}
	}
}

// Channel is a conversation the bot can see.
type Channel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsPrivate  bool   `json:"is_private"`
	IsMember   bool   `json:"is_member"`
	IsArchived bool   `json:"is_archived"`
}

// Channels lists the public channels and the private ones the bot is in
// (conversations.list), archived ones left out.
func (c *Client) Channels(ctx context.Context) ([]Channel, error) {
	var all []Channel
	cursor := ""
	for {
		f := url.Values{"types": {"public_channel,private_channel"}, "exclude_archived": {"true"}, "limit": {"1000"}}
		if cursor != "" {
			f.Set("cursor", cursor)
		}
		var out struct {
			Channels []Channel `json:"channels"`
			Meta     struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.call(ctx, c.BotToken, "conversations.list", f, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Channels...)
		if cursor = out.Meta.Next; cursor == "" {
			return all, nil
		}
	}
}

// Create makes a channel, private unless public is set, and returns its id.
func (c *Client) Create(ctx context.Context, name string, public bool) (string, error) {
	var out struct {
		Channel Channel `json:"channel"`
	}
	err := c.call(ctx, c.BotToken, "conversations.create", url.Values{"name": {name}, "is_private": {fmt.Sprint(!public)}}, &out)
	return out.Channel.ID, err
}

// Join puts the bot in a public channel.
func (c *Client) Join(ctx context.Context, channel string) error {
	return c.call(ctx, c.BotToken, "conversations.join", url.Values{"channel": {channel}}, nil)
}

// Invite adds a user to a channel; one already there is no error.
func (c *Client) Invite(ctx context.Context, channel, user string) error {
	err := c.call(ctx, c.BotToken, "conversations.invite", url.Values{"channel": {channel}, "users": {user}}, nil)
	switch Code(err) {
	case "already_in_channel", "cant_invite_self":
		return nil
	}
	return err
}

// Archive archives a channel; one already archived is no error.
func (c *Client) Archive(ctx context.Context, channel string) error {
	err := c.call(ctx, c.BotToken, "conversations.archive", url.Values{"channel": {channel}}, nil)
	if Code(err) == "already_archived" {
		return nil
	}
	return err
}

// Topic sets a channel's topic.
func (c *Client) Topic(ctx context.Context, channel, topic string) error {
	return c.call(ctx, c.BotToken, "conversations.setTopic", url.Values{"channel": {channel}, "topic": {topic}}, nil)
}
