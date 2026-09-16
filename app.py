#!/usr/bin/env python3
"""
Extract a timestamped screenshot from a Synology Surveillance Station
H.265 MixStream. Configuration is read from a .env file and can be
overridden with command-line arguments.

Setup:
    cp .env.example .env
    # edit .env with your host, credentials and camera name
    pip install -r requirements.txt
    python extract_screenshot.py

Any setting can be overridden per-run, e.g.:
    python extract_screenshot.py --camera "Back Yard" --timeout 60
"""

from __future__ import annotations

import argparse
import datetime
import os
import struct
import sys
import threading
from dataclasses import dataclass
from typing import Optional
from urllib.parse import urlencode

import av
import requests
import urllib3
import websocket
from dotenv import load_dotenv

# ---------------------------------------------------------------------------
# Config from .env
# ---------------------------------------------------------------------------

load_dotenv()


def _str_to_bool(raw: str) -> bool:
    return raw.strip().lower() not in ("0", "false", "no", "off")


def build_arg_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Extract a screenshot from a Synology Surveillance Station camera stream. "
        "Any option can also be set via .env; a command-line flag overrides the .env value.",
    )
    parser.add_argument("--host", help="Synology host:port (overrides SYNO_HOST)")
    parser.add_argument("--user", help="Username (overrides SYNO_USER)")
    parser.add_argument("--password", help="Password (overrides SYNO_PASSWORD)")
    parser.add_argument("--camera", help="Camera name (overrides SYNO_CAMERA)")
    parser.add_argument(
        "--profile", type=int, help="Stream profile: 0=main, 1=sub (overrides SYNO_PROFILE)"
    )
    parser.add_argument(
        "--timeout", type=int, help="Seconds to wait for a frame (overrides SYNO_TIMEOUT)"
    )
    parser.add_argument(
        "--output-dir", help="Directory to write screenshots into (overrides SYNO_OUTPUT_DIR)"
    )
    parser.add_argument(
        "--no-verify-ssl",
        dest="verify_ssl",
        action="store_false",
        default=None,
        help="Disable SSL verification, e.g. for self-signed certs (overrides SYNO_VERIFY_SSL)",
    )
    return parser


@dataclass
class Config:
    host: str
    user: str
    password: str
    camera: str
    profile: int
    timeout: int
    output_dir: str
    verify_ssl: bool

    @classmethod
    def from_env_and_args(cls, args: argparse.Namespace) -> "Config":
        def pick(arg_value, env_key: str, default: Optional[str] = None) -> Optional[str]:
            if arg_value is not None:
                return str(arg_value)
            return os.getenv(env_key, default)

        required = {
            "SYNO_HOST": pick(args.host, "SYNO_HOST"),
            "SYNO_USER": pick(args.user, "SYNO_USER"),
            "SYNO_PASSWORD": pick(args.password, "SYNO_PASSWORD"),
            "SYNO_CAMERA": pick(args.camera, "SYNO_CAMERA"),
        }
        missing = [k for k, v in required.items() if not v]
        if missing:
            raise SystemExit(
                f"Missing required config: {', '.join(missing)}\n"
                f"Set them in .env (copy .env.example to .env) or pass as --flags."
            )

        if args.verify_ssl is not None:
            verify_ssl = args.verify_ssl
        else:
            verify_ssl = _str_to_bool(os.getenv("SYNO_VERIFY_SSL", "true"))

        return cls(
            host=required["SYNO_HOST"].strip(),
            user=required["SYNO_USER"].strip(),
            password=required["SYNO_PASSWORD"],
            camera=required["SYNO_CAMERA"].strip(),
            profile=int(pick(args.profile, "SYNO_PROFILE", "0")),
            timeout=int(pick(args.timeout, "SYNO_TIMEOUT", "30")),
            output_dir=pick(args.output_dir, "SYNO_OUTPUT_DIR", ".").strip(),
            verify_ssl=verify_ssl,
        )

# ---------------------------------------------------------------------------
# Synology API helpers
# ---------------------------------------------------------------------------

def syno_login(cfg: Config) -> tuple[str, str]:
    """Login; return (sid, synotoken)."""
    url = f"https://{cfg.host}/webapi/entry.cgi"
    params = {
        "api": "SYNO.API.Auth",
        "version": "6",
        "method": "login",
        "account": cfg.user,
        "passwd": cfg.password,
        "session": "SurveillanceStation",
        "format": "sid",
    }
    r = requests.get(url, params=params, verify=cfg.verify_ssl, timeout=15)
    r.raise_for_status()
    data = r.json()

    if not data.get("success"):
        raise RuntimeError(f"Login failed: {data.get('error')}")

    sid = data["data"]["sid"]
    synotoken = data["data"].get("synotoken", "")
    return sid, synotoken


def syno_list_cameras(cfg: Config, sid: str) -> list[dict]:
    url = f"https://{cfg.host}/webapi/entry.cgi"
    params = {
        "api": "SYNO.SurveillanceStation.Camera",
        "version": "8",
        "method": "List",
        "basic": "true",
        "streamInfo": "true",
        "_sid": sid,
    }
    r = requests.get(url, params=params, verify=cfg.verify_ssl, timeout=15)
    r.raise_for_status()
    data = r.json()

    if not data.get("success"):
        raise RuntimeError(f"List cameras failed: {data.get('error')}")

    return data["data"]["cameras"]


def resolve_camera(cameras: list[dict], name: str) -> dict:
    target = name.strip().lower()
    for cam in cameras:
        if cam.get("name", "").strip().lower() == target:
            return cam
    for cam in cameras:
        if target in cam.get("name", "").strip().lower():
            return cam
    available = ", ".join(c.get("name", "?") for c in cameras)
    raise RuntimeError(f"Camera '{name}' not found. Available: {available}")


def syno_logout(cfg: Config, sid: str) -> None:
    try:
        requests.get(
            f"https://{cfg.host}/webapi/entry.cgi",
            params={
                "api": "SYNO.API.Auth",
                "version": "6",
                "method": "logout",
                "session": "SurveillanceStation",
                "_sid": sid,
            },
            verify=cfg.verify_ssl,
            timeout=5,
        )
    except Exception:
        pass

# ---------------------------------------------------------------------------
# WebSocket frame unwrapping
# ---------------------------------------------------------------------------

def unwrap_frame(data: bytes) -> tuple[dict, bytes]:
    if len(data) < 4:
        return {}, data
    hlen = struct.unpack(">I", data[:4])[0]
    if hlen == 0 or hlen > 512 or 4 + hlen > len(data):
        return {}, data
    raw_header = data[4 : 4 + hlen]
    try:
        header_str = raw_header.decode("latin-1")
    except UnicodeDecodeError:
        return {}, data
    if "=" not in header_str:
        return {}, data
    header: dict = {}
    for part in header_str.split("&"):
        if "=" in part:
            k, v = part.split("=", 1)
            header[k] = v
    return header, data[4 + hlen :]

# ---------------------------------------------------------------------------
# fMP4 extractor
# ---------------------------------------------------------------------------

MP4_BOXES = (b"ftyp", b"styp", b"moov", b"moof", b"mdat", b"sidx", b"free", b"skip")
MOOF_CHILDREN = (b"mfhd", b"traf", b"tref", b"mvex", b"udta")


def looks_like_hevc_nal_no_startcode(data: bytes) -> bool:
    if len(data) < 2:
        return False
    nal_type = (data[0] >> 1) & 0x3F
    return nal_type in (32, 33, 34, 39, 40)


def looks_like_annexb(data: bytes) -> bool:
    return data.startswith(b"\x00\x00\x00\x01") or data.startswith(b"\x00\x00\x01")


class Fmp4Extractor:
    def __init__(self, output_path: str):
        self.output_path = output_path
        self.done = False
        self.ctx = av.CodecContext.create("hevc", "r")

    def feed_box(self, payload: bytes) -> bool:
        if len(payload) < 8:
            return False
        name = payload[:4]
        if name == b"mdat":
            return self._handle_mdat(payload[4:])
        if name == b"moof":
            moof_end = self._moof_end(payload)
            if moof_end is None:
                return False
            return self._handle_tail(payload[moof_end:])
        if name in MP4_BOXES:
            return False
        return self._handle_mdat(payload)

    @classmethod
    def _moof_end(cls, payload: bytes) -> Optional[int]:
        off = 4
        n = len(payload)
        last_end = off
        while off + 8 <= n:
            child_size = struct.unpack(">I", payload[off : off + 4])[0]
            child_name = payload[off + 4 : off + 8]
            if child_name not in MOOF_CHILDREN:
                break
            if child_size < 8 or off + child_size > n:
                break
            off += child_size
            last_end = off
        return last_end if last_end > 4 else None

    def _handle_tail(self, tail: bytes) -> bool:
        if len(tail) < 8:
            return False
        size = struct.unpack(">I", tail[:4])[0]
        name = tail[4:8]
        if name == b"mdat":
            body_end = min(size, len(tail)) if size >= 8 else len(tail)
            return self._handle_mdat(tail[8:body_end])
        return self._handle_mdat(tail)

    def _handle_mdat(self, body: bytes) -> bool:
        if not body:
            return False
        converted = self._avcc_to_annexb(body)
        if converted is not None and self._try_decode(converted):
            return True
        return self._try_decode(body)

    def _try_decode(self, data: bytes) -> bool:
        try:
            packets = self.ctx.parse(data)
        except av.AVError:
            return False
        for pkt in packets:
            try:
                frames = self.ctx.decode(pkt)
            except av.AVError:
                continue
            for frame in frames:
                if not frame.key_frame:
                    continue
                frame.to_image().save(self.output_path)
                print(
                    f"[+] Saved {self.output_path} "
                    f"({frame.width}x{frame.height}, key={frame.key_frame})"
                )
                self.done = True
                return True
        return False

    @staticmethod
    def _avcc_to_annexb(data: bytes) -> Optional[bytes]:
        out = bytearray()
        off, n = 0, len(data)
        while off + 4 <= n:
            length = struct.unpack(">I", data[off : off + 4])[0]
            if length == 0 or length > 5_000_000 or off + 4 + length > n:
                return None
            out.extend(b"\x00\x00\x00\x01")
            out.extend(data[off + 4 : off + 4 + length])
            off += 4 + length
        return bytes(out) if out and off == n else None


class AnnexBExtractor:
    def __init__(self, output_path: str):
        self.output_path = output_path
        self.ctx = av.CodecContext.create("hevc", "r")
        self.done = False

    def feed(self, payload: bytes) -> bool:
        try:
            packets = self.ctx.parse(payload)
        except av.AVError:
            return False
        for pkt in packets:
            try:
                frames = self.ctx.decode(pkt)
            except av.AVError:
                continue
            for frame in frames:
                if not frame.key_frame:
                    continue
                frame.to_image().save(self.output_path)
                print(
                    f"[+] Saved {self.output_path} "
                    f"({frame.width}x{frame.height}, key={frame.key_frame})"
                )
                self.done = True
                return True
        return False


class Extractor:
    def __init__(self, output_path: str):
        self.output_path = output_path
        self.backend: Optional[object] = None
        self.done = False

    def handle(self, message) -> None:
        if self.done:
            return
        if isinstance(message, str):
            return
        header, payload = unwrap_frame(message)
        if not header:
            header, payload = {}, message
        if not payload:
            return
        mt = header.get("mediaType") or header.get("mediatype")
        if mt is not None and mt != "1":
            return
        if looks_like_hevc_nal_no_startcode(payload):
            return
        if self.backend is None:
            if len(payload) >= 4 and payload[:4] in MP4_BOXES:
                print("[i] fMP4 transport detected")
                self.backend = Fmp4Extractor(self.output_path)
            elif looks_like_annexb(payload):
                print("[i] raw Annex-B H.265 transport detected")
                self.backend = AnnexBExtractor(self.output_path)
            else:
                return
        if isinstance(self.backend, Fmp4Extractor):
            if self.backend.feed_box(payload):
                self.done = True
        else:
            if self.backend.feed(payload):
                self.done = True

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def make_output_path(cfg: Config, camera_name: str) -> str:
    safe = "".join(c if c.isalnum() or c in "-_" else "_" for c in camera_name)
    ts = datetime.datetime.now().strftime("%Y%m%d_%H%M%S")
    os.makedirs(cfg.output_dir, exist_ok=True)
    return os.path.join(cfg.output_dir, f"screenshot_{safe}_{ts}.png")


def main() -> int:
    args = build_arg_parser().parse_args()
    cfg = Config.from_env_and_args(args)

    if not cfg.verify_ssl:
        urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)
        print("[!] SSL verification DISABLED")

    # 1. Login
    print(f"[i] Logging in to https://{cfg.host} as {cfg.user} …")
    sid, synotoken = syno_login(cfg)
    print(f"[i] Login OK (sid=…{sid[-6:]}, synotoken=…{synotoken[-4:]})")

    # 2. Resolve camera
    print("[i] Fetching camera list …")
    cameras = syno_list_cameras(cfg, sid)
    cam = resolve_camera(cameras, cfg.camera)
    cam_id = int(cam["id"])
    print(f"[i] Camera '{cam['name']}' → id={cam_id}")

    # 3. Build WS URL
    ws_params = {
        "method": "MixStream",
        "blMux": "true",
        "browser": "2",
        "stmSrc": "0",
        "blLiveSharing": "true",
        "blAudio": "false",
        "profile": str(cfg.profile),
        "pause": "false",
        "dsId": "0",
        "SynoToken": synotoken,
        "id": str(cam_id),
    }
    ws_url = f"wss://{cfg.host}/ss_webstream_task/?{urlencode(ws_params)}"

    headers = {
        "Origin": f"https://{cfg.host}",
        "User-Agent": (
            "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
            "AppleWebKit/537.36 (KHTML, like Gecko) "
            "Chrome/150.0.0.0 Safari/537.36"
        ),
        "Accept-Language": "en-US,en;q=0.9",
        "Cache-Control": "no-cache",
        "Pragma": "no-cache",
        "Cookie": f"id={sid}",
    }

    output = make_output_path(cfg, cam["name"])
    print(f"[i] Output: {output}")

    ex = Extractor(output)

    def on_message(ws, message):
        ex.handle(message)
        if ex.done:
            ws.close()

    ws = websocket.WebSocketApp(
        ws_url,
        header=headers,
        on_open=lambda w: print("[+] WebSocket open"),
        on_message=on_message,
        on_error=lambda w, e: print(f"[!] WS error: {e}"),
        on_close=lambda w, c, m: print(f"[-] WS closed: {c} {m}"),
    )

    # websocket-client uses sslopt to control TLS verification.
    # sslopt={"cert_reqs": ssl.CERT_NONE} disables verification.
    import ssl
    sslopt = None if cfg.verify_ssl else {"cert_reqs": ssl.CERT_NONE}

    t = threading.Thread(
        target=lambda: ws.run_forever(sslopt=sslopt),
        daemon=True,
    )
    t.start()
    t.join(timeout=cfg.timeout)

    # 4. Logout (best-effort)
    syno_logout(cfg, sid)

    if not ex.done:
        print(f"[!] Timed out after {cfg.timeout}s.")
        try:
            ws.close()
        except Exception:
            pass
        return 1

    print("[✓] Screenshot saved.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
