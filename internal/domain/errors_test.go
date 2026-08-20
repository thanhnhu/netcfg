package domain

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestErrorTextDoesNotRepeatTheCause guards a trap the constructors set: they
// pick the error out of the arguments and keep it for Unwrap, while every call
// site has already formatted it into the message with %v. Appending it again
// produced text like "... no such file or directory: ... no such file or
// directory" in front of operators.
func TestErrorTextDoesNotRepeatTheCause(t *testing.T) {
	cause := os.ErrNotExist
	err := Unavailable("cannot read %s: %v", "/run/wpa_supplicant", cause)

	got := err.Error()
	if n := strings.Count(got, cause.Error()); n != 1 {
		t.Fatalf("cause appears %d times in %q, want once", n, got)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Error("the cause must stay reachable through errors.Is")
	}
	if errors.Unwrap(err) != cause {
		t.Error("Unwrap must return the original cause")
	}
}

func TestErrorWithoutACauseIsJustItsMessage(t *testing.T) {
	err := Invalid("gateway %s is outside subnet %s", "10.0.0.1", "192.168.1.0/24")

	want := "gateway 10.0.0.1 is outside subnet 192.168.1.0/24"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if errors.Unwrap(err) != nil {
		t.Error("an error built from plain arguments wraps nothing")
	}
}
