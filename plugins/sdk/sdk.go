// Package sdk is the Go SDK of Quai's generator plugins: a plugin implements Plugin, registers it
// from its main package's init, and is built as a WebAssembly reactor for WASI preview 1:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
//
// The SDK exports the functions Quai calls (alloc, write and read) and moves their JSON through
// the module's memory; the plugin sees Go values. How to write a plugin, and the ABI, are in
// docs/engineering/plugins.md of the Quai repository.
//
// A plugin runs in a sandbox: no file system, no network, a clock fixed at a start time and a
// fixed random source, a memory and a time limit per call, and a new instance for each call. It
// must be deterministic: the same request gives the same output.
package sdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Plugin writes a resource's values into its file, and reads them back.
type Plugin interface {
	// Write returns the file's text with the request's values in it. On update, Current is the
	// file before the request: keep what the values do not describe (comments, order, other
	// settings) as it is.
	Write(WriteRequest) (string, error)
	// Read returns the values a file holds. Quai reads back every file the plugin writes, and
	// refuses a request whose values do not come back.
	Read(ReadRequest) (map[string]any, error)
}

// WriteRequest is a call to Write.
type WriteRequest struct {
	// Values are the resource's values, as the type's schema describes them. Numbers are
	// float64, as encoding/json decodes them.
	Values map[string]any `json:"values"`
	// Current is the file's text before the request; nil on create.
	Current *string `json:"current,omitempty"`
	// Params are the resource's locator params, such as its name.
	Params map[string]string `json:"params,omitempty"`
	// Config is the type's spec.generate.config.
	Config map[string]any `json:"config,omitempty"`
}

// ReadRequest is a call to Read.
type ReadRequest struct {
	Region string            `json:"region"` // the file's text
	Params map[string]string `json:"params,omitempty"`
	Config map[string]any    `json:"config,omitempty"`
}

var registered Plugin

// Register makes p the plugin the module runs. Call it once, from the main package's init.
func Register(p Plugin) {
	if registered != nil {
		panic("sdk: Register called twice")
	}
	registered = p
}

// Handle runs one call of the ABI on p: fn is "write" or "read", input the call's JSON. It returns
// the output's JSON: {"region": …} or {"values": …}, or {"error": …} when the plugin refuses
// the input. The module's exports call it; tests can too.
func Handle(p Plugin, fn string, input []byte) []byte {
	out, err := handle(p, fn, input)
	if err != nil {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return b
	}
	return out
}

func handle(p Plugin, fn string, input []byte) ([]byte, error) {
	if p == nil {
		return nil, errors.New("no plugin is registered: call sdk.Register from init")
	}
	switch fn {
	case "write":
		var r WriteRequest
		if err := decode(input, &r); err != nil {
			return nil, err
		}
		if r.Values == nil {
			r.Values = map[string]any{}
		}
		region, err := p.Write(r)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"region": region})
	case "read":
		var r ReadRequest
		if err := decode(input, &r); err != nil {
			return nil, err
		}
		values, err := p.Read(r)
		if err != nil {
			return nil, err
		}
		if values == nil {
			values = map[string]any{}
		}
		out, err := json.Marshal(map[string]any{"values": values})
		if err != nil {
			return nil, fmt.Errorf("the values cannot be sent to Quai: %w", err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown call %q", fn)
}

func decode(input []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(input))
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("the request is not valid JSON: %w", err)
	}
	return nil
}
