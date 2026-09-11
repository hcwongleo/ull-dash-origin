# Isolate chunked-body handling AT THE ORIGIN, bypassing CloudFront entirely.
cd /tmp
dd if=/dev/urandom of=/tmp/ref.bin bs=64k count=12 2>/dev/null
S=$(wc -c < /tmp/ref.bin)
echo "reference: $S bytes"

echo "--- A. Content-Length PUT (baseline) ---"
curl -s -o /dev/null -X PUT --data-binary @/tmp/ref.bin http://127.0.0.1:9094/a.mp4
curl -s http://127.0.0.1:9094/a.mp4 -o /tmp/a.out
echo "   got $(wc -c </tmp/a.out); $(cmp -s /tmp/ref.bin /tmp/a.out && echo IDENTICAL || echo DIFFERS)"

echo "--- B. chunked via -T - , curl chunks naturally ---"
curl -s -o /dev/null -X PUT -T - http://127.0.0.1:9094/b.mp4 < /tmp/ref.bin
curl -s http://127.0.0.1:9094/b.mp4 -o /tmp/b.out
echo "   got $(wc -c </tmp/b.out); $(cmp -s /tmp/ref.bin /tmp/b.out && echo IDENTICAL || echo DIFFERS)"

echo "--- C. chunked with an explicit Transfer-Encoding header added ---"
curl -s -o /dev/null -X PUT -H 'Transfer-Encoding: chunked' -T - http://127.0.0.1:9094/c.mp4 < /tmp/ref.bin
curl -s http://127.0.0.1:9094/c.mp4 -o /tmp/c.out
echo "   got $(wc -c </tmp/c.out); $(cmp -s /tmp/ref.bin /tmp/c.out && echo IDENTICAL || echo DIFFERS)"
head -c 16 /tmp/c.out | od -c | head -2
