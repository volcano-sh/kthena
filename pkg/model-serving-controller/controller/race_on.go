// Copyright The Volcano Authors.
// Licensed under the Apache License, Version 2.0
//go:build race

package controller

// raceEnabled returns true when the race detector is enabled
func raceEnabled() bool {
	return true
}
