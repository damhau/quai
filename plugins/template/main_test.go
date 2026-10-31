package main

import (
	"testing"

	"github.com/damhau/quai/plugins/sdk/sdktest"
)

// TestGolden: each case under testdata writes its after.env, reads it back and rewrites it
// unchanged (sdktest.Golden).
func TestGolden(t *testing.T) { sdktest.Golden(t, dotenv{}, "testdata") }
