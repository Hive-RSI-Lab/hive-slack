// Command slack is the Slack module's program: a door that carries the
// person's Slack messages into the Hive's chats and the answers back into the
// same Slack threads, over Socket Mode (outbound only; no port opens here).
//
//	slack            the module's program, as the daemon runs it
//	slack check      read slack.json and ask Slack who the app is
//	slack manifest   print the Slack app manifest to make the app from
//	slack where      print where slack.json goes
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	hiveslack "github.com/Hive-RSI-Lab/hive-slack"
	"github.com/Hive-RSI-Lab/hive-slack/internal/bridge"
	"github.com/Hive-RSI-Lab/hive-slack/internal/hive"
)

func main() {
	home := os.Getenv("HOME")
	cfgPath := filepath.Join(home, "slack.json")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "manifest":
		fmt.Print(hiveslack.Manifest)
		return
	case "where":
		fmt.Println(cfgPath)
		return
	case "check":
		cfg, err := bridge.LoadConfig(cfgPath)
		if err != nil {
			fail(err)
		}
		dispatch(&cfg, home)
		b := bridge.New(cfg, nil, "slack", home)
		b.Slack.Proxy = http.ProxyFromEnvironment
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		user, team, err := b.Slack.Whoami(c)
		if err != nil {
			fail(err)
		}
		if _, err := b.Slack.Open(c); err != nil {
			fail(fmt.Errorf("the app-level token: %v", err))
		}
		fmt.Printf("slack.json reads, Socket Mode opens, and the bot token is %s in workspace %s.\n", user, team)
		fmt.Printf("people (reach everything): %v\n", cfg.People)
		fmt.Printf("side files read from %s\n", cfg.Dispatch)
		for name, p := range b.Projects() {
			fmt.Printf("project %s: channel %s, its own people %v\n", name, p.Channel, p.People)
		}
		return
	case "", "serve":
	case "help", "-h", "--help":
		fmt.Println("usage: slack [check | manifest | where]; see docs/README.md")
		return
	default:
		fail(fmt.Errorf("unknown command %q; try help", cmd))
	}
	log.SetFlags(log.LstdFlags)
	h := &hive.Client{API: os.Getenv("HIVE_API"), Token: os.Getenv("HIVE_MODULE_TOKEN")}
	if h.API == "" {
		fail(fmt.Errorf("HIVE_API is not set: the Hive's daemon runs this program"))
	}
	name := os.Getenv("HIVE_MODULE")
	if name == "" {
		name = "slack"
	}
	// No slack.json yet: the person makes the Slack app first. Say so once,
	// then look again every 30 seconds while the Hive is up.
	var cfg bridge.Config
	told := ""
	for {
		var err error
		if cfg, err = bridge.LoadConfig(cfgPath); err == nil {
			break
		}
		if msg := err.Error(); msg != told {
			told = msg
			log.Print(msg)
			h.Report(ctx, map[string]any{"kind": "waiting", "text": "waiting for the person's Slack app: " + msg + " (docs/README.md says how)"})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		if err := h.Ping(ctx); err != nil {
			fail(fmt.Errorf("the Hive is not answering: %v", err))
		}
	}
	os.Chmod(cfgPath, 0o600)
	dispatch(&cfg, home)
	b := bridge.New(cfg, h, name, home)
	b.Slack.Proxy = http.ProxyFromEnvironment
	b.Log = log.Printf
	if err := b.Start(ctx); err != nil {
		h.Report(ctx, map[string]any{"kind": "error", "text": "slack: " + err.Error()})
		fail(err)
	}
	if err := b.Run(ctx); err != nil && ctx.Err() == nil {
		fail(err)
	}
}

// dispatch points the side files at the workspace's projects/ folder, where
// an exported project's <name>.dispatch.json sits beside its clone, unless
// slack.json names another folder. HOME is <workspace>/.hive/modules/<name>.
func dispatch(cfg *bridge.Config, home string) {
	if cfg.Dispatch == "" {
		cfg.Dispatch = filepath.Join(home, "..", "..", "..", "projects")
	} else if !filepath.IsAbs(cfg.Dispatch) {
		cfg.Dispatch = filepath.Join(home, cfg.Dispatch)
	}
	cfg.Dispatch = filepath.Clean(cfg.Dispatch)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "slack:", err)
	os.Exit(1)
}
