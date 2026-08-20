package domain

import (
	"encoding/json"
	"fmt"
)

// Message is a translatable, user facing string. Format is the English source
// text and doubles as the catalog key, in the style of gettext msgids, so the
// Go code stays readable and no separate key registry has to be maintained.
//
// Args are pre-rendered to strings so a Message survives a JSON round trip
// unchanged; every verb in Format must therefore be %s, %q or %v.
type Message struct {
	Format string   `json:"format"`
	Args   []string `json:"args,omitempty"`
}

// Msg builds a Message from an English format string and its arguments.
func Msg(format string, args ...any) Message {
	m := Message{Format: format}
	for _, a := range args {
		m.Args = append(m.Args, fmt.Sprint(a))
	}
	return m
}

// String renders the English text, which is what logs and err.Error() show.
func (m Message) String() string {
	if len(m.Args) == 0 {
		return m.Format
	}
	args := make([]any, len(m.Args))
	for i, a := range m.Args {
		args[i] = a
	}
	return fmt.Sprintf(m.Format, args...)
}

// Empty reports whether there is nothing to show.
func (m Message) Empty() bool { return m.Format == "" }

// MarshalJSON emits null for an empty message so clients can test it directly.
func (m Message) MarshalJSON() ([]byte, error) {
	if m.Format == "" {
		return []byte("null"), nil
	}
	type alias Message
	return json.Marshal(alias(m))
}

func (m *Message) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*m = Message{}
		return nil
	}
	type alias Message
	var out alias
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*m = Message(out)
	return nil
}
