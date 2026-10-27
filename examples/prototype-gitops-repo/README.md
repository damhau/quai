# Example GitOps repository

Quai seeds this into an empty repository on first start. It contains two
independent things, to show both integration styles:

- `blueprints/` + `deployments/`: Kubernetes-native blueprints. Quai renders
  manifests (KubeVirt, Crossplane, Argo CD) and Argo CD reconciles them.
- `envs/<env>/vms/*.yaml` + `.quai/types/vm.yaml`: a Terraform repository that
  already exists. Each environment is a stack (`envs/<env>/main.tf`) that reads
  one YAML file per VM. Quai edits those files and nothing else; its knowledge
  of the layout lives in `.quai/types/vm.yaml`, and the file contract in
  `catalog/schemas/vm.schema.json`, which CI can validate with as well.
