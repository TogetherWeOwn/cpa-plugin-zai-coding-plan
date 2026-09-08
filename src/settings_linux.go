//go:build linux

package main

import "golang.org/x/sys/unix"

const syscallNoFollow = unix.O_NOFOLLOW
