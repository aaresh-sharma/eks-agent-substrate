#!/usr/bin/env bash
# Launches the substrate-eks installer: uses the checkout this script sits in
# (or clones the repository), builds the installer with `make build`, and
# hands over to it. Modelled on substrate-gke's install.sh.
#
# This script is a launcher only. Every install step, every AWS call and every
# Kubernetes call is made by the installer, never here.
#
#   ./native/install.sh [installer flags, e.g. --dry-run]
#   curl -fsSL <raw URL of this file> | bash
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/csantanapr/eks-agent-substrate.git}"
REPO_BRANCH="${REPO_BRANCH:-main}"

# The Go the installer needs. The installer's own go.mod asks for 1.26, and
# its control plane step runs Agent Substrate's ate-setup, whose go.mod (at
# v1.0.0) asks for 1.27. The install runs Go with GOTOOLCHAIN=local, so an
# older Go cannot fetch a newer toolchain by itself.
GO_MIN_MAJOR=1
GO_MIN_MINOR=27

# check_go stops with the fix when the Go found in dir, the directory the
# build runs in, is older than GO_MIN_MAJOR.GO_MIN_MINOR. It asks from dir
# because version managers (goenv, asdf, mise) pick the version per directory.
check_go() {
    local dir="$1" found re='go([0-9]+)\.([0-9]+)'
    found="$(cd "${dir}" && GOTOOLCHAIN=local go env GOVERSION 2>/dev/null || true)"
    if [[ ! "${found}" =~ ${re} ]]; then
        echo "warning: could not read the Go version ('${found}'); the build needs Go ${GO_MIN_MAJOR}.${GO_MIN_MINOR} or newer." >&2
        return 0
    fi
    local major="${BASH_REMATCH[1]}" minor="${BASH_REMATCH[2]}"
    if (( major > GO_MIN_MAJOR || (major == GO_MIN_MAJOR && minor >= GO_MIN_MINOR) )); then
        return 0
    fi
    echo "error: the substrate-eks installer needs Go ${GO_MIN_MAJOR}.${GO_MIN_MINOR} or newer; found ${found} ($(command -v go))." >&2
    echo "  Install it from https://go.dev/dl/, with your package manager (macOS: brew install go)," >&2
    echo "  or with your Go version manager, then run this again." >&2
    exit 1
}

main() {
    # Piped in (curl ... | bash), the wizard still needs the keyboard. Hand
    # /dev/tty to the installer alone: bash reads this script from the pipe,
    # so redirecting bash's own stdin would make it read the rest of the
    # script from the terminal. Opening the tty proves it is usable; the
    # node exists without a controlling terminal in CI and containers.
    local tty_in=""
    if [ ! -t 0 ] && (exec < /dev/tty) 2>/dev/null; then
        tty_in="/dev/tty"
    fi

    local tool
    for tool in git go make; do
        if ! command -v "${tool}" >/dev/null 2>&1; then
            echo "error: '${tool}' is required to build the substrate-eks installer." >&2
            exit 1
        fi
    done

    # The native/ directory to build: this script's own, the current
    # directory's, or a clone.
    local script_dir="" native_dir
    if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
        script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    fi
    if [ -n "${script_dir}" ] && [ -f "${script_dir}/installer/main.go" ]; then
        native_dir="${script_dir}"
    elif [ -f "./native/installer/main.go" ]; then
        native_dir="$(pwd)/native"
    else
        local target_dir="${SUBSTRATE_EKS_DIR:-$HOME/.substrate-eks}"
        if [ -d "${target_dir}/.git" ]; then
            echo "==> Updating ${target_dir}..."
            git -C "${target_dir}" fetch --quiet origin "${REPO_BRANCH}"
            git -C "${target_dir}" checkout --quiet "${REPO_BRANCH}"
            git -C "${target_dir}" pull --quiet --ff-only origin "${REPO_BRANCH}" || true
        else
            echo "==> Cloning ${REPO_URL} into ${target_dir}..."
            mkdir -p "$(dirname "${target_dir}")"
            git clone --quiet --depth 1 --branch "${REPO_BRANCH}" "${REPO_URL}" "${target_dir}"
        fi
        native_dir="${target_dir}/native"
    fi

    # The same Go defaults as native/Makefile, for the build and for the
    # `go run` steps the installer starts: the pinned local toolchain, and
    # modules fetched straight from their source.
    export GOTOOLCHAIN="${GOTOOLCHAIN:-local}" GOPROXY="${GOPROXY:-direct}" GOSUMDB="${GOSUMDB:-off}"

    check_go "${native_dir}"

    echo "==> Building the substrate-eks installer in ${native_dir}..."
    make -C "${native_dir}" build

    echo "==> Launching the substrate-eks installer..."
    if [ -n "${tty_in}" ]; then
        exec "${native_dir}/bin/substrate-eks" "$@" < "${tty_in}"
    fi
    exec "${native_dir}/bin/substrate-eks" "$@"
}

main "$@"
