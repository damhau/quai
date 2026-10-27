#!/bin/sh
# Runs a quai sub-command with the local stack's configuration, once the seed step has left
# Gitea's token: the token goes into the repository's URL, the only way Quai takes Git
# credentials for now.
set -eu
token=$(cat /run/quai/git-token)
sed "s/@GIT_TOKEN@/$token/" /etc/quai/quai.yaml >/tmp/quai.yaml
export QUAI_CONFIG=/tmp/quai.yaml
exec quai "$@"
