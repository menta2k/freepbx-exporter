package ami

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeServer accepts a single AMI client and replies based on a scripted
// handler. It returns the address the test should dial.
type fakeServer struct {
	t       *testing.T
	ln      net.Listener
	handler func(t *testing.T, in *bufio.Reader, out net.Conn)
}

func newFakeServer(t *testing.T, handler func(t *testing.T, in *bufio.Reader, out net.Conn)) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fs := &fakeServer{t: t, ln: ln, handler: handler}
	go fs.run()
	t.Cleanup(func() { _ = ln.Close() })
	return fs
}

func (fs *fakeServer) addr() string { return fs.ln.Addr().String() }

func (fs *fakeServer) run() {
	conn, err := fs.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("Asterisk Call Manager/2.10.5\r\n")); err != nil {
		return
	}
	fs.handler(fs.t, bufio.NewReader(conn), conn)
}

// readAction reads one AMI message (until blank line) from the client.
func readAction(t *testing.T, r *bufio.Reader) Message {
	t.Helper()
	m, err := ReadMessage(r)
	if err != nil {
		t.Fatalf("readAction: %v", err)
	}
	return m
}

func TestClientLoginSuccess(t *testing.T) {
	fs := newFakeServer(t, func(t *testing.T, in *bufio.Reader, out net.Conn) {
		req := readAction(t, in)
		if req.Get("Action") != "Login" {
			t.Errorf("Action = %q", req.Get("Action"))
		}
		if req.Get("Username") != "admin" || req.Get("Secret") != "s3cret" {
			t.Errorf("creds wrong: %+v", req.Fields())
		}
		resp := NewMessage(
			"Response", "Success",
			"ActionID", req.Get("ActionID"),
			"Message", "Authentication accepted",
		)
		_, _ = out.Write(resp.Encode())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := Dial(ctx, Config{Address: fs.addr(), Username: "admin", Secret: "s3cret"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.Login(ctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
}

func TestClientLoginRejected(t *testing.T) {
	fs := newFakeServer(t, func(t *testing.T, in *bufio.Reader, out net.Conn) {
		req := readAction(t, in)
		resp := NewMessage(
			"Response", "Error",
			"ActionID", req.Get("ActionID"),
			"Message", "Authentication failed",
		)
		_, _ = out.Write(resp.Encode())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := Dial(ctx, Config{Address: fs.addr(), Username: "x", Secret: "y"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	err = c.Login(ctx)
	if err == nil {
		t.Fatal("expected login error")
	}
	if !strings.Contains(err.Error(), "Authentication failed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestClientListAction(t *testing.T) {
	fs := newFakeServer(t, func(t *testing.T, in *bufio.Reader, out net.Conn) {
		// Login
		login := readAction(t, in)
		_, _ = out.Write(NewMessage(
			"Response", "Success",
			"ActionID", login.Get("ActionID"),
			"Message", "ok",
		).Encode())

		// Action: SIPpeers
		req := readAction(t, in)
		if req.Get("Action") != "SIPpeers" {
			t.Errorf("Action = %q", req.Get("Action"))
		}
		aid := req.Get("ActionID")

		// Initial response
		_, _ = out.Write(NewMessage(
			"Response", "Success",
			"ActionID", aid,
			"EventList", "start",
		).Encode())
		// List items
		_, _ = out.Write(NewMessage(
			"Event", "PeerEntry",
			"ActionID", aid,
			"ObjectName", "100",
			"Status", "OK (12 ms)",
		).Encode())
		_, _ = out.Write(NewMessage(
			"Event", "PeerEntry",
			"ActionID", aid,
			"ObjectName", "101",
			"Status", "UNREACHABLE",
		).Encode())
		// Completion
		_, _ = out.Write(NewMessage(
			"Event", "PeerlistComplete",
			"ActionID", aid,
			"EventList", "Complete",
			"ListItems", "2",
		).Encode())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := Dial(ctx, Config{Address: fs.addr(), Username: "u", Secret: "p"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Login(ctx); err != nil {
		t.Fatalf("Login: %v", err)
	}

	items, err := c.ListAction(ctx, NewMessage("Action", "SIPpeers"), "PeerlistComplete")
	if err != nil {
		t.Fatalf("ListAction: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Get("ObjectName") != "100" || items[1].Get("ObjectName") != "101" {
		t.Errorf("unexpected items: %+v / %+v", items[0].Fields(), items[1].Fields())
	}
}

func TestClientListActionFiltersForeignActionID(t *testing.T) {
	fs := newFakeServer(t, func(t *testing.T, in *bufio.Reader, out net.Conn) {
		login := readAction(t, in)
		_, _ = out.Write(NewMessage(
			"Response", "Success",
			"ActionID", login.Get("ActionID"),
		).Encode())
		req := readAction(t, in)
		aid := req.Get("ActionID")

		_, _ = out.Write(NewMessage("Response", "Success", "ActionID", aid).Encode())
		// Foreign event with a different ActionID — must be ignored.
		_, _ = out.Write(NewMessage(
			"Event", "PeerEntry",
			"ActionID", "999",
			"ObjectName", "ghost",
		).Encode())
		_, _ = out.Write(NewMessage(
			"Event", "PeerEntry",
			"ActionID", aid,
			"ObjectName", "200",
		).Encode())
		_, _ = out.Write(NewMessage(
			"Event", "PeerlistComplete",
			"ActionID", aid,
		).Encode())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := Dial(ctx, Config{Address: fs.addr(), Username: "u", Secret: "p"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Login(ctx); err != nil {
		t.Fatalf("Login: %v", err)
	}

	items, err := c.ListAction(ctx, NewMessage("Action", "SIPpeers"), "PeerlistComplete")
	if err != nil {
		t.Fatalf("ListAction: %v", err)
	}
	if len(items) != 1 || items[0].Get("ObjectName") != "200" {
		t.Errorf("expected only ObjectName=200, got %+v", items)
	}
}

func TestDialRejectsBadBanner(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err = Dial(ctx, Config{Address: ln.Addr().String(), Username: "u", Secret: "p"})
	if err == nil {
		t.Fatal("expected banner error")
	}
}
