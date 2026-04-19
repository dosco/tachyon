#!/usr/bin/env bash
# bench/gcloud-up.sh — manage the tachyon-dev GCE spot instance.
#
# The instance lives in ~/.config/gcloud's default project. Spot provisioning
# means Google can preempt it at any time (with 30s notice). On preemption
# the VM is STOPPED (not deleted), so bringing it back up is fast.
#
# Usage:
#   bench/gcloud-up.sh status      # show instance state
#   bench/gcloud-up.sh start       # start if stopped (no-op if running)
#   bench/gcloud-up.sh stop        # stop without deleting (preserves disk)
#   bench/gcloud-up.sh delete      # destroy instance (but keep boot disk? no — full delete)
#   bench/gcloud-up.sh recreate    # delete + create (use when config drifts)
#   bench/gcloud-up.sh ssh         # ssh in
#   bench/gcloud-up.sh ip          # print external IP
set -euo pipefail

NAME=tachyon-dev
ZONE=us-central1-a
MACHINE=c4-standard-16
IMAGE_FAMILY=ubuntu-2404-lts-amd64
IMAGE_PROJECT=ubuntu-os-cloud

create() {
  gcloud compute instances create "$NAME" \
    --zone="$ZONE" \
    --machine-type="$MACHINE" \
    --provisioning-model=SPOT \
    --instance-termination-action=STOP \
    --image-family="$IMAGE_FAMILY" \
    --image-project="$IMAGE_PROJECT" \
    --boot-disk-size=50GB \
    --boot-disk-type=hyperdisk-balanced \
    --tags=tachyon-dev \
    --labels=purpose=tachyon-dev \
    --format='value(name,status,networkInterfaces[0].accessConfigs[0].natIP)'
}

status() {
  gcloud compute instances describe "$NAME" --zone="$ZONE" \
    --format='value(status,networkInterfaces[0].accessConfigs[0].natIP)' 2>/dev/null \
    || echo "MISSING"
}

case "${1:-status}" in
  status)   status ;;
  start)    gcloud compute instances start "$NAME" --zone="$ZONE" ;;
  stop)     gcloud compute instances stop  "$NAME" --zone="$ZONE" ;;
  delete)   gcloud compute instances delete "$NAME" --zone="$ZONE" --quiet ;;
  recreate) gcloud compute instances delete "$NAME" --zone="$ZONE" --quiet 2>/dev/null || true; create ;;
  create)   create ;;
  ssh)      shift; exec gcloud compute ssh "$NAME" --zone="$ZONE" -- "$@" ;;
  ip)       gcloud compute instances describe "$NAME" --zone="$ZONE" \
              --format='value(networkInterfaces[0].accessConfigs[0].natIP)' ;;
  *)        echo "unknown: $1" >&2; exit 2 ;;
esac
