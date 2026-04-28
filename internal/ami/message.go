package ami

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// Message is a single AMI message — a sequence of "Key: Value" lines
// terminated by an empty line. Keys are matched case-insensitively.
type Message struct {
	fields []field
}

type field struct {
	key   string
	value string
}

// Get returns the first value for the given key (case-insensitive).
func (m Message) Get(key string) string {
	lk := strings.ToLower(key)
	for _, f := range m.fields {
		if strings.ToLower(f.key) == lk {
			return f.value
		}
	}
	return ""
}

// Has reports whether the key is present.
func (m Message) Has(key string) bool {
	lk := strings.ToLower(key)
	for _, f := range m.fields {
		if strings.ToLower(f.key) == lk {
			return true
		}
	}
	return false
}

// Set adds or replaces a field.
func (m *Message) Set(key, value string) {
	lk := strings.ToLower(key)
	for i, f := range m.fields {
		if strings.ToLower(f.key) == lk {
			m.fields[i].value = value
			return
		}
	}
	m.fields = append(m.fields, field{key: key, value: value})
}

// Fields returns a copy of all key/value pairs in order.
func (m Message) Fields() [][2]string {
	out := make([][2]string, 0, len(m.fields))
	for _, f := range m.fields {
		out = append(out, [2]string{f.key, f.value})
	}
	return out
}

// Encode serializes the message in AMI wire format.
func (m Message) Encode() []byte {
	var b strings.Builder
	for _, f := range m.fields {
		b.WriteString(f.key)
		b.WriteString(": ")
		b.WriteString(f.value)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

// ReadMessage reads one AMI message from r. The "Asterisk Call Manager/X.Y.Z"
// banner sent on connection is not a Message and must be consumed separately.
//
// Returns io.EOF if the stream ends cleanly between messages.
func ReadMessage(r *bufio.Reader) (Message, error) {
	var msg Message
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			// Allow a half-read message at EOF only if we have no fields yet.
			if errors.Is(err, io.EOF) && len(msg.fields) == 0 && line == "" {
				return Message{}, io.EOF
			}
			if errors.Is(err, io.EOF) {
				return Message{}, io.ErrUnexpectedEOF
			}
			return Message{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(msg.fields) == 0 {
				// Skip stray blank lines between messages.
				continue
			}
			return msg, nil
		}
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			// Some commands return free-form output (Response: Follows ...
			// then text lines ending with --END COMMAND--). We capture those
			// under the synthetic "Output" key, one entry per line.
			msg.fields = append(msg.fields, field{key: "Output", value: line})
			continue
		}
		value := strings.TrimLeft(rest, " ")
		msg.fields = append(msg.fields, field{key: key, value: value})
	}
}
