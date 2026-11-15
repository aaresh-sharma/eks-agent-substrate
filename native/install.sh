#!/usr/bin/env bash
# Launches the substrate-eks installer, one of two ways. Modelled on
# substrate-gke's install.sh.
#
#   From source (the default): uses the checkout this script sits in (or
#   clones the repository), builds the installer with `make build`, and
#   hands over to it. Needs git, make and Go 1.27 or newer.
#
#   From a release (--from-release [vX.Y.Z], as the first argument; no
#   version means the latest release): downloads the release archive for
#   this OS and CPU from GitHub Releases, with its checksums.txt, refuses
#   the archive unless its sha256 matches, extracts it under
#   ~/.substrate-eks/releases/<version>, and hands over to its substrate-eks.
#   Needs curl and tar, and no git, make or Go to start; the install itself
#   still needs Go 1.27, which the installer's preflight checks (see
#   README.md, "Two ways to run it").
#
# This script is a launcher only. Every install step, every AWS call and every
# Kubernetes call is made by the installer, never here. --from-release and its
# version are this script's; the installer gets the arguments after them.
#
#   ./native/install.sh [installer flags, e.g. --dry-run]
#   ./native/install.sh --from-release [vX.Y.Z] [installer flags]
#   curl -fsSL <raw URL of this file> | bash
#   curl -fsSL <raw URL of this file> | bash -s -- --from-release [vX.Y.Z]
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/csantanapr/eks-agent-substrate.git}"
REPO_BRANCH="${REPO_BRANCH:-main}"
# Where --from-release downloads from: <RELEASES_URL>/download/<version>/.
RELEASES_URL="${RELEASES_URL:-https://github.com/csantanapr/eks-agent-substrate/releases}"

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

# sha256_of prints file's sha256: sha256sum on Linux, shasum on macOS.
sha256_of() {
    local sum
    if command -v sha256sum >/dev/null 2>&1; then
        sum="$(sha256sum "$1")"
    else
        sum="$(shasum -a 256 "$1")"
    fi
    echo "${sum%% *}"
}

# download fetches url to file over https only, redirects included.
download() {
    if ! curl -fsSL --proto '=https' --proto-redir '=https' -o "$2" "$1"; then
        echo "error: could not download $1" >&2
        exit 1
    fi
}

# cleanup_download removes fetch_release's downloads and their directory.
dl_tmp="" dl_archive=""
cleanup_download() {
    if [ -n "${dl_tmp}" ]; then
        rm -f "${dl_tmp}/${dl_archive}" "${dl_tmp}/checksums.txt"
        rmdir "${dl_tmp}" 2>/dev/null || true
        dl_tmp=""
    fi
}

# fetch_release downloads the release archive of version (the latest when
# empty) for this machine, checks it against the release's checksums.txt
# and extracts it to release_dir, which it sets. A missing or different
# checksum stops it before anything is extracted.
release_dir=""
fetch_release() {
    local version="$1" tool os arch
    for tool in curl tar uname mktemp awk; do
        if ! command -v "${tool}" >/dev/null 2>&1; then
            echo "error: '${tool}' is required to download a substrate-eks release." >&2
            exit 1
        fi
    done
    if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
        echo "error: 'sha256sum' or 'shasum' is required to check a substrate-eks release." >&2
        exit 1
    fi
    case "$(uname -s)" in
        Linux) os=linux ;;
        Darwin) os=darwin ;;
        *) echo "error: no substrate-eks release is built for $(uname -s); run without --from-release to build from source." >&2; exit 1 ;;
    esac
    case "$(uname -m)" in
        x86_64 | amd64) arch=amd64 ;;
        arm64 | aarch64) arch=arm64 ;;
        *) echo "error: no substrate-eks release is built for $(uname -m); run without --from-release to build from source." >&2; exit 1 ;;
    esac
    if [ -z "${version}" ]; then
        # GitHub redirects <releases>/latest to <releases>/tag/<latest tag>.
        local latest
        if ! latest="$(curl -fsSL --proto '=https' --proto-redir '=https' -o /dev/null -w '%{url_effective}' "${RELEASES_URL}/latest")"; then
            echo "error: could not find the latest release at ${RELEASES_URL}; name one: --from-release v2.0.0" >&2
            exit 1
        fi
        version="${latest##*/}"
    fi
    # The tag names a directory below, so it is checked before any use.
    local tag_re='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'
    if [[ ! "${version}" =~ ${tag_re} ]]; then
        echo "error: '${version}' is not a substrate-eks release tag such as v2.0.0; name one: --from-release v2.0.0" >&2
        exit 1
    fi

    local archive="substrate-eks_${version#v}_${os}_${arch}.tar.gz"
    local base="${RELEASES_URL}/download/${version}"
    local tmp
    tmp="$(mktemp -d "${TMPDIR:-/tmp}/substrate-eks.XXXXXX")"
    dl_tmp="${tmp}" dl_archive="${archive}"
    # On any exit; exec runs no EXIT trap, so it is also called before it.
    trap cleanup_download EXIT

    echo "==> Downloading ${archive} (${version})..."
    download "${base}/${archive}" "${tmp}/${archive}"
    download "${base}/checksums.txt" "${tmp}/checksums.txt"

    local want got sum_re='^[0-9a-f]{64}$'
    want="$(awk -v f="${archive}" '$2 == f { print $1 }' "${tmp}/checksums.txt")"
    if [[ ! "${want}" =~ ${sum_re} ]]; then
        echo "error: the ${version} checksums.txt has no sha256 line for ${archive}; refusing it." >&2
        exit 1
    fi
    got="$(sha256_of "${tmp}/${archive}")"
    if [ "${got}" != "${want}" ]; then
        echo "error: ${archive} has sha256 ${got}, but the ${version} checksums.txt says ${want}; refusing it." >&2
        exit 1
    fi
    echo "==> sha256 matches checksums.txt: ${want}"

    release_dir="${SUBSTRATE_EKS_DIR:-$HOME/.substrate-eks}/releases/${version}"
    echo "==> Extracting into ${release_dir}..."
    mkdir -p "${release_dir}"
    tar -xzf "${tmp}/${archive}" -C "${release_dir}"
    cleanup_download
    trap - EXIT
}

# launch hands over to the installer binary bin with the remaining args,
# its stdin the terminal (tty_in) when this script is piped in.
tty_in=""
launch() {
    local bin="$1"
    shift
    echo "==> Launching the substrate-eks installer..."
    if [ -n "${tty_in}" ]; then
        exec "${bin}" "$@" < "${tty_in}"
    fi
    exec "${bin}" "$@"
}

main() {
    # Piped in (curl ... | bash), the wizard still needs the keyboard. Hand
    # /dev/tty to the installer alone: bash reads this script from the pipe,
    # so redirecting bash's own stdin would make it read the rest of the
    # script from the terminal. Opening the tty proves it is usable; the
    # node exists without a controlling terminal in CI and containers.
    if [ ! -t 0 ] && (exec < /dev/tty) 2>/dev/null; then
        tty_in="/dev/tty"
    fi

    # The same Go defaults as native/Makefile, for the build and for the
    # `go run` steps the installer starts: the pinned local toolchain, and
    # modules fetched straight from their source.
    export GOTOOLCHAIN="${GOTOOLCHAIN:-local}" GOPROXY="${GOPROXY:-direct}" GOSUMDB="${GOSUMDB:-off}"

    if [ "${1:-}" = "--from-release" ]; then
        shift
        local version=""
        if [ -n "${1:-}" ] && [[ "$1" != -* ]]; then
            version="$1"
            shift
        fi
        fetch_release "${version}"
        launch "${release_dir}/substrate-eks" "$@"
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

    check_go "${native_dir}"

    echo "==> Building the substrate-eks installer in ${native_dir}..."
    make -C "${native_dir}" build

    launch "${native_dir}/bin/substrate-eks" "$@"
}

main "$@"
