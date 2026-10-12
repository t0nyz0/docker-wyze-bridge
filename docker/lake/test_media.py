"""Opt-in real media regression, using generated video and no camera/SDK login."""

import dataclasses
import contextlib
import json
import os
from pathlib import Path
import queue
import shutil
import subprocess
import tempfile
import threading
import time
import unittest
import urllib.request

import receiver


@unittest.skipUnless(os.environ.get("LAKE_TEST_MEDIA") == "1",
                     "set LAKE_TEST_MEDIA=1 with ffmpeg, ffprobe and mediamtx installed")
class MediaTests(unittest.TestCase):
    def run_tool(self, args, timeout=60):
        result = subprocess.run(args, capture_output=True, timeout=timeout)
        self.assertEqual(result.returncode, 0, result.stderr.decode(errors="replace")[-2000:])
        return result.stdout

    def wait_ready(self, config, streams):
        deadline = time.monotonic() + 20
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        while time.monotonic() < deadline:
            try:
                ready = []
                for stream in streams:
                    with opener.open(f"http://127.0.0.1:{config.api_port}/v3/paths/get/{stream}", timeout=1) as response:
                        ready.append(json.load(response).get("ready"))
                if all(ready): return
            except Exception:
                pass
            time.sleep(0.1)
        self.fail("Synthetic media publishers did not become ready")

    def record_and_decode(self, config, directory, label):
        jobs = []
        for native in (True, False):
            path = directory / (label + ("-native" if native else "-preview") + ".mp4")
            command = ["ffmpeg", "-hide_banner", "-loglevel", "error", "-rtsp_transport", "tcp",
                       "-i", config.url(native), "-t", "3", "-an", "-c:v", "copy", "-y", str(path)]
            jobs.append((native, path, subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)))
        try:
            for native, path, process in jobs:
                _, errors = process.communicate(timeout=20)
                self.assertEqual(process.returncode, 0, errors.decode(errors="replace")[-2000:])
                info = json.loads(self.run_tool(["ffprobe", "-v", "error", "-count_frames",
                    "-show_entries", "stream=codec_name,width,height,nb_read_frames", "-of", "json", str(path)]))["streams"][0]
                dimensions = tuple(map(int, os.environ.get("LAKE_TEST_SIZE", "1280x720").split("x")))
                self.assertEqual((info["width"], info["height"]), dimensions if native else (config.width, config.height))
                self.assertEqual(info["codec_name"], "hevc" if native else "h264")
                # Joining mid-GOP can trim up to a keyframe interval from a clip.
                self.assertGreaterEqual(int(info["nb_read_frames"]), 10)
                # Exit on any decode error; probing codec metadata alone is insufficient.
                self.run_tool(["ffmpeg", "-hide_banner", "-loglevel", "error", "-xerror",
                    "-err_detect", "explode", "-i", str(path), "-an", "-f", "null", "-"])
                if native:
                    decoded = self.frame_hashes(path)
                    self.assertTrue(decoded)
                    self.assertTrue(decoded.issubset(self.reference_hashes),
                                    "native output changed decoded camera frames")
                receiver.report("media_test_clip", phase=label, native=native,
                                width=info["width"], height=info["height"],
                                decoded_frames=int(info["nb_read_frames"]))
        finally:
            for _, _, process in jobs:
                receiver.stop_process(process, group=False)

    def frame_hashes(self, path):
        output = self.run_tool(["ffmpeg", "-v", "error", "-i", str(path),
                               "-pix_fmt", "yuv420p", "-f", "framemd5", "-"])
        return {line.rsplit(",", 1)[-1].strip() for line in output.decode().splitlines()
                if line and not line.startswith("#")}

    def test_native_copy_preview_decode_and_preview_recovery(self):
        for tool in ("ffmpeg", "ffprobe", "mediamtx"):
            self.assertIsNotNone(shutil.which(tool), tool + " missing")
        artifacts = os.environ.get("LAKE_TEST_ARTIFACT_DIR")
        workspace = contextlib.nullcontext(artifacts) if artifacts else tempfile.TemporaryDirectory()
        with workspace as temporary:
            directory = Path(temporary)
            directory.mkdir(parents=True, exist_ok=True)
            size = os.environ.get("LAKE_TEST_SIZE", "1280x720")
            config = receiver.Settings.load({"LAKE_CAMERA":"synthetic_camera",
                "LAKE_RTSP_PORT":os.environ.get("LAKE_TEST_RTSP_PORT", "29555"),
                "LAKE_API_PORT":os.environ.get("LAKE_TEST_API_PORT", "29556")})
            config = dataclasses.replace(config, runtime=directory,
                width=1920 if size == "3840x2160" else 640,
                height=1080 if size == "3840x2160" else 360)
            fixture = directory / "fixture.hevc"
            self.run_tool(["ffmpeg", "-v", "error", "-f", "lavfi", "-i", f"smptebars=size={size}:rate=20",
                "-t", "4", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params",
                "pools=1:frame-threads=1:bframes=0:keyint=40:repeat-headers=1:log-level=error",
                "-pix_fmt", "yuv420p", "-f", "hevc", "-y", str(fixture)], timeout=120)
            packets = json.loads(self.run_tool(["ffprobe", "-v", "error", "-show_packets",
                "-show_entries", "packet=pos,size", "-of", "json", str(fixture)]))["packets"]
            data = fixture.read_bytes()
            frames = [data[int(packet["pos"]):int(packet["pos"])+int(packet["size"])] for packet in packets]
            self.reference_hashes = self.frame_hashes(fixture)
            relay = preview = None
            failed, done = threading.Event(), threading.Event()
            stats = {"frames":0, "last_frame":time.monotonic()}
            pending = queue.Queue(maxsize=150)
            failure = {}
            writer = threading.Thread(target=receiver.pump_frames,
                args=(config, pending, failed, failure, stats, done))

            def feed():
                index = 0
                while not done.is_set():
                    pending.put(frames[index % len(frames)], timeout=1)
                    index += 1
                    done.wait(0.05)

            feeder = threading.Thread(target=feed)
            try:
                relay = receiver.start_relay(config)
                writer.start()
                feeder.start()
                self.wait_ready(config, [config.stream + "_native"])
                with open(directory / "preview-ffmpeg.log", "wb") as log:
                    preview = subprocess.Popen(receiver.preview_args(config), stdout=log, stderr=log)
                self.wait_ready(config, [config.stream])
                self.record_and_decode(config, directory, "initial")
                before = stats["frames"]
                preview.kill()
                preview.wait()
                time.sleep(0.5)
                self.assertGreater(stats["frames"], before, "preview failure stopped native frame pumping")
                self.assertFalse(failed.is_set(), failure)
                with open(directory / "preview-ffmpeg.log", "ab") as log:
                    preview = subprocess.Popen(receiver.preview_args(config), stdout=log, stderr=log)
                self.wait_ready(config, [config.stream])
                self.record_and_decode(config, directory, "recovered")
                self.assertFalse(failed.is_set(), failure)
            finally:
                done.set()
                receiver.stop_process(preview, group=False)
                if feeder.ident: feeder.join(3)
                if writer.ident: writer.join(12)
                receiver.stop_process(relay)
                self.assertFalse(writer.is_alive(), "native frame pump did not stop")
