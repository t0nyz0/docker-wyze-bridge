"""Optional Lake receiver: one camera/session per container, no account secrets.

Transport groundwork: alphabin/ClaudeWyze4Sln. Receiver design reference:
mitchross/homelab-images. Credits and dependency terms: THIRD_PARTY_NOTICES.md.
"""

import argparse
import base64
import json
import os
import queue
import re
import selectors
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path

STOP = threading.Event()


def report(event, **fields):
    print(json.dumps(dict(event=event, **fields)), flush=True)


class ConfigError(ValueError):
    pass


class ViewerError(RuntimeError):
    pass


def number(env, key, default, low, high):
    try:
        value = int(env.get(key, str(default)))
    except ValueError:
        raise ConfigError(key + " must be an integer") from None
    if not low <= value <= high:
        raise ConfigError(key + " is out of range")
    return value


@dataclass(frozen=True)
class Settings:
    camera: str
    stream: str
    bridge: str
    runtime: Path
    quality: str
    preview: bool
    width: int
    height: int
    fps: int
    rtsp_port: int
    api_port: int
    session_seconds: int
    renew_seconds: int

    @classmethod
    def load(cls, env=os.environ):
        camera = env.get("LAKE_CAMERA", "")
        stream = env.get("LAKE_STREAM", camera)
        for value in (camera, stream):
            if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", value):
                raise ConfigError("LAKE_CAMERA and LAKE_STREAM must be normalized lowercase names")
        bridge = env.get("LAKE_BRIDGE_URL", "http://127.0.0.1:5080").rstrip("/")
        parsed = urllib.parse.urlsplit(bridge)
        if (parsed.scheme not in ("http", "https") or
                parsed.hostname not in ("127.0.0.1", "::1", "localhost") or
                parsed.username or parsed.password or parsed.path or parsed.query or parsed.fragment):
            raise ConfigError("LAKE_BRIDGE_URL must be a loopback Bridge URL")
        # Resolve the port now so malformed values fail before a session is minted.
        _ = parsed.port
        quality = env.get("LAKE_QUALITY", "2k")
        if quality not in ("2k", "360p"):
            raise ConfigError("LAKE_QUALITY must be 2k or 360p")
        preview_value = env.get("LAKE_PREVIEW", "true").lower()
        if preview_value not in ("true", "false"):
            raise ConfigError("LAKE_PREVIEW must be true or false")
        width = number(env, "LAKE_PREVIEW_WIDTH", 1920, 2, 3840)
        height = number(env, "LAKE_PREVIEW_HEIGHT", 1080, 2, 2160)
        if width % 2 or height % 2:
            raise ConfigError("Preview dimensions must be even for H.264")
        rtsp = number(env, "LAKE_RTSP_PORT", 8555, 1024, 65535)
        api = number(env, "LAKE_API_PORT", 18555, 1024, 65535)
        if rtsp == api:
            raise ConfigError("Receiver listener ports must differ")
        return cls(camera, stream, bridge,
                   Path(env.get("LAKE_RUNTIME_DIR", "/run/lake")), quality,
                   preview_value == "true", width, height,
                   number(env, "LAKE_FPS", 20, 1, 60), rtsp, api,
                   number(env, "LAKE_SESSION_SECONDS", 3300, 60, 3300),
                   number(env, "LAKE_RENEW_SECONDS", 1800, 30, 1800))

    def url(self, native=False):
        return f"rtsp://127.0.0.1:{self.rtsp_port}/{self.stream}" + ("_native" if native else "")


def bridge_request(settings, suffix="", body=None):
    request = urllib.request.Request(
        settings.bridge + "/internal/wyze/lake/" + settings.camera + suffix,
        method="POST", data=json.dumps(body).encode() if body is not None else b"",
        headers={"Accept": "application/json", "Content-Type":"application/json"})
    # Do not send loopback credentials through environment HTTP proxies.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(request, timeout=45) as response:
        return json.loads(response.read(65536))


def provision(settings):
    session = bridge_request(settings)
    required = ("app_id", "channel", "uid", "rtc_token", "user_id", "encryption_key")
    if any(not session.get(key) for key in required) or session.get("encryption_mode") != 7:
        raise ValueError("Incomplete or unsupported Lake session")
    salt = base64.b64decode(session["encryption_salt"], validate=True)
    if len(salt) != 32:
        raise ValueError("Invalid Lake encryption salt")
    return session, salt


def checked(result, operation="SDK operation"):
    if result != 0:
        raise ViewerError(operation + " failed (code " + str(int(result)) + ")")


def sdk_service(app_id, runtime):
    from agora.rtc.agora_service import AgoraService
    from agora.rtc.agora_base import AgoraServiceConfig, ChannelProfileType
    service = AgoraService()
    checked(service.initialize(AgoraServiceConfig(
        appid=app_id, enable_video=1, enable_audio_device=0,
        enable_audio_processor=0,
        channel_profile=ChannelProfileType.CHANNEL_PROFILE_COMMUNICATION,
        log_path=str(runtime / "agora.log"), log_file_size_kb=512,
        data_dir=str(runtime), config_dir=str(runtime))))
    return service


def stop_process(process, group=True):
    if process is None or (not group and process.poll() is not None):
        return
    # FFmpeg may be waiting for pipe data. Stop the whole child group.
    try:
        if group: os.killpg(process.pid, signal.SIGTERM)
        else: process.terminate()
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        if group: os.killpg(process.pid, signal.SIGKILL)
        else: process.kill()
        process.wait()
    except ProcessLookupError:
        process.wait()
    finally:
        if group:
            # An exited viewer can leave a publisher that ignores SIGTERM.
            # Waiting for the viewer alone does not wait for its descendants.
            try: os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError: pass


def encoder_args(settings):
    # Native remuxing must never wait on a preview decoder/encoder.
    return ["ffmpeg", "-hide_banner", "-loglevel", "error", "-fflags", "+genpts",
            "-use_wallclock_as_timestamps", "1", "-probesize", "32768",
            "-analyzeduration", "1000000", "-f", "hevc",
            "-framerate", str(settings.fps), "-i", "pipe:0",
            "-map", "0:v:0", "-an", "-c:v", "copy", "-f", "rtsp",
            "-rtsp_transport", "tcp", settings.url(native=True)]


def preview_args(settings):
    # Pull from the local native relay: one camera connection, independent muxing.
    return ["ffmpeg", "-hide_banner", "-loglevel", "error", "-rtsp_transport", "tcp",
            "-threads", "2", "-i", settings.url(native=True), "-map", "0:v:0", "-an",
            # A mid-GOP join has no reference pictures. Drop initial P frames
            # instead of encoding the decoder's missing-reference gray filler.
            "-vf", "select='if(eq(pict_type,I),1,not(isnan(prev_selected_t)))',"
                   f"scale={settings.width}:{settings.height}", "-fps_mode", "passthrough",
            "-c:v", "libx264", "-threads", "2", "-preset", "veryfast", "-tune", "zerolatency",
            # Multiple slices can arrive as an incomplete picture at RTSP join.
            "-x264-params", "sliced-threads=0",
            "-pix_fmt", "yuv420p", "-g", str(settings.fps * 2), "-b:v", "4M",
            "-f", "rtsp", "-rtsp_transport", "tcp", settings.url()]


class FrameSink:
    """Own pipe readiness for one publisher; never drop part of a HEVC frame.

    A cancelled session must release its pump even if FFmpeg stops reading.
    Reuse one selector for the publisher's lifetime and enforce a per-frame
    deadline; any incomplete write fails the session rather than corrupting
    subsequent frames in the same stream.
    """

    def __init__(self, process, cancelled, timeout=5):
        self.process = process
        self.cancelled = cancelled
        self.timeout = timeout
        self.pipe = process.stdin.fileno()
        os.set_blocking(self.pipe, False)
        self.ready = selectors.DefaultSelector()
        try:
            self.ready.register(self.pipe, selectors.EVENT_WRITE)
        except Exception:
            self.ready.close()
            raise

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()

    def close(self):
        self.ready.close()

    def send(self, frame):
        expires = time.monotonic() + self.timeout
        offset = 0
        payload = memoryview(frame)
        while offset < len(payload):
            if self.cancelled.is_set() or STOP.is_set():
                raise ViewerError("Video session cancelled")
            if self.process.poll() is not None:
                raise ViewerError("Native publisher exited")
            remaining = expires - time.monotonic()
            if remaining <= 0:
                raise ViewerError("Native publisher write timed out")
            if not self.ready.select(min(remaining, 0.2)):
                continue
            try:
                written = os.write(self.pipe, payload[offset:])
            except (BlockingIOError, InterruptedError):
                continue
            except OSError:
                raise ViewerError("Native publisher pipe failed") from None
            if written == 0:
                raise ViewerError("Native publisher pipe closed")
            offset += written


def controls(user_id, quality):
    result = [{"cmd": "run_action", "action": "sight-safe::check-user",
               "params": {"userId": user_id}}]
    if quality is not None:
        result.append({"cmd": "set_property", "props": {"camera::resolution": quality}})
    return result



def pump_frames(settings, frames, failed, failure, stats, done):
    """Keep media moving even while session/control HTTP requests are pending."""
    process = sink = None
    health = settings.runtime / "frames.json"
    last_report = 0
    try:
        while not STOP.is_set() and not done.is_set():
            try:
                frame = frames.get(timeout=0.25)
            except queue.Empty:
                continue
            if process is None:
                with open(settings.runtime / "native-ffmpeg.log", "wb", buffering=0) as log:
                    process = subprocess.Popen(encoder_args(settings), stdin=subprocess.PIPE,
                                               stdout=log, stderr=log)
                sink = FrameSink(process, done)
                report("video_started", preview=settings.preview)
            sink.send(frame)
            stats["last_frame"] = time.monotonic()
            stats["frames"] += 1
            if stats["last_frame"] - last_report > 3:
                tmp = health.with_suffix(".tmp")
                tmp.write_text(json.dumps({"last_frame":time.time(), "frames":stats["frames"]}))
                tmp.replace(health)
                last_report = stats["last_frame"]
    except Exception as error:
        if not done.is_set() and not STOP.is_set():
            failure["reason"] = str(error) if isinstance(error, ViewerError) else "Video publisher failed"
            failed.set()
    finally:
        if sink is not None:
            sink.close()
        health.unlink(missing_ok=True)
        stop_process(process, group=False)
        if process is not None and process.stdin is not None:
            process.stdin.close()


def viewer(settings):
    from agora.rtc.agora_base import (RTCConnConfig, RtcConnectionPublishConfig,
                                     ChannelProfileType, AudioScenarioType,
                                     EncryptionConfig, VideoSubscriptionOptions)
    from agora.rtc.rtc_connection_observer import IRTCConnectionObserver
    from agora.rtc.video_encoded_frame_observer import IVideoEncodedFrameObserver
    session, salt = provision(settings)
    service = sdk_service(session["app_id"], settings.runtime)
    connected, failed, renew_needed = threading.Event(), threading.Event(), threading.Event()
    frames = queue.Queue(maxsize=150)
    failure = {"reason":"Lake disconnected"}
    connection = None
    done = threading.Event()
    stats = {"frames":0, "last_frame":time.monotonic()}
    writer = preview = None
    last_preview = 0

    class Observer(IRTCConnectionObserver):
        def on_connected(self, *args): connected.set()
        def on_disconnected(self, *args): failed.set()
        def on_connection_failure(self, *args): failed.set()
        def on_encryption_error(self, *args): failed.set()
        def on_token_privilege_did_expire(self, *args): failed.set()
        def on_token_privilege_will_expire(self, *args): renew_needed.set()
        def on_error(self, connection, code, message):
            report("sdk_error", code=code)
            failed.set()

    class VideoObserver(IVideoEncodedFrameObserver):
        def on_encoded_video_frame(self, uid, image, length, info):
            # SDK callbacks only copy data; network/control/encoding runs below.
            if int(info.codec_type) != 3 or length <= 0 or length > 8 * 1024 * 1024:
                failure["reason"] = "Unexpected video codec/length (codec " + str(int(info.codec_type)) + ")"
                failed.set()
                return 1
            try:
                frames.put_nowait(bytes(image[:length]))
            except queue.Full:
                failure["reason"] = "Video queue overflow"
                failed.set()
            return 1

    observer, video_observer = Observer(), VideoObserver()
    try:
        connection = service.create_rtc_connection(
            RTCConnConfig(auto_subscribe_audio=0, auto_subscribe_video=0,
                          channel_profile=ChannelProfileType.CHANNEL_PROFILE_COMMUNICATION),
            RtcConnectionPublishConfig(is_publish_audio=False, is_publish_video=False,
                                      audio_scenario=AudioScenarioType.AUDIO_SCENARIO_GAME_STREAMING))
        checked(connection.register_observer(observer))
        checked(connection.enable_encryption(1, EncryptionConfig(
            encryption_mode=7, encryption_key=session["encryption_key"],
            encryption_kdf_salt=bytearray(salt), datastream_encryption_enabled=False)))
        checked(connection.register_video_encoded_frame_observer(video_observer))
        checked(connection.get_local_user().subscribe_all_video(VideoSubscriptionOptions(encodedFrameOnly=True)))
        writer = threading.Thread(target=pump_frames, args=(settings,frames,failed,failure,stats,done), daemon=True)
        writer.start()
        checked(connection.connect(session["rtc_token"], session["channel"], str(session["uid"])))
        begin = time.monotonic()
        last_control = 0
        lifetime = min(settings.session_seconds, int(session.get("lifetime_seconds", 3600)) - 60)
        next_renewal = begin + settings.renew_seconds
        while not STOP.is_set() and time.monotonic() - begin < lifetime:
            now = time.monotonic()
            if failed.is_set() or now - stats["last_frame"] > 60:
                raise ViewerError(failure["reason"] if failed.is_set() else "Video stalled")
            if settings.preview and stats["frames"] and (preview is None or preview.poll() is not None) and now-last_preview>3:
                with open(settings.runtime / "preview-ffmpeg.log", "wb", buffering=0) as log:
                    preview = subprocess.Popen(preview_args(settings), stdout=log, stderr=log)
                last_preview = now
            if connected.is_set() and now - last_control > 12:
                # Reassert quality for later competing-viewer downgrades too.
                result = connection.send_stream_message(json.dumps(
                    controls(session["user_id"], settings.quality if now-begin>20 else None),
                    separators=(",", ":")).encode())
                if result: report("camera_control_retry", code=int(result))
                last_control = now
            if renew_needed.is_set() or now >= next_renewal:
                report("token_renewal_started")
                renewed = bridge_request(settings, "/renew", {"uid":session["uid"]})
                if not renewed.get("rtc_token"): raise ValueError("Incomplete renewed session")
                checked(connection.renew_token(renewed["rtc_token"]), "Token renewal")
                next_renewal = time.monotonic() + settings.renew_seconds
                renew_needed.clear()
                report("token_renewed")
            STOP.wait(0.25)
        if not STOP.is_set(): report("session_refresh")
    except ViewerError as error:
        report("viewer_recovery", reason=str(error), queued=frames.qsize(), frames=stats["frames"])
        raise
    finally:
        done.set()
        stop_process(preview, group=False)
        if writer: writer.join(timeout=6)
        if connection:
            connection.disconnect()
            connection.release()
        service.release()


def start_relay(settings):
    paths = {settings.stream + "_native": {"source": "publisher"}}
    if settings.preview: paths[settings.stream] = {"source": "publisher"}
    config = {"logLevel": "warn", "api": True,
              "apiAddress": f"127.0.0.1:{settings.api_port}",
              "rtsp": True, "rtspAddress": f"127.0.0.1:{settings.rtsp_port}",
              "rtspTransports": ["tcp"], "rtmp": False, "hls": False,
              "webrtc": False, "srt": False, "moq": False, "paths": paths}
    path = settings.runtime / "mediamtx.yml"
    path.write_text(json.dumps(config))
    process = subprocess.Popen(["mediamtx", str(path)], start_new_session=True)
    for _ in range(100):
        if process.poll() is not None: break
        try:
            with socket.create_connection(("127.0.0.1", settings.rtsp_port), timeout=0.1):
                return process
        except OSError:
            if STOP.wait(0.1): break
    stop_process(process)
    raise RuntimeError("Receiver RTSP server did not start")


def healthy(settings):
    try:
        status = json.loads((settings.runtime / "frames.json").read_text())
        if time.time() - status["last_frame"] > 20: return False
        streams = [settings.stream + "_native"] + ([settings.stream] if settings.preview else [])
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        for stream in streams:
            with opener.open(f"http://127.0.0.1:{settings.api_port}/v3/paths/get/{stream}", timeout=2) as r:
                if not json.load(r).get("ready"): return False
        return True
    except Exception:
        return False


def supervise(settings):
    relay = child = None
    delay = 1
    health = settings.runtime / "frames.json"
    try:
        while not STOP.is_set():
            health.unlink(missing_ok=True)
            started = time.monotonic()
            try:
                if relay is None or relay.poll() is not None:
                    stop_process(child)
                    relay = start_relay(settings)
                child = subprocess.Popen([sys.executable, __file__, "--viewer"], start_new_session=True)
                while not STOP.wait(1):
                    if child.poll() is not None or relay.poll() is not None: break
                    # This also catches a child hung inside an HTTP or SDK call.
                    elapsed = time.time() - health.stat().st_mtime if health.exists() else time.monotonic() - started
                    if elapsed > 90:
                        report("viewer_watchdog")
                        break
            except Exception as error:
                report("receiver_retry", error_type=type(error).__name__)
            finally:
                stop_process(child)
                health.unlink(missing_ok=True)
            if STOP.is_set(): break
            STOP.wait(delay)
            delay = 1 if time.monotonic() - started > 60 else min(30, delay * 2)
    finally:
        stop_process(child)
        stop_process(relay)
        health.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--health", action="store_true")
    parser.add_argument("--smoke", action="store_true")
    parser.add_argument("--viewer", action="store_true")
    args = parser.parse_args()
    runtime = Path(os.environ.get("LAKE_RUNTIME_DIR", "/run/lake"))
    if args.smoke:
        runtime.mkdir(mode=0o700, parents=True, exist_ok=True)
        service = sdk_service("0" * 32, runtime)
        service.release()
        report("native_sdk_ready")
        return 0
    settings = Settings.load()
    if args.health: return 0 if healthy(settings) else 1
    settings.runtime.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(settings.runtime, 0o700)
    signal.signal(signal.SIGTERM, lambda *_: STOP.set())
    signal.signal(signal.SIGINT, lambda *_: STOP.set())
    if args.viewer: viewer(settings)
    else: supervise(settings)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except ViewerError as error:
        report("receiver_failed", reason=str(error))
        sys.exit(1)
    except ConfigError as error:
        report("receiver_configuration_error", reason=str(error))
        sys.exit(1)
    except Exception as error:
        # Never print library exception text, tokens, keys, or session bodies.
        fields = {"error_type":type(error).__name__}
        if isinstance(error, urllib.error.HTTPError): fields["http_status"] = error.code
        report("receiver_failed", **fields)
        sys.exit(1)
