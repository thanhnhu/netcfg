//go:build !linux

package prober

import "syscall"

// bindToDevice has no counterpart outside Linux; the dial then follows the
// routing table, which is only ever the case on a developer machine.
func bindToDevice(string) func(network, address string, c syscall.RawConn) error { return nil }
