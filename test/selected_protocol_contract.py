#!/usr/bin/env python3
"""Compose the pinned public client proof with the owning local payload proof."""

import pathlib
import subprocess
import sys

directory = pathlib.Path(__file__).resolve().parent
subprocess.run([sys.executable, str(directory / "codex_web_protocol_contract.py"), *sys.argv[1:]], check=True)
if len(sys.argv) > 1 and sys.argv[1] != "--coverage-only":
    subprocess.run([sys.executable, str(directory / "workspace_protocol_contract.py"), sys.argv[1]], check=True)
