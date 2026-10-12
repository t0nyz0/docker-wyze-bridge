"""Fetch pinned vendor artifacts during the image build, never at runtime."""
import hashlib
import os
from pathlib import Path
import platform
import shutil
import tarfile
import tempfile
import urllib.request
import zipfile

WRAPPER = ("https://files.pythonhosted.org/packages/33/4d/715e0c325e42ff20484729c1503ac5d89097d31abd2b9db27f48bcac6eec/agora_python_server_sdk-2.4.9.tar.gz",
           "3f9453ce4730c1b9db86e56ea9b96713ffc576060b357d9bf0de8f65b473031e")
SDK = {
    "amd64": ("https://download.agora.io/sdk/release/agora_rtc_sdk_x86_64-linux-gnu-v4.4.32.174_28020_SERVER_20260601_1240_1144448_20251021_1427-3a.zip",
              "66f8a30dc75bb4f50a0597b52445c2b197bbc1970cd632262a24cdffe39b9130"),
    "arm64": ("https://download.agora.io/sdk/release/Agora-RTC-aarch64-linux-gnu-v4.4.32.174-20260601_142442-1144588.zip",
              "bb30df296ec28ee7bcd788557e20aba2e7d62f5c971d1894be38c012aba1cb7d"),
}
RELAY = {
    "amd64": "121be6e00397e473a95d83c75f4fcd3819a6af3b0e1a50889cba43ab4fab6a27",
    "arm64": "180a235374a548d9133cdd5e20f5dbd8b01317be1c86e0d7e66df585dc42ed6f",
}


def download(url, digest, path):
    # The default HTTPS context verifies the vendor's certificate.
    with urllib.request.urlopen(url, timeout=90) as source, open(path, "wb") as out:
        shutil.copyfileobj(source, out)
    if hashlib.sha256(path.read_bytes()).hexdigest() != digest:
        raise RuntimeError("Vendor artifact checksum mismatch")


def main():
    arch = os.environ.get("TARGETARCH") or {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if arch not in SDK:
        raise RuntimeError("Lake image supports Linux amd64 and arm64")
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        archive = root / "wrapper.tar.gz"
        download(*WRAPPER, archive)
        with tarfile.open(archive) as package:
            package.extractall(root / "wrapper", filter="data")
        package_root = root / "wrapper/agora_python_server_sdk-2.4.9"
        target = Path("/opt/agora")
        target.mkdir()
        shutil.copytree(package_root / "agora", target / "agora")
        # Preserve upstream package metadata/documentation, not SDK binaries in Git.
        for name in ("PKG-INFO", "README.md"):
            shutil.copy2(package_root / name, target / name)
        init = target / "agora/__init__.py"
        source = init.read_text()
        unsafe = "ssl._create_default_https_context = ssl._create_unverified_context"
        bootstrap = "\n_check_download_and_extract_sdk()\n"
        if source.count(unsafe) != 1 or source.count(bootstrap) != 1:
            raise RuntimeError("Pinned SDK bootstrap changed")
        init.write_text(source.replace(unsafe, "# Keep Python's verified HTTPS defaults.").replace(
            bootstrap, "\n# SDK installed and checksum-verified at image build time.\n"))
        native = root / "sdk.zip"
        download(*SDK[arch], native)
        with zipfile.ZipFile(native) as package:
            for member in package.infolist():
                dest = (target / "agora" / member.filename).resolve()
                if not dest.is_relative_to((target / "agora").resolve()):
                    raise RuntimeError("Unsafe SDK archive member")
                if (member.external_attr >> 16) & 0o170000 == 0o120000:
                    raise RuntimeError("SDK archive contains a symbolic link")
            package.extractall(target / "agora")
        (target / "agora/version.txt").write_text(SDK[arch][0])
        if not (target / "agora/agora_sdk/libagora_rtc_sdk.so").is_file():
            raise RuntimeError("Native SDK library missing")
        relay = root / "relay.tar.gz"
        download(f"https://github.com/bluenviron/mediamtx/releases/download/v1.21.2/mediamtx_v1.21.2_linux_{arch}.tar.gz",
                 RELAY[arch], relay)
        with tarfile.open(relay) as package:
            package.extractall(root / "relay", filter="data")
        shutil.copy2(root / "relay/mediamtx", "/usr/local/bin/mediamtx")
        Path("/usr/local/bin/mediamtx").chmod(0o755)
        shutil.copy2(root / "relay/LICENSE", target / "MEDIAMTX-LICENSE")


if __name__ == "__main__":
    main()
