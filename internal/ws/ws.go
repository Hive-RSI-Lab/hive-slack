// Package ws is the small WebSocket client (RFC 6455) Slack's Socket Mode
// needs: dial ws:// or wss:// (through an HTTPS proxy when the environment
// names one), read text messages, answer pings, write text frames. The
// standard library has none, and the module fetches nothing.
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// MaxMessage caps one message's size; Slack's envelopes are far smaller.
const MaxMessage = 16 << 20

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opCont  = 0x0
	opText  = 0x1
	opBin   = 0x2
	opClose = 0x8
	opPing  = 0x9
	opPong  = 0xA
)

// Conn is one client connection.
type Conn struct {
	c   net.Conn
	br  *bufio.Reader
	wmu sync.Mutex
}

// Dial opens a WebSocket to rawURL. proxy (http.ProxyFromEnvironment, or nil
// for none) picks an HTTP proxy to tunnel through with CONNECT.
func Dial(ctx context.Context, rawURL string, proxy func(*http.Request) (*url.URL, error)) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	secure := false
	switch u.Scheme {
	case "wss":
		secure = true
	case "ws":
	default:
		return nil, fmt.Errorf("ws: scheme %q is not ws or wss", u.Scheme)
	}
	host := u.Host
	if u.Port() == "" {
		if secure {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	var d net.Dialer
	var conn net.Conn
	var pu *url.URL
	if proxy != nil {
		// The proxy is chosen as for the https:// (or http://) URL of the same host.
		scheme := "http"
		if secure {
			scheme = "https"
		}
		if pu, err = proxy(&http.Request{URL: &url.URL{Scheme: scheme, Host: u.Host}}); err != nil {
			return nil, err
		}
	}
	if pu != nil {
		ph := pu.Host
		if pu.Port() == "" {
			ph += ":80"
		}
		if conn, err = d.DialContext(ctx, "tcp", ph); err != nil {
			return nil, err
		}
		if err = connect(conn, host, pu); err != nil {
			conn.Close()
			return nil, err
		}
	} else if conn, err = d.DialContext(ctx, "tcp", host); err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if secure {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err = tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tc
	}
	c := &Conn{c: conn, br: bufio.NewReader(conn)}
	if err = c.handshake(u); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return c, nil
}

// connect asks an HTTP proxy for a tunnel to host.
func connect(conn net.Conn, host string, pu *url.URL) error {
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: host}, Host: host, Header: http.Header{}}
	if pu.User != nil {
		pass, _ := pu.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(pu.User.Username()+":"+pass)))
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := req.Write(conn); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ws: proxy refused the tunnel: %s", resp.Status)
	}
	return nil
}

func (c *Conn) handshake(u *url.URL) error {
	raw := make([]byte, 16)
	rand.Read(raw)
	key := base64.StdEncoding.EncodeToString(raw)
	path := u.RequestURI()
	req := &http.Request{Method: http.MethodGet, URL: &url.URL{Opaque: path}, Host: u.Host, Header: http.Header{
		"Upgrade":               {"websocket"},
		"Connection":            {"Upgrade"},
		"Sec-Websocket-Key":     {key},
		"Sec-Websocket-Version": {"13"},
	}}
	if err := req.Write(c.c); err != nil {
		return err
	}
	resp, err := http.ReadResponse(c.br, req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		return fmt.Errorf("ws: handshake answered %s", resp.Status)
	}
	if resp.Header.Get("Sec-Websocket-Accept") != Accept(key) {
		return errors.New("ws: handshake answered with the wrong Sec-WebSocket-Accept")
	}
	return nil
}

// Accept is the Sec-WebSocket-Accept a server answers key with.
func Accept(key string) string {
	h := sha1.Sum([]byte(key + guid))
	return base64.StdEncoding.EncodeToString(h[:])
}

// SetReadDeadline bounds the next Read.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.c.SetReadDeadline(t) }

// Read returns the next text or binary message, answering pings on the way.
// A close from the server is io.EOF.
func (c *Conn) Read() ([]byte, error) {
	var msg []byte
	started := false
	for {
		fin, op, payload, err := ReadFrame(c.br)
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := c.write(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			c.write(opClose, nil)
			return nil, io.EOF
		case opText, opBin:
			if started {
				return nil, errors.New("ws: a new message began inside another")
			}
			started = true
			msg = payload
		case opCont:
			if !started {
				return nil, errors.New("ws: a continuation with no message")
			}
			if len(msg)+len(payload) > MaxMessage {
				return nil, errors.New("ws: message over the size limit")
			}
			msg = append(msg, payload...)
		default:
			return nil, fmt.Errorf("ws: unknown opcode %d", op)
		}
		if fin {
			return msg, nil
		}
	}
}

// ReadFrame reads one frame, unmasking it if it came masked.
func ReadFrame(r io.Reader) (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > MaxMessage {
		err = errors.New("ws: frame over the size limit")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(r, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// WriteFrame writes one final frame; mask is nil for a server's frame.
func WriteFrame(w io.Writer, op byte, payload []byte, mask []byte) error {
	b := []byte{0x80 | op}
	m := byte(0)
	if mask != nil {
		m = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		b = append(b, m|byte(n))
	case n <= 0xFFFF:
		b = append(b, m|126, byte(n>>8), byte(n))
	default:
		b = append(b, m|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	if mask != nil {
		b = append(b, mask...)
		start := len(b)
		b = append(b, payload...)
		for i := start; i < len(b); i++ {
			b[i] ^= mask[(i-start)%4]
		}
	} else {
		b = append(b, payload...)
	}
	_, err := w.Write(b)
	return err
}

func (c *Conn) write(op byte, payload []byte) error {
	mask := make([]byte, 4)
	rand.Read(mask)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return WriteFrame(c.c, op, payload, mask)
}

// WriteText sends one text message.
func (c *Conn) WriteText(b []byte) error { return c.write(opText, b) }

// Ping sends a ping; the server's pong keeps reads alive.
func (c *Conn) Ping() error { return c.write(opPing, nil) }

// Close ends the connection.
func (c *Conn) Close() error {
	c.write(opClose, nil)
	return c.c.Close()
}
