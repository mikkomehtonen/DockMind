#!/usr/bin/env bash

die()
{
    echo "ERROR: $*"
    exit 1
}


[[ "${EUID}" -eq 0 ]] || die "This helper must run as root"

install -o root -g root -m 0755 \
  dockmind-egpu-unbind \
  /usr/local/libexec/dockmind-egpu-unbind

install -o root -g root -m 0755 \
  dockmind-egpu-unbind.service \
  /etc/systemd/system/dockmind-egpu-unbind.service

systemctl daemon-reload
