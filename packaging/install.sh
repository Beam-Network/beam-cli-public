#!/bin/sh
set -eu

cdn_base="${BEAM_CDN_BASE_URL:-https://cdn.b1m.ai/cli}"
install_dir="${BEAM_INSTALL_DIR:-/usr/local/bin}"
version="${BEAM_VERSION:-latest}"
requested_pr="${BEAM_PR:-}"
cdn_base="${cdn_base%/}"

case "${requested_pr}" in
  '') ;;
  *[!0-9]*) echo "BEAM_PR must be a positive integer" >&2; exit 1 ;;
  0) echo "BEAM_PR must be a positive integer" >&2; exit 1 ;;
esac
if [ -n "${requested_pr}" ] && [ "${requested_pr}" != "$(printf '%s' "${requested_pr}" | sed 's/^0*//')" ]; then
  echo "BEAM_PR must not contain leading zeroes" >&2
  exit 1
fi

if [ -t 1 ] && [ -z "${NO_COLOR+x}" ] && [ "${TERM:-}" != "dumb" ]; then
  bold='\033[1m'
  green='\033[32m'
  cyan='\033[36m'
  reset='\033[0m'
else
  bold=''
  green=''
  cyan=''
  reset=''
fi

success() {
  printf '%b\342\234\223%b %s\n' "${green}" "${reset}" "$1"
}

printf '\n%bBeam CLI Installer%b\n\n' "${bold}" "${reset}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "${arch}" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "Unsupported architecture: ${arch}" >&2; exit 1 ;;
esac
case "${os}" in
  darwin|linux) ;;
  *) echo "This installer supports macOS and Linux. Use the Windows release archive on Windows." >&2; exit 1 ;;
esac
success "Detected ${os}/${arch}"

if [ "${version}" = "latest" ]; then
  manifest="latest.json"
  if [ -n "${requested_pr}" ]; then
    manifest="pr-${requested_pr}.json"
  fi
  version="$(curl -fsSL "${cdn_base}/${manifest}" |
    sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
fi
plain_version="${version#v}"
case "${plain_version}" in
  ""|*[!0-9A-Za-z.-]*)
    echo "Invalid Beam version returned by the CDN: ${version}" >&2
    exit 1
    ;;
esac
version="v${plain_version}"
pr_number="$(printf '%s\n' "${plain_version}" | sed -n 's/^0\.0\.0-pr\.\([1-9][0-9]*\)\..*/\1/p')"
if [ -n "${requested_pr}" ] && [ "${requested_pr}" != "${pr_number}" ]; then
  echo "CDN manifest did not resolve bundle for PR ${requested_pr}" >&2
  exit 1
fi
case "${plain_version}" in
  dev|dev.*|*-dev|*-dev.*)
    cli_binary="beam-dev"
    legacy_cli_binary="beam-cli-dev"
    agent_binary="beam-tunnel-agent-dev"
    ;;
  0.0.0-pr.*)
    test -n "${pr_number}" || {
      echo "Invalid PR bundle version returned by the CDN: ${version}" >&2
      exit 1
    }
    cli_binary="beam-pr-${pr_number}"
    legacy_cli_binary=""
    agent_binary="beam-tunnel-agent-pr${pr_number}"
    ;;
  *)
    cli_binary="beam"
    legacy_cli_binary="beam-cli"
    agent_binary="beam-tunnel-agent"
    ;;
esac
archive="beam_${plain_version}_${os}_${arch}.tar.gz"
base="${cdn_base}/releases/${version}"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT INT TERM

download() {
  curl -fsSL "$1" -o "$2"
}

download "${base}/${archive}" "${work_dir}/${archive}"
download "${base}/checksums.txt" "${work_dir}/checksums.txt"
expected="$(awk -v archive="${archive}" '$2 == archive { print $1 }' "${work_dir}/checksums.txt")"
if [ -z "${expected}" ]; then
  echo "Checksum for ${archive} is missing." >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${work_dir}/${archive}" | awk '{ print $1 }')"
else
  actual="$(shasum -a 256 "${work_dir}/${archive}" | awk '{ print $1 }')"
fi
if [ "${expected}" != "${actual}" ]; then
  echo "Checksum verification failed for ${archive}." >&2
  exit 1
fi
success "Package downloaded and verified"

tar -C "${work_dir}" -xzf "${work_dir}/${archive}" "${cli_binary}" "${agent_binary}"
success "Package extracted"

fresh_install=1
if [ -x "${install_dir}/${cli_binary}" ]; then
  fresh_install=0
elif [ -n "${legacy_cli_binary}" ] && [ -x "${install_dir}/${legacy_cli_binary}" ]; then
  fresh_install=0
fi

# A previous installation may live in another PATH directory. Try every Beam
# CLI name so its matching CLI can gracefully stop the running companion
# before this bundle is activated.
stop_existing_cli() {
  existing_cli="$1"
  if [ -n "${existing_cli}" ] && [ -x "${existing_cli}" ]; then
    "${existing_cli}" tunnel stop >/dev/null 2>&1 || true
  fi
}
if [ -n "${pr_number}" ]; then
  stop_existing_cli "${install_dir}/${cli_binary}"
  stop_existing_cli "$(command -v "${cli_binary}" 2>/dev/null || true)"
else
  for existing_cli in \
    "${install_dir}/${cli_binary}" \
    "${install_dir}/${legacy_cli_binary}" \
    "$(command -v beam 2>/dev/null || true)" \
    "$(command -v beam-dev 2>/dev/null || true)" \
    "$(command -v beam-cli 2>/dev/null || true)" \
    "$(command -v beam-cli-dev 2>/dev/null || true)"
  do
    stop_existing_cli "${existing_cli}"
  done
fi

if [ ! -d "${install_dir}" ]; then
  if ! mkdir -p "${install_dir}" 2>/dev/null; then
    sudo mkdir -p "${install_dir}"
  fi
fi
success "Install directory ready"

if [ -w "${install_dir}" ]; then
  install -m 0755 "${work_dir}/${cli_binary}" "${install_dir}/.${cli_binary}.new"
  install -m 0755 "${work_dir}/${agent_binary}" "${install_dir}/.${agent_binary}.new"
  mv "${install_dir}/.${cli_binary}.new" "${install_dir}/${cli_binary}"
  mv "${install_dir}/.${agent_binary}.new" "${install_dir}/${agent_binary}"
else
  sudo install -m 0755 "${work_dir}/${cli_binary}" "${install_dir}/.${cli_binary}.new"
  sudo install -m 0755 "${work_dir}/${agent_binary}" "${install_dir}/.${agent_binary}.new"
  sudo mv "${install_dir}/.${cli_binary}.new" "${install_dir}/${cli_binary}"
  sudo mv "${install_dir}/.${agent_binary}.new" "${install_dir}/${agent_binary}"
fi

if [ -z "${pr_number}" ]; then
  for legacy_binary in "${legacy_cli_binary}" beam-agentd; do
    if [ -e "${install_dir}/${legacy_binary}" ]; then
      if [ -w "${install_dir}" ]; then
        rm -f "${install_dir}/${legacy_binary}"
      else
        sudo rm -f "${install_dir}/${legacy_binary}"
      fi
    fi
  done
fi
success "${cli_binary} and ${agent_binary} ${version} installed successfully in ${install_dir}"

printf '\n%b\342\234\250 Installation Complete!%b\n' "${green}" "${reset}"

skip_onboarding=0
case "${BEAM_SKIP_ONBOARDING:-}" in
  1|true|True|TRUE|yes|Yes|YES) skip_onboarding=1 ;;
esac
if [ "${fresh_install}" -eq 1 ] && [ "${skip_onboarding}" -eq 0 ]; then
  if [ -t 1 ] && [ -r /dev/tty ] && [ -w /dev/tty ]; then
    printf '\n%bBeam onboarding%b\n\n' "${bold}" "${reset}"
    if ! "${install_dir}/${cli_binary}" setup </dev/tty; then
      printf '\nOnboarding was not completed. Run "%s setup" to try again.\n' "${cli_binary}" >&2
    fi
  else
    printf '\nConfigure Beam when ready:\n   %s setup\n' "${cli_binary}"
  fi
fi

printf '\n%bStart using Beam:%b\n' "${bold}" "${reset}"
printf '   %s\n' "${cli_binary}"
printf '\n%bHappy beaming! \360\237\232\200%b\n\n' "${cyan}" "${reset}"
