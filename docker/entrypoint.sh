#!/bin/sh
# The container's main program, run by tini.
#
# With arguments it runs one steel command and exits, with no desktop:
#   docker compose run --rm steel config set model gpt-4.1-mini
# Without, it starts the desktop, then becomes steel. The desktop's
# programs are restarted whenever they exit; steel isn't. When steel exits
# the container stops, and compose's restart policy starts a fresh one.
set -eu
cd "$HOME"

# A fresh volume gets a config that wires the browser in. An existing one
# is the user's: never touch it.
mkdir -p .steel
[ -e .steel/config.json ] || cp /usr/local/share/steel/config.json .steel/config.json

if [ $# -gt 0 ]; then
	exec steel "$@"
fi

# keep NAME CMD... runs CMD in the background, again whenever it exits.
keep() {
	name=$1
	shift
	(
		while :; do
			"$@" >/dev/null 2>&1 && status=0 || status=$?
			echo "steel-desktop: $name exited (status $status); restarting" >&2
			sleep 2
		done
	) &
}

# A restarted container keeps /tmp, and with it the last X server's lock.
rm -f /tmp/.X0-lock /tmp/.X11-unix/X0
keep xvfb Xvfb :0 -screen 0 "${STEEL_SCREEN:-1280x800}x24" -nolisten tcp
i=0
until [ -S /tmp/.X11-unix/X0 ]; do
	i=$((i + 1))
	if [ "$i" -gt 100 ]; then
		echo "steel-desktop: the X server didn't start" >&2
		exit 1
	fi
	sleep 0.1
done

keep openbox openbox

vnc_auth=-nopw
if [ -n "${STEEL_VNC_PASSWORD:-}" ]; then
	mkdir -p .vnc
	x11vnc -storepasswd "$STEEL_VNC_PASSWORD" .vnc/passwd >/dev/null 2>&1
	vnc_auth="-rfbauth $HOME/.vnc/passwd"
fi
# shellcheck disable=SC2086 # vnc_auth is one or two words on purpose
keep x11vnc x11vnc -display :0 -rfbport 5900 -localhost -forever -shared $vnc_auth
keep novnc websockify --web /usr/share/novnc 6080 127.0.0.1:5900

# Chromium's sandbox needs user namespaces, which Docker's default seccomp
# profile denies; the container is the sandbox here. Its profile is on the
# volume, so logins you make through noVNC are the agent's too. The
# singleton lock names the last container's processes, now gone. Stopping
# the container kills Chromium, so skip the "Restore pages?" bubble it
# would show after every restart.
keep chromium sh -c 'rm -f "$HOME"/.config/chromium/Singleton*; exec chromium \
	--no-sandbox --disable-dev-shm-usage --no-first-run --no-default-browser-check \
	--hide-crash-restore-bubble \
	--password-store=basic --start-maximized \
	--user-data-dir="$HOME/.config/chromium" --remote-debugging-port=9222'

exec steel
