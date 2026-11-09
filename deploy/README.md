# 部署说明（新模型，已迁移）

单二进制构建：

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o km ./cmd/km
```

systemd 安装与 unit 模板已内嵌为 `km install`（模板源 `cmd/km/km.service.tmpl`，部署源按约定归 `BSV8_Engineering/ops/km/`）。旧 `deploy/km.service` 重复模板已删除，不再维护：

```sh
sudo ./km install
sudo ./km install --data-dir /var/lib/km
systemctl daemon-reload
systemctl enable --now km
```

- 首次初始化用 `km init`（网络最后选择）；systemd 只起 `km daemon`（锁定启动）。
- 不再使用 `LoadCredentialEncrypted wallet-password`；密码仅经 `km unlock`/Web 解锁，TTY 隐藏输入。
- 旧 `-config/-server/-admin` 与 `config.json`/`admin.apiKey` 路径已退役，见 `README.md` 新布局。
