# Quai plugin template

A [Quai](https://github.com/damhau/quai) generator plugin for `.env` files, to start a plugin
from. Quai calls a plugin to write a resource's values into its file and to read them back, for a
format its built-in editors (YAML, JSON, HCL) do not know.

```bash
make test    # the golden cases in testdata, natively
make build   # plugin.wasm, and the digest to pin in the type
make check   # gofmt, go vet (native and wasip1), the tests and the build
```

Go 1.24 or later builds it (`//go:wasmexport`). The digest depends on the Go version: rebuild and
pin again after an upgrade.

## Make it yours

1. Copy this directory, change the module path in `go.mod`, and require a released
   `github.com/damhau/quai/plugins/sdk` instead of the `replace` to `../sdk`.
2. Change `Write` and `Read` in `main.go`. `Write` gets the values and, on update, the file before
   the request: keep what the values do not describe (comments, order, other lines) as it is.
   `Read` returns the values a file holds; Quai reads back every file the plugin writes and
   refuses the request if a value does not come back.
3. Keep the cases in `testdata` up to date: each directory holds `request.json` (the values, and
   `params` and `config` if the plugin uses them), `before.env` for an update and `after.env`, the
   file the request must write. A case with `error` must be refused with that message.

## Use it in a type

Commit the module to the repository and pin it:

```yaml
apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: service-env, title: Service environment }
spec:
  locate: { strategy: file, path: "services/{name}.env" }
  params: [{ name: name, pattern: "[a-z][a-z0-9-]*" }]
  schema: catalog/schemas/service-env.schema.json
  generate:
    kind: plugin
    module: .quai/plugins/dotenv.wasm
    digest: sha256:…   # what make build prints
```

The schema describes the values, a flat object of strings for this plugin.

Writing a plugin, the ABI and the sandbox it runs in: `docs/engineering/plugins.md` in the Quai
repository.
