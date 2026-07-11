<div align="center">
  <a href="https://github.com/synctv-org/docs"><img height="100px" alt="logo" src="https://cdn.jsdelivr.net/gh/synctv-org/docs@main/logo/logo.png"/></a>
  <p><em>👫A program that allows you to watch movies/live broadcasts together remotely🍿</em></p>
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

English | [中文](./README-CN.md)

# What is SyncTV?

SyncTV is a program that allows you to watch movies and live broadcasts together remotely. It provides features such as synchronized viewing, live streaming, and chat. With SyncTV, you can watch videos and live broadcasts with friends and family, no matter where they are.

SyncTV's synchronized viewing feature ensures that everyone watching the video is at the same point. This means that you can pause, fast forward, rewind, change playback speed, and other operations, and everyone else will be synchronized to the same point.

# Features

- [x] Synchronized viewing
  - [x] Videos Sync
  - [x] Live streaming
- [x] Theater
  - [x] Chat
  - [x] Bullet chat
- [x] Proxy
  - [x] Videos proxy
  - [x] Live proxy
  - [x] Proxy cache
- [x] Parse video
  - [x] Alist
  - [x] Bilibili
  - [x] Emby
- [x] Parse live
  - [x] Bilibili
- [x] WebRTC online call
  - [x] Audio
  - [ ] Video
  - [ ] Screen

---

# Demo

[https://demo.synctv.wiki](https://demo.synctv.wiki)

---

# Installation

## Binary

You can download the latest binary from [release page](https://github.com/synctv-org/synctv/releases) and install it manually.

## Script

You can use the script to install and run SyncTV.

```bash
sudo -v ; curl -fsSL https://raw.githubusercontent.com/synctv-org/synctv/main/script/install.sh | sudo bash -s -- -v latest
```

## Docker

You can also use docker to install and run SyncTV.

```bash
docker run -d --name synctv -v /opt/synctv:/root/.synctv -p 8080:8080 synctvorg/synctv
```

## Docker compose

### Run the official release image

The existing [`script/docker-compose.yml`](./script/docker-compose.yml) configuration runs the official published SyncTV image. Copy and run this command from the repository root:

```bash
docker compose -f script/docker-compose.yml up -d
```

### Build and run from local source

Use this option when you want to build and run the code in your current local workspace, including changes that have not been published in an official release yet.

The host only needs Docker Desktop, or Docker Engine with Docker Compose v2. You do not need to install Go or Node.js.

For the first run, copy and run these commands in order from the repository root:

```bash
git submodule update --init --recursive
docker compose -f docker-compose.local.yml up -d --build
```

When the container is ready, open [http://localhost:8080](http://localhost:8080). On first initialization, sign in with username `root` and password `root`, then change the password immediately.

Application data is stored in the Docker named volume `synctv-local-data`. Normal rebuilds and container replacement do not delete this data.

If port `8080` is already used by an existing container, stop that container first. Alternatively, change the number on the left side of the port mapping in `docker-compose.local.yml`, for example from `8080:8080` to `18080:8080`, and then open `http://localhost:18080` instead.

Common commands:

- Follow the container logs:

  ```bash
  docker compose -f docker-compose.local.yml logs -f
  ```

- Rebuild and restart after updating the source code:

  ```bash
  docker compose -f docker-compose.local.yml up -d --build
  ```

- Stop and remove the container while keeping all application data:

  ```bash
  docker compose -f docker-compose.local.yml down
  ```

- Rebuild completely without using the Docker build cache, then start the container:

  ```bash
  docker compose -f docker-compose.local.yml build --no-cache
  docker compose -f docker-compose.local.yml up -d
  ```

- Delete the container and its local data:

  > **Warning:** The following command permanently deletes the `synctv-local-data` volume, including all accounts, settings, and database data.

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

# Run

`synctv server` to start the server

```bash
synctv server
# or
synctv server --data-dir ./
```

> Every time it starts, it will check for users with root permissions. If none are found, it will initialize a `root` user with the password `root`. Please change the username and password promptly.
>
> The user registration function requires the use of any `OAuth2` service, such as `Google`, `Github`, etc. For specific configuration, please refer to [documentation](https://docs.synctv.wiki/#/oauth2).

# Documentation

[https://docs.synctv.wiki](https://docs.synctv.wiki)

# Special sponsors

- [亚洲云](https://www.asiayun.com) supports the server for the [demo](https://demo.synctv.wiki) site.
- [SwarmCloud](https://swarmcloud.net/) ¥ 200 and provides video P2P acceleration.
  - When the server network bandwidth is insufficient, you can consider using P2P video acceleration technology.
  - If you want to use P2P video acceleration technology for free, you can view the documentation [P2P video acceleration](https://docs.synctv.wiki/#/p2p).
- [LucasYuYu](https://github.com/LucasYuYu) ¥ 18.88
- [爱发电用户_5vDc](https://afdian.com/u/48fa38ce0e0211ef944d5254001e7c00) ¥ 228
- masha
- [T-rabbit](https://github.com/T-rabbit) ¥ 5
- 矿神SPK源 ¥ 100

# Contributors

Thanks goes to these wonderful people:

[![Contributors](https://contrib.nn.ci/api?repo=synctv-org/synctv&repo=synctv-org/synctv-web&repo=synctv-org/docs)](https://github.com/synctv-org/synctv/graphs/contributors)

# License

The `SyncTV` is open-source software licensed under the AGPL-3.0 license.

# Disclaimer

- This program is a free and open-source project. It aims to play video files on the internet, making it convenient for multiple people to watch videos and learn golang together.
- Please comply with relevant laws and regulations when using it, and do not abuse it.
- The program only plays video files/forwards traffic on the client-side and will not intercept, store, or tamper with any user data.
- Before using the program, you should understand and assume the corresponding risks, including but not limited to copyright disputes, legal restrictions, etc., which are not related to the program.
- If there is any infringement, please contact me via [email](mailto:pyh1670605849@gmail.com), and it will be dealt with promptly.

# Discussion

- [Telegram](https://t.me/synctv)
