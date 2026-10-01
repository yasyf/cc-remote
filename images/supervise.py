#!/usr/bin/python3
import fcntl
import os
import signal
import subprocess
import sys
import time

name = sys.argv[1]
services = os.path.expanduser("~/.cc-remote/services")
recipe_path = os.path.join(services, name)


def read(path):
    with open(path) as f:
        return f.read()


lock = open(os.path.join(services, name + ".lock"), "w")
try:
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
except BlockingIOError:
    sys.exit(0)
log = os.open(os.path.join(services, name + ".log"), os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o644)
os.dup2(log, 1)
os.dup2(log, 2)
os.environ["PATH"] = os.path.expanduser("~/.local/bin") + ":/usr/local/bin:/usr/bin:/bin"
child = None


def stop(signum, frame):
    if child is not None and child.poll() is None:
        child.terminate()
        child.wait()
    sys.exit(128 + signum)


signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
signal.signal(signal.SIGHUP, stop)
script = read(__file__)
while read(__file__) == script:
    recipe = read(recipe_path)
    child = subprocess.Popen(["sh", "-c", recipe], stdin=subprocess.DEVNULL)
    while child.poll() is None:
        time.sleep(2)
        if read(recipe_path) != recipe or read(__file__) != script:
            child.terminate()
            child.wait()
    print(f"supervise: {name} exited {child.returncode}; restarting", flush=True)
    time.sleep(1)
lock.close()
os.execv(sys.executable, [sys.executable, __file__, name])
