# 品牌图标

分流页域名组卡片的厂商标识，文件名即 `web/service/routing_brand.go` 里的 slug
（`TestBrandIconsExist` 守着两边一致）。只收识别表用得到的那些，全彩原样显示在白底方块上。

| 来源 | 版本 | 许可 | 文件 |
|---|---|---|---|
| [gilbarbara/logos](https://github.com/gilbarbara/logos) | main 分支，2026-10-31 取 | CC0-1.0 | 除下两行以外的全部（取 `*-icon.svg` 图形版，没有时取同名文件） |
| [Lobe Icons](https://github.com/lobehub/lobe-icons)（npm `@lobehub/icons-static-svg`） | 1.101.0 | MIT（npm registry 的 license 字段；tarball 内不含 LICENSE 文件） | copilot、kimi、doubao、poe、bilibili（`*-color.svg`） |
| [Simple Icons](https://github.com/simple-icons/simple-icons)（npm `simple-icons`） | 16.36.0 | CC0-1.0 | speedtest、wikipedia、playstation、hbomax、crunchyroll |

前两个来源是按官方 logo 原样绘制的全彩版，文件未改动。Simple Icons 只有单色剪影，
这五个是前两个来源都没有全彩版的，唯一的改动是在 `<svg>` 上加了官方品牌色的 `fill`。

图标许可只覆盖图形文件本身；图中的标识是各厂商的商标，这里仅用于在管理界面里
标明域名组对应的服务。
