# 部署说明

## 单二进制

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o km ./cmd/km
```

产物是静态链接的单个文件（约 30 MB），不依赖 libc 或运行时目录；拷到目标机即可运行。

## systemd

1. 创建专用用户与目录：

   ```sh
   useradd --system --home /var/lib/km --create-home km
   install -d -o km -g km -m 0700 /etc/km /home/km/.km
   install -d -o km -g km -m 0755 /srv/files /srv/downloads
   ```

2. 首次初始化（交互式，创建 `~/.km/config.json` 与钱包）：

   ```sh
   sudo -u km -H /usr/local/bin/km -config /etc/km/config.json
   # 或手动执行一次 km 的首次运行流程后把配置放到 /etc/km/config.json
   ```

3. 加密保存钱包密码凭据：

   ```sh
   systemd-creds encrypt --name=wallet-password - /etc/km/wallet-password.cred
   ```

4. 安装并启用服务：

   ```sh
   install -m 0644 deploy/km.service /etc/systemd/system/km.service
   systemctl daemon-reload
   systemctl enable --now km
   journalctl -u km -f
   ```

   `deploy/km.service` 中的 `ExecStart` 按下载或售卖模式修改；`Restart=on-failure`
   与 `TimeoutStopSec=30s` 对应 CLI 的退出码与有界收尾。

## 网络与数据

- 配置里的 `bsvNetwork` 决定 WhatsonChain 端点；测试网用 `test`。
- `-admin` 默认只监听 `127.0.0.1:8790`；API Key 在配置的 `admin.apiKey`。
- 数据：`~/.km/config.json`、`~/.km/client/<seed-hash>/`（下载/账本/日志）、
  `~/.km/server/`（目录索引、定价、售卖会话）、`~/.km/client/unresolved/`（未决广播）。
- 备份 `~/.km/config.json` 与 server 会话目录即可；私钥只存在加密的 KeyHold 文档里。
