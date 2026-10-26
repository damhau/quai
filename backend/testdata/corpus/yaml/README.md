# YAML file-editing corpus

Real-world YAML files and the edits made to them, for `internal/engine/edit/yamledit`
([file editing](../../../../docs/design/03-file-editing.md)). Every editing bug becomes a case here
before it is fixed.

Each directory is one case:

- `input.yaml`: the file as found, byte for byte.
- `edit.yaml`: the change, as `set` operations applied to the decoded input to build the intended
  document. A path is a list of keys and list indices.

  ```yaml
  # Why this case exists.
  set:
    - path: [disks, 0, size_gb]
      value: 120
  ```

- `want.yaml`: the exact expected output, or `want.error`: a fragment of the expected refusal.

`go test ./internal/engine/edit/yamledit -run TestCorpus -update` rewrites the `want.yaml` files
from the current output. Review the diff before committing it.

Sources: the `examples/gitops-repo/envs/**` files of the prototype.
