#!/usr/bin/env bash
# Public bootstrap: curl -fsSL <repository>/main/install.sh | sudo bash
# Keep execution in main so a truncated download cannot run a partial installer.
set -euo pipefail

usage() {
  cat <<'EOF'
agentbox Linux installer (new installations only)

Usage: sudo bash install.sh [--version vX.Y.Z] [--listen 0.0.0.0:8180]

Downloads a verified release, builds the pinned workspace image, and starts
agentbox.service. v0.1.1+ also installs all five bundled abox-link clients.
No Go or Node required on the host.
Ubuntu/Debian: missing dependencies and Docker are installed with apt.
Other systemd Linux distributions: install Python 3.9+, Git, curl, CA certificates,
tzdata and a local Docker Engine first. Supports x86_64 and arm64.
Existing installations are preserved; use deploy/release.py for upgrades.
EOF
}

fail() { echo "agentbox: $*" >&2; exit 1; }

main() {
  local version='' listen='0.0.0.0:8180' arch task_tmp distro missing path dependency asset cleanup_cmd
  while (($#)); do
    case "$1" in
      --version|--listen)
        (($# >= 2)) || fail "Missing value for $1"
        if [[ "$1" == --version ]]; then version=$2; else listen=$2; fi
        shift 2 ;;
      -h|--help) usage; return ;;
      *) fail "Unknown argument: $1 (see --help)" ;;
    esac
  done
  [[ -z "$version" || "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail 'Invalid release version'
  [[ $(uname -s) == Linux ]] || fail 'Run this installer on the target Linux server'
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) fail 'Only x86_64 and arm64 are supported' ;;
  esac
  [[ $EUID == 0 ]] || fail 'Root is required; pipe the installer to sudo bash'
  [[ -d /run/systemd/system ]] && command -v systemctl >/dev/null || fail 'A running systemd host is required'
  # Check before installing packages or starting Docker. Never adopt an existing deployment.
  for path in /etc/agentbox /opt/agentbox /var/lib/agentbox /var/cache/agentbox /etc/systemd/system/agentbox.service; do
    [[ ! -e "$path" && ! -L "$path" ]] || fail "Existing installation path: $path. Use deploy/release.py to upgrade or resume; no data was changed."
  done
  [[ -z $(systemctl show agentbox.service --property=FragmentPath --value) ]] || fail 'An agentbox service already exists; use the migration/upgrade instructions'

  distro=''
  if [[ -r /etc/os-release ]]; then
    distro=$(. /etc/os-release; echo "${ID:-}")
  fi
  missing=''
  for dependency in curl python3 git; do
    command -v "$dependency" >/dev/null || missing="$missing $dependency"
  done
  [[ -f /etc/ssl/certs/ca-certificates.crt || -f /etc/pki/tls/certs/ca-bundle.crt ]] || missing="$missing ca-certificates"
  [[ -f /usr/share/zoneinfo/Asia/Shanghai ]] || missing="$missing tzdata"
  if [[ -n "$missing" ]]; then
    case "$distro" in
      ubuntu|debian)
        apt-get update
        DEBIAN_FRONTEND=noninteractive apt-get install -y curl python3 git ca-certificates tzdata ;;
      *) fail "Install missing dependencies first:$missing" ;;
    esac
  fi
  python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 9) else "Python 3.9+ is required")'

  if [[ -e /sys/fs/selinux/enforce ]] && ! command -v restorecon >/dev/null; then
    fail 'SELinux requires restorecon; install policycoreutils first'
  fi

  umask 077
  task_tmp=$(mktemp -d)
  # EXIT also covers download/checksum failures. Do not remove installation data.
  # EXIT can run after main's locals have unwound; capture a shell-quoted path.
  printf -v cleanup_cmd 'rm -rf -- %q' "$task_tmp"
  trap "$cleanup_cmd" EXIT
  if [[ -z "$version" ]]; then
    echo 'Finding the latest stable agentbox release...'
    curl --proto '=https' --proto-redir '=https' -fsSL --retry 3 --connect-timeout 15 --max-time 60 \
      https://api.github.com/repos/devilcoolyue/agentbox-releases/releases/latest -o "$task_tmp/latest.json" || \
      fail 'No stable release could be downloaded. Check GitHub connectivity, or select a published prerelease with --version.'
    version=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["tag_name"])' "$task_tmp/latest.json")
  fi
  [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail 'Release has an invalid version'
  local archive="agentbox_${version}_linux_${arch}.tar.gz"
  local base="https://github.com/devilcoolyue/agentbox-releases/releases/download/$version"
  echo "Downloading $version (linux/$arch)..."
  for asset in SHA256SUMS "$archive"; do
    curl --proto '=https' --proto-redir '=https' -fsSL --retry 3 --connect-timeout 15 --max-time 600 \
      "$base/$asset" -o "$task_tmp/$asset" || fail "Release asset unavailable: $asset"
  done
  # Verify exactly the selected asset, and extract only regular files/directories
  # beneath its expected root. Works on Python 3.9 without tar filter='data'.
  python3 - "$task_tmp" "$archive" <<'PY'
import hashlib, pathlib, re, sys, tarfile
stage = pathlib.Path(sys.argv[1]); name = sys.argv[2]; root = name[:-7]
matches = []
for line in (stage / 'SHA256SUMS').read_text().splitlines():
    parts = line.split()
    if len(parts) == 2 and parts[1] == name:
        matches.append(parts[0])
if len(matches) != 1 or not re.fullmatch('[0-9a-fA-F]{64}', matches[0]):
    raise SystemExit('Missing/duplicate/invalid checksum for selected release')
digest = hashlib.sha256()
with (stage / name).open('rb') as source:
    for chunk in iter(lambda: source.read(1024 * 1024), b''):
        digest.update(chunk)
if digest.hexdigest() != matches[0].lower():
    raise SystemExit('Release checksum mismatch; installation stopped')
with tarfile.open(stage / name, 'r:gz') as archive:
    members = archive.getmembers(); seen = set(); total = 0
    for member in members:
        path = pathlib.PurePosixPath(member.name)
        if (not path.parts or path.parts[0] != root or path.is_absolute() or '..' in path.parts
                or not (member.isfile() or member.isdir()) or path in seen
                or member.mode & 0o7000 or member.size < 0):
            raise SystemExit('Unsafe release archive member')
        seen.add(path); total += member.size
    if len(members) > 100000 or total > 2 * 1024**3:
        raise SystemExit('Release archive exceeds extraction limits')
    for member in members:
        archive.extract(member, stage, set_attrs=False)
        (stage / member.name).chmod(member.mode & 0o777)
PY
  local package="$task_tmp/${archive%.tar.gz}"
  [[ -f "$package/deploy/bootstrap.py" ]] || fail 'This release predates the one-command installer; select a newer release'
  # Pin every Docker operation to the same local daemon used by systemd.
  unset DOCKER_CONTEXT DOCKER_TLS_VERIFY DOCKER_CERT_PATH
  export DOCKER_HOST=unix:///var/run/docker.sock
  if ! command -v docker >/dev/null; then
    case "$distro" in
      ubuntu|debian)
        echo 'Installing Docker Engine from the distribution packages...'
        apt-get update
        DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io ;;
      *) fail 'Install a local Docker Engine first, then run this installer again' ;;
    esac
  fi
  if ! docker info >/dev/null 2>&1; then
    systemctl enable --now docker.service
  fi
  docker info >/dev/null || fail 'Local Docker Engine is unavailable'
  python3 "$package/deploy/bootstrap.py" --package "$package" --listen "$listen"
  # Run cleanup while task_tmp is still in scope, then disarm EXIT.
  rm -rf -- "$task_tmp"
  trap - EXIT
}

main "$@"
