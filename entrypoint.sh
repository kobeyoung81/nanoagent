#!/bin/bash
# nanosec agent container entrypoint.
#
# Starts as root: creates (or renames the preinstalled uid-1000 'ubuntu' user
# to) the per-user runtime identity named after the account display name
# (AGENT_USERNAME, sanitized by the runner; fallback "nanosec"), claims the
# home/data-root volume only when ownership is wrong (fresh volumes), grants
# NOPASSWD sudo, then drops to that user and execs nanosec.
set -euo pipefail

NAME="${AGENT_USERNAME:-nanosec}"
# Re-sanitize defensively with the same rules as the runner.
NAME="$(printf '%s' "$NAME" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9_-' '-' | sed -e 's/-\{2,\}/-/g' -e 's/^-*//' -e 's/-*$//' | cut -c1-32)"
case "$NAME" in
  ''|root|daemon|bin|sys|sync|games|man|lp|mail|news|uucp|proxy|www-data|backup|list|irc|gnats|nobody|sshd|messagebus|dbus|systemd-*)
    NAME="nanosec" ;;
esac

uid_target=1000

if ! id -u "$NAME" >/dev/null 2>&1; then
  # Remove the preinstalled uid-1000 'ubuntu' user if it exists, then create
  # the per-user identity at the same uid (fresh volumes/writable home).
  if [ "$(id -u ubuntu 2>/dev/null || echo "")" = "$uid_target" ]; then
    userdel -r ubuntu >/dev/null 2>&1 || true
  fi
  if ! useradd -m -s /bin/bash -u "$uid_target" "$NAME" >/dev/null 2>&1 && [ "$NAME" != "nanosec" ]; then
    NAME="nanosec"
    useradd -m -s /bin/bash -u "$uid_target" nanosec >/dev/null 2>&1 || true
  fi
fi

# NOPASSWD sudo for terminal sessions.
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$NAME" > /etc/sudoers.d/agent
chmod 0440 /etc/sudoers.d/agent

HOME_DIR="/home/$NAME"
mkdir -p "$HOME_DIR"

# Claim the data root (fresh named/bind volume) only when ownership is wrong —
# cheap on restarts, no migration of pre-existing data.
if [ "$(stat -c %u "$HOME_DIR" 2>/dev/null || echo "-1")" != "$uid_target" ]; then
  chown -R "$NAME:$NAME" "$HOME_DIR" >/dev/null 2>&1 || true
fi

cp -n /etc/skel/.bashrc /etc/skel/.profile "$HOME_DIR"/ >/dev/null 2>&1 || true
# Seed the dircolors database (see Dockerfile): the skel ~/.bashrc evals
# ~/.dircolors when present, giving ls --color=auto real colors (dirs, links,
# files). Copy without clobbering user customizations.
cp -n /usr/share/nanosec/dircolors "$HOME_DIR/.dircolors" >/dev/null 2>&1 || true
# The copies above run as root; hand them to the runtime user so the shell can
# edit them (the guarded recursive chown below otherwise skips a restart).
chown "$NAME:$NAME" "$HOME_DIR"/.bashrc "$HOME_DIR"/.profile "$HOME_DIR"/.dircolors >/dev/null 2>&1 || true

# Neutralise a legacy per-home nvm install, once.
#
# The image provides nvm system-wide (/usr/local/nvm, loaded via
# /etc/profile.d/nvm.sh), but a home seeded before that carries its own ~/.nvm
# and a ~/.bashrc that points NVM_DIR at it. ~/.bashrc is sourced after
# /etc/profile.d, so it wins — interactive shells then run a different Node
# than the agent process, in the same container.
#
# Only the export line needs disabling: with NVM_DIR unset, the two guarded
# `[ -s "$NVM_DIR/..." ]` lines that follow are already no-ops. The comment
# marker makes this idempotent (the line no longer starts with `export`), and
# the file is handed back to the runtime user because sed -i rewrites it as
# root.
if [ -d "$HOME_DIR/.nvm" ] && grep -qE '^[[:space:]]*export NVM_DIR=.*\.nvm' "$HOME_DIR/.bashrc" 2>/dev/null; then
  sed -i -E 's|^([[:space:]]*)export NVM_DIR=.*\.nvm.*$|\1# [nanosec] disabled: nvm is system-wide at /usr/local/nvm|' "$HOME_DIR/.bashrc"
  chown "$NAME:$NAME" "$HOME_DIR/.bashrc" 2>/dev/null || true
  printf 'entrypoint: disabled the per-home nvm override in %s/.bashrc\n' "$HOME_DIR" >&2
fi

export HOME="$HOME_DIR"
cd "$HOME_DIR"

exec gosu "$NAME" nanosec "$@"