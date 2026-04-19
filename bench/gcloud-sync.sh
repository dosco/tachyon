#!/usr/bin/env bash
# bench/gcloud-sync.sh — sync the local tachyon tree to the GCE dev VM and
# (optionally) build it there.
#
# Why tar-over-scp instead of rsync: gcloud compute scp wraps ssh with IAP/OS
# Login quirks that rsync-over-ssh doesn't always navigate cleanly. A one-shot
# tar ship is simpler and the tree is ~40 KiB of source.
#
# Usage:
#   bench/gcloud-sync.sh              # sync only
#   bench/gcloud-sync.sh --build      # sync + go build ./...
#   bench/gcloud-sync.sh --smoke      # sync + build + Phase 0 smoke test
set -euo pipefail

NAME=tachyon-dev
ZONE=us-central1-a
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TARBALL=/tmp/tachyon-src.tgz

cd "$REPO_ROOT"

# Build the tarball. List directories explicitly so we don't drag in build
# artifacts or .git. (BSD tar's --exclude matches anywhere in path, which
# bit us once already — do not use --exclude=tachyon with cmd/tachyon.)
rm -f "$TARBALL"
tar czf "$TARBALL" \
  --exclude='.git' \
  --exclude='results' \
  README.md go.mod go.sum config.yaml config.yaml.example \
  cmd buf http1 http2 iouring internal tlsutil metrics bench docs

echo "sending $(du -h "$TARBALL" | cut -f1) -> $NAME:$ZONE..."
gcloud compute scp --zone="$ZONE" "$TARBALL" "$NAME:/tmp/tachyon-src.tgz" >/dev/null

remote_cmd='set -e
mkdir -p ~/tachyon
# Extract over the existing tree rather than wiping it — this preserves
# compiled artifacts (Cargo target/, Go binaries) that live inside the dir.
tar xzf /tmp/tachyon-src.tgz -C ~/tachyon 2>/dev/null
echo "sync OK: $(find ~/tachyon -name \*.go | wc -l) .go files extracted"
'

case "${1:-}" in
  --build)
    remote_cmd+='
cd ~/tachyon
go build ./...
go build -o tachyon ./cmd/tachyon
go build -o origin ./bench/origin
echo "build OK"
'
    ;;
  --smoke)
    remote_cmd+='
cd ~/tachyon
go build -o tachyon ./cmd/tachyon
go build -o origin ./bench/origin
pkill -f "^./origin" 2>/dev/null || true
pkill -f "^./tachyon" 2>/dev/null || true
sleep 1
./origin -addr :9000 -size 1024 > /tmp/origin.log 2>&1 &
sleep 0.5
./tachyon -config config.yaml -workers 1 > /tmp/tachyon.log 2>&1 &
sleep 0.5
echo "--- 100 sequential ---"
for i in $(seq 1 100); do curl -sS -o /dev/null -w "%{http_code}\n" -H "Host: example.com" http://127.0.0.1:8080/; done | sort | uniq -c
echo "--- 500 parallel (-P 50) ---"
seq 1 500 | xargs -P 50 -I{} curl -sS -o /dev/null -w "%{http_code}\n" -H "Host: example.com" http://127.0.0.1:8080/ | sort | uniq -c
pkill -f "^./tachyon" 2>/dev/null || true
pkill -f "^./origin" 2>/dev/null || true
true
'
    ;;
esac

ssh -o StrictHostKeyChecking=no vr@"$(gcloud compute instances describe "$NAME" --zone="$ZONE" --format='value(networkInterfaces[0].accessConfigs[0].natIP)')" "bash -s" <<ENDSSH
$remote_cmd
ENDSSH
