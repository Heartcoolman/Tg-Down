#!/usr/bin/env bash
# Build & install TDLib (libtdjson) at the commit pinned by zelenin/go-tdlib v1.0.0-beta1.
# Installs to a writable custom prefix (no sudo). Used by local dev and CI.
#
#   TDLIB_PREFIX  install prefix            (default: $HOME/.tdlib)
#   TDLIB_COMMIT  tdlib/td commit to build  (default: pinned below)
#   TDLIB_SRC     source checkout dir       (default: $HOME/.cache/tdlib-src)
#   JOBS          parallel build jobs       (default: 4; raise if you have >16GB RAM)
#
# After install, build the Go project with:
#   export CGO_CFLAGS="-I$TDLIB_PREFIX/include"
#   export CGO_LDFLAGS="-Wl,-rpath,$TDLIB_PREFIX/lib -L$TDLIB_PREFIX/lib -ltdjson"
set -euo pipefail

PREFIX="${TDLIB_PREFIX:-$HOME/.tdlib}"
# Pinned to match github.com/zelenin/go-tdlib v1.0.0-beta1 (TDLib 2024-11-27).
TDLIB_COMMIT="${TDLIB_COMMIT:-b498497bbfd6b80c86f800b3546a0170206317d3}"
SRC="${TDLIB_SRC:-$HOME/.cache/tdlib-src}"
JOBS="${JOBS:-4}"

echo ">>> TDLib build: prefix=$PREFIX commit=${TDLIB_COMMIT:0:10} jobs=$JOBS"

OPENSSL_ROOT=""
case "$(uname -s)" in
  Darwin)
    if command -v brew >/dev/null 2>&1; then
      # No '|| true': a failed dep install must surface here, not as a confusing cmake error later.
      brew install gperf cmake openssl@3 >/dev/null
      OPENSSL_ROOT="$(brew --prefix openssl@3)"
    fi
    ;;
  Linux)
    if command -v apt-get >/dev/null 2>&1; then
      sudo apt-get update -y || true # transient mirror failures are non-fatal; the install below still gates
      sudo apt-get install -y make git zlib1g-dev libssl-dev gperf cmake g++
    fi
    ;;
esac

mkdir -p "$SRC"
if [ ! -d "$SRC/.git" ]; then
  git clone https://github.com/tdlib/td.git "$SRC"
fi
cd "$SRC"
git fetch origin "$TDLIB_COMMIT" 2>/dev/null || git fetch --all --tags || true
git checkout -f "$TDLIB_COMMIT"

rm -rf build
mkdir build
cd build
cmake -DCMAKE_BUILD_TYPE=Release \
  ${OPENSSL_ROOT:+-DOPENSSL_ROOT_DIR="$OPENSSL_ROOT"} \
  -DCMAKE_INSTALL_PREFIX="$PREFIX" ..
cmake --build . --target install -j"$JOBS"

echo ">>> TDLib installed to $PREFIX"
ls -la "$PREFIX/lib/"libtdjson* 2>/dev/null || true
cat <<EOF

Done. Build the project with (statically linked against TDLib):
  export CGO_CFLAGS="-I$PREFIX/include${OPENSSL_ROOT:+ -I$OPENSSL_ROOT/include}"
  export CGO_LDFLAGS="-L$PREFIX/lib -Wl,-rpath,$PREFIX/lib${OPENSSL_ROOT:+ -L$OPENSSL_ROOT/lib}"
  go build ./...

Or simply: make build   (the Makefile sets these automatically)
EOF
