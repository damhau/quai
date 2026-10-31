package main

import (
	"testing"

	"github.com/damhau/quai/plugins/sdk/sdktest"
)

// TestGolden: each case under testdata writes its after.ini, reads it back and rewrites it
// unchanged, or is refused with its error (sdktest.Golden).
func TestGolden(t *testing.T) { sdktest.Golden(t, ini{}, "testdata") }
