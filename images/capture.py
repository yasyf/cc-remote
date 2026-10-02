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

payload_path, host, root, build, user_home, loader_py = sys.argv[1:7]
host = os.path.normpath(host)
LIB = "/usr/lib/" + platform.machine() + "-linux-gnu"
ALIASES = ("/lib64/", "/lib/", "/bin/", "/sbin/")
NEVER = ("/var/lib/dpkg/", "/var/lib/apt/", "/var/cache/apt/", "/var/log/", "/etc/ssh/", "/etc/machine-id", "/home/", "/root/")
LOADER_CONF = "/etc/ld.so.conf.d/zz-cc-remote-closure.conf"
FONTS_CONF = "/etc/fonts/conf.d/99-cc-remote-closure.conf"
CACHES = (
    (LIB + "/gtk-3.0/3.0.0/immodules/", ".so", LIB + "/gtk-3.0/3.0.0/immodules.cache", "rewrite"),
    (LIB + "/gdk-pixbuf-2.0/2.10.0/loaders/", ".so", LIB + "/gdk-pixbuf-2.0/2.10.0/loaders.cache", "rewrite"),
    (LIB + "/gio/modules/", ".so", LIB + "/gio/modules/giomodule.cache", "copy"),
    ("/usr/share/glib-2.0/schemas/", ".gschema.xml", "/usr/share/glib-2.0/schemas/gschemas.compiled", "copy"),
    ("/usr/share/mime/packages/", ".xml", "/usr/share/mime/mime.cache", "copy"),
)
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


def partition(seeds, new, base):
    meta, providers, satisfied = {}, {}, set(base)
    for line in dpkg("-W", "-f", "${Package}\t${Depends}\t${Pre-Depends}\t${Provides}\n", *sorted(new | base)).splitlines():
        pkg, depends, predepends, provides = line.split("\t")
        provided = [re.sub(r"[\s(].*$", "", name.strip()) for name in provides.split(",") if name.strip()]
        if pkg in base:
            satisfied.update(provided)
        else:
            meta[pkg] = (clauses(depends) + clauses(predepends), provided)
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
            if any(alt in satisfied for alt in clause):
                continue
            candidates = [c for alt in clause for c in ([alt] if alt in new else providers.get(alt, []))]
            if candidates and not any(c in resident for c in candidates):
                queue.append(candidates[0])
    return resident


def versions(packages):
    out = dpkg("-W", "-f", "${Package}\t${Version}\t${Architecture}\n", *packages)
    return {pkg: {"version": version, "arch": arch} for pkg, version, arch in (line.split("\t") for line in out.splitlines())}


def listed(package):
    return [line for line in dpkg("-L", package).splitlines() if line.startswith("/")]


def search(paths):
    result = run(["dpkg-query", "-S", *paths])
    reports = result.stderr.splitlines()
    unexpected = [line for line in reports if not re.fullmatch(r"dpkg-query: no path found matching pattern .+", line)]
    if result.returncode not in (0, 1) or unexpected or (result.returncode == 1 and not reports):
        fatal(f"dpkg-query -S exited {result.returncode}" + (f": {result.stderr.strip()}" if result.stderr.strip() else " without reporting why"))
    return result.stdout


def owners(paths):
    found = {}
    paths = sorted(set(paths))
    for at in range(0, len(paths), 300):
        for line in search(paths[at:at + 300]).splitlines():
            match = re.match(r"^(.+?): (/.+)$", line)
            if match and not line.startswith("diversion"):
                found[match.group(2)] = [owner.split(":")[0] for owner in match.group(1).split(", ")]
    return found


def unowned(path):
    return not search([path]).strip()


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


def confine(directory, subject):
    existing = directory
    while not os.path.lexists(existing):
        existing = os.path.dirname(existing)
    if os.path.realpath(existing) != existing or not os.path.isdir(existing):
        fatal(f"{subject} would be written through {existing}, which is not a directory inside the closure")


def confined_directories(top):
    for d, names, _ in os.walk(top):
        confine(d, d[len(root):])
        for name in names:
            confine(os.path.join(d, name), os.path.join(d, name)[len(root):])
        yield d


def directory(path):
    confine(path, path[len(root):])
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


def check_projections(projections, closure, captured):
    owned = owners(projections)
    for path in projections:
        if path not in captured or not os.path.isdir(hostpath(path)):
            fatal(f"apt.payload.projections: {path} is not a directory a closure package ships")
        foreign = sorted(set(owned.get(path, [])) - closure)
        if foreign:
            fatal(f"apt.payload.projections: {path} is also owned by {' '.join(foreign)}, so a link there would hide their files")


def check_projected(projections):
    for path in projections:
        resolved = os.path.realpath(root + path)
        if not resolved.startswith(root + "/") or not os.path.isdir(resolved):
            fatal(f"apt.payload.projections: {path} resolves to {resolved[len(root):] if resolved.startswith(root + '/') else resolved}, outside the closure")


def capture_files(paths_by_package, captured, new):
    files, absolute = [], []
    for package, paths in paths_by_package.items():
        for path in paths:
            source = hostpath(path)
            confine(os.path.dirname(root + canonical(path)), canonical(path))
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
                    if canonical(target) in captured:
                        target = os.path.relpath(canonical(target), os.path.dirname(canonical(path)))
                    else:
                        absolute.append((path, target))
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
    owned = owners([target for _, target in absolute] + [canonical(target) for _, target in absolute])
    for path, target in absolute:
        owner = owned.get(target) or owned.get(canonical(target))
        if not owner or set(owner) & new:
            fatal(f"symlink {path} -> {target} leaves the closure and no pre-existing package owns its target ({owner})")
    return files


def captured_themes(captured):
    return sorted(path.split("/")[4] for path in captured if path.startswith("/usr/share/icons/") and path.endswith("/index.theme") and path.count("/") == 5)


def generated_paths(captured):
    paths = {}
    for modules, suffix, cache, how in CACHES:
        if any(path.startswith(modules) and path.endswith(suffix) for path in captured):
            paths[cache] = how
    if "/usr/share/mime/mime.cache" in paths:
        paths.update({path: "copy" for path in walk("/usr/share/mime") if path not in captured and not path.startswith("/usr/share/mime/packages/")})
    for theme in captured_themes(captured):
        paths[f"/usr/share/icons/{theme}/icon-theme.cache"] = "copy"
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
    if not os.path.lexists(fonts):
        return
    directories = []
    for d in confined_directories(fonts):
        info = os.lstat(d)
        os.utime(d, (int(info.st_atime), int(info.st_mtime)))
        directories.append(d)
    built = run(["fc-cache", "-f"], FONTCONFIG_FILE=conf)
    if built.returncode != 0:
        fatal(f"fc-cache -f exited {built.returncode}: {built.stderr.strip()}")
    user = os.environ.get("SUDO_USER")
    if not user:
        fatal("capture needs SUDO_USER to prove the font cache valid unprivileged")
    proof = run(["runuser", "-u", user, "--", "env", "FONTCONFIG_FILE=" + conf, "fc-cache", "-v"])
    if proof.returncode != 0:
        fatal(f"unprivileged fc-cache -v exited {proof.returncode}: {proof.stderr.strip()}")
    check_font_scan(proof.stdout, fonts, directories)
    for path in walk("/var/cache/fontconfig", root):
        generated[path] = "fontconfig"


def check_font_scan(output, fonts, directories):
    valid, looped = "skipping, existing cache is valid", "skipping, looped directory detected"
    first = {}
    for line in output.splitlines():
        path, sep, status = line.partition(": ")
        if not sep or not (path == fonts or path.startswith(fonts + "/")):
            continue
        if path not in first:
            first[path] = status
            if status.split(":")[0] != valid:
                fatal(f"the font cache is not valid for {path}: {status}")
        elif status != looped:
            fatal(f"fc-cache -v scanned {path} again: {status}")
    missing = [d for d in directories if d not in first]
    if missing:
        fatal(f"fc-cache -v never scanned {' '.join(missing)}")


def settle_icon_caches(themes):
    for theme in themes:
        confine(f"{root}/usr/share/icons/{theme}", f"/usr/share/icons/{theme}")
        stamp = int(os.lstat(f"{root}/usr/share/icons/{theme}").st_mtime) + 1
        os.utime(f"{root}/usr/share/icons/{theme}/icon-theme.cache", (stamp, stamp))


def libraries(path):
    result = run([sys.executable, loader_py, "--host", host, hostpath(path)])
    if result.returncode != 0:
        fatal(f"listing the libraries of {path} exited {result.returncode}: {result.stderr.strip()}")
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
    base = installed(os.path.join(build, "packages.before"))
    new = installed(os.path.join(build, "packages.after")) - base
    if not new:
        fatal("packages full installed nothing new, so there is no transaction to capture")
    with open(os.path.join(build, "seeds"), encoding="utf-8") as fh:
        seeds = fh.read().split()
    consumers = resolve_consumers(payload["consumers"])
    resident = partition(seeds, new, base)
    if new - resident != closure:
        fatal(f"apt.payload.closure differs from the measured partition: resident or absent {sorted(closure - (new - resident))}, undeclared {sorted((new - resident) - closure)}")
    packages = versions(sorted(closure))
    paths_by_package = {package: listed(package) for package in sorted(closure)}
    captured = {canonical(path) for paths in paths_by_package.values() for path in paths}
    check_exposures(payload, captured)
    check_projections(payload["projections"], closure, captured)
    if os.path.lexists(root):
        fatal(f"{root} already exists on the build machine")
    os.makedirs(root)
    if os.path.realpath(root) != root:
        fatal(f"{root} resolves to {os.path.realpath(root)}, so consumers would not find the same paths")
    files = capture_files(paths_by_package, captured, new)
    check_projected(payload["projections"])
    generated = generated_paths(captured)
    capture_generated(generated)
    capture_cursor_theme(generated)
    seal_fonts(generated)
    themes = captured_themes(captured)
    settle_icon_caches(themes)
    links = [link for link in payload["links"] if link.get("requirement") != "optional" or os.path.isdir(root + "/" + link["target"])]
    bins = ["/usr/bin/" + bin for bin in payload["bins"]]
    consumer_owners = check_consumers(consumers + bins, new)
    with open(hostpath("/etc/os-release"), encoding="utf-8") as fh:
        os_release = dict(line.rstrip("\n").split("=", 1) for line in fh if "=" in line)
    manifest = {
        "packages": packages,
        "resident": sorted(resident),
        "files": files,
        "generated": {path: {"how": how, "sha256": sha256(root + path), "mode": f"{stat.S_IMODE(os.lstat(root + path).st_mode):04o}"} for path, how in sorted(generated.items())},
        "links": links,
        "projections": payload["projections"],
        "consumers": consumer_owners,
        "os": {key: os_release.get(key, "").strip('"') for key in ("VERSION", "VERSION_ID")},
        "libc6": versions(["libc6"])["libc6"]["version"],
    }
    with open(destination("/closure.json"), "w", encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=1, sort_keys=True)
        fh.write("\n")
    for link in links:
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
