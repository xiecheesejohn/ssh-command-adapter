#!/bin/sh
# Build from this checkout and install without modifying shell or SSH config.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(dirname -- "$script_dir")
install_dir=${INSTALL_DIR:-"$HOME/.local/bin"}
build_dir=
staged=

cleanup() {
    if [ -n "$build_dir" ]; then rm -rf -- "$build_dir"; fi
    if [ -n "$staged" ]; then rm -f -- "$staged"; fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

case $(uname -s) in
    Darwin|Linux) ;;
    *) printf '%s\n' 'Supported platforms: macOS and Linux.' >&2; exit 1 ;;
esac
if ! command -v go >/dev/null 2>&1; then
    printf '%s\n' 'Go 1.26+ is required to build. Install Go, then run this script again.' >&2
    exit 1
fi

build_dir=$(mktemp -d "${TMPDIR:-/tmp}/ssh-command-adapter-install.XXXXXX")
(
    cd "$project_dir"
    go build -trimpath -o "$build_dir/ssh-command-adapter" .
)
mkdir -p -- "$install_dir"
install_dir=$(CDPATH= cd -- "$install_dir" && pwd)
# Stage in the destination directory, then rename atomically. Re-running the
# installer updates the binary without touching private state or running sessions.
staged=$(mktemp "$install_dir/.ssh-command-adapter.XXXXXX")
install -m 755 "$build_dir/ssh-command-adapter" "$staged"
mv -f -- "$staged" "$install_dir/ssh-command-adapter"
staged=

printf 'Installed: %s/ssh-command-adapter\n' "$install_dir"
case ":${PATH:-}:" in
    *":$install_dir:"*)
        printf '%s\n' 'Next: ssh-command-adapter init --no-client-auth'
        ;;
    *)
        printf '\nAdd this to your shell startup file, then open a new terminal:\n'
        printf '  export PATH="%s:$PATH"\n' "$install_dir"
        printf '\nThen run: ssh-command-adapter init --no-client-auth\n'
        ;;
esac
