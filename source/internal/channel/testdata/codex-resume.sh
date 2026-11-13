#!/bin/sh
set -eu
umask 077
fail() { printf '%s\n' "$1" >&2; exit 64; }
t=${1:?usage: codex-resume THREAD-UUID [codex args...]}
shift
case "$t" in ????????-????-????-????-????????????) ;; *) exit 64 ;; esac
hex=$(printf '%s' "$t" | tr -d '-')
case "$hex" in *[!0-9a-f]*) exit 64 ;; esac
[ "${#hex}" -eq 32 ] || exit 64
unset CODEX_THREAD_ID
embedded=0 remote= remote_count=0 expect=
for a do
    if [ "$expect" = remote ]; then
        [ "$a" != -- ] && [ -n "$a" ] || fail 'codex-resume --remote requires an endpoint'
        remote=$a expect=
        continue
    fi
    if [ "$expect" = value ]; then expect=; continue; fi
    case "$a" in
        --) break ;;
        --last|--last=*|--all|--all=*|--include-non-interactive|--include-non-interactive=*)
            fail 'codex-resume requires its exact thread UUID; picker selectors are not supported' ;;
        --remote|--remote=*)
            remote_count=$((remote_count + 1))
            [ "$remote_count" -eq 1 ] || fail 'codex-resume accepts only one --remote'
            case "$a" in
                --remote) expect=remote ;;
                *) remote=${a#--remote=}; [ -n "$remote" ] || fail 'codex-resume --remote requires an endpoint' ;;
            esac ;;
        -c|--config|--enable|--disable) embedded=1 expect=value ;;
        -c*|--config*|--enable*|--disable*|--search*|--no-daemon*|--dangerously-bypass-hook-trust*) embedded=1 ;;
    esac
done
[ -z "$expect" ] || fail 'codex-resume option requires a value'
if [ "$embedded" -eq 1 ] && [ "$remote_count" -ne 0 ]; then
    fail 'codex-resume cannot combine a remote daemon with embedded-only options; run codex resume <UUID> ... directly (first-prompt behavior)'
fi
case "$(uname -s)" in Darwin) system=darwin ;; Linux) system=linux ;; *) exit 64 ;; esac
if [ "$embedded" -eq 1 ]; then
    if [ -n "${AGENT_SESSION_BROKER_STATE_DIR:-}" ]; then
        state=$AGENT_SESSION_BROKER_STATE_DIR
    elif [ "$system" = darwin ]; then
        state="$HOME/Library/Application Support/agent-session-broker"
    else
        state="${XDG_CONFIG_HOME:-$HOME/.config}/agent-session-broker"
    fi
    case "$state" in /*) ;; *) exit 64 ;; esac
    d="$state/codex-resume-claims"
    mkdir -p "$d"
    chmod 700 "$d"
    printf '{"v":1,"thread":"%s"}\n' "$t" > "$d/$$.json.tmp"
    mv "$d/$$.json.tmp" "$d/$$.json"
    exec codex resume "$t" "$@"
fi
home=${CODEX_HOME:-$HOME/.codex}
case "$home" in /*) ;; *) home="$PWD/$home" ;; esac
if [ "$remote_count" -ne 0 ]; then
    case "$remote" in
        unix://*) socket=${remote#unix://}; [ -n "$socket" ] || fail 'codex-resume requires an explicit Unix socket path' ;;
        *) exec codex resume "$t" "$@" ;;
    esac
else
    codex app-server daemon start >/dev/null 2>&1 || exec codex resume "$t" "$@"
    socket="$home/app-server-control/app-server-control.sock"
fi
canonical_socket() {
    candidate=$socket
    if [ -L "$candidate" ]; then
        link=$(readlink "$candidate") || return 1
        case "$link" in /*) candidate=$link ;; *) candidate="$(dirname "$candidate")/$link" ;; esac
    fi
    [ ! -L "$candidate" ] && [ -S "$candidate" ] || return 1
    directory=$(CDPATH= cd -P "$(dirname "$candidate")" && pwd -P) || return 1
    canonical="$directory/$(basename "$candidate")"
    if [ "$system" = darwin ]; then
        inode=$(stat -f '%i' "$canonical" 2>/dev/null) || return 1
    else
        inode=$(stat -c '%i' "$canonical" 2>/dev/null) || return 1
    fi
    case "$inode" in ''|*[!0-9]*) return 1 ;; esac
    [ "$inode" != 0 ]
}
tries=0
until canonical_socket; do
    [ "$tries" -lt 50 ] || exec codex resume "$t" "$@"
    tries=$((tries + 1))
    sleep .1
done
helper_socket=$socket
case "$helper_socket" in /*) ;; *) helper_socket="$PWD/$helper_socket" ;; esac
npx -y "@xelexis/x-broker@${XB_VERSION:-1.2.1}" codex-resume-helper \
    --thread "$t" --pid "$$" --codex-home "$home" --socket "$helper_socket" --inode "$inode" \
    </dev/null >/dev/null 2>&1 &
# Rotate the original positional arguments; never join or eval caller input.
remaining=$#
set -- "$@" __xbroker_args_end
parsing=1 added=$remote_count
while [ "$remaining" -gt 0 ]; do
    a=$1; shift
    remaining=$((remaining - 1))
    if [ "$parsing" -eq 1 ]; then
        case "$a" in
            --)
                if [ "$added" -eq 0 ]; then set -- "$@" --remote "unix://$socket"; added=1; fi
                parsing=0 ;;
        esac
    fi
    set -- "$@" "$a"
done
shift
if [ "$added" -eq 0 ]; then set -- "$@" --remote "unix://$socket"; fi
exec codex resume "$t" "$@"
