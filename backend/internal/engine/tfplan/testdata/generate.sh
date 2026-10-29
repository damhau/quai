#!/usr/bin/env bash
# Regenerates the recorded plans (docs/plan/phase-2-backlog.md#test-infrastructure) with the
# Terraform on the PATH. Tests never run Terraform; they read these files. Only the built-in
# terraform_data resource is used: no provider is downloaded and no network is reached.
#
#   prod.json    examples/gitops-repo's envs/prod stack, applied as it is, then planned with a
#                VM resized (web01), one added (api01), one removed (db01) and one replaced
#                (legacy-ftp, with -replace).
#   nested.json  nested/ planned from nothing: VMs in a module inside a module, a sensitive
#                input, unknown values, and a resource no Quai type claims.
#   nested-update.json
#                nested/ applied, then planned with another password: an update whose
#                changed attributes include a sensitive one.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
examples=$(cd "$here/../../../../../examples/gitops-repo" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export CHECKPOINT_DISABLE=1 TF_IN_AUTOMATION=1 TF_INPUT=0

plan() { # dir out [plan args]
	local dir=$1 out=$2
	shift 2
	terraform -chdir="$dir" plan -no-color -out=plan.bin "$@" >/dev/null
	terraform -chdir="$dir" show -json plan.bin | jq --sort-keys 'del(.timestamp)' > "$here/$out"
}

mkdir -p "$work/envs"
cp -r "$examples/modules" "$work/modules"
cp -r "$examples/envs/prod" "$work/envs/prod"
terraform -chdir="$work/envs/prod" init -no-color >/dev/null
terraform -chdir="$work/envs/prod" apply -no-color -auto-approve >/dev/null
sed -i 's/^cpu: .*/cpu: 8/' "$work/envs/prod/vms/web01.yaml"
cp "$work/envs/prod/vms/web01.yaml" "$work/envs/prod/vms/api01.yaml"
rm "$work/envs/prod/vms/db01.yaml"
plan "$work/envs/prod" prod.json '-replace=module.vm["legacy-ftp"].terraform_data.vm'

cp -r "$here/nested" "$work/nested"
terraform -chdir="$work/nested" init -no-color >/dev/null
plan "$work/nested" nested.json
terraform -chdir="$work/nested" apply -no-color -auto-approve >/dev/null
plan "$work/nested" nested-update.json -var=admin_password=fixture-new-secret

terraform version -json | jq -r '"Generated with Terraform " + .terraform_version'
