#!/usr/bin/env python3
"""Pin the Homebrew cask to the exact signed binaries published by this release."""

import hashlib
from pathlib import Path
import re
import sys


def render(version, artifacts):
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:-\d+\.[0-9a-f]+)?", version):
        raise ValueError(f"invalid release version: {version!r}")
    template = Path(__file__).resolve().parents[1] / "src/packaging/homebrew/quesma-shipper.rb.in"
    result = template.read_text().replace("@VERSION@", version)
    for arch in ("arm64", "amd64"):
        binary = artifacts / f"quesma-shipper-darwin-{arch}"
        digest = hashlib.sha256(binary.read_bytes()).hexdigest()
        result = result.replace(f"@{arch.upper()}_SHA256@", digest)
    return result


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit("usage: homebrew-cask.py RELEASE_VERSION ARTIFACT_DIRECTORY")
    print(render(sys.argv[1], Path(sys.argv[2])), end="")
