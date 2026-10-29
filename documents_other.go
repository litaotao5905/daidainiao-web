//go:build !windows

package main

import "errors"

func winKnownDocuments() (string, error) { return "", errors.New("not windows") }
