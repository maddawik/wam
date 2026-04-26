package main

import "testing"

func TestMain(t *testing.T) {
	got := getGreeting()

	if got != "Hello, World!" {
		t.Errorf("expected 'Hello, World' but got %q", got)
	}
}
