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

Shared files, many resources in one file, each a stack of its own directory:

- `clusters/<cluster>/namespaces.yaml`: a YAML list of namespaces, which
  `clusters/<cluster>/main.tf` turns into instances of `modules/namespace`. The
  `namespace` type writes one entry each (`entry` locator); the
  `namespace-quota` type writes only the `quota` of an entry (`field` locator),
  owned by the developers rather than the namespace's owner, so that anyone may
  ask for more.
- `dns/<zone>/records.auto.tfvars`: an HCL map of record sets, instances of
  `modules/dns-record`. The `dns-record` type writes one entry each; the
  `ttl = 24 * 3600` of `shop.internal`'s `shop-db` is an expression, which Quai
  shows as defined in code and leaves as it is.
- `databases/<env>/databases.auto.tfvars.json`: a JSON list of databases,
  instances of `modules/database`. The `database` type's form asks for an engine
  and a size; its `mapping` generator writes the vCPUs, memory and storage from
  the size and reads the size back from the vCPUs.
- `.quai/tests/`: the golden examples `quai lint` runs, the VMs' and, for the
  shared files, a database created and a record set moved beside its TTL
  defined in code.

Quai edits these YAML, HCL and JSON files and nothing else; in a shared file, the
lines of the entry a request changes and nothing else.
