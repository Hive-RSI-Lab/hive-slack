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

// call POSTs a Web API method with a JSON body and decodes its answer into
// out; Slack's {"ok": false, "error": …} comes back as an error.
func (c *Client) call(ctx context.Context, token, method string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+method, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimited{After: resp.Header.Get("Retry-After")}
	}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return err
	}
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		return fmt.Errorf("slack %s: %s: %v", method, resp.Status, err)
	}
	if !res.OK {
		return fmt.Errorf("slack %s: %s", method, res.Error)
	}
	if out != nil {
		return json.Unmarshal(buf.Bytes(), out)
	}
	return nil
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
// as Slack retries one it doesn't hear back about within 3 seconds. opened
// (if not nil) is called when Slack's hello says the connection is live.
func (c *Client) Listen(ctx context.Context, handle func(Message), opened func()) error {
	u, err := c.Open(ctx)
	if err != nil {
		return err
	}
	conn, err := ws.Dial(ctx, u, c.Proxy)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Slack's pings and our pings' pongs arrive well within this; it is
	// counted from the last frame of any kind, not the last message, as an
	// idle workspace sends no messages for hours.
	conn.Idle = 2 * time.Minute
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
		case "hello":
			if opened != nil {
				opened()
			}
		case "disconnect":
			return fmt.Errorf("%w (%s)", ErrDisconnect, env.Reason)
		case "events_api":
			if env.Payload.Event.Type == "message" {
				handle(env.Payload.Event)
			}
		}
	}
}
