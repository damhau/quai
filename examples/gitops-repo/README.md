# Example repository

An existing Terraform repository with Quai definitions added: the demo (the
compose stack seeds it into Gitea) and the fixture of the backend's tests and the
end-to-end scenarios.

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
- `pipelines/`: Azure Pipelines that plan `envs/prod` on pull requests (as build
  validation) and apply it on main, each reporting to Quai with the `quai report`
  step template of `deploy/runners/azure-pipelines`.

Quai edits the YAML files and nothing else.
