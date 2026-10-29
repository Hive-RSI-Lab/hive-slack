package ws

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	for _, n := range []int{0, 5, 125, 126, 70000} {
		payload := bytes.Repeat([]byte("x"), n)
		for _, mask := range [][]byte{nil, {1, 2, 3, 4}} {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, opText, payload, mask); err != nil {
				t.Fatal(err)
			}
			fin, op, got, err := ReadFrame(&buf)
			if err != nil || !fin || op != opText || !bytes.Equal(got, payload) {
				t.Fatalf("n=%d mask=%v: fin=%v op=%d len=%d err=%v", n, mask, fin, op, len(got), err)
			}
		}
	}
}

// server is a WebSocket server for the test: it sends a ping, a message in two
// fragments, then echoes what it reads, then closes.
func server(t *testing.T, got chan<- string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" || r.URL.RawQuery != "a=1" {
			http.Error(w, "no", 400)
			return
		}
		conn, rw, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + Accept(r.Header.Get("Sec-Websocket-Key")) + "\r\n\r\n")
		rw.Flush()
		WriteFrame(conn, opPing, []byte("hi"), nil)
		conn.Write([]byte{opText, 3, 'a', 'b', 'c'}) // not final
		WriteFrame(conn, opCont, []byte("def"), nil)
		br := bufio.NewReader(rw)
		for {
			_, op, p, err := ReadFrame(br)
			if err != nil {
				return
			}
			got <- fmt.Sprintf("%d:%s", op, p)
			if op == opText {
				WriteFrame(conn, opClose, nil, nil)
			}
		}
	}))
}

func TestDialReadWrite(t *testing.T) {
	got := make(chan string, 10)
	srv := server(t, got)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/socket?a=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg, err := c.Read()
	if err != nil || string(msg) != "abcdef" {
		t.Fatalf("read %q, %v", msg, err)
	}
	if p := <-got; p != "10:hi" {
		t.Fatalf("the ping was answered with %q, not a pong", p)
	}
	if err := c.WriteText([]byte("ack")); err != nil {
		t.Fatal(err)
	}
	if p := <-got; p != "1:ack" {
		t.Fatalf("server read %q", p)
	}
	if _, err := c.Read(); err != io.EOF {
		t.Fatalf("a close reads as %v, not EOF", err)
	}
}

func TestDialRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 403) }))
	defer srv.Close()
	if _, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil); err == nil {
		t.Fatal("a refused handshake dialled")
	}
	if _, err := Dial(context.Background(), "http://x", nil); err == nil {
		t.Fatal("an http:// address dialled")
	}
}

// With Idle set, a connection that sends no message but keeps pinging stays
// readable past Idle; one that falls silent times out.
func TestIdleCountsEveryFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + Accept(r.Header.Get("Sec-Websocket-Key")) + "\r\n\r\n")
		rw.Flush()
		for i := 0; i < 8; i++ {
			time.Sleep(50 * time.Millisecond)
			WriteFrame(conn, opPing, nil, nil)
		}
		WriteFrame(conn, opText, []byte("late"), nil)
		time.Sleep(time.Second)
	}))
	defer srv.Close()
	c, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Idle = 200 * time.Millisecond
	if msg, err := c.Read(); err != nil || string(msg) != "late" {
		t.Fatalf("read %q, %v: pings didn't keep the read alive", msg, err)
	}
	if _, err := c.Read(); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("a silent connection read as %v, not a timeout", err)
	}
}
