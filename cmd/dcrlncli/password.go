// +build !windows

package main

import (
	"syscall"

	"golang.org/x/crypto/ssh/terminal"
)

func readPassword() ([]byte, error) {
	return terminal.ReadPassword(syscall.Stdin)
}
