#!/bin/sh
# Seeds the local Gitea: a quai account with a random password and an access token for Quai,
# the infra-live repository holding examples/gitops-repo (public, so that its pull requests can
# be read without signing in), its main protected so that pull
# requests need quai/approval, its webhook to Quai, and an empty network repository the
# end-to-end scenarios connect with the registration wizard. Each step runs once. The token and
# the webhook secret are left in /run/quai, where run-quai.sh reads them; nothing secret is in
# the repository.
set -eu
api=http://gitea:3000/api/v1
if [ -s /run/quai/git-token ]; then
	token=$(cat /run/quai/git-token)
else
	password=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
	su-exec git gitea admin user create --config /data/gitea/conf/app.ini --admin \
		--username quai --email quai@localhost --password "$password" --must-change-password=false
	token=$(curl -fsS -u "quai:$password" -H 'Content-Type: application/json' \
		-d '{"name":"quai","scopes":["write:repository","write:issue","write:user"]}' "$api/users/quai/tokens" |
		sed -n 's/.*"sha1":"\([0-9a-f]*\)".*/\1/p')
	[ -n "$token" ]
	# Readable by Quai's user, whose group is root.
	(umask 027 && printf '%s' "$token" >/run/quai/git-token)
fi
if [ ! -s /run/quai/webhook-secret ]; then
	(umask 027 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >/run/quai/webhook-secret)
fi
exists() { curl -fs -o /dev/null -H "Authorization: token $token" "$api/repos/quai/$1"; }

if ! exists infra-live; then
	curl -fsS -H "Authorization: token $token" -H 'Content-Type: application/json' \
		-d '{"name":"infra-live","default_branch":"main","private":false}' "$api/user/repos" >/dev/null
	work=$(mktemp -d)
	cp -R /seed/gitops-repo/. "$work"
	(
		cd "$work"
		git init -q -b main
		git add -A
		git -c user.name="Platform team" -c user.email=platform@localhost commit -qm "the existing infrastructure"
		git push -q "http://quai:$token@gitea:3000/quai/infra-live.git" main
	)
	echo "Seeded quai/infra-live"
fi
# Enforcement: merging into main needs quai/approval; only Quai pushes to it directly (the
# designer publishes blueprints that way).
if ! exists infra-live/branch_protections/main; then
	curl -fsS -H "Authorization: token $token" -H 'Content-Type: application/json' \
		-d '{"rule_name":"main","enable_push":true,"enable_push_whitelist":true,"push_whitelist_usernames":["quai"],"enable_status_check":true,"status_check_contexts":["quai/approval"]}' \
		"$api/repos/quai/infra-live/branch_protections" >/dev/null
	echo "Protected quai/infra-live's main"
fi
# The webhook: Gitea tells Quai about pushes, pull requests and statuses at once; polling is the
# safety net.
if ! curl -fsS -H "Authorization: token $token" "$api/repos/quai/infra-live/hooks" | grep -q 'server:8080/webhooks/infra-live'; then
	secret=$(cat /run/quai/webhook-secret)
	curl -fsS -H "Authorization: token $token" -H 'Content-Type: application/json' \
		-d "{\"type\":\"gitea\",\"active\":true,\"events\":[\"push\",\"pull_request\",\"status\"],\"config\":{\"url\":\"http://server:8080/webhooks/infra-live\",\"content_type\":\"json\",\"secret\":\"$secret\"}}" \
		"$api/repos/quai/infra-live/hooks" >/dev/null
	echo "Added quai/infra-live's webhook"
fi
if ! exists network; then
	curl -fsS -H "Authorization: token $token" -H 'Content-Type: application/json' \
		-d '{"name":"network","default_branch":"main","private":true,"auto_init":true,"readme":"Default"}' "$api/user/repos" >/dev/null
	echo "Seeded quai/network"
fi
