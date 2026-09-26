#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Run Tymbal's ALSA smoke checks in a KVM Linux guest with snd-dummy and snd-aloop.

Usage: scripts/alsa-guest.sh [options]

Options:
  --duration DURATION   snd-dummy playback duration (default: 2s)
  --loopback-duration DURATION
                        Native snd-aloop continuity duration (default: 2s)
  --load LIST           Native loopback load: cpu, gc, or cpu,gc
  --rate HZ             Sample rate (default: 48000)
  --period FRAMES       Period size (default: 256)
  --periods COUNT       Buffer depth (default: 2)
  --log PATH            Save guest output (default: cache/guest-TIMESTAMP.log)
  --report PATH         Save native loopback JSON outside the repo
  -h, --help            Show this help

The script uses an installed generic kernel when available. Otherwise it
downloads and extracts the current Ubuntu generic kernel into the Tymbal cache;
it does not install a kernel or change the host boot configuration. Set
TYMBAL_ALSA_KERNEL_IMAGE and TYMBAL_ALSA_MODULES_DIR to select an existing
kernel and matching modules directory.

Host packages: qemu-system-x86, virtme-ng, busybox-static, kmod, and Go.
USAGE
}

duration=2s
loopback_duration=2s
load=
rate=48000
period=256
periods=2
log_path=
report_path=

while (($#)); do
  case "$1" in
    --duration)
      (($# >= 2)) || { usage >&2; exit 2; }
      duration=$2
      shift 2
      ;;
    --loopback-duration)
      (($# >= 2)) || { usage >&2; exit 2; }
      loopback_duration=$2
      shift 2
      ;;
    --load)
      (($# >= 2)) || { usage >&2; exit 2; }
      load=$2
      shift 2
      ;;
    --rate)
      (($# >= 2)) || { usage >&2; exit 2; }
      rate=$2
      shift 2
      ;;
    --period)
      (($# >= 2)) || { usage >&2; exit 2; }
      period=$2
      shift 2
      ;;
    --periods)
      (($# >= 2)) || { usage >&2; exit 2; }
      periods=$2
      shift 2
      ;;
    --log)
      (($# >= 2)) || { usage >&2; exit 2; }
      log_path=$2
      shift 2
      ;;
    --report)
      (($# >= 2)) || { usage >&2; exit 2; }
      report_path=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'unknown option: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

for tool in virtme-run qemu-system-x86_64 depmod dpkg-deb go; do
  command -v "$tool" >/dev/null 2>&1 || {
    printf 'required host tool is missing: %s\nInstall qemu-system-x86, virtme-ng, busybox-static, kmod, and Go.\n' "$tool" >&2
    exit 1
  }
done

if [[ ! -r /dev/kvm || ! -w /dev/kvm ]]; then
  printf '/dev/kvm is not readable and writable for %s; an accessible KVM device is required.\n' "$(id -un)" >&2
  exit 1
fi

cache_root=${TYMBAL_ALSA_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/tymbal/alsa-guest}
mkdir -p "$cache_root"

kernel_image=${TYMBAL_ALSA_KERNEL_IMAGE:-}
modules_dir=${TYMBAL_ALSA_MODULES_DIR:-}
kernel_release=${TYMBAL_ALSA_KERNEL_RELEASE:-}

if [[ -z "$kernel_image" ]]; then
  shopt -s nullglob
  for candidate in /boot/vmlinuz-*-generic; do
    candidate_release=${candidate##*/vmlinuz-}
    if [[ -d "/lib/modules/$candidate_release" ]]; then
      kernel_image=$candidate
      kernel_release=$candidate_release
      modules_dir=/lib/modules/$candidate_release
      break
    fi
  done
  shopt -u nullglob
fi

if [[ -z "$kernel_image" ]]; then
  for tool in apt-cache apt; do
    command -v "$tool" >/dev/null 2>&1 || {
      printf 'no installed generic kernel was found, and %s is required to prepare one in the cache.\n' "$tool" >&2
      exit 1
    }
  done
  command -v apt-cache >/dev/null 2>&1 || exit 1
  apt_metadata=$(apt-cache show linux-image-generic 2>/dev/null)
  image_package=$(printf '%s\n' "$apt_metadata" \
    | sed -n 's/^Depends: //p' \
    | sed -n '1p' \
    | tr ',' '\n' \
    | sed 's/[[:space:]].*$//' \
    | awk '/^linux-image-[0-9].*-generic$/ { print; exit }')
  [[ -n "$image_package" ]] || {
    printf 'could not find the Ubuntu generic kernel package in apt metadata.\n' >&2
    exit 1
  }
  kernel_release=${image_package#linux-image-}
  modules_package="linux-modules-$kernel_release"
  extra_package="linux-modules-extra-$kernel_release"
  package_dir=$cache_root/packages
  kernel_root=$cache_root/kernel-$kernel_release
  kernel_image=$kernel_root/boot/vmlinuz-$kernel_release
  modules_dir=$kernel_root/lib/modules/$kernel_release
  if [[ ! -r "$kernel_image" || ! -d "$modules_dir" ]]; then
    mkdir -p "$package_dir" "$kernel_root"
    (
      cd "$package_dir"
      apt download "$image_package" "$modules_package" "$extra_package"
    )
    for package in "$package_dir"/linux-image-"$kernel_release"_*.deb \
      "$package_dir"/linux-modules-"$kernel_release"_*.deb \
      "$package_dir"/linux-modules-extra-"$kernel_release"_*.deb; do
      [[ -f "$package" ]] || continue
      dpkg-deb -x "$package" "$kernel_root"
    done
  fi
  [[ -r "$kernel_image" && -d "$modules_dir" ]] || {
    printf 'Ubuntu kernel packages did not provide the expected kernel image and modules.\n' >&2
    exit 1
  }
  chmod a+r "$kernel_image"
fi

[[ -n "$kernel_release" ]] || kernel_release=${kernel_image##*/vmlinuz-}
[[ -n "$modules_dir" ]] || modules_dir=/lib/modules/$kernel_release
[[ -r "$kernel_image" ]] || { printf 'kernel image is not readable: %s\n' "$kernel_image" >&2; exit 1; }
[[ -d "$modules_dir" ]] || { printf 'kernel modules directory is missing: %s\n' "$modules_dir" >&2; exit 1; }

for module in snd-dummy snd-aloop; do
  find "$modules_dir" -type f \( -name "$module.ko" -o -name "$module.ko.*" \) -print -quit | grep -q . || {
    printf '%s is missing from %s\n' "$module" "$modules_dir" >&2
    exit 1
  }
done

if [[ ! -f "$modules_dir/modules.dep" ]]; then
  module_base=${modules_dir%/lib/modules/$kernel_release}
  if [[ -z "$module_base" || "$module_base" == "$modules_dir" ]]; then
    module_base=/
  fi
  depmod -b "$module_base" "$kernel_release"
fi

go_root=$(go env GOROOT)
go_bin=$go_root/bin/go
[[ -x "$go_bin" ]] || { printf 'Go binary is not readable in guest root: %s\n' "$go_bin" >&2; exit 1; }

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
log_path=${log_path:-$cache_root/guest-$timestamp.log}
report_path=${report_path:-$cache_root/native-loopback-$timestamp.json}
mkdir -p "$(dirname "$log_path")"
mkdir -p "$(dirname "$report_path")"
run_dir=$(mktemp -d "$cache_root/run.XXXXXX")
go_cache=$cache_root/go-build-cache
mkdir -p "$go_cache"
trap '[[ ! -f "$run_dir/native-loopback.json" ]] || cp "$run_dir/native-loopback.json" "$report_path"; rm -rf "$run_dir"' EXIT

guest_script=$run_dir/run.sh
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  printf 'cd %q\n' "$repo_root"
  printf 'export PATH=%q:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n' "$go_root/bin"
  printf 'export GOCACHE=%q\n' /tmp/tymbal-go-cache
  printf 'export GOTOOLCHAIN=local\n'
  printf 'rate=%q\nperiod=%q\nperiods=%q\nduration=%q\nloopback_duration=%q\nload=%q\n' \
    "$rate" "$period" "$periods" "$duration" "$loopback_duration" "$load"
  cat <<'GUEST'
echo "guest kernel: $(uname -r)"
modprobe snd-dummy
modprobe snd-aloop
go test ./internal/alsa
go build -buildvcs=false -o /tmp/tymbal-alsa-run/tymbal ./cmd/tymbal
devices=$(/tmp/tymbal-alsa-run/tymbal devices)
printf '%s\n' "$devices"
grep -Fq $'hw:Dummy,0\t' <<<"$devices"
grep -Fq $'hw:Loopback,0\t' <<<"$devices"
/tmp/tymbal-alsa-run/tymbal tone \
  -host alsa -device hw:Dummy,0 -rate "$rate" -period "$period" \
  -periods "$periods" -dur "$duration"
/tmp/tymbal-alsa-run/tymbal loopback \
  -host alsa -out hw:Loopback,0 -in hw:Loopback,1 -channels 1 \
  -rate "$rate" -period "$period" -periods "$periods" \
  -dur "$loopback_duration" -load "$load" \
  -json /tmp/tymbal-alsa-run/native-loopback.json
GUEST
} >"$guest_script"
chmod +x "$guest_script"

virtme_args=(
  --kimg "$kernel_image"
  --user root
  --cpus "${TYMBAL_ALSA_CPUS:-2}"
  --memory "${TYMBAL_ALSA_MEMORY:-2048}"
  --show-boot-console
  --rwdir "/tmp/tymbal-alsa-run=$run_dir"
  --rwdir "/tmp/tymbal-go-cache=$go_cache"
)
if [[ "$modules_dir" != "/lib/modules/$kernel_release" ]]; then
  virtme_args+=(--rwdir "/lib/modules/$kernel_release=$modules_dir")
fi

printf 'kernel: %s\nmodules: %s\nlog: %s\nreport: %s\n' "$kernel_release" "$modules_dir" "$log_path" "$report_path"
virtme-run "${virtme_args[@]}" --script-sh 'exec /bin/bash /tmp/tymbal-alsa-run/run.sh' 2>&1 | tee "$log_path"
