//go:build !windows

package main

import "os"

// enableVT is a no-op: non-Windows terminals interpret ANSI natively.
func enableVT(*os.File) bool { return true }
