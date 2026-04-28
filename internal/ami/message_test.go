package ami

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestMessageGetCaseInsensitive(t *testing.T) {
	var m Message
	m.Set("Response", "Success")
	m.Set("ActionID", "42")

	if got := m.Get("response"); got != "Success" {
		t.Errorf("Get(response) = %q, want Success", got)
	}
	if got := m.Get("ACTIONID"); got != "42" {
		t.Errorf("Get(ACTIONID) = %q, want 42", got)
	}
	if got := m.Get("missing"); got != "" {
		t.Errorf("Get(missing) = %q, want empty", got)
	}
	if !m.Has("Response") {
		t.Error("Has(Response) = false, want true")
	}
}

func TestMessageEncode(t *testing.T) {
	var m Message
	m.Set("Action", "Login")
	m.Set("Username", "admin")
	m.Set("Secret", "s3cret")

	want := "Action: Login\r\nUsername: admin\r\nSecret: s3cret\r\n\r\n"
	if got := string(m.Encode()); got != want {
		t.Errorf("Encode = %q, want %q", got, want)
	}
}

func TestReadMessageSimple(t *testing.T) {
	input := "Response: Success\r\nMessage: Authentication accepted\r\n\r\n"
	r := bufio.NewReader(strings.NewReader(input))

	msg, err := ReadMessage(r)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if msg.Get("Response") != "Success" {
		t.Errorf("Response = %q", msg.Get("Response"))
	}
	if msg.Get("Message") != "Authentication accepted" {
		t.Errorf("Message = %q", msg.Get("Message"))
	}
}

func TestReadMessageMultiple(t *testing.T) {
	input := "Event: PeerEntry\r\nObjectName: 100\r\nStatus: OK\r\n\r\n" +
		"Event: PeerEntry\r\nObjectName: 101\r\nStatus: UNREACHABLE\r\n\r\n" +
		"Event: PeerlistComplete\r\nListItems: 2\r\n\r\n"
	r := bufio.NewReader(strings.NewReader(input))

	for i, want := range []string{"100", "101", ""} {
		msg, err := ReadMessage(r)
		if err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
		if got := msg.Get("ObjectName"); got != want {
			t.Errorf("msg %d ObjectName = %q, want %q", i, got, want)
		}
	}

	if _, err := ReadMessage(r); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestReadMessageSkipsBlankLines(t *testing.T) {
	input := "\r\n\r\nEvent: Foo\r\nKey: Value\r\n\r\n"
	r := bufio.NewReader(strings.NewReader(input))
	msg, err := ReadMessage(r)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if msg.Get("Event") != "Foo" {
		t.Errorf("Event = %q", msg.Get("Event"))
	}
}

func TestReadMessageHandlesValueWithColon(t *testing.T) {
	input := "Response: Success\r\nMessage: Reload: Reloaded\r\n\r\n"
	r := bufio.NewReader(strings.NewReader(input))
	msg, err := ReadMessage(r)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got := msg.Get("Message"); got != "Reload: Reloaded" {
		t.Errorf("Message = %q", got)
	}
}

func TestReadMessageOutputLines(t *testing.T) {
	// "Response: Follows" command outputs may include free-form text lines.
	input := "Response: Follows\r\nfreepbx version 16.0.0\r\nasterisk version 18.2.0\r\n\r\n"
	r := bufio.NewReader(strings.NewReader(input))
	msg, err := ReadMessage(r)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if msg.Get("Response") != "Follows" {
		t.Errorf("Response = %q", msg.Get("Response"))
	}
	out := msg.Fields()
	if len(out) < 3 {
		t.Fatalf("got %d fields, want >=3", len(out))
	}
}
