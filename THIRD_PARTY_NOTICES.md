# Third-Party Notices

## AlexxIT/go2rtc

This project bundles the `go2rtc` binary for the native RTSP sidecar.

- Project: `https://github.com/AlexxIT/go2rtc`
- Author: Alexey Khit
- License: MIT

### MIT License

Copyright (c) 2022 Alexey Khit

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

## wlatic/hacky-wyze-gwell

The `gwell` sidecar uses a vendored copy of the Go protocol packages from `wlatic/hacky-wyze-gwell`.

- Project: `github.com/wlatic/hacky-wyze-gwell`
- Author: _unknown real name_ [wlatic](https://github.com/wlatic) aka [wlatic](https://www.reddit.com/user/wlatic/)
- License: MIT

### MIT License (template since no actual LICENSE file in repository)

Copyright (c) 2026

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.




## Optional Lake receiver dependencies

The Lake protocol implementation draws on alphabin/ClaudeWyze4Sln's published
MIT-licensed protocol research. Its copyright and permission notice are reproduced
below. The receiver's architecture was also informed by mitchross/homelab-images,
which demonstrated native Linux Pan v4 reception, RTSP publishing, token renewal,
and recovery. That work lives in a separate repository; it is not already included
in Wyze Bridge. See `DOCS/LAKE_RECEIVER.md` for source links and the distinction
between prior work and this integration.

No standalone license was found in the inspected mitchross repository at commit
`a821061c25f5759c00ec29c0300deac146096251`. It is a credited design reference, not a
dependency fetched or installed by this image. The matching frame-writing helper
identified during review was removed; the receiver now owns a cancellation-aware
publisher sink with persistent readiness registration and byte-preservation tests.
Attribution here does not assign a license to that separate project. Lake
reception and the Pan v4 quality command must not be described as new discoveries.

The optional image fetches Agora Python Server SDK 2.4.9 and its matching native
SDK from the official package/vendor hosts at build time, with pinned SHA-256
checksums. Agora is a third-party runtime dependency, not code relicensed under
this repository's AGPL license. Package metadata and README are retained in the
image at `/opt/agora`. No Agora binaries are included in Git. Consult the vendor's
current terms before publishing SDK-containing images.

MediaMTX v1.21.2 is MIT licensed; its release LICENSE is retained at
`/opt/agora/MEDIAMTX-LICENSE` inside the receiver image. FFmpeg and its dependencies
come from Debian's packages, which retain their package copyright information
under `/usr/share/doc`. The Dockerfile and fetch manifest identify the pinned
Python/Agora/MediaMTX artifacts.

## alphabin/ClaudeWyze4Sln

Project: https://github.com/alphabin/ClaudeWyze4Sln

MIT License

Copyright (c) 2026 ClaudeWyze4Sln contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
