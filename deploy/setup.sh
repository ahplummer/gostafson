#!/bin/bash
# One-time (and re-runnable) host setup for gostafson. Run on the host as an
# admin user with sudo; `make setup` copies this directory over and runs it.
#
#   sudo ./setup.sh
#
# What it does, each step skipped when already done:
#   1. Creates the hidden `agent` user and group (no admin rights, no login
#      shell, home /Users/agent with mode 0700).
#   2. Creates the agent's directories: work/napps, logs, var, .marmot-agents.
#   3. Creates /usr/local/libexec/gostafson (owned by the admin user who ran
#      sudo; the agent can read but not change its own binary) and
#      /usr/local/etc/gostafson (owned by root, for config.toml).
#   4. Copies wn-agent into /usr/local/libexec/gostafson (the agent can't read
#      the admin's home, where it's usually installed).
#   5. Moves gostafson's White Noise account from the admin's home to the
#      agent's, with wn-agent stopped. The old directory is renamed, not
#      deleted, so two wn-agents can never run on one account.
#   6. Installs the LaunchDaemons: com.gostafson.wn-agent starts now;
#      com.gostafson.agent starts once a binary is deployed and a config pushed.
#   7. Adds a sudoers rule so the admin user can restart gostafson without a
#      password (used by `make deploy`).
#
# Environment overrides:
#   WN_AGENT_SRC  wn-agent binary to copy (default ~ADMIN/.local/bin/wn-agent)
#   WN_HOME_SRC   White Noise home to move (default ~ADMIN/.marmot-agents/gostafson)
#   SKIP_SUDOERS=1  don't install the sudoers rule

set -euo pipefail

AGENT=agent
AGENT_HOME=/Users/$AGENT
LIBEXEC=/usr/local/libexec/gostafson
ETC=/usr/local/etc/gostafson
DAEMONS=/Library/LaunchDaemons
HERE=$(cd "$(dirname "$0")" && pwd)

say() { printf '\033[38;5;209m==>\033[0m %s\n' "$*"; }
skip() { printf '    %s\n' "$*"; }
die() { printf '\033[38;5;203merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run with sudo: sudo $0"
ADMIN=${SUDO_USER:-}
[ -n "$ADMIN" ] && [ "$ADMIN" != root ] || die "run with sudo from your admin account, not as root directly"
ADMIN_HOME=$(dscl . -read "/Users/$ADMIN" NFSHomeDirectory | awk '{print $2}')
WN_AGENT_SRC=${WN_AGENT_SRC:-$ADMIN_HOME/.local/bin/wn-agent}
WN_HOME_SRC=${WN_HOME_SRC:-$ADMIN_HOME/.marmot-agents/gostafson}
WN_HOME=$AGENT_HOME/.marmot-agents/gostafson

# free_id prints the first ID from 450 up that no user or group uses.
free_id() {
	local id=450
	while dscl . -list /Users UniqueID | awk '{print $2}' | grep -qx "$id" ||
		dscl . -list /Groups PrimaryGroupID | awk '{print $2}' | grep -qx "$id"; do
		id=$((id + 1))
	done
	echo "$id"
}

say "1. agent user and group"
if dscl . -read "/Users/$AGENT" >/dev/null 2>&1; then
	skip "user $AGENT exists (uid $(id -u $AGENT))"
else
	ID=$(free_id)
	dscl . -create "/Groups/$AGENT"
	dscl . -create "/Groups/$AGENT" PrimaryGroupID "$ID"
	dscl . -create "/Groups/$AGENT" RealName "gostafson agent"
	dscl . -create "/Users/$AGENT"
	dscl . -create "/Users/$AGENT" UniqueID "$ID"
	dscl . -create "/Users/$AGENT" PrimaryGroupID "$ID"
	dscl . -create "/Users/$AGENT" RealName "gostafson agent"
	dscl . -create "/Users/$AGENT" NFSHomeDirectory "$AGENT_HOME"
	dscl . -create "/Users/$AGENT" UserShell /usr/bin/false
	dscl . -create "/Users/$AGENT" IsHidden 1
	dscl . -create "/Users/$AGENT" Password '*' # no password: nobody can log in as agent
	skip "created user and group $AGENT (id $ID)"
fi

say "2. agent directories"
install -d -o "$AGENT" -g "$AGENT" -m 700 "$AGENT_HOME"
for d in work work/napps logs var .marmot-agents; do
	install -d -o "$AGENT" -g "$AGENT" -m 700 "$AGENT_HOME/$d"
done
skip "$AGENT_HOME/{work/napps,logs,var,.marmot-agents} (0700, owned by $AGENT)"

say "3. program and config directories"
install -d -o "$ADMIN" -g staff -m 755 "$LIBEXEC"
install -d -o root -g wheel -m 755 "$ETC"
skip "$LIBEXEC (owned by $ADMIN), $ETC (owned by root)"

say "4. wn-agent binary"
if [ -x "$WN_AGENT_SRC" ]; then
	if ! cmp -s "$WN_AGENT_SRC" "$LIBEXEC/wn-agent" 2>/dev/null; then
		install -o "$ADMIN" -g staff -m 755 "$WN_AGENT_SRC" "$LIBEXEC/wn-agent"
		skip "copied $("$LIBEXEC/wn-agent" --version) to $LIBEXEC/wn-agent"
	else
		skip "$LIBEXEC/wn-agent is up to date"
	fi
else
	skip "no wn-agent at $WN_AGENT_SRC; White Noise won't start (set WN_AGENT_SRC)"
fi

say "5. White Noise account"
# The original stays in the admin's home until the very last step (the
# rename), so "original still there" means the move hasn't finished, and an
# interrupted run is simply redone. The copy goes to a staging directory and
# is swapped in only when complete.
if [ -d "$WN_HOME_SRC/accounts" ]; then
	pids=$(pgrep -f "wn-agent --home $WN_HOME_SRC" || true)
	if [ -n "$pids" ]; then
		skip "stopping wn-agent running from $WN_HOME_SRC (pid $pids)"
		kill $pids
		for _ in 1 2 3 4 5 6 7 8 9 10; do pgrep -f "wn-agent --home $WN_HOME_SRC" >/dev/null || break; sleep 1; done
		pgrep -f "wn-agent --home $WN_HOME_SRC" >/dev/null && die "wn-agent from $WN_HOME_SRC didn't stop; stop it and re-run"
	fi
	# A destination account that differs from the original is not a leftover
	# from an interrupted move: never overwrite it.
	for f in "$WN_HOME"/accounts/*/account.json; do
		[ -e "$f" ] || continue
		cmp -s "$f" "$WN_HOME_SRC/${f#"$WN_HOME"/}" ||
			die "$WN_HOME holds a different account than $WN_HOME_SRC; move one aside and re-run"
	done
	rm -f "$WN_HOME_SRC"/dev/*.sock "$WN_HOME_SRC/.marmot-runtime.lock" # stale: wn-agent is stopped; sockets can't be copied
	staging="$WN_HOME.partial"
	rm -rf "$staging"
	ditto "$WN_HOME_SRC" "$staging"
	chown -R "$AGENT:$AGENT" "$staging"
	chmod -R go-rwx "$staging"
	rm -rf "$WN_HOME" # leftover from an interrupted run (checked above)
	mv "$staging" "$WN_HOME"
	backup="$WN_HOME_SRC.moved-to-agent-$(date +%Y%m%d%H%M%S)"
	mv "$WN_HOME_SRC" "$backup"
	skip "moved the account to $WN_HOME; old copy renamed to $backup (delete it once White Noise works)"
elif [ -d "$WN_HOME/accounts" ]; then
	skip "$WN_HOME already has an account"
else
	skip "no account at $WN_HOME_SRC; bootstrap one as $AGENT later (see docs/reference-architecture.md section 10)"
fi

say "6. LaunchDaemons"
load() { # load <label>: (re)load a daemon from its plist
	local label=$1
	install -o root -g wheel -m 644 "$HERE/$label.plist" "$DAEMONS/$label.plist"
	launchctl bootout "system/$label" 2>/dev/null || true
	launchctl enable "system/$label"
	launchctl bootstrap system "$DAEMONS/$label.plist"
}
if [ -x "$LIBEXEC/wn-agent" ] && [ -d "$WN_HOME/accounts" ]; then
	load com.gostafson.wn-agent
	skip "com.gostafson.wn-agent loaded"
else
	skip "com.gostafson.wn-agent not loaded (needs wn-agent and an account)"
fi
if [ -e "$LIBEXEC/current" ] && [ -f "$ETC/config.toml" ]; then
	load com.gostafson.agent
	skip "com.gostafson.agent loaded"
else
	install -o root -g wheel -m 644 "$HERE/com.gostafson.agent.plist" "$DAEMONS/com.gostafson.agent.plist"
	skip "com.gostafson.agent installed, not started: run make push-config, then make deploy (it starts it)"
fi

say "7. sudoers rule for restarts"
if [ "${SKIP_SUDOERS:-}" = 1 ]; then
	skip "skipped (SKIP_SUDOERS=1)"
else
	rule=$(mktemp)
	cat >"$rule" <<-EOF
		# Installed by gostafson's setup.sh: lets $ADMIN restart or start gostafson without a password.
		$ADMIN ALL=(root) NOPASSWD: /bin/launchctl kickstart -k system/com.gostafson.agent, /bin/launchctl bootstrap system $DAEMONS/com.gostafson.agent.plist
	EOF
	if visudo -cf "$rule" >/dev/null; then
		install -o root -g wheel -m 440 "$rule" /etc/sudoers.d/gostafson
		skip "/etc/sudoers.d/gostafson"
	else
		die "generated sudoers rule failed visudo -c; not installed"
	fi
	rm -f "$rule"
fi

say "done"
cat <<EOF
    Next, from the dev Mac:
      make push-config    install config.host.toml as $ETC/config.toml
      make push-secrets   install keys and tokens as $AGENT_HOME/secrets.toml
      make deploy         copy the binary and start com.gostafson.agent
    Logs: make logs (audit), make service-log (stderr); status: make service-status
EOF
