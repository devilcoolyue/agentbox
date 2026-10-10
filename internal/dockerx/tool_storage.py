# Runs only inside a session container, as UID 1000. No host path is accepted.
import json, os, re, signal, sys
signal.alarm(20)
platform = sys.argv[1]
if not re.fullmatch(r'linux-(amd64|arm64)-[a-z0-9]+-[a-z0-9.]+', platform):
    raise ValueError('unsupported tool platform')
public = '/opt/agentbox-tools'
cache = '/var/cache/agentbox-user'
flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW

def sub(parent, name):
    try: os.mkdir(name, 0o700, dir_fd=parent)
    except FileExistsError: pass
    return os.open(name, flags, dir_fd=parent)

def link(parent, name, target):
    try: os.symlink(target, name, dir_fd=parent)
    except FileExistsError: pass  # Never replace user downloads or custom links.

root = os.open(cache, flags)
base = sub(root, platform)
os.close(root)
try:
    for name in ('go-build', 'go-mod', 'npm', 'playwright', 'puppeteer'):
        fd = sub(base, name)
        try:
            source = f'{public}/{name}/{platform}'
            if name == 'playwright' and os.path.isdir(source):
                for entry in os.scandir(source):
                    if re.fullmatch(r'[a-z][a-z0-9_]*-[0-9]+', entry.name) and entry.is_dir(follow_symlinks=False):
                        link(fd, entry.name, source + '/' + entry.name)
            if name == 'puppeteer' and os.path.isdir(source):
                for browser in ('chrome', 'chrome-headless-shell', 'chromium', 'firefox'):
                    directory = source + '/' + browser
                    if not os.path.isdir(directory): continue
                    dest = sub(fd, browser)
                    try:
                        for entry in os.scandir(directory):
                            if re.fullmatch(r'[a-z0-9]+-[0-9.]+', entry.name) and entry.is_dir(follow_symlinks=False):
                                link(dest, entry.name, directory + '/' + entry.name)
                    finally: os.close(dest)
        finally: os.close(fd)
finally: os.close(base)
print(json.dumps({'platform': platform, 'cache_ready': True}))
