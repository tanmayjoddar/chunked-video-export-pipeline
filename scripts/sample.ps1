# Generates a deterministic 1080p sample with ffmpeg inside the api container
# (dev machine has no local ffmpeg). Uses testsrc if no input is provided.
param([int]$Seconds = 30)
$ErrorActionPreference = "Stop"
$out = "D:\pipe\testdata\sample-1080p.mp4"
New-Item -ItemType Directory -Force "D:\pipe\testdata" | Out-Null
# render inside the api image (ffmpeg pinned there) to /data (named volume; NOT /tmp —
# docker cp cannot copy from tmpfs mounts) then copy out
docker compose exec -T api ffmpeg -y -nostdin -f lavfi -i "testsrc=size=1920x1080:rate=30:duration=$Seconds" -f lavfi -i "sine=frequency=440:duration=$Seconds" -c:v libx264 -preset veryfast -crf 20 -pix_fmt yuv420p -g 48 -keyint_min 48 -sc_threshold 0 -c:a aac -b:a 128k -ar 48000 -ac 2 -threads 1 -fflags +bitexact -flags:v +bitexact -map_metadata -1 /data/sample-1080p.mp4
docker compose cp api:/data/sample-1080p.mp4 "$out"
$h = Get-FileHash "$out" -Algorithm SHA256
Write-Output "sample: $out"
Write-Output "sha256: $($h.Hash)"
Write-Output "bytes: $((Get-Item $out).Length)"
