package bridge

import (
	"os"
	"testing"
)

// TestMain runs the package tests from a throwaway working directory. The
// plugin's default data directory is a relative path (mirroring how CPA runs it
// from its own working directory), so without isolation the tests would share
// and persist page settings under the package directory between runs.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ccbridge-test-")
	if err != nil {
		panic(err)
	}
	previous, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.Chdir(previous)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
