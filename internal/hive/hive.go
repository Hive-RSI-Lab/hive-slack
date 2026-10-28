// Package hive is the slice of the Hive's control API the module's channel
// hook reaches: post in a chat, read the journal, report. Its address and
// token come from the daemon ($HIVE_API, $HIVE_MODULE_TOKEN).
package hive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Client is the module's view of the daemon.
type Client struct {
	API   string
	Token string
	HTTP  *http.Client
}

// Event is one journal line.
type Event struct {
	Seq  int64           `json:"seq"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// Message is a chat line, as /api/chat answers a post.
type Message struct {
	ID     string `json:"id"`
	Chat   string `json:"chat"`
	Thread string `json:"thread"`
	From   string `json:"from"`
	Name   string `json:"name"`
	Text   string `json:"text"`
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.API, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s", e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Ping says whether the daemon is there.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/api/ping", nil, nil)
}

// Post writes text in a chat (lead, project:<name>, agent:<run>), in thread
// ("" starts one). The lead reads it as "Module <name>: …".
func (c *Client) Post(ctx context.Context, chat, thread, text string) (Message, error) {
	var m Message
	err := c.do(ctx, http.MethodPost, "/api/chat", map[string]string{"chat": chat, "thread": thread, "text": text}, &m)
	return m, err
}

// Events reads the journal after seq (at most 2000 events a read).
func (c *Client) Events(ctx context.Context, after int64) ([]Event, error) {
	var evs []Event
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/events?after=%d", after), nil, &evs)
	return evs, err
}

// Report journals a module.reported line.
func (c *Client) Report(ctx context.Context, fields map[string]any) error {
	return c.do(ctx, http.MethodPost, "/api/module/report", fields, nil)
}
