import dataclasses
import json
import os
from pathlib import Path
import queue
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import receiver


class ReceiverTests(unittest.TestCase):
    def settings(self, **values):
        return receiver.Settings.load(dict(LAKE_CAMERA="front_door", **values))

    def test_generic_camera_and_listener_settings(self):
        config = self.settings(LAKE_STREAM="other_camera", LAKE_RTSP_PORT="19555")
        self.assertEqual(config.url(True), "rtsp://127.0.0.1:19555/other_camera_native")
        for values in ({"LAKE_CAMERA":"../camera"}, {"LAKE_BRIDGE_URL":"http://example.com"},
                       {"LAKE_BRIDGE_URL":"http://secret@localhost"}, {"LAKE_PREVIEW_WIDTH":"1919"},
                       {"LAKE_SESSION_SECONDS":"3600"}, {"LAKE_RTSP_PORT":"18555"}):
            with self.assertRaises(ValueError):
                receiver.Settings.load({"LAKE_CAMERA":"front_door", **values})

    def test_native_is_copied_and_preview_is_optional(self):
        config = self.settings()
        args = receiver.encoder_args(config)
        self.assertEqual(args[args.index("-c:v")+1], "copy")
        self.assertNotIn("libx264", args)
        self.assertNotIn("-vf", args)
        preview = receiver.preview_args(config)
        self.assertIn("libx264", preview)
        self.assertIn("scale=1920:1080", preview[preview.index("-vf")+1])
        self.assertIn(config.url(True), preview)
        self.assertNotIn("cfr", preview)
        native = receiver.encoder_args(dataclasses.replace(config, preview=False))
        self.assertNotIn("libx264", native)
        self.assertNotIn("-vf", native)

    def test_quality_control_is_configurable(self):
        for quality in ("2k", "360p"):
            commands = receiver.controls("test-user", quality)
            self.assertEqual(commands[1]["props"]["camera::resolution"], quality)

    def test_frame_write_detects_exited_encoder(self):
        process = subprocess.Popen(["/bin/sh", "-c", "exit 0"], stdin=subprocess.PIPE)
        process.wait()
        try:
            with receiver.FrameSink(process, threading.Event()) as sink:
                with self.assertRaises(receiver.ViewerError): sink.send(b"frame")
        finally:
            process.stdin.close()

    def test_blocked_publisher_has_bounded_write_time(self):
        process = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"],
                                   stdin=subprocess.PIPE)
        start = time.monotonic()
        try:
            with receiver.FrameSink(process, threading.Event(), timeout=0.3) as sink:
                with self.assertRaisesRegex(receiver.ViewerError, "timed out"):
                    sink.send(b"x" * (1024 * 1024))
            self.assertLess(time.monotonic() - start, 2)
        finally:
            process.terminate()
            process.wait()
            process.stdin.close()

    def test_cancelled_session_releases_blocked_frame_pump(self):
        process = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"],
                                   stdin=subprocess.PIPE)
        cancelled = threading.Event()
        timer = threading.Timer(0.1, cancelled.set)
        try:
            with receiver.FrameSink(process, cancelled) as sink:
                timer.start()
                started = time.monotonic()
                with self.assertRaisesRegex(receiver.ViewerError, "cancelled"):
                    sink.send(b"x" * (1024 * 1024))
                self.assertLess(time.monotonic() - started, 1)
        finally:
            timer.cancel()
            if timer.ident is not None: timer.join()
            process.terminate()
            process.wait()
            process.stdin.close()

    def test_partial_writes_and_interrupts_preserve_every_frame_byte(self):
        frame = bytes(range(256)) * 800
        actual_write = os.write
        calls = 0

        def short_write(fd, chunk):
            nonlocal calls
            calls += 1
            if calls == 1: raise InterruptedError()
            if calls == 2: raise BlockingIOError()
            return actual_write(fd, chunk[:137])

        with tempfile.TemporaryFile() as output:
            process = subprocess.Popen([sys.executable, "-c",
                "import sys; sys.stdout.buffer.write(sys.stdin.buffer.read())"],
                stdin=subprocess.PIPE, stdout=output)
            try:
                with receiver.FrameSink(process, threading.Event()) as sink:
                    with patch("receiver.os.write", side_effect=short_write):
                        sink.send(frame)
                process.stdin.close()
                process.wait(timeout=5)
                self.assertEqual(process.returncode, 0)
                output.seek(0)
                self.assertEqual(output.read(), frame)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
                process.stdin.close()

    def test_cleanup_kills_stubborn_publisher_after_viewer_crash(self):
        with tempfile.TemporaryDirectory() as directory:
            heartbeat = Path(directory) / "heartbeat"
            child_code = ("import signal,time,pathlib; signal.signal(signal.SIGTERM, signal.SIG_IGN); "
                          f"p=pathlib.Path({str(heartbeat)!r}); "
                          "exec('while True:\\n p.write_text(str(time.time()))\\n time.sleep(0.05)')")
            parent_code = ("import subprocess,sys,time; "
                           f"subprocess.Popen([sys.executable,'-c',{child_code!r}]); time.sleep(30)")
            process = subprocess.Popen([sys.executable, "-c", parent_code], start_new_session=True)
            try:
                deadline = time.monotonic() + 3
                while not heartbeat.exists() and time.monotonic() < deadline:
                    time.sleep(0.02)
                self.assertTrue(heartbeat.exists())
                process.kill()
                process.wait()
                receiver.stop_process(process)
                time.sleep(0.15)
                last = heartbeat.read_text()
                time.sleep(0.15)
                self.assertEqual(heartbeat.read_text(), last, "orphan publisher survived cleanup")
            finally:
                try: os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError: pass
                process.wait()

    def test_frame_pump_runs_while_control_thread_waits(self):
        # A slow renewal request must not stop frames draining from the SDK queue.
        with tempfile.TemporaryDirectory() as directory:
            config = dataclasses.replace(self.settings(), runtime=Path(directory))
            frames = queue.Queue()
            failed, done, delivered = threading.Event(), threading.Event(), threading.Event()
            stats = {"frames":0, "last_frame":time.monotonic()}
            with patch("receiver.subprocess.Popen"), patch("receiver.stop_process"), \
                    patch("receiver.report"), patch("receiver.FrameSink") as sink:
                sink.return_value.send.side_effect = lambda *_: delivered.set()
                worker = threading.Thread(target=receiver.pump_frames,
                    args=(config, frames, failed, {}, stats, done))
                worker.start()
                try:
                    frames.put(b"encoded-camera-frame")
                    self.assertTrue(delivered.wait(2))
                    done.set()
                    worker.join(2)
                    self.assertFalse(worker.is_alive())
                    self.assertFalse(failed.is_set())
                    self.assertEqual(stats["frames"], 1)
                finally:
                    done.set()
                    worker.join(2)

    def test_healthy_needs_recent_frames_and_ready_outputs(self):
        with tempfile.TemporaryDirectory() as directory:
            config = dataclasses.replace(self.settings(), runtime=Path(directory))
            status = config.runtime / "frames.json"
            self.assertFalse(receiver.healthy(config))
            status.write_text(json.dumps({"last_frame":time.time()-60}))
            self.assertFalse(receiver.healthy(config))
            status.write_text(json.dumps({"last_frame":time.time()}))
            class Response:
                def __enter__(self): return self
                def __exit__(self,*args): pass
                def read(self,*args): return b'{"ready":true}'
            with patch("receiver.urllib.request.build_opener") as build:
                build.return_value.open.return_value=Response()
                self.assertTrue(receiver.healthy(config))
                self.assertEqual(build.return_value.open.call_count,2)


if __name__ == "__main__": unittest.main()
