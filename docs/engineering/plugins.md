# Writing a generator plugin

Status: Draft · 2026-10-31 · Design: [the `plugin` generator](../design/02-resource-types-and-generators.md#plugin-anything-else-in-a-sandbox)

A plugin writes a resource's values into its file, and reads them back, for a format Quai's
built-in editors (YAML, JSON, HCL) do not know: INI, TOML, XML, a team's own convention. It is a
WebAssembly module that the repository commits and that the type pins by digest. Quai runs it in a
sandbox and reads back every file it writes. Plugins should be rare: a format the editors know is
better served by `identity` or `mapping`, which edit the file without a module to review.

## The modules

`plugins/` in this repository holds three Go modules:

| Directory | What |
| --- | --- |
| `plugins/sdk` | The SDK, `github.com/damhau/quai/plugins/sdk`: it exports the ABI with `//go:wasmexport` and moves its JSON; `sdktest` runs a plugin's golden cases with `go test` |
| `plugins/template` | The template to start from: a `.env` plugin, its golden cases and a `Makefile` that builds the module and prints its digest |
| `plugins/ini` | The example: INI files, with sections, keeping comments, spacing, order and line endings |

The template becomes a repository of its own when the owner wants one; until then it requires the
SDK next to it (`replace ../sdk`). `make -C plugins check` runs gofmt, `go vet` and staticcheck
(natively and for `wasip1`), the tests and the builds of all three; CI runs it in `plugins.yml`.

## A plugin with the SDK

A plugin implements `sdk.Plugin` and registers it from its main package's `init`:

```go
package main

import "github.com/damhau/quai/plugins/sdk"

func init() { sdk.Register(myFormat{}) }

func main() {} // not run: Quai calls the exports

type myFormat struct{}

// Write returns the file's text with r.Values in it; r.Current is the file before the request,
// nil on create.
func (myFormat) Write(r sdk.WriteRequest) (string, error) { … }

// Read returns the values the file's text holds.
func (myFormat) Read(r sdk.ReadRequest) (map[string]any, error) { … }
```

- **Values** are the resource's values as the type's schema describes them (the form's values,
  with the owner field and the param mirrors), decoded by `encoding/json`: objects are
  `map[string]any`, numbers `float64`. `Params` are the locator's params, `Config` the type's
  `spec.generate.config`.
- **Write keeps what the values do not describe.** On update, change the lines of the values that
  changed and leave the others byte for byte: comments, order, spacing, settings the type does not
  manage. A reviewer reads the pull request's diff; a plugin that rewrites the file makes it
  unreadable.
- **Read gives back what Write was given.** Quai reads back every file a plugin writes and refuses
  the request when a value does not come back, so a format that cannot hold a value (a number in
  INI, a value on two lines) must refuse it in `Write` with an error saying why. The error is shown
  to the requester.
- **Deterministic.** The same request gives the same file: sort what has no order of its own (new
  keys, map iteration).

Build it as a reactor for WASI preview 1, with Go 1.24 or later:

```bash
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildvcs=false -buildmode=c-shared -o plugin.wasm .
```

The template's `make build` does this and prints the digest. The build is reproducible: the same
sources and Go version give the same module, so a reviewer can rebuild it and compare the digest
the type pins. It changes with the Go version: rebuild and pin again after an upgrade.

## Golden cases

`sdktest.Golden(t, plugin, "testdata")` runs each directory of `testdata` as a case, natively,
through the same JSON the module exchanges with Quai:

| File | What |
| --- | --- |
| `request.json` | `values` (and `params`, `config` when the plugin uses them); or `error`, the message the plugin must refuse the case with |
| `before.<ext>` | The file before the request; absent for a create |
| `after.<ext>` | The file the request must write, byte for byte |

A case checks that the write gives `after`, that `after` reads back as the values, and that
writing what was read into `after` changes nothing. A case with `error` and values must be refused
by `Write`; with no values, by `Read` of `before`. Quai's own tests run the template's and the
example's cases again in its runtime, from the built modules
(`internal/engine/generate/plugin`, `TestExamples`).

## Using it in a type

The repository commits the module and the type pins it:

```yaml
apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: service, title: Service }
spec:
  locate: { strategy: file, path: "services/{name}.ini" }
  params: [{ name: name, pattern: "[a-z]+" }]
  schema: catalog/schemas/service.schema.json   # the form, and what the plugin is given
  generate:
    kind: plugin
    module: .quai/plugins/ini.wasm
    digest: sha256:…                              # what make build prints
  ownership: { field: owner }
```

A plugin type uses the `file` locator, with any extension, and `spec.schema`. `quai lint` checks
the module (digest, imports, ABI), reads every file of the type through it, and runs the round
trip and the type's golden examples. A new module is a new digest: a change to the plugin is a
reviewed change to the type that pins it.

## The ABI, for other languages

The SDK is one way to meet the ABI; a module in any language that compiles to WASI preview 1 can.
The module is a reactor (it exports `_initialize`, or nothing to start, and no `_start`), imports
nothing but `wasi_snapshot_preview1`, and exports its memory and:

| Export | Signature | What |
| --- | --- | --- |
| `alloc` | `(size i32) -> i32` | A buffer of `size` bytes for the call's input |
| `write` | `(ptr, len i32) -> i64` | Input `{values, current, params, config}` (`current` absent on create); output `{region}` |
| `read` | `(ptr, len i32) -> i64` | Input `{region, params, config}`; output `{values}` |

The host writes the input, JSON, into a buffer from `alloc` and calls the function, which returns
its output's address in the high 32 bits and its length in the low 32. Either function returns
`{error}` to refuse its input. A region is UTF-8 text.

## The sandbox

Each call runs in a new instance, so nothing is kept between calls. WASI sees no file system,
no network, a clock that starts at a fixed time and a fixed random source; arguments and
environment are empty. Standard output is discarded; standard error is shown when a call fails.
A call has 64 MiB of memory and 2 seconds, from instantiation to output; a Go plugin starts its
runtime in each call, a few milliseconds. A plugin that panics, runs out of memory or time, or
writes what it cannot read back makes the request fail with that reason, and writes nothing.
