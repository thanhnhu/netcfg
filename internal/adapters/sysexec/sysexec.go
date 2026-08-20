// Package sysexec runs external commands with a fixed environment and never
// through a shell, so untrusted values cannot inject extra commands.
package sysexec

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"

	"netcfg/internal/domain"
)

// SafePath replaces the inherited PATH so a tampered environment cannot
// redirect us to an attacker supplied binary.
const SafePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// DefaultTimeout bounds any single command.
const DefaultTimeout = 25 * time.Second

// Run executes name with args and returns stdout.
func Run(ctx context.Context, name string, args ...string) (string, error) {
	return RunTimeout(ctx, DefaultTimeout, name, args...)
}

// RunTimeout is Run with an explicit deadline.
func RunTimeout(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=" + SafePath}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", domain.Unavailable("%s: timed out", name)
	}
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			detail = err.Error()
		}
		return "", domain.Unavailable("%s: %s", name, detail)
	}
	return stdout.String(), nil
}

// Available reports whether a binary exists on the safe PATH.
func Available(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
