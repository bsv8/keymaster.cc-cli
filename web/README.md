# km web

Vite + React 管理界面，唯一来源为 Go 后台 `/api/*`。

- 开发：`npm run dev`（Vite 热更新，`/api` 代理到 `http://127.0.0.1:2100`）。
- 发布：`npm run build` 输出 `web/dist/`，由 Go `embed` 进 `km` 单二进制；运行时不依赖 Node 或外部前端文件。
- 主题、语言唯一持久来源为 `settings.json`；缺省跟随系统，读取设置后应用；不使用 `localStorage` 保存主题、语言、密码或 token。
- 解锁密码只用于解密 KeyHold，不建立登录会话，提交后清空表单，不写入浏览器存储。
- 开发 Origin 仅在开发模式允许；正式版本不扩大 Host/Origin 校验。
