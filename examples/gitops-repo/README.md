# Example repository

An existing Terraform repository with Quai definitions added: the demo and the
end-to-end fixture of the Go backend. The prototype reads its own copy,
`examples/prototype-gitops-repo`, until it is removed.

- `envs/<env>/vms/*.yaml`: one file per VM. Each environment is a Terraform
  stack (`envs/<env>/main.tf`) that reads them with `fileset()` and
  `yamldecode()` and instantiates `modules/vm` once per file.
- `catalog/schemas/vm.schema.json`: the contract between the VM files and the
  module. CI validates every file against it; Quai generates its form from it.
- `.quai/types/vm.yaml`: what Quai needs to know about the layout: where VM
  files live, which schema they follow, who owns them, which changes need
  approval, and the day-2 actions.
- `blueprints/`: catalog items built from the `vm` type: a single VM sized from
  t-shirt sizes, and a web server with its database server.

Quai edits the YAML files and nothing else.
