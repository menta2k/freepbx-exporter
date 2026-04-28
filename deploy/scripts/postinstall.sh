#!/bin/sh
set -e

USER=freepbx-exporter
GROUP=freepbx-exporter

if ! getent group "$GROUP" >/dev/null; then
    groupadd --system "$GROUP"
fi
if ! getent passwd "$USER" >/dev/null; then
    useradd --system --gid "$GROUP" --no-create-home \
            --home-dir /nonexistent --shell /usr/sbin/nologin "$USER"
fi

chown root:"$GROUP" /etc/default/freepbx-exporter || true
chmod 0640 /etc/default/freepbx-exporter || true

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    systemctl enable freepbx-exporter.service || true
    systemctl restart freepbx-exporter.service || true
fi

exit 0
