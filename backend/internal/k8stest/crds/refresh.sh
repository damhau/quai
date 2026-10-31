#!/bin/sh
# Refreshes the CRDs the components render for from their upstream releases, pinned below
# (docs/plan/phase-3-backlog.md, test infrastructure). k8stest installs them in the API server.
# To upgrade one: change its version, run this script, run the tests, commit.
set -eu

ARGOCD=v3.5.6                # github.com/argoproj/argo-cd
FLUX_KUSTOMIZE=v1.10.1       # github.com/fluxcd/kustomize-controller (Flux v2.10)
CROSSPLANE=v2.4.3            # github.com/crossplane/crossplane

cd "$(dirname "$0")"

fetch() { # fetch <file> <url>
	tmp=$(mktemp)
	curl --fail --silent --show-error --location --proto '=https' --output "$tmp" "$2"
	{
		printf '# Vendored from %s\n# by refresh.sh: do not edit.\n' "$2"
		cat "$tmp"
	} >"$1"
	rm -f "$tmp"
}

fetch argocd-application.yaml \
	"https://raw.githubusercontent.com/argoproj/argo-cd/$ARGOCD/manifests/crds/application-crd.yaml"
fetch flux-kustomization.yaml \
	"https://raw.githubusercontent.com/fluxcd/kustomize-controller/$FLUX_KUSTOMIZE/config/crd/bases/kustomize.toolkit.fluxcd.io_kustomizations.yaml"
fetch crossplane-xrd.yaml \
	"https://raw.githubusercontent.com/crossplane/crossplane/$CROSSPLANE/cluster/crds/apiextensions.crossplane.io_compositeresourcedefinitions.yaml"
