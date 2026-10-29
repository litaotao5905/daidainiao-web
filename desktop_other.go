//go:build !windows

package main

func runDesktop(url, dataDir string) bool {
	return false
}
