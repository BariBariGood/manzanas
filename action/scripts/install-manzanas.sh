#!/bin/bash
# Download the manzanas CLI from GitHub Release artifacts and put it on PATH.
#
# Pinning strategy: workflows should pin `manzanas-version` to an exact tag
# (e.g. v0.2.0) so runs are reproducible; "latest" resolves the newest
# release at run time and should only be used for experimentation.
set -euo pipefail

VERSION="${MANZANAS_VERSION:-latest}"
REPO="BariBariGood/manzanas"

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64)        ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac

# ${AUTH[@]+...} guard: bash 3.2 (macOS /bin/bash) treats an empty array as
# unbound under `set -u`; `if` instead of `&&` so `set -e` survives an empty
# token.
AUTH=()
if [[ -n "${GH_TOKEN:-}" ]]; then
  AUTH=(-H "Authorization: Bearer $GH_TOKEN")
fi

if [[ "$VERSION" == "latest" ]]; then
  VERSION="$(curl -fsSL ${AUTH[@]+"${AUTH[@]}"} \
    "https://api.github.com/repos/$REPO/releases/latest" \
    | jq -r .tag_name)"
fi
BARE="${VERSION#v}"

TARBALL="manzanasd_${BARE}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"
echo "downloading $BASE/$TARBALL"
TMP="$(mktemp -d)"
curl -fsSL ${AUTH[@]+"${AUTH[@]}"} "$BASE/$TARBALL" -o "$TMP/$TARBALL"

# Verify against the release's published checksums.txt.
curl -fsSL ${AUTH[@]+"${AUTH[@]}"} "$BASE/checksums.txt" -o "$TMP/checksums.txt"
(cd "$TMP" && grep " $TARBALL\$" checksums.txt | shasum -a 256 -c -)

tar -C "$TMP" -xzf "$TMP/$TARBALL" manzanas

DEST="$RUNNER_TEMP/manzanas-bin"
mkdir -p "$DEST"
install -m 0755 "$TMP/manzanas" "$DEST/manzanas"
echo "$DEST" >> "$GITHUB_PATH"
"$DEST/manzanas" --version || true
