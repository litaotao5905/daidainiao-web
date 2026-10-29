# 网页呆呆鸟 v1.9.8 部署手册

> 面向 Workbuddy / 异机部署，纯实战步骤，无冗余废话。

---

## 1. 飞书开放平台配置（3 分钟）

在浏览器登录 [飞书开放平台](https://open.feishu.cn/app)（企业自建应用）：

1. **创建应用**：点击「创建企业自建应用」，获取 `App ID` 与 `App Secret`。
2. **配置重定向 URL**：
   - 进入「安全设置」->「重定向 URL」
   - 添加：`http://localhost:8088/callback`
   *(若部署在服务器/局域网，请按实际 IP 或域名替换 `localhost`，端口为 8088)*
3. **开通权限**：
   - 进入「权限管理」，开通以下权限（可直接在搜索框搜索对应英文权限名）：
     - `im:chat:readonly` (获取群组信息)
     - `im:message:readonly` (读取消息)
     - `im:message.group_msg:get_as_user` (以用户身份获取群消息)
     - `contact:contact.base:readonly` (获取联系人基础信息)
     - `contact:user.base:readonly` (获取用户基础信息)
     - `im:message` (发送消息)
     - `im:message.send_as_user` (以用户身份发送消息)
     - `im:resource` (获取富媒体资源)
     - `drive:file:upload` (上传云空间文件)
     - `drive:file:download` (下载云空间文件)
     - `drive:drive.metadata:readonly` (获取云空间文件元数据)
     - `docs:permission.member:create` (设置云文档协作者/分享权限)
     - `docs:permission.member:retrieve` (验证协作者)
     - `docs:permission.setting:read` (验证文件及父目录分享设置)
     - `space:document:retrieve` (读取文件夹清单)
     - `wiki:node:read` (解析 Wiki 实际资源)
     - `docs:document:export` (按需导出在线文档、表格与幻灯片)
     - `docs:permission.setting:write_only` (设置云文档链接分享关闭/未开启)
     - `space:folder:create` (创建云空间文件夹)
4. **发布应用**：
   - 进入「版本管理与发布」->「创建版本」-> 提交审批并发布通过。

---

## 2. 方式一：Docker 部署（推荐，无需装 Go）

### 2.1 准备配置
解压项目包后，在项目根目录下执行：
```bash
# 复制配置文件模板
cp config.example.json config.json
```
编辑 `config.json`，只需替换填入你的 `app_id` 和 `app_secret`：
```json
{
  "app_id": "cli_xxxxxxxxxxxxxxxx",
  "app_secret": "你的飞书AppSecret",
  "base_url": "https://open.feishu.cn",
  "redirect_uri": "http://localhost:8088/callback",
  "addr": "0.0.0.0:8088",
  "data_dir": "data",
  "upload_mode": "drive",
  "drive_share_mode": "bot_only"
}
```

> v1.9.8 升级前先退出程序并备份个人配置、数据及下载目录。发布新权限后，在程序中重新登录授权。运行包不携带真实凭据；Windows 配置中的 `data_dir` 使用 `data`，不要沿用容器内的 `/app/data`。

> **安全与权限规范**：`drive_share_mode` 必须为 `"bot_only"`（默认）。上传至飞书云空间的附件会关闭链接公开访问。发送时默认勾选「加密附件」，只授权红色呆呆鸟阅读。取消勾选后，才额外授予**当前群**阅读权限，不开放给其他群或全租户。程序会在发送前读取结果验证，任何权限步骤失败均不发送链接；发送采用独立文本块保存完整链接，避免飞书自动改写协作者，成功后再复查权限。网页文件栏仍可直接打开或下载；仅新上传附件使用 `网页呆呆鸟`，历史附件不作权限修改。


### 2.2 启动容器
```bash
# 构建并后台启动
docker compose up -d --build

# 查看运行状态与日志
docker compose logs -f
```

### 2.3 访问使用
- 打开浏览器访问：`http://localhost:8088`
- 点击「登录飞书账号」完成 OAuth 授权，即可开始使用。
- 点击「下载」后的文件保存至宿主机挂载目录：`~/Documents/FeishuTopics`（Windows 默认为相应文稿目录）。

### 2.4 常用维护命令
```bash
# 重启服务
docker compose restart

# 更新代码后重新构建启动
docker compose up -d --build

# 停止并移除容器
docker compose down
```

---

## 3. 方式二：本地 Go 原生运行（适合开发/调试）

### 3.1 前置要求
- 安装 Go 1.22 及以上版本（`go version` 检查）。

### 3.2 运行
```bash
# 准备配置
cp config.example.json config.json
# 修改 config.json 中的 app_id 与 app_secret

# 直接启动（前端资源已通过 go:embed 打包在程序中）
go run .

# 或编译后运行
go build -o feishu-topics .
./feishu-topics
```
访问：`http://localhost:8088`。

---

## 4. 常见排查速查

| 现象 / 报错 | 根本原因 | 解决办法 |
| :--- | :--- | :--- |
| **redirect_uri mismatch** | 飞书开放平台填写的重定向 URL 与当前访问不一致 | 检查开放平台安全设置中的重定向 URL，必须精确匹配 `http://<实际访问域名或IP>:8088/callback` |
| **99991663 / Permission denied** | 缺少飞书 API 权限 | 检查应用「权限管理」是否已全部开通第 1 节列出的权限并**发布了新版本** |
| **token.json invalid / 登录失效** | 本地授权过期或被吊销 | 直接在界面点击重新授权登录，或删除 `data/token.json` 后重启应用重登 |
| **8088 端口被占用** | 其他服务占用了 8088 端口 | 修改 `docker-compose.yml` 中的端口映射（如 `"127.0.0.1:8089:8088"`），并同步修改 `config.json` 及飞书后台的 `redirect_uri` |
| **Docker 启动提示 config.json 是目录** | 执行 compose 前未先创建 `config.json` 文件 | 执行 `docker compose down`，然后 `rm -rf config.json && cp config.example.json config.json`，再重新启动 |
