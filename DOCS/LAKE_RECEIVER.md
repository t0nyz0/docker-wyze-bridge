# Pan v4 Lake receiver and Bridge integration

This optional setup includes both pieces needed for a Lake-based Cam Pan v4:
Bridge provisions the camera session using its existing Wyze login, and a
companion receiver connects through Agora and publishes native H.265 video plus
an optional H.264 preview. There are no fixed account IDs, camera IDs, nicknames,
or NAS paths. Cameras without an RTSP override keep their existing routing.

This is experimental support for `HL_PAN4` using the `lake` provider and media
encryption mode 7. A model name alone does not prove that its firmware uses this
provider. Provisioning rejects other providers/encryption modes. The image has
pinned Linux amd64 and arm64 vendor artifacts; live validation currently covers
one x86_64 Synology and one Pan v4. ARM, multiple simultaneous cameras, audio,
and PTZ have not been validated. This is video-only support, not SD-card playback.

## Start both components

This setup targets Linux Docker; Home Assistant add-on packaging is not included.
The supplied Compose file builds the Bridge code in
this checkout and the receiver; the unchanged stable/beta Bridge image does not
include the new session API. Existing Bridge/receiver containers using the same
ports must be stopped first. Keep their configuration and rollback copies.

1. Copy `docker/lake/lake.env.example` to `.env.lake` in the repository root.
   Set `LAKE_CAMERA` to the **normalized camera name shown by Bridge** (for
   example, `front_door`). This also determines the stream names. Avoid names
   that collide with another camera or extra stream. Filters must include it.
2. Create `.env.wyze` with `WYZE_EMAIL`, `WYZE_PASSWORD`, `WYZE_API_ID`,
   `WYZE_API_KEY`, and, if required, `WYZE_TOTP_KEY`. An existing private
   credentials file can be selected with `WYZE_CREDENTIALS_FILE` in `.env.lake`.
   Keep these files private; the repository ignores `.env*` files.
3. Choose persistent Bridge config/media directories with `BRIDGE_CONFIG_DIR`
   and `BRIDGE_MEDIA_DIR`. Defaults are `./config` and `./media`.
4. Run from the repository root:

```sh
docker compose --env-file .env.lake -f docker-compose.lake.yml up -d --build
```

The example uses Linux host networking so the receiver can reach Bridge's
**loopback-only** session API. The receiver's RTSP and health API also listen on
loopback. There is no receiver account login, auth-state mount, exposed session
endpoint, or Docker socket mount. Only Bridge gets the Wyze credentials. Do not
proxy `/internal/` through a remote-access reverse proxy. Ordinary Bridge stream
access/authentication settings still apply to clients.

For `LAKE_CAMERA=front_door`, the outputs through Bridge are:

| Endpoint | Use |
| --- | --- |
| `rtsp://HOST:8554/front_door` | H.264 preview, 1920x1080 at the default settings |
| `rtsp://HOST:8554/front_door_native` | Original H.265 camera video, copied without scaling/re-encoding |

The receiver requests `camera::resolution=2k`, observed to deliver 3840x2160
on the tested Pan v4. **Probe the actual stream dimensions**; neither the
requested quality nor a stream name proves the resolution. The H.264 preview
may temporarily upscale a lower-quality feed while the camera switches modes.
Scrypted/Home can use the preview, while an H.265-capable NVR can select the
native endpoint. Bridge's default camera recording still uses the preview;
this setup does not enable 4K recording in Apple Home or import SD-card history.

## Settings and recovery

| Receiver variable | Default | Purpose |
| --- | --- | --- |
| `LAKE_CAMERA` | required | Discovered Bridge normalized camera name |
| `LAKE_STREAM` | same as camera | Receiver output name; update Bridge source settings if changed |
| `LAKE_BRIDGE_URL` | `http://127.0.0.1:5080` | Loopback Bridge URL |
| `LAKE_QUALITY` | `2k` | Camera command: `2k` or `360p` |
| `LAKE_PREVIEW` | `true` | Optional H.264 output; native output is always supplied |
| `LAKE_PREVIEW_WIDTH/HEIGHT` | `1920` / `1080` | Even H.264 output dimensions |
| `LAKE_FPS` | `20` | Raw-input frame-rate hint and preview keyframe sizing; SDK can report zero |
| `LAKE_RTSP_PORT` | `8555` | Private receiver RTSP listener |
| `LAKE_API_PORT` | `18555` | Private MediaMTX health listener |
| `LAKE_RENEW_SECONDS` | `1800` | In-place Agora token renewal, 30–1800 seconds |
| `LAKE_SESSION_SECONDS` | `3300` | Full session reprovisioning interval, 60–3300 seconds |
| `LAKE_RUNTIME_DIR` | `/run/lake` | Private temporary logs/status, no account-state file |

The Compose file uses a fixed H.264 preview arrangement. For native-only use,
set `LAKE_PREVIEW=false` on the receiver and point `RTSP_SOURCE_<CAM>` at its
`<stream>_native` output. For additional cameras, add one receiver service per
camera with distinct RTSP/API ports and matching Bridge overrides/extra streams.
The process architecture supports this configuration; simultaneous cameras are
not yet a live-tested claim. Encoding previews consumes CPU per camera.

Bridge's receiver input can be supplied through environment or YAML:

```ini
RTSP_SOURCE_FRONT_DOOR=rtsp://127.0.0.1:8555/front_door
GO2RTC_EXTRA_STREAMS=front_door_native=rtsp://127.0.0.1:8555/front_door_native
```

```yaml
CAM_OPTIONS:
  - CAM_NAME: front_door
    RTSP_SOURCE: rtsp://127.0.0.1:8555/front_door
```

Environment values override YAML, including an empty value disabling the
override. Only absolute `rtsp://` and `rtsps://` source URLs without fragments
are accepted. `GO2RTC_EXTRA_STREAMS` is an existing Bridge feature reused here.
Receiver session provisioning is enabled only for a configured Pan v4 input.
When renaming a camera, update the camera setting and Bridge overrides together.

Native remuxing has its own frame pump. The preview encoder reads the local
native RTSP stream. Native frame writing runs separately from preview encoding
and control/renewal HTTP requests, avoiding synchronous waits on those operations.
All processes still share the host's CPU and memory; allow capacity for preview
encoding. Preview encoding preserves input timestamps rather than duplicating
frames to catch up with a wall clock.

Quality is reasserted periodically. Token renewal happens outside SDK callbacks.
Use one receiver session per physical camera. A second receiver or a Wyze app
live view can compete over camera quality; concurrent receiver tests on the
tested camera coincided with corrupted video in the production preview. Test
against a spare camera or stop its existing receiver before opening another
session. A separate container or relay does not isolate the physical camera.
Failed connections, missing frames, blocked FFmpeg writes, queue overflow, or a
failed relay trigger recovery with capped backoff. A supervisor owns the child
process group so failed viewers do not leave publishers behind. Full session
reprovisioning can briefly interrupt video; this does not promise gapless output.
The Docker health check requires recent received frames **and** ready native /
preview publishers, not just a successful SDK connection.

Bridge keeps the configured receiver source through connect, reconnect, and
HD/SD controls. Those controls update Bridge's preference but do not alter
receiver resolution or disconnect its viewers. An idle pull
source is left registered. Metrics show `rtsp`; Bridge's existing `streaming`
label still describes registration. Check receiver health and actual client
frames for video availability. Normal Bridge cameras remain on their old paths.

## Build, tests, and prior work

The main Bridge image stays Go/Alpine. The optional companion includes Python,
the pinned Agora wrapper/native SDK, MediaMTX, and FFmpeg. Native SDK binaries
are fetched from vendor sources during a checksum-verified image build, not
committed into this repository. The wrapper's runtime downloader and global
TLS-verification override are disabled; the image does not fetch code at runtime.
Third-party vendor terms remain separate from this repository's license.

```sh
go test ./...
go vet ./...
python3 -m unittest discover -s docker/lake -v
docker build -t wyze-lake-receiver:local docker/lake
docker run --rm wyze-lake-receiver:local --smoke
```

The opt-in media regression generates synthetic HEVC video and needs no Wyze
account, camera connection, or SDK session. It records and fully decodes both
outputs before and after a preview process interruption, checks native decoded
frames against the source, and verifies that preview failure does not stop the
native pump. Run it inside the built image to use the packaged FFmpeg/MediaMTX:

```sh
docker run --rm --init --network none --read-only \
  --tmpfs /run/lake:uid=65532,gid=65532,mode=0700,size=64m \
  --tmpfs /tmp:uid=65532,gid=65532,mode=0700,size=128m \
  -v "$PWD/docker/lake:/tests:ro" -w /tests \
  -e LAKE_TEST_MEDIA=1 -e LAKE_TEST_SIZE=3840x2160 \
  --entrypoint python3 wyze-lake-receiver:local \
  -W error::ResourceWarning -m unittest discover -s /tests -v
```

The pull-request workflow also builds the optional image, initializes its SDK,
and runs these network-disabled media tests. Go race tests include concurrent
Lake requests during authentication replacement. These checks supplement the
real-camera tests; synthetic playback cannot validate Wyze's cloud or firmware.

The optional Go live test accepts `WYZE_BRIDGE_TEST_GO2RTC_URL` and
`WYZE_BRIDGE_TEST_RTSP_SOURCE`. Run it only against a disposable relay; it creates
and removes the `lake_integration_test` stream. The receiver tests do not need a
camera, SDK, or credentials. Live checks must additionally cover real provisioning,
both codecs/resolutions, token renewal, reprovisioning, and forced recovery.

## Credits and what this integration adds

[alphabin's protocol and 4K research](https://github.com/alphabin/ClaudeWyze4Sln/blob/main/WRITEUP.md)
documented Lake provisioning, encrypted media credentials, and the `2k` quality
command. [mitchross's native Linux receiver](https://github.com/mitchross/homelab-images/pull/8)
demonstrated Pan v4 Agora reception, H.265 RTSP publishing, token renewal, and
recovery. Those are substantial prior contributions, not discoveries made here.

The mitchross receiver is in `mitchross/homelab-images`, not this Bridge repository.
At the inspected commit `a821061c25f5759c00ec29c0300deac146096251`, it requests
360p and publishes one H.265 stream, using Bridge's authentication file or its
own Web View authentication. This change instead supplies Bridge-owned session
and renewal endpoints, configured camera lifecycle routing, and original-quality
video plus a separately encoded H.264 HD preview from one Agora session. It also
adds decoded media/recovery regressions. It does not include the other receiver's
Web View authentication option and does not claim to replace every feature there.

Merging this source makes the documented setup available within Wyze Bridge.
Users must still build/run the patched Bridge and companion and configure their
camera as described above. An ordinary Bridge update does not automatically start
an Agora receiver. The optional companion introduces vendor/runtime dependencies;
the main Bridge's Go dependencies and Dockerfile remain unchanged.

See `THIRD_PARTY_NOTICES.md` for the MIT research notice, design attribution,
the receiver source-overlap review, and separate vendor dependency terms.
