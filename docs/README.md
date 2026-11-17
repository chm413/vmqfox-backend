# 部署文档（docs）

本目录收录 Go-only 版（V3.0 起，当前 V3.4）的部署、升级与运营文档，以及设计方案，是 [项目 Wiki](https://github.com/hulisang/vmqfox-backend/wiki) 部署相关页面的离线副本，方便随源码仓库直接查阅；**部署类内容以 Wiki 为准，设计类文档以本目录为准**。

| 文档 | 适用场景 |
| :--- | :--- |
| [Docker 一键部署](docker一键部署.md) | 使用 docker-compose 快速上线（推荐） |
| [裸机二进制部署](裸机二进制部署.md) | 直接下载 Release 产物 + systemd 托管 |
| [反向代理与 HTTPS](反向代理与HTTPS.md) | nginx 反代、脱敏与转发头配置 |
| [升级与迁移](升级与迁移.md) | 旧库升级、`-migrate` 步骤、回滚边界 |
| [EPay 商户接入与运营](EPay商户接入与运营.md) | **运营/商户视角**：开商户、一次性密钥、签名与回调、轮换、排障 |
| [EPay 兼容与多商户设计](EPay兼容与多商户设计.md) | 设计方案与实现状态（V3.4 已实施；V2 未开放） |

其余对接类文档（API 契约、签名协议、epay 插件、监控端 App、安全清单）请直接访问 [Wiki](https://github.com/hulisang/vmqfox-backend/wiki)。
