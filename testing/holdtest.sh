# Does -w return as soon as data arrives, or always wait the full expiration?
# This is the difference between "adds up to 1s of latency" and "removes latency".
echo "player earliness -> how long the GET was actually held"
for early in 0.1 0.3 0.6 1.0; do
  KEY="hold_$(echo $early | tr -d .).mp4"
  ( sleep "$early"; dd if=/dev/zero bs=64k count=1 2>/dev/null \
      | curl -s -o /dev/null -X PUT -H 'Cache-Control: max-age=21600' -T - "http://127.0.0.1:9094/$KEY" ) &
  R=$(curl -s -o /dev/null -w '%{http_code} %{time_total} %{size_download}' --max-time 5 "http://127.0.0.1:9094/$KEY")
  echo "  encoder ${early}s late : status/held/bytes = $R"
  wait
done
echo
echo "and when the encoder never comes at all:"
R=$(curl -s -o /dev/null -w '%{http_code} after %{time_total}s' --max-time 5 http://127.0.0.1:9094/never_arrives.mp4)
echo "  $R"
