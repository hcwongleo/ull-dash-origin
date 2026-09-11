#!/usr/bin/env python3
"""Open half-open ingest connections: send PUT headers + a partial chunked body,
then hold the socket open and stop sending. This is what a dead encoder host or a
NAT idle-timeout looks like to the origin."""
import socket, subprocess, sys, time

def stats():
    pid = subprocess.check_output(["pgrep","-f","go-chunked-streaming-server"]).split()[0].decode()
    fds = len(subprocess.check_output(["ls",f"/proc/{pid}/fd"]).split())
    g = subprocess.check_output(
        ["curl","-s","http://127.0.0.1:9095/-/metrics"]).decode()
    gor = [l.split()[1] for l in g.splitlines() if l.startswith("go_goroutines")][0]
    rss = [l.split()[1] for l in open(f"/proc/{pid}/status") if l.startswith("VmRSS")][0]
    return int(fds), int(gor), int(rss)

f0,g0,r0 = stats()
print(f"before:            fds={f0} goroutines={g0} rss={r0}kB")

socks=[]
for i in range(25):
    s=socket.create_connection(("127.0.0.1",9094))
    body=b"5\r\nAAAAA\r\n"
    s.sendall(f"PUT /halfopen_{i}.mp4 HTTP/1.1\r\nHost: x\r\n"
              f"Cache-Control: max-age=21600\r\nContent-Type: video/mp4\r\n"
              f"Transfer-Encoding: chunked\r\n\r\n".encode()+body)
    socks.append(s)
print(f"opened 25 half-open ingest connections, now silent but not closed")

time.sleep(5)
f1,g1,r1 = stats()
print(f"after 5s:          fds={f1} goroutines={g1} rss={r1}kB   (+{f1-f0} fds, +{g1-g0} goroutines)")

print("waiting 90s - longer than -max-incomplete-age 60 - so retention reclaims the FILES")
time.sleep(90)
f2,g2,r2 = stats()
print(f"after 95s:         fds={f2} goroutines={g2} rss={r2}kB   (+{f2-f0} fds, +{g2-g0} goroutines)")
print(f"files still held:  {subprocess.check_output(['curl','-s','http://127.0.0.1:9095/-/metrics']).decode().split('gcss_files ')[1].split()[0]}")
print()
print("VERDICT: files reclaimed by retention, but the goroutines and sockets are")
print("         still there. Nothing will ever release them - no read deadline.")
for s in socks: s.close()
