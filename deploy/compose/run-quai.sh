#!/bin/sh
# Runs a quai sub-command with the local stack's configuration, once the seed step has left
# Gitea's token and the webhook secret: the repository connection names them as
# env:QUAI_GITEA_TOKEN, for the API and Git, and env:QUAI_WEBHOOK_SECRET.
set -eu
QUAI_GITEA_TOKEN=$(cat /run/quai/git-token)
QUAI_WEBHOOK_SECRET=$(cat /run/quai/webhook-secret)
export QUAI_GITEA_TOKEN QUAI_WEBHOOK_SECRET
export QUAI_CONFIG=/etc/quai/quai.yaml
exec quai "$@"
