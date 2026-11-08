# Pilot types example

The pilot's M2 types and their synthetic data, as they are on the stand-in pilot repository
`pilot-infra` in the Azure DevOps sandbox organization (P3-19,
[#133](https://github.com/damhau/quai/issues/133); pull requests !407 to !410 there). This copy
holds only what Quai reads: the Terraform modules the schemas follow, the stacks and the pipelines
are in `pilot-infra`. The backend's tests lint it, and request each type through pull requests on
Gitea.

Each type is an entry of a file shared by many resources, in a different format:

| Type | Locator | File | Approved by |
| --- | --- | --- | --- |
| `namespace` | `entry`, keyed by `name` | `clusters/<cluster>/namespaces.yaml`, a YAML list (`dev-1` in block style, `prod-1` in flow style) | `platform-admins`: a create, a delete, any change on a `prod-` cluster, a quota that grows |
| `namespace-quota` | `field` `quota` of the same entries | the same | `platform-admins`, always; any developer may ask |
| `dns-record` | `entry` of a map | `stacks/dns/<zone>/records.auto.tfvars`, an HCL map (multi-line, and one line per entry) | `network`, always; `security` too for a CNAME leaving the `.internal` zones |
| `firewall-rule` | `entry`, keyed by `name` | `stacks/firewall/<env>/rules.auto.tfvars.json`, a JSON list | `security`, always; `network` too in `prod` |

In the stacks, a namespace and its quota are two Terraform resources
(`module.namespace["<name>"].terraform_data.namespace` and `…terraform_data.quota`), so that
`quai lint --plan` gives each change to exactly one of the two types.
