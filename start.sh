#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "$0")"
package_dir=$(pwd -P)
if [[ $(id -u) != 0 ]]; then echo '请在 1Panel 终端用 root 执行：bash start.sh' >&2; exit 1; fi
case $(uname -m) in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; *) echo '仅支持 Linux x86_64 / arm64'; exit 1;; esac
binary="vmqfox-api-linux-${arch}"
[[ -f "$binary" ]] || { echo "缺少 $binary，请使用完整安装包"; exit 1; }
command -v systemctl >/dev/null || { echo '需要支持 systemd 的 Linux 系统'; exit 1; }
app_dir=/opt/vmqfox-web
if [[ "$package_dir" != "$app_dir" ]]; then
  if [[ -e "$app_dir/.env" || -e "$app_dir/.installed" ]]; then echo '目标目录已有安装，请从 /opt/vmqfox-web 启动，禁止覆盖已有配置'; exit 1; fi
  mkdir -p -- "$app_dir"
  # Replace inodes so retrying an unfinished installation can update a running binary.
  for file in "$binary" start.sh run.sh; do
    install -m 750 -- "$file" "$app_dir/$file.new"
    mv -f -- "$app_dir/$file.new" "$app_dir/$file"
  done
fi
cd -- "$app_dir"
id vmqfox-web >/dev/null 2>&1 || useradd --system --home-dir "$app_dir" --shell /usr/sbin/nologin vmqfox-web
chown -R vmqfox-web:vmqfox-web "$app_dir"
chmod 750 "$binary" start.sh run.sh
[[ ! -f .env ]] || chmod 600 .env
cat > /etc/systemd/system/vmqfox-web.service <<EOF
[Unit]
Description=VMQ Fox Web Installer and Updater
After=network-online.target docker.service
Wants=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=8

[Service]
Type=simple
User=vmqfox-web
Group=vmqfox-web
WorkingDirectory=$app_dir
Environment=VMQ_MANAGED_RESTART=1
Environment=VMQ_BINARY=$binary
ExecStart=/bin/bash $app_dir/run.sh
Restart=always
RestartSec=2
TimeoutStopSec=20
UMask=0077

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable vmqfox-web.service
systemctl restart vmqfox-web.service
for ((i=0;i<30;i++)); do
  if [[ -s .install-token || -s .env ]]; then break; fi
  sleep 0.2
done
if ! systemctl is-active --quiet vmqfox-web.service; then
  journalctl -u vmqfox-web.service -n 30 --no-pager
  exit 1
fi
echo
if [[ -s .install-token ]]; then
  printf '浏览器打开（把「服务器IP」替换为你的公网 IP）：\nhttp://服务器IP:8080/install?token=%s\n' "$(cat .install-token)"
  echo '如打不开，请在服务器安全组和 1Panel 防火墙放行 TCP 8080。'
else
  echo '服务已配置。访问原网站地址；查看状态：systemctl status vmqfox-web --no-pager'
fi
echo '查看日志：journalctl -u vmqfox-web -n 50 --no-pager'
