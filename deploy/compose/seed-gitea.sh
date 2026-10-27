#!/bin/sh
# Seeds the local Gitea once: a quai account with a random password, an access token for Quai,
# and the infra-live repository holding examples/gitops-repo. The token is left in /run/quai,
# where run-quai.sh reads it; nothing secret is in the repository.
set -eu
if [ -s /run/quai/git-token ]; then
	echo "Gitea is seeded already"
	exit 0
fi
password=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
su-exec git gitea admin user create --config /data/gitea/conf/app.ini --admin \
	--username quai --email quai@localhost --password "$password" --must-change-password=false
api=http://gitea:3000/api/v1
token=$(curl -fsS -u "quai:$password" -H 'Content-Type: application/json' \
	-d '{"name":"quai","scopes":["write:repository","write:user"]}' "$api/users/quai/tokens" |
	sed -n 's/.*"sha1":"\([0-9a-f]*\)".*/\1/p')
[ -n "$token" ]
curl -fsS -H "Authorization: token $token" -H 'Content-Type: application/json' \
	-d '{"name":"infra-live","default_branch":"main","private":true}' "$api/user/repos" >/dev/null
work=$(mktemp -d)
cp -R /seed/gitops-repo/. "$work"
cd "$work"
git init -q -b main
git add -A
git -c user.name="Platform team" -c user.email=platform@localhost commit -qm "the existing infrastructure"
git push -q "http://quai:$token@gitea:3000/quai/infra-live.git" main
# Readable by Quai's user, whose group is root.
umask 027
printf '%s' "$token" >/run/quai/git-token
echo "Seeded quai/infra-live"
