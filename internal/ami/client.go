// Package ami implements a minimal Asterisk Manager Interface (AMI) client
// suitable for synchronous "scrape-and-disconnect" use by a Prometheus
// exporter. It is not a full event-driven client.
package ami

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Default AMI port.
const DefaultPort = 5038

// Config holds AMI connection settings.
type Config struct {
	Address  string        // host:port
	Username string        // AMI manager user
	Secret   string        // AMI manager secret
	Timeout  time.Duration // per-action read/write timeout (0 = 10s)

	// EventClasses controls the AMI "Events" header sent at Login time.
	// Empty defaults to "off" — no async events delivered (right for the
	// per-scrape collector). Set to e.g. "call,reporting" for an
	// EventStream consumer.
	EventClasses string
}

// Client is an AMI connection. It is not safe for concurrent use; create one
// per scrape.
type Client struct {
	cfg     Config
	conn    net.Conn
	reader  *bufio.Reader
	actions atomic.Uint64
}

// Dial opens a TCP connection to the AMI, consumes the banner, and returns a
// non-authenticated client. Caller must call Login next.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	d := net.Dialer{Timeout: cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("ami dial %s: %w", cfg.Address, err)
	}

	c := &Client{
		cfg:    cfg,
		conn:   conn,
		reader: bufio.NewReader(conn),
	}

	// Banner: "Asterisk Call Manager/x.y.z\r\n"
	if err := conn.SetReadDeadline(time.Now().Add(cfg.Timeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	banner, err := c.reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ami read banner: %w", err)
	}
	if !strings.HasPrefix(banner, "Asterisk Call Manager") {
		_ = conn.Close()
		return nil, fmt.Errorf("ami unexpected banner: %q", strings.TrimSpace(banner))
	}
	return c, nil
}

// Close terminates the connection. Safe to call multiple times.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	conn := c.conn
	c.conn = nil
	return conn.Close()
}

// nextActionID returns a unique ActionID for this client.
func (c *Client) nextActionID() string {
	return strconv.FormatUint(c.actions.Add(1), 10)
}

// Login authenticates with username/secret. Returns an error if AMI rejects
// the credentials. The Events header is set from cfg.EventClasses (default
// "off" when empty).
func (c *Client) Login(ctx context.Context) error {
	events := c.cfg.EventClasses
	if events == "" {
		events = "off"
	}
	resp, err := c.Action(ctx, Message{}.with(
		"Action", "Login",
		"Username", c.cfg.Username,
		"Secret", c.cfg.Secret,
		"Events", events,
	))
	if err != nil {
		return fmt.Errorf("ami login: %w", err)
	}
	if !strings.EqualFold(resp.Get("Response"), "Success") {
		return fmt.Errorf("ami login rejected: %s", resp.Get("Message"))
	}
	return nil
}

// NextEvent blocks until the next AMI message is available and returns it.
// No read deadline is set, so callers must cancel ctx (which closes the
// connection) to break out of a blocked read. Intended for long-running
// event-stream consumers, not request/response use.
func (c *Client) NextEvent(ctx context.Context) (Message, error) {
	if c.conn == nil {
		return Message{}, errors.New("ami: connection closed")
	}
	if err := c.conn.SetReadDeadline(time.Time{}); err != nil {
		return Message{}, err
	}
	return ReadMessage(c.reader)
}

// Logoff sends an Action: Logoff. Errors are ignored by the caller in most
// cases since the connection is about to be closed anyway.
func (c *Client) Logoff(ctx context.Context) error {
	_, err := c.Action(ctx, Message{}.with("Action", "Logoff"))
	return err
}

// Action sends a single non-list AMI action and returns the immediate
// response. The ActionID field is set automatically.
func (c *Client) Action(ctx context.Context, req Message) (Message, error) {
	id := c.nextActionID()
	req.Set("ActionID", id)

	if err := c.write(ctx, req); err != nil {
		return Message{}, err
	}
	resp, err := c.read(ctx)
	if err != nil {
		return Message{}, err
	}
	if got := resp.Get("ActionID"); got != "" && got != id {
		return Message{}, fmt.Errorf("ami: ActionID mismatch (want %s, got %s)", id, got)
	}
	return resp, nil
}

// ListAction sends an action that returns a stream of list events terminated
// by a "complete" event whose Event name is configurable per action (e.g.
// PeerlistComplete, CoreShowChannelsComplete, EndpointListComplete).
//
// completeEvent is matched case-insensitively. The returned slice contains
// every list-item event (excluding the initial response and the completion
// event).
func (c *Client) ListAction(ctx context.Context, req Message, completeEvent string) ([]Message, error) {
	id := c.nextActionID()
	req.Set("ActionID", id)

	if err := c.write(ctx, req); err != nil {
		return nil, err
	}

	// First message is the initial response.
	first, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(first.Get("Response"), "Success") {
		return nil, fmt.Errorf("ami list action %q rejected: %s",
			req.Get("Action"), first.Get("Message"))
	}

	var items []Message
	completeLower := strings.ToLower(completeEvent)
	for {
		msg, err := c.read(ctx)
		if err != nil {
			return nil, err
		}
		// Filter by ActionID when present so unrelated events on a noisy
		// connection don't poison the result.
		if aid := msg.Get("ActionID"); aid != "" && aid != id {
			continue
		}
		if strings.EqualFold(msg.Get("Event"), completeLower) {
			return items, nil
		}
		// Skip messages without an Event field (shouldn't normally occur
		// during a list response but defend against it).
		if msg.Get("Event") == "" {
			continue
		}
		items = append(items, msg)
	}
}

func (c *Client) write(ctx context.Context, m Message) error {
	if c.conn == nil {
		return errors.New("ami: connection closed")
	}
	deadline := deadlineFromCtx(ctx, c.cfg.Timeout)
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if _, err := c.conn.Write(m.Encode()); err != nil {
		return fmt.Errorf("ami write: %w", err)
	}
	return nil
}

func (c *Client) read(ctx context.Context) (Message, error) {
	if c.conn == nil {
		return Message{}, errors.New("ami: connection closed")
	}
	deadline := deadlineFromCtx(ctx, c.cfg.Timeout)
	if err := c.conn.SetReadDeadline(deadline); err != nil {
		return Message{}, err
	}
	return ReadMessage(c.reader)
}

func deadlineFromCtx(ctx context.Context, fallback time.Duration) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	return time.Now().Add(fallback)
}

// with is a chainable helper for building a Message inline.
func (m Message) with(kv ...string) Message {
	for i := 0; i+1 < len(kv); i += 2 {
		m.Set(kv[i], kv[i+1])
	}
	return m
}

// NewMessage builds a Message from key/value pairs.
func NewMessage(kv ...string) Message {
	return Message{}.with(kv...)
}

// Conn is the subset of *Client used by callers; it allows tests to swap in
// a fake without spinning up a real TCP listener.
type Conn interface {
	Login(ctx context.Context) error
	Logoff(ctx context.Context) error
	Action(ctx context.Context, req Message) (Message, error)
	ListAction(ctx context.Context, req Message, completeEvent string) ([]Message, error)
	NextEvent(ctx context.Context) (Message, error)
	Close() error
}

// Dialer abstracts the act of opening an AMI connection.
type Dialer interface {
	Dial(ctx context.Context, cfg Config) (Conn, error)
}

// DefaultDialer dials a real TCP AMI socket.
type DefaultDialer struct{}

// Dial implements Dialer.
func (DefaultDialer) Dial(ctx context.Context, cfg Config) (Conn, error) {
	return Dial(ctx, cfg)
}
