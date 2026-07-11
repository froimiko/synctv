<div align="center">
  <a href="https://github.com/synctv-org/docs"><img height="100px" alt="logo" src="https://cdn.jsdelivr.net/gh/synctv-org/docs@main/logo/logo.png"/></a>
  <p><em>👫一个可以远程一起看电影/直播的程序🍿</em></p>
    <div>
        <a href="https://goreportcard.com/report/github.com/synctv-org/synctv">
            <img src="https://goreportcard.com/badge/github.com/synctv-org/synctv" alt="latest version" />
        </a>
        <a href="https://github.com/synctv-org/synctv/blob/main/LICENSE">
            <img src="https://img.shields.io/github/license/synctv-org/synctv" alt="License" />
        </a>
        <a href="https://github.com/synctv-org/synctv/actions?query=workflow%3Arelease">
            <img src="https://img.shields.io/github/actions/workflow/status/synctv-org/synctv/release.yml?branch=main" alt="Release status" />
        </a>
        <a href="https://github.com/synctv-org/synctv/releases">
            <img src="https://img.shields.io/github/release/synctv-org/synctv" alt="latest version" />
        </a>
    </div>
    <div>
        <a href="https://github.com/synctv-org/synctv/releases">
            <img src="https://img.shields.io/github/downloads/synctv-org/synctv/total?color=%239F7AEA&logo=github" alt="Downloads" />
        </a>
        <a href="https://hub.docker.com/r/synctvorg/synctv">
            <img src="https://img.shields.io/docker/pulls/synctvorg/synctv?color=%2348BB78&logo=docker&label=pulls" alt="Downloads" />
        </a>
    </div>
</div>

---

[English](./README.md) | 中文

# 什么是SyncTV?

SyncTV 是一个允许您远程一起观看电影和直播的程序。它提供了同步观影、直播、聊天等功能。使用 SyncTV，您可以与朋友和家人一起观看视频和直播，无论他们在哪里。

SyncTV 的同步观影功能确保所有观看视频的人都在同一点上。这意味着您可以暂停、快进快退、倍速等操作，其他人也会同步到同一点。

# 特点

- [x] 同步观看
  - [x] 视频同步
  - [x] 直播同步
- [x] 影院模式
  - [x] 聊天
  - [x] 弹幕
- [x] 代理
  - [x] 视频代理
  - [x] 直播代理
  - [x] 代理缓存
- [x] 视频解析
  - [x] Alist
  - [x] Bilibili
  - [x] Emby
- [x] 直播解析
  - [x] 哔哩哔哩
- [x] WebRTC 在线通话
  - [x] 语音
  - [ ] 视频
  - [ ] 屏幕共享

---

# 演示站点

[https://demo.synctv.wiki](https://demo.synctv.wiki)

---

# 安装方式

## 二进制

您可以从[发布页面](https://github.com/synctv-org/synctv/releases)下载最新的二进制文件并手动安装。

## 一键脚本

您可以使用该脚本来安装和运行 SyncTV。

```bash
sudo -v ; curl -fsSL https://raw.githubusercontent.com/synctv-org/synctv/main/script/install.sh | sudo bash -s -- -v latest
```

## Docker

您也可以使用 docker 安装并运行 SyncTV。

```bash
docker run -d --name synctv -v /opt/synctv:/root/.synctv -p 8080:8080 synctvorg/synctv
```

## Docker compose

### 运行官方发布镜像

现有的 [`script/docker-compose.yml`](./script/docker-compose.yml) 配置用于运行官方已发布的 SyncTV 镜像。在仓库根目录直接复制并执行：

```bash
docker compose -f script/docker-compose.yml up -d
```

### 从当前本地源码构建运行

此方式会构建并运行当前本地工作区中的代码，包括尚未发布到官方版本的修改。

宿主机只需安装 Docker Desktop，或 Docker Engine + Docker Compose v2；不需要安装 Go 或 Node.js。

首次运行时，在仓库根目录按顺序复制并执行：

```bash
git submodule update --init --recursive
docker compose -f docker-compose.local.yml up -d --build
```

容器启动后，访问 [http://localhost:8080](http://localhost:8080)。首次初始化的用户名和密码都是 `root`，登录后请立即修改密码。

程序数据保存在 Docker 命名卷 `synctv-local-data` 中，普通重建或替换容器不会丢失数据。

如果 `8080` 端口已被现有容器占用，请先停止旧容器。也可以修改 `docker-compose.local.yml` 中端口映射左侧的数字，例如把 `8080:8080` 改为 `18080:8080`，然后访问 `http://localhost:18080`。

常用命令：

- 持续查看容器日志：

  ```bash
  docker compose -f docker-compose.local.yml logs -f
  ```

- 更新源码后重新构建并启动：

  ```bash
  docker compose -f docker-compose.local.yml up -d --build
  ```

- 停止并删除容器，但保留全部程序数据：

  ```bash
  docker compose -f docker-compose.local.yml down
  ```

- 完全不使用 Docker 构建缓存重新构建，然后启动容器：

  ```bash
  docker compose -f docker-compose.local.yml build --no-cache
  docker compose -f docker-compose.local.yml up -d
  ```

- 删除容器和本地数据：

  > **警告：** 以下命令会永久删除 `synctv-local-data` 数据卷，其中的账号、配置和数据库数据都会被删除。

  ```bash
  docker compose -f docker-compose.local.yml down -v
  ```

## Helm

### Helm Install

```bash
helm repo add synctv https://docs.synctv.wiki/helm-charts
helm repo update synctv
helm upgrade --install synctv synctv/synctv \
  -n synctv --create-namespace \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set 'ingress.hosts[0].host=<yourdomain.com>' \
  --set 'ingress.hosts[0].secretName=<yourdomain-secretName>'
```

### Helm Upgrade

```bash
helm repo update synctv
helm upgrade --install synctv synctv/synctv \
  -n synctv \
  --reuse-values
```

> More Helm Values:[helm-values](helm-values.md)

### Helm Uninstall

```bash
helm uninstall -n synctv synctv
```

---

# 运行

`synctv server` 启动服务器

```bash
synctv server
# or
synctv server --data-dir ./
```

> 每次启动会检测是否有root权限的用户，如果没有则会初始化一个`root`用户，密码为`root`，请及时修改用户名密码。
>
> 用户注册功能需要启用任意 `OAuth2` 服务，如 `Google`、`Github` 等等，具体配置请参考[文档](https://docs.synctv.wiki/#/zh-cn/oauth2)。

# 文档

[https://docs.synctv.wiki](https://docs.synctv.wiki)

# 特别赞助商

- [亚洲云](https://www.asiayun.com) 为[演示站](https://demo.synctv.wiki)点提供服务器支持。
- [SwarmCloud](https://www.cdnbye.com/cn/) ¥ 200 并提供视频P2P加速。
  - 当服务器网络带宽不足时，可以考虑使用P2P视频加速技术。
  - 如果你想免费使用P2P视频加速技术，可以查看文档[P2P视频加速](https://docs.synctv.wiki/#/zh-cn/p2p)。
- [LucasYuYu](https://github.com/LucasYuYu) ¥ 18.88
- [爱发电用户_5vDc](https://afdian.com/u/48fa38ce0e0211ef944d5254001e7c00) ¥ 228
- masha
- [T-rabbit](https://github.com/T-rabbit) ¥ 5
- 矿神SPK源 ¥ 100

# 贡献者

感谢这些出色的人们：

[![贡献者](https://contrib.nn.ci/api?repo=synctv-org/synctv&repo=synctv-org/synctv-web&repo=synctv-org/docs)](https://github.com/synctv-org/synctv/graphs/contributors)

# 免责声明

- 这个程序是一个免费且开源的项目。它旨在播放网络上的视频文件，方便多人共同观看视频和学习golang。
- 在使用时，请遵守相关法律法规，不要滥用。
- 该程序仅进行客户端播放视频文件/流量转发，不会拦截、存储或篡改任何用户数据。
- 在使用该程序之前，您应该了解并承担相应的风险，包括但不限于版权纠纷、法律限制等，这与该程序无关。
- 如果有任何侵权行为，请通过[电子邮件](mailto:pyh1670605849@gmail.com)与我联系，将及时处理。

# 讨论

- [Telegram](https://t.me/synctv)
