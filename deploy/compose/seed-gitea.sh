#!/bin/sh
# Seeds the local Gitea: a quai account with a random password and an access token for Quai,
# the infra-live repository holding examples/gitops-repo, and an empty network repository the
# end-to-end scenarios connect with the registration wizard. Each step runs once. The token is
# left in /run/quai, where run-quai.sh reads it; nothing secret is in the repository.
set -eu
api=http://gitea:3000/api/v1
if [ -s /run/quai/git-token ]; then
	token=$(cat /run/quai/git-token)
else
	password=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
	su-exec git gitea admin user create --config /data/gitea/conf/app.ini --admin \
		--username quai --email quai@localhost --password "$password" --must-change-password=false
	token=$(curl -fsS -u "quai:$password" -H 'Content-Type: application/json' \
		-d '{"name":"quai","scopes":["write:repository","write:user"]}' "$api/users/quai/tokens" |
		sed -n 's/.*"sha1":"\([0-9a-f]*\)".*/\1/p')
	[ -n "$token" ]
	# Readable by Quai's user, whose group is root.
	(umask 027 && printf '%s' "$token" >/run/quai/git-token)
fi
exists() { curl -fs -o /dev/null -H "Authorization: token $token" "$api/repos/quai/$1"; }

if ! exists infra-live; then
	curl -fsS -H "Authorization: token $token" -H 'Content-Type: application/json' \
		-d '{"name":"infra-live","default_branch":"main","private":true}' "$api/user/repos" >/dev/null
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
if ! exists network; then
	curl -fsS -H "Authorization: token $token" -H 'Content-Type: application/json' \
		-d '{"name":"network","default_branch":"main","private":true,"auto_init":true,"readme":"Default"}' "$api/user/repos" >/dev/null
	echo "Seeded quai/network"
fi
