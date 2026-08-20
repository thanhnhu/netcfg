package domain

// Secret wraps credential material so it cannot leak through logs or JSON.
// Only Reveal returns the plaintext, which makes every escape point greppable.
type Secret struct {
	value string
}

func NewSecret(v string) Secret { return Secret{value: v} }

func (s Secret) Reveal() string { return s.value }

func (s Secret) Empty() bool { return s.value == "" }

func (s Secret) String() string { return "[REDACTED]" }

func (s Secret) GoString() string { return "[REDACTED]" }

// LogValue satisfies slog.LogValuer without importing log/slog.
func (s Secret) LogValue() any { return "[REDACTED]" }

func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }
