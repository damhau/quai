# JSON file-editing corpus

JSON and `*.tfvars.json` files and the edits made to them, for `internal/engine/edit/jsonedit`
([file editing](../../../../docs/design/03-file-editing.md)). Every editing bug becomes a case here
before it is fixed.

Each directory is one case, laid out as the [YAML corpus](../yaml/README.md):

- `input.json`: the file as found, byte for byte.
- `edit.yaml`: the change, as `set` and `delete` operations applied to the decoded input to build
  the intended document. A path is a list of keys and array indices.
- `want.json`: the exact expected output, or `want.error`: a fragment of the expected refusal.

`go test ./internal/engine/edit/jsonedit -run TestCorpus -update` rewrites the `want.json` files
from the current output. Review the diff before committing it.

Sources: hand-written in the shapes these files have in practice (npm, Terraform, Kubernetes,
Azure Resource Manager). No customer data.
