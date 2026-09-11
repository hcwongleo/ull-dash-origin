#!/bin/bash
# Continuous production-shape load: the real ladder from the live MPD.
#   video  1080p50 HEVC ~763 KB / 2.0 s
#   audio  2 x 96 kbps  ~24 KB / 2.0 s
# Plus a live-edge reader per segment, and a half-open connection every ~5 min to
# keep exercising the C3 abort path under load.
O=http://127.0.0.1:9094
A=http://127.0.0.1:9095
dd if=/dev/urandom of=/tmp/v.bin bs=763k count=1 2>/dev/null
dd if=/dev/urandom of=/tmp/a.bin bs=24k count=1 2>/dev/null
P=$(pgrep -f go-chunked-streaming-server | head -1)
: > /tmp/soak.csv
echo "elapsed_s,segs,rss_kB,goroutines,fds,resident_bytes,files,expired_too_soon,put_aborted" >> /tmp/soak.csv
START=$(date +%s); N=0
while true; do
  N=$((N+1))
  SEQ=$(printf "%09d" $N)
  for trk in v-fhd a1 a2; do
    case $trk in v-fhd) F=/tmp/v.bin;; *) F=/tmp/a.bin;; esac
    curl -s -o /dev/null -X PUT -H 'Cache-Control: max-age=21600' -H 'Content-Type: video/mp4' \
      --data-binary @$F "$O/ch18_live/ch18live-46-${trk}-20260911T000000_${SEQ}.mp4" &
  done
  sleep 0.3
  curl -s -o /dev/null --max-time 5 "$O/ch18_live/ch18live-46-v-fhd-20260911T000000_${SEQ}.mp4" &
  printf '<MPD timeShiftBufferDepth="PT12S" minimumUpdatePeriod="PT2S"/>' \
    | curl -s -o /dev/null -X PUT -H 'Cache-Control: max-age=2' -T - "$O/ch18_live/ch18live.mpd"
  if [ $((N % 150)) -eq 0 ]; then python3 -c "
import socket
s=socket.create_connection(('127.0.0.1',9094))
s.sendall(b'PUT /soak_halfopen.mp4 HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nAAAAA\r\n')
" 2>/dev/null & fi
  if [ $((N % 30)) -eq 0 ]; then
    M=$(curl -s $A/-/metrics)
    g() { echo "$M" | awk -v k="$1" '$1==k{print $2}'; }
    echo "$(( $(date +%s) - START )),$N,$(awk '/VmRSS/{print $2}' /proc/$P/status),$(g go_goroutines),$(ls /proc/$P/fd | wc -l),$(g gcss_resident_bytes),$(g gcss_files),$(g gcss_expired_too_soon_total),$(g gcss_put_aborted_total)" >> /tmp/soak.csv
  fi
  wait
  sleep 1.6
done
