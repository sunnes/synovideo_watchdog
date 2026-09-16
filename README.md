# synovideo_watchdog

Detect whether Synology Surveillance Station is providing a usable screenshot
from a camera's live H.265 `MixStream`.

## Table of contents

- [synovideo\_watchdog](#synovideo_watchdog)
  - [Table of contents](#table-of-contents)
  - [Project history and implementations](#project-history-and-implementations)
  - [Synology failure being detected](#synology-failure-being-detected)
  - [Configuration](#configuration)
    - [Go watchdog settings](#go-watchdog-settings)
  - [Python reference application](#python-reference-application)
    - [Requirements and setup](#requirements-and-setup)
    - [Usage](#usage)
  - [Go watchdog](#go-watchdog)
    - [Build](#build)
    - [Synology watchdog example](#synology-watchdog-example)

## Project history and implementations

The stream format and the sporadic Synology failure were researched with the
Python application in [`app.py`](app.py). The Python implementation remains the
reference extractor: it logs in to Surveillance Station, opens the live H.265
WebSocket stream, decodes a keyframe, and saves it as a timestamped PNG.

That behavior was ported to [`go_watchdog`](go_watchdog/) so the health check can
run directly on the Synology storage without installing Python, PyAV, CGo, or
FFmpeg. The Go implementation mirrors the Python extractor's payload filtering,
transport detection, decoder-error handling, and keyframe-only capture. It does
not save a screenshot unless debugging is enabled; its primary interface is the
process exit code.

## Synology failure being detected

A healthy H.265 stream must provide decoder initialization data—the Video
Parameter Set (VPS), Sequence Parameter Set (SPS), and Picture Parameter Set
(PPS)—followed by a usable intra random access point (IRAP/keyframe).

The sporadic Synology failure leaves the `MixStream` WebSocket open and may
continue sending messages or binary data, but it stops providing a complete,
decodable H.265 sequence. Required VPS/SPS/PPS data or the following usable
keyframe is absent or no longer delivered in the expected framing. Checking
only that the WebSocket is connected or that bytes are arriving would therefore
incorrectly report the stream as healthy.

The Go watchdog succeeds only after it decodes a keyframe, encodes it as PNG,
and verifies that the PNG is larger than `WATCHDOG_MIN_BYTES`. If Synology keeps
the socket open without the required video data, the attempt times out and the
watchdog exits non-zero. The check detects the inability to obtain a valid
screenshot; the exact missing NAL unit can vary between failure occurrences.

## Configuration

Both implementations read connection settings from `.env`:

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `SYNO_HOST` | yes | — | Synology host and port, without a scheme |
| `SYNO_USER` | yes | — | Surveillance Station username |
| `SYNO_PASSWORD` | yes | — | Surveillance Station password |
| `SYNO_CAMERA` | yes | — | Camera name |
| `SYNO_PROFILE` | no | `0` | Stream profile: `0` main, `1` sub-stream |
| `SYNO_TIMEOUT` | no | `30` | Seconds to wait for a decodable frame |
| `SYNO_VERIFY_SSL` | no | `true` | Set to `false` for a self-signed certificate |

Copy the example and set the connection details:

```sh
cp .env.example .env
```

### Go watchdog settings

| Variable | Default | Description |
| --- | --- | --- |
| `WATCHDOG_ATTEMPTS` | `2` | Capture attempts before returning failure |
| `WATCHDOG_MIN_BYTES` | `10240` | PNG must be strictly larger than this size |
| `WATCHDOG_DEBUG_SCREENSHOT` | empty | Optional path at which to save the validated PNG |

The watchdog exits `0` when any capture attempt succeeds and exits non-zero
when all attempts fail. It does not execute a restart or recovery command.

## Python reference application

### Requirements and setup

- Python 3.9+
- A reachable Synology NAS running Surveillance Station

```sh
python3 -m venv venv
source venv/bin/activate
pip install -r requirements.txt
```

### Usage

```sh
python app.py
```

The screenshot is saved as
`screenshot_<camera>_<timestamp>.png` in `SYNO_OUTPUT_DIR`, which defaults to
the current directory.

Python-specific settings and command-line overrides:

| Environment variable | Default | Command-line override |
| --- | --- | --- |
| `SYNO_OUTPUT_DIR` | `.` | `--output-dir` |
| `SYNO_HOST` | — | `--host` |
| `SYNO_USER` | — | `--user` |
| `SYNO_PASSWORD` | — | `--password` |
| `SYNO_CAMERA` | — | `--camera` |
| `SYNO_PROFILE` | `0` | `--profile` |
| `SYNO_TIMEOUT` | `30` | `--timeout` |
| `SYNO_VERIFY_SSL` | `true` | `--no-verify-ssl` |

Example:

```sh
python app.py --camera "Back Yard" --timeout 60
```

## Go watchdog

Run from the repository root so `.env` resolves correctly:

```sh
./go_watchdog/go_watchdog
```

Save the validated screenshot while debugging:

```sh
./go_watchdog/go_watchdog --debug-screenshot watchdog-debug.png
```

### Build

Run these commands from the `go_watchdog` directory.

Linux ARM64, including ARM-based Synology systems:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
  -trimpath -ldflags="-s -w" -o go_watchdog-linux-arm64 .
```

macOS ARM64 (Apple Silicon):

```sh
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build \
  -trimpath -ldflags="-s -w" -o go_watchdog-darwin-arm64 .
```

Linux AMD64:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -trimpath -ldflags="-s -w" -o go_watchdog-linux-amd64 .
```

macOS AMD64 (Intel):

```sh
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build \
  -trimpath -ldflags="-s -w" -o go_watchdog-darwin-amd64 .
```

Copy the binary for the target system as `go_watchdog` and make it executable:

```sh
cp go_watchdog-linux-arm64 go_watchdog
chmod +x go_watchdog
```

### Synology watchdog example

The Go program only reports the check result through its exit code. The wrapper
script decides what recovery action to perform when the check fails.

`/var/services/homes/andy/watchdog.sh`:

```bash
#!/bin/bash

LOCATION="/var/services/homes/andy"
cd "${LOCATION}" || exit 1

./go_watchdog
if [ $? -ne 0 ]; then
  TIMESTAMP=$(date '+%Y-%m-%d %H:%M:%S')
  echo "${TIMESTAMP} restarting SurveillanceVideoExtension" >> "${LOCATION}/restarts.log"
  systemctl restart pkgctl-SurveillanceVideoExtension.service
fi
```

Make the wrapper executable:

```sh
chmod +x /var/services/homes/andy/watchdog.sh
```

`/etc/cron.d/synovideo.watchdog.task` runs the check every five minutes as
`root`:

```cron
*/5 * * * * root /var/services/homes/andy/watchdog.sh
```
