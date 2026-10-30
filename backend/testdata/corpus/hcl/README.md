# HCL file-editing corpus

Terraform `*.tfvars` files and `locals` blocks, and the edits made to them, for
`internal/engine/edit/hcledit` ([file editing](../../../../docs/design/03-file-editing.md)). Every
editing bug becomes a case here before it is fixed.

Each directory is one case, laid out as the [YAML corpus](../yaml/README.md):

- `input.hcl`: the file as found, byte for byte.
- `edit.yaml`: the change, as `set` and `delete` operations applied to the decoded body to build
  the intended document; `block: locals` edits the file's locals block instead of its top-level
  attributes.
- `want.hcl`: the exact expected output, or `want.error`: a fragment of the expected refusal.

`go test ./internal/engine/edit/hcledit -run TestCorpus -update` rewrites the `want.hcl` files
from the current output. Review the diff before committing it.

Sources: hand-written in the shapes these files have in Terraform repositories. No customer
data.
