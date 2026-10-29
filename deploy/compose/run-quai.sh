#!/bin/sh
# Runs a quai sub-command with the local stack's configuration, once the seed step has left
# Gitea's token, the webhook secret and the run token's hash: the repository connection names
# them as env:QUAI_GITEA_TOKEN, for the API and Git, env:QUAI_WEBHOOK_SECRET and
# env:QUAI_RUN_TOKEN_SHA256.
set -eu
QUAI_GITEA_TOKEN=$(cat /run/quai/git-token)
QUAI_WEBHOOK_SECRET=$(cat /run/quai/webhook-secret)
QUAI_RUN_TOKEN_SHA256=$(cat /run/quai/run-token.sha256)
export QUAI_GITEA_TOKEN QUAI_WEBHOOK_SECRET QUAI_RUN_TOKEN_SHA256
export QUAI_CONFIG=/etc/quai/quai.yaml
exec quai "$@"
