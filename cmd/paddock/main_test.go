package main

import (
	"os"
	"testing"
)

// TestMain makes the package's git-invoking tests hermetic. Several tests init a
// throwaway repo and assert it has *no* identity/signing config — but git also
// reads the developer's global and system config, and almost every real machine
// has a global user.name, so those assertions failed locally (they passed in CI
// only because the runner has no global config). Pointing git at os.DevNull for
// both scopes isolates the tests from the host.
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	os.Exit(m.Run())
}
