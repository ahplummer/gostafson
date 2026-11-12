#!/bin/bash
# Prints the host's secrets.toml to stdout from environment variables set in
# this shell (bwset ...). Used by `make push-secrets`; nothing is written to
# disk here. Every run replaces the whole file on the host, so set every
# variable the host needs before running it.
set -euo pipefail

n=0
for pair in OPENROUTER_API_KEY=openrouter GOSTAFSON_TELEGRAM_TOKEN=telegram_token GOSTAFSON_WEB_TOKEN=web_token; do
	var=${pair%%=*}
	key=${pair#*=}
	val=${!var:-}
	if [ -z "$val" ]; then
		echo "  $var not set: $key left out" >&2
		continue
	fi
	case $val in
	*[\"\\]* | *$'\n'*)
		echo "$var contains a quote, backslash or newline" >&2
		exit 1
		;;
	esac
	printf '%s = "%s"\n' "$key" "$val"
	echo "  $var -> $key" >&2
	n=$((n + 1))
done
[ "$n" -gt 0 ] || {
	echo "none of the variables are set; run bwset for each first" >&2
	exit 1
}
