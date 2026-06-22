//go:build !windows

package main

// launchGUI is a no-op on non-Windows builds (server-only / headless).
func launchGUI(url, title string) bool { return false }
