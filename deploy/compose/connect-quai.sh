#!/bin/sh
# Connects quai/infra-live to Quai through its API, as the platform admin alice of the development
# sign-in, once the server is up: a connection managed in Quai, which an admin changes with the
# API, a repository freeze included (docs/design/01-repository-connections.md). Its document,
# infra-live.json: Gitea's token for the API and Git, the webhook secret and the run token's hash
# are references to the environment of Quai's processes (run-quai.sh); pull requests are merged by
# Quai once approved, since main requires quai/approval (seed-gitea.sh); the fake runner reports
# with the run token whose hash it names. A connection that exists is left as it is. The session
# and its CSRF token stay in this container.
set -eu
quai=http://server:8080
jar=$(mktemp)
trap 'rm -f "$jar"' EXIT
curl -fsS -o /dev/null -b "$jar" -c "$jar" -H 'Content-Type: application/json' -d '{"name":"alice"}' "$quai/auth/dev/login"
if curl -fs -o /dev/null -b "$jar" -c "$jar" "$quai/api/v1/repositories/infra-live"; then
	echo "infra-live is connected"
	exit 0
fi
csrf=$(curl -fsS -b "$jar" -c "$jar" "$quai/api/v1/me" | sed -n 's/.*"csrfToken":"\([^"]*\)".*/\1/p')
[ -n "$csrf" ]
curl -fsS -o /dev/null -b "$jar" -c "$jar" -H 'Content-Type: application/json' -H "X-CSRF-Token: $csrf" \
	--data-binary @/etc/quai/infra-live.json "$quai/api/v1/repositories"
echo "Connected infra-live"
