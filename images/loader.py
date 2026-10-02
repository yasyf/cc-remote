#!/usr/bin/python3
import os
import subprocess
import sys

PT_INTERP = 3

args = sys.argv[1:]
host = "/"
if args[:1] == ["--host"]:
    host, args = os.path.normpath(args[1]), args[2:]
if len(args) != 1:
    print("usage: loader.py [--host DIR] ELF", file=sys.stderr)
    sys.exit(2)
elf = args[0]


def fail(message):
    print(f"cc-remote: {elf} {message}", file=sys.stderr)
    sys.exit(1)


def word(data, at, size):
    return int.from_bytes(data[at:at + size], "little")


def interpreter():
    with open(elf, "rb") as fh:
        header = fh.read(64)
        if header[:4] != b"\x7fELF" or header[4] != 2 or header[5] != 1:
            fail("is not a little-endian ELF64 file")
        offset, entry, count = word(header, 32, 8), word(header, 54, 2), word(header, 56, 2)
        for index in range(count):
            fh.seek(offset + index * entry)
            program = fh.read(entry)
            if word(program, 0, 4) == PT_INTERP:
                fh.seek(word(program, 8, 8))
                return fh.read(word(program, 32, 8)).rstrip(b"\0").decode()
    fail("has no PT_INTERP, so no loader can list its libraries")


try:
    loader = os.path.join(host, interpreter().lstrip("/"))
except OSError as err:
    fail(f"cannot be read: {err}")
if not os.path.isfile(loader):
    fail(f"names the loader {loader}, which is not a file")
sys.exit(subprocess.run([loader, "--list", elf], stdin=subprocess.DEVNULL).returncode)
