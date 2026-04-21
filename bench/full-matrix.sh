#!/usr/bin/env bash
# bench/full-matrix.sh — one-shot end-to-end matrix used to regenerate
# docs/throughput-bars.svg. Prints a single machine-parseable block at
# the bottom for the SVG generator.
#
# Scenarios:
#   H1 keep c=256       (plain HTTP; all three proxies)
#   TLS H1              (nginx-tls + tachyon-tls; Pingora bench has no TLS)
#   TLS H2              (nginx-tls + tachyon-tls)
#   POST 1 KB body      (wrk2; all three)
#   POST 64 KB body     (wrk2; all three)
#   H3 throughput       (h2load-h3; nginx-h3 + tachyon-h3; Pingora has no H3)
#   TLS resume rate     (bench/resume-probe; nginx-tls + tachyon-tls)
set -uo pipefail
cd ~/tachyon
exec > >(tee /tmp/full-matrix.out) 2>&1

pgrep -f '\./origin' >/dev/null \
  || (setsid nohup ./origin -addr :9000 -size 1024 </dev/null >/tmp/origin.log 2>&1 &)
sleep 0.5

# Self-signed P-256 cert (shared by tachyon-tls + nginx-tls).
if [[ ! -f /tmp/bench-tls.crt ]]; then
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
    -keyout /tmp/bench-tls.key -out /tmp/bench-tls.crt \
    -days 365 -nodes -subj '/CN=bench' 2>/dev/null
fi

cleanup() {
  pkill -9 -f h2load 2>/dev/null || true
  pkill -9 -f wrk2   2>/dev/null || true
  pkill -f pingora-bench-proxy 2>/dev/null || true
  pkill -f '^\./tachyon'       2>/dev/null || true
  sudo nginx -s quit 2>/dev/null || true
  sleep 1.2
}
wait_port() { for _ in $(seq 1 80); do ss -lnt 2>/dev/null | grep -q ":$1 " && return 0; sleep 0.1; done; return 1; }

start_nginx()      { bash bench/proxies/nginx.start     >/tmp/p.log 2>&1 && wait_port 8080; }
start_nginx_tls()  { bash bench/proxies/nginx-tls.start >/tmp/p.log 2>&1 && wait_port 8443; }
start_nginx_h3()   { bash bench/proxies/nginx-h3.start  >/tmp/p.log 2>&1 && wait_port 8443; }
start_pingora()    { bash bench/proxies/pingora.start   >/tmp/p.log 2>&1 & sleep 2; wait_port 8080; }
start_tach()       { setsid nohup ./tachyon -config intent/ -workers "$(nproc)" </dev/null >/tmp/p.log 2>&1 & wait_port 8080; }
start_tach_tls()   { bash bench/proxies/tachyon-tls.start >/tmp/p.log 2>&1 && wait_port 8443; }
start_tach_h3()    { bash bench/proxies/tachyon-h3.start  >/tmp/p.log 2>&1 && wait_port 8443; }

# rps_h2load <args...>  -> prints bare integer RPS
rps_h2load() {
  local out
  out=$(timeout 60 h2load "$@" 2>&1)
  echo "$out" | awk '/finished in/ {for(i=1;i<=NF;i++)if($i=="req/s,")print $(i-1)}' | tr -d ','
}
# rps_wrk2 <script> <c> <rate>  -> prints bare integer RPS
rps_wrk2() {
  local script=$1 conns=$2 rate=$3 out
  out=$(timeout 70 wrk2 -t4 -c"$conns" -d60s -R"$rate" -s "$script" http://127.0.0.1:8080/ 2>&1)
  echo "$out" | awk '/Requests\/sec:/ {print $2}'
}
# rps_h3 <conns> <streams> <total> -> bare integer RPS via h2load-h3
rps_h3() {
  local conns=$1 streams=$2 total=$3 out
  out=$(timeout 90 h2load-h3 --alpn-list h3 \
    -n "$total" -c "$conns" -m "$streams" \
    "https://127.0.0.1:8443/" 2>&1)
  echo "$out" | awk '/finished in/ {for(i=1;i<=NF;i++)if($i=="req/s,")print $(i-1)}' | tr -d ','
}
# resume_rate -> bare float (0..1) from bench/resume-probe
resume_rate() {
  local out
  out=$(timeout 30 ./resume-probe -addr 127.0.0.1:8443 -n 200 2>/dev/null || true)
  echo "$out" | awk -F'[:,}]' '/resume_rate/ {for(i=1;i<=NF;i++)if($i~/resume_rate/){gsub(/[^0-9.]/,"",$(i+1));print $(i+1);exit}}'
}

declare -A R   # R[proxy_scenario]=rps

echo "=== $(nproc) cpus, $(uname -r) ==="

# ---- Plain H1 (keep c=256, all three) ------------------------------------
echo; echo "--- plain HTTP (h2load c=256 n=1M) ---"
for p in nginx pingora tachyon; do
  cleanup
  case $p in
    nginx)   start_nginx   || continue ;;
    pingora) start_pingora || continue ;;
    tachyon) start_tach    || continue ;;
  esac
  rps=$(rps_h2load -n 1000000 -c 256 --h1 http://127.0.0.1:8080/)
  R[${p}_h1]=$rps
  printf '  %-10s rps=%s\n' "$p" "$rps"
done

# ---- TLS H1 + TLS H2 (nginx-tls, tachyon-tls) ----------------------------
echo; echo "--- HTTPS H1 + H2 (h2load, c=256 n=500k) ---"
for p in nginx-tls tachyon-tls; do
  cleanup
  case $p in
    nginx-tls)    start_nginx_tls   || continue ;;
    tachyon-tls)  start_tach_tls    || continue ;;
  esac
  h1=$(rps_h2load -n 500000 -c 256 --h1 https://127.0.0.1:8443/)
  h2=$(rps_h2load -n 500000 -c 256 -m 10 https://127.0.0.1:8443/)
  R[${p}_tlsh1]=$h1
  R[${p}_tlsh2]=$h2
  printf '  %-12s tls-h1=%s  tls-h2=%s\n' "$p" "$h1" "$h2"
done

# ---- POST small + large (wrk2 via plain :8080) ---------------------------
echo; echo "--- POST 1KB + 64KB (wrk2, rate capped) ---"
for p in nginx pingora tachyon; do
  cleanup
  case $p in
    nginx)   start_nginx   || continue ;;
    pingora) start_pingora || continue ;;
    tachyon) start_tach    || continue ;;
  esac
  ps=$(rps_wrk2 bench/post-small.lua 256 50000)
  pl=$(rps_wrk2 bench/post-large.lua  64  5000)
  R[${p}_postsm]=$ps
  R[${p}_postlg]=$pl
  printf '  %-10s post-small=%s  post-large=%s\n' "$p" "$ps" "$pl"
done

# ---- H3 throughput (h2load-h3, nginx-h3 + tachyon-h3) --------------------
# h2load-h3 is the H3-capable h2load built by bench/install-h2load-h3.sh.
# If it's missing we skip the row rather than block the whole matrix.
if command -v h2load-h3 >/dev/null; then
  echo; echo "--- H3 throughput (h2load-h3 c=64 m=32 n=200k) ---"
  for p in nginx-h3 tachyon-h3; do
    cleanup
    case $p in
      nginx-h3)   start_nginx_h3 || continue ;;
      tachyon-h3) start_tach_h3  || continue ;;
    esac
    r=$(rps_h3 64 32 200000)
    R[${p}_h3]=${r:-NA}
    printf '  %-12s h3=%s\n' "$p" "${r:-NA}"
  done
else
  echo "  (h2load-h3 missing — run bench/install-h2load-h3.sh to populate the H3 row)"
fi

# ---- TLS resume rate (bench/resume-probe, nginx-tls + tachyon-tls) -------
# Builds the probe if needed. Ratio = fraction of DidResume==true over 200
# fresh TCP connections. Phase-A target: tachyon ≥ 0.95 on multi-worker.
if [[ ! -x ./resume-probe ]]; then
  (cd bench/resume-probe && go build -o ../../resume-probe .) || true
fi
if [[ -x ./resume-probe ]]; then
  echo; echo "--- TLS resume rate (resume-probe n=200) ---"
  for p in nginx-tls tachyon-tls; do
    cleanup
    case $p in
      nginx-tls)   start_nginx_tls || continue ;;
      tachyon-tls) start_tach_tls  || continue ;;
    esac
    rr=$(resume_rate)
    R[${p}_resume]=${rr:-NA}
    printf '  %-12s resume_rate=%s\n' "$p" "${rr:-NA}"
  done
else
  echo "  (resume-probe build failed — skipping resume row)"
fi

cleanup

# ---- Emit the summary block the SVG generator consumes -------------------
echo
echo "===SUMMARY==="
for k in nginx_h1 pingora_h1 tachyon_h1 \
         nginx-tls_tlsh1 tachyon-tls_tlsh1 \
         nginx-tls_tlsh2 tachyon-tls_tlsh2 \
         nginx_postsm pingora_postsm tachyon_postsm \
         nginx_postlg pingora_postlg tachyon_postlg \
         nginx-h3_h3 tachyon-h3_h3 \
         nginx-tls_resume tachyon-tls_resume; do
  printf '%s=%s\n' "$k" "${R[$k]:-NA}"
done
echo "===END==="
