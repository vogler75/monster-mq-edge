#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

submodule_name="mochi-mqtt-server"
mode="${1:-}"

if [[ "${mode}" != "" && "${mode}" != "--recorded" ]]; then
  echo "Usage: $0 [--recorded]"
  echo
  echo "Without arguments, clone/update ${submodule_name} to the latest commit from its configured branch."
  echo "With --recorded, update ${submodule_name} to the exact commit recorded by this repo."
  exit 2
fi

if [[ ! -f .gitmodules ]]; then
  echo "Error: .gitmodules not found. Run this script from the monster-mq-edge checkout."
  exit 1
fi

submodule_path="$(git config --file .gitmodules --get "submodule.${submodule_name}.path" 2>/dev/null || true)"

if [[ -z "${submodule_path}" ]]; then
  echo "Error: ${submodule_name} is not configured in .gitmodules."
  exit 1
fi

echo "Synchronizing ${submodule_name} submodule URL..."
git submodule sync -- "${submodule_path}"

if [[ "${mode}" == "--recorded" ]]; then
  echo "Cloning/updating ${submodule_name} submodule to recorded commit..."
  git submodule update --init --recursive "${submodule_path}"
  echo
  echo "${submodule_name} submodule is at recorded commit:"
else
  echo "Cloning/updating ${submodule_name} submodule to latest remote commit..."
  git submodule update --init --remote --merge "${submodule_path}"
  echo
  echo "${submodule_name} submodule is at:"
fi

git submodule status "${submodule_path}"
