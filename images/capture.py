#!/usr/bin/python3
import hashlib
import json
import os
import platform
import re
import shutil
import stat
import subprocess
import sys

payload_path, host, root, build, user_home = sys.argv[1:6]
host = os.path.normpath(host)
LIB = "/usr/lib/" + platform.machine() + "-linux-gnu"
ALIASES = ("/lib64/", "/lib/", "/bin/", "/sbin/")
NEVER = ("/var/lib/dpkg/", "/var/lib/apt/", "/var/cache/apt/", "/var/log/", "/etc/ssh/", "/etc/machine-id", "/home/", "/root/")
LOADER_CONF = "/etc/ld.so.conf.d/zz-cc-remote-closure.conf"
FONTS_CONF = "/etc/fonts/conf.d/99-cc-remote-closure.conf"
REQUIRED = ("/usr/share/mime/mime.cache", "/usr/share/glib-2.0/schemas/gschemas.compiled", LIB + "/gio/modules/giomodule.cache")
MODULES = LIB + "/gtk-3.0"
env = dict(os.environ, LC_ALL="C")


class Fatal(Exception):
    pass


def fatal(message):
    raise Fatal(message)


def hostpath(path):
    return os.path.join(host, path.lstrip("/"))


def canonical(path):
    for alias in ALIASES:
        if path.startswith(alias):
            return "/usr" + path
    return path


def run(argv, **overrides):
    return subprocess.run(argv, env=dict(env, **overrides), stdin=subprocess.DEVNULL, capture_output=True, text=True, errors="replace")


def dpkg(*args):
    result = run(["dpkg-query", *args])
    if result.returncode != 0:
        fatal(f"dpkg-query {' '.join(args[:2])} exited {result.returncode}: {result.stderr.strip()}")
    return result.stdout


def installed(path):
    with open(path, encoding="utf-8") as fh:
        return {pkg for status, pkg in (line.split("\t") for line in fh.read().splitlines()) if status.startswith("ii")}


def clauses(field):
    return [[re.sub(r"[\s(:].*$", "", alt.strip()) for alt in clause.split("|") if alt.strip()] for clause in field.split(",") if clause.strip()]


def partition(seeds, new):
    meta, providers = {}, {}
    for line in dpkg("-W", "-f", "${Package}\t${Depends}\t${Pre-Depends}\t${Provides}\n", *sorted(new)).splitlines():
        pkg, depends, predepends, provides = line.split("\t")
        meta[pkg] = (clauses(depends) + clauses(predepends), [re.sub(r"[\s(].*$", "", name.strip()) for name in provides.split(",") if name.strip()])
    for pkg in sorted(new):
        for provided in meta[pkg][1]:
            providers.setdefault(provided, []).append(pkg)
    resident, queue = set(), [seed for seed in seeds if seed in new]
    while queue:
        pkg = queue.pop()
        if pkg in resident:
            continue
        resident.add(pkg)
        for clause in meta[pkg][0]:
            candidates = [c for alt in clause for c in ([alt] if alt in new else providers.get(alt, []))]
            if candidates and not any(c in resident for c in candidates):
                queue.append(candidates[0])
    return resident


def versions(packages):
    out = dpkg("-W", "-f", "${Package}\t${Version}\t${Architecture}\n", *packages)
    return {pkg: {"version": version, "arch": arch} for pkg, version, arch in (line.split("\t") for line in out.splitlines())}


def listed(package):
    return [line for line in dpkg("-L", package).splitlines() if line.startswith("/")]


def owners(paths):
    found = {}
    paths = sorted(set(paths))
    for at in range(0, len(paths), 300):
        for line in run(["dpkg-query", "-S", *paths[at:at + 300]]).stdout.splitlines():
            match = re.match(r"^(.+?): (/.+)$", line)
            if match and not line.startswith("diversion"):
                found[match.group(2)] = [owner.split(":")[0] for owner in match.group(1).split(", ")]
    return found


def unowned(path):
    return run(["dpkg-query", "-S", path]).returncode != 0


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def walk(directory, base=None):
    top = hostpath(directory) if base is None else base + directory
    for d, _, names in os.walk(top):
        for name in names:
            yield directory + os.path.join(d, name)[len(top):]


def directory(path):
    existing = path
    while not os.path.lexists(existing):
        existing = os.path.dirname(existing)
    if os.path.realpath(existing) != existing or not os.path.isdir(existing):
        fatal(f"{path[len(root):]} would be written through {existing}, which is not a directory inside the closure")
    os.makedirs(path, exist_ok=True)


def destination(path):
    dest = root + canonical(path)
    directory(os.path.dirname(dest))
    if os.path.lexists(dest):
        fatal(f"{canonical(path)} is already in the closure")
    return dest


def resolve_consumers(consumers):
    paths = []
    for consumer in consumers:
        path = consumer if consumer.startswith("/") else os.path.join(user_home, consumer)
        if not os.path.isfile(hostpath(path)):
            fatal(f"apt.payload.consumers: {consumer} is not a file at {path}")
        paths.append(path)
    return paths


def check_exposures(payload, captured):
    for bin in payload["bins"]:
        if "/usr/bin/" + bin not in captured:
            fatal(f"apt.payload.bins: no closure package ships /usr/bin/{bin}")
    for path in [link["path"] for link in payload["links"]] + [LOADER_CONF, FONTS_CONF]:
        if not unowned(path):
            fatal(f"{path} is owned by a package, so the payload cannot own it")
    for link in payload["links"]:
        if os.path.lexists(hostpath(link["path"])):
            fatal(f"{link['path']} already exists on the build machine")


def capture_files(paths_by_package, captured):
    files = []
    for package, paths in paths_by_package.items():
        for path in paths:
            source = hostpath(path)
            if not os.path.lexists(source):
                continue
            info = os.lstat(source)
            if stat.S_ISDIR(info.st_mode):
                continue
            if any(path.startswith(never) or path == never.rstrip("/") for never in NEVER):
                fatal(f"{package} ships {path}, which the closure never carries")
            dest = destination(path)
            entry = {"path": canonical(path), "package": package}
            if stat.S_ISLNK(info.st_mode):
                target = os.readlink(source)
                if os.path.isabs(target):
                    if canonical(target) not in captured:
                        fatal(f"{package} symlink {path} -> {target} leaves the closure")
                    target = os.path.relpath(canonical(target), os.path.dirname(canonical(path)))
                elif not os.path.normpath(os.path.join(os.path.dirname(dest), target)).startswith(root + "/"):
                    fatal(f"{package} symlink {path} -> {target} escapes the closure")
                os.symlink(target, dest)
                entry["link"] = target
            elif stat.S_ISREG(info.st_mode):
                mode = stat.S_IMODE(info.st_mode)
                if mode & 0o6000:
                    fatal(f"{package} ships setuid or setgid {path}")
                try:
                    shutil.copy2(source, dest)
                except OSError as err:
                    fatal(f"cannot copy {path}: {err}")
                mode &= 0o1755
                os.chmod(dest, mode)
                entry.update(sha256=sha256(dest), mode=f"{mode:04o}")
            else:
                fatal(f"{package} ships {path}, which is neither a regular file nor a symlink")
            files.append(entry)
    return files


def captured_themes(captured):
    return sorted(path.split("/")[4] for path in captured if path.startswith("/usr/share/icons/") and path.endswith("/index.theme") and path.count("/") == 5)


def generated_paths(captured):
    paths = {path: "copy" for path in walk("/usr/share/mime") if path not in captured}
    for path in REQUIRED:
        paths[path] = "copy"
    for theme in captured_themes(captured):
        paths[f"/usr/share/icons/{theme}/icon-theme.cache"] = "copy"
    caches = [path for path in walk(MODULES) if path.endswith(".cache") and path not in captured]
    if not caches:
        fatal(f"no module cache was generated under {MODULES}")
    for path in caches:
        paths[path] = "rewrite"
    for path in paths:
        source = hostpath(path)
        if os.path.islink(source) or not os.path.isfile(source):
            fatal(f"{path} was not generated on the build machine")
    return paths


def capture_generated(paths):
    for path, how in sorted(paths.items()):
        source, dest = hostpath(path), destination(path)
        if how == "copy":
            shutil.copy2(source, dest)
        else:
            with open(source, encoding="utf-8") as src:
                text = src.read()
            for module in re.findall(r'"(/usr/lib/[^"]+)"', text):
                if not os.path.lexists(root + canonical(module)):
                    fatal(f"{path} names {module}, which the closure lacks")
            with open(dest, "w", encoding="utf-8") as dst:
                dst.write(text.replace('"/usr/lib/', '"' + root + "/usr/lib/"))
        os.chmod(dest, 0o644)


def capture_cursor_theme(generated):
    cursor = hostpath("/etc/alternatives/x-cursor-theme")
    if not os.path.lexists(cursor):
        return
    target = os.readlink(cursor)
    resolved = hostpath(target) if os.path.isabs(target) else os.path.join(os.path.dirname(cursor), target)
    if not os.path.isfile(resolved):
        fatal(f"the x-cursor-theme alternative resolves to {target}, which is not a file")
    dest = destination("/usr/share/icons/default/index.theme")
    shutil.copyfile(resolved, dest)
    os.chmod(dest, 0o644)
    generated["/usr/share/icons/default/index.theme"] = "x-cursor-theme"


def seal_fonts(generated):
    fonts, cache = root + "/usr/share/fonts", root + "/var/cache/fontconfig"
    conf = destination("/share/cc-remote/fonts.conf")
    with open(conf, "w", encoding="utf-8") as fh:
        fh.write('<?xml version="1.0"?>\n<!DOCTYPE fontconfig SYSTEM "urn:fontconfig:fonts.dtd">\n<fontconfig>\n')
        fh.write(f"  <dir>{fonts}</dir>\n  <cachedir>{cache}</cachedir>\n</fontconfig>\n")
    os.chmod(conf, 0o644)
    generated["/share/cc-remote/fonts.conf"] = "snippet"
    directory(cache)
    if not os.path.isdir(fonts):
        return
    for d, _, _ in os.walk(root):
        info = os.lstat(d)
        os.utime(d, (int(info.st_atime), int(info.st_mtime)))
    built = run(["fc-cache", "-f"], FONTCONFIG_FILE=conf)
    if built.returncode != 0:
        fatal(f"fc-cache -f exited {built.returncode}: {built.stderr.strip()}")
    user = os.environ.get("SUDO_USER")
    if not user:
        fatal("capture needs SUDO_USER to prove the font cache valid unprivileged")
    proof = run(["runuser", "-u", user, "--", "env", "FONTCONFIG_FILE=" + conf, "fc-cache", "-v"])
    if proof.returncode != 0:
        fatal(f"unprivileged fc-cache -v exited {proof.returncode}: {proof.stderr.strip()}")
    scanned = [line for line in proof.stdout.splitlines() if line.startswith(fonts + ":") or line.startswith(fonts + "/")]
    stale = [line for line in scanned if not re.search(r": skipping, (existing cache is valid|looped directory detected)", line)]
    if not scanned or stale:
        fatal(f"the font cache is not valid for every closure font directory: {stale or 'no directory was scanned'}")
    fractional = sorted({d for d in (line.split(": ", 1)[0] for line in scanned) if os.stat(d).st_mtime_ns % 1_000_000_000})
    if fractional:
        fatal(f"squashfs stores whole seconds, so the font cache would go stale for {fractional}")
    for path in walk("/var/cache/fontconfig", root):
        generated[path] = "fontconfig"


def settle_icon_caches(themes):
    for theme in themes:
        stamp = int(os.lstat(f"{root}/usr/share/icons/{theme}").st_mtime) + 1
        os.utime(f"{root}/usr/share/icons/{theme}/icon-theme.cache", (stamp, stamp))


def libraries(path):
    result = run(["ld.so", "--list", hostpath(path)])
    if result.returncode != 0:
        fatal(f"ld.so --list {path} exited {result.returncode}: {result.stderr.strip()}")
    missing = [line.split()[0] for line in result.stdout.splitlines() if "not found" in line]
    if missing:
        fatal(f"{path} cannot load {' '.join(missing)}")
    return re.findall(r"=> (/\S+)", result.stdout)


def check_consumers(paths, new):
    resolved = {path: libraries(path) for path in paths}
    owned = owners([lib for libs in resolved.values() for lib in libs] + [canonical(lib) for libs in resolved.values() for lib in libs])
    return {path: sorted({owner for lib in libs for owner in owned.get(lib, owned.get(canonical(lib), [])) if owner in new}) for path, libs in resolved.items()}


def capture():
    with open(payload_path, encoding="utf-8") as fh:
        payload = {key: value or [] for key, value in json.load(fh).items()}
    closure = set(payload["closure"])
    for record in ("packages.before", "packages.after", "seeds"):
        if not os.path.isfile(os.path.join(build, record)):
            fatal(f"{build}/{record} is missing, so this machine did not run packages full")
    new = installed(os.path.join(build, "packages.after")) - installed(os.path.join(build, "packages.before"))
    if not new:
        fatal("packages full installed nothing new, so there is no transaction to capture")
    with open(os.path.join(build, "seeds"), encoding="utf-8") as fh:
        seeds = fh.read().split()
    consumers = resolve_consumers(payload["consumers"])
    resident = partition(seeds, new)
    if new - resident != closure:
        fatal(f"apt.payload.closure differs from the measured partition: resident or absent {sorted(closure - (new - resident))}, undeclared {sorted((new - resident) - closure)}")
    packages = versions(sorted(closure))
    paths_by_package = {package: listed(package) for package in sorted(closure)}
    captured = {canonical(path) for paths in paths_by_package.values() for path in paths}
    check_exposures(payload, captured)
    if os.path.lexists(root):
        fatal(f"{root} already exists on the build machine")
    os.makedirs(root)
    if os.path.realpath(root) != root:
        fatal(f"{root} resolves to {os.path.realpath(root)}, so consumers would not find the same paths")
    files = capture_files(paths_by_package, captured)
    generated = generated_paths(captured)
    capture_generated(generated)
    capture_cursor_theme(generated)
    seal_fonts(generated)
    themes = captured_themes(captured)
    settle_icon_caches(themes)
    bins = ["/usr/bin/" + bin for bin in payload["bins"]]
    consumer_owners = check_consumers(consumers + bins, new)
    with open(hostpath("/etc/os-release"), encoding="utf-8") as fh:
        os_release = dict(line.rstrip("\n").split("=", 1) for line in fh if "=" in line)
    manifest = {
        "packages": packages,
        "resident": sorted(resident),
        "files": files,
        "generated": {path: {"how": how, "sha256": sha256(root + path), "mode": f"{stat.S_IMODE(os.lstat(root + path).st_mode):04o}"} for path, how in sorted(generated.items())},
        "links": payload["links"],
        "consumers": consumer_owners,
        "os": {key: os_release.get(key, "").strip('"') for key in ("VERSION", "VERSION_ID")},
        "libc6": versions(["libc6"])["libc6"]["version"],
    }
    with open(destination("/closure.json"), "w", encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=1, sort_keys=True)
        fh.write("\n")
    for link in payload["links"]:
        path = hostpath(link["path"])
        os.makedirs(os.path.dirname(path), exist_ok=True)
        os.symlink(root + "/" + link["target"], path)
    size = sum(os.lstat(root + entry["path"]).st_size for entry in files if "sha256" in entry)
    print(f"cc-remote: captured {len(closure)} packages, {len(files)} files, {size} bytes into {root}")


os.umask(0o022)
try:
    capture()
except Fatal as err:
    print(f"cc-remote: {err}", file=sys.stderr)
    sys.exit(1)
