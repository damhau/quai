#!/bin/sh
# Runs a quai sub-command with the local stack's configuration, once the seed step has left
# Gitea's token: the repository connection names it as env:QUAI_GITEA_TOKEN, for the API and Git.
set -eu
QUAI_GITEA_TOKEN=$(cat /run/quai/git-token)
export QUAI_GITEA_TOKEN
export QUAI_CONFIG=/etc/quai/quai.yaml
exec quai "$@"
