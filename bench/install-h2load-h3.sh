#!/usr/bin/env bash
# bench/install-h2load-h3.sh — build h2load with HTTP/3 support on the
# GCP Ubuntu 24.04 bench box.
#
# Ubuntu's packaged h2load is built without HTTP/3, and the packaged
# ngtcp2 (0.12) is older than h2load 1.64 requires (>= 1.4).
# So we build ngtcp2 + nghttp3 + nghttp2 from source against gnutls,
# then build h2load alone from the nghttp2 app tree linked against
# those three. Total: ~5 minutes on a 16-vCPU box.
#
# Installs to /usr/local/bin/h2load-h3. The distro "h2load" stays
# intact so the H1/H2 matrix rows keep working.
set -euxo pipefail

if command -v h2load-h3 >/dev/null && h2load-h3 --version 2>&1 | grep -q 'HTTP/3'; then
  echo "h2load-h3 already present: $(h2load-h3 --version | head -1)"
  exit 0
fi

sudo apt-get update
sudo apt-get install -y --no-install-recommends \
  build-essential pkg-config ca-certificates \
  libev-dev libjansson-dev zlib1g-dev \
  libgnutls28-dev libc-ares-dev libevent-dev libsystemd-dev libxml2-dev \
  cmake autoconf automake libtool git

PREFIX=/usr/local
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"

# --- nghttp3 --------------------------------------------------------------
git clone --depth 1 --branch v1.6.0 https://github.com/ngtcp2/nghttp3.git
cd nghttp3
git submodule update --init --depth 1
autoreconf -i
./configure --prefix="$PREFIX" --enable-lib-only
make -j"$(nproc)"
sudo make install
cd ..

# --- ngtcp2 (crypto_gnutls) ----------------------------------------------
git clone --depth 1 --branch v1.8.1 https://github.com/ngtcp2/ngtcp2.git
cd ngtcp2
git submodule update --init --depth 1
autoreconf -i
./configure --prefix="$PREFIX" --enable-lib-only --with-gnutls
make -j"$(nproc)"
sudo make install
cd ..

# --- nghttp2 (libs for h2load linkage staging) ---------------------------
git clone --depth 1 --branch v1.64.0 https://github.com/nghttp2/nghttp2.git nghttp2-lib
cd nghttp2-lib
autoreconf -i
./configure --prefix=/tmp/nghttp2-prefix --enable-lib-only --disable-shared
make -j"$(nproc)"
make install
cd ..

sudo ldconfig

# --- h2load (app-only build) ---------------------------------------------
git clone --depth 1 --branch v1.64.0 https://github.com/nghttp2/nghttp2.git h2load-src
cd h2load-src
autoreconf -i
PKG_CONFIG_PATH="$PREFIX/lib/pkgconfig:/tmp/nghttp2-prefix/lib/pkgconfig" \
  ./configure --enable-app --enable-http3 --with-gnutls \
    --with-libngtcp2 --with-libnghttp3 \
    CPPFLAGS="-I/tmp/nghttp2-prefix/include -I$PREFIX/include" \
    LDFLAGS="-L/tmp/nghttp2-prefix/lib -L$PREFIX/lib -Wl,-rpath,$PREFIX/lib"
make -j"$(nproc)" -C src h2load
sudo install -m 0755 src/h2load "$PREFIX/bin/h2load-h3"

echo
"$PREFIX/bin/h2load-h3" --version
echo "installed: $PREFIX/bin/h2load-h3"
