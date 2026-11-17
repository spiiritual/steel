package main

import (
	"strings"
	"testing"
)

// In the container a swapped binary would be replaced by the image's on the
// next start, so update and rollback must refuse and say what to do instead,
// before touching the network or the binary.
func TestUpdateRefusesInContainer(t *testing.T) {
	t.Setenv("STEEL_CONTAINER", "1")
	for name, run := range map[string]func() error{
		"update":   func() error { return runUpdate(nil) },
		"rollback": runRollback,
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "docker compose pull") {
			t.Errorf("%s in a container: got %v, want a refusal pointing at docker compose pull", name, err)
		}
	}
}
