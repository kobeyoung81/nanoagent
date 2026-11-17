# nanoagent

真正跑完一轮对话的东西。每人一个容器：模型循环、工具，以及智能体生活的那个工作区。

它是**执行体，不是一级服务** —— 不持有数据库，由供给服务按需创建。

## 它是什么

- **ReAct 循环。** 提示词、模型、工具调用、观察、回答 —— 带供应商故障转移，上下文预算由它自己管。
- **真的工具。** shell、文件读写、glob、grep、git 面板、网页抓取与搜索、定时任务、后台子智能体、MCP 服务。
- **技能与提示词。** 平台目录里播种的内建技能，加上这个人自己写的。
- **能留下来的工作区。** 文件、记忆、对话正文都在容器卷里；对话正文是权威源。
- **一个 hypervisor。** 平台拨进来取文件、开终端、走 git 的那个小 HTTP 桥。

## 它在哪

| | |
|---|---|
| 集合 | 无 —— 智能体不持有任何数据库连接 |
| 存储 | 自己的卷：工作区文件、`memory/`、每个会话一个 JSONL 正文 |
| 镜像 | `docker.io/kobeyoung81/nanoagent:<version>`；里面的二进制叫 `nanosec` |
| 与谁对话 | `nanobus`（消息）、`nanoportal`（自己那一行与设置）、`nanoadmin`（配置与目录） |
| 由谁拉起 | `nanoprovisioner`，按用户一容器，带该档位的限额 |

## 快速开始

部署跑的是镜像；两个二进制说明它的形状。

```bash
# 容器内：平台隧道拨进来的那个桥
nanosec hypervisor --user <id> --hypervisor-addr :9090

# 这个用户的对话循环
nanosec agent --user <id> --agent <agent-id> --bus-url http://nanobus:9000
```

`nanosec home reconcile` 按模板（`AGENT.md`、`bootstrap/`、`memory/`）重建工作区骨架，
但不动智能体自己写过的东西。

## 配置

| 变量 | 含义 |
|---|---|
| `NANOSEC_AGENT_BUS_URL` | 总线，收发消息与事件流 |
| `NANOSEC_AGENT_TUNNEL_URL` | hypervisor 把隧道注册到哪儿（优先 provisioner，否则总线） |
| `NANOSEC_PORTAL_URL` | 这个智能体的行与设置 |
| `NANOSEC_ADMIN_URL` | 配置与目录 |
| `NANOSEC_BUS_INTERNAL_TOKEN` | 所有内部面共用的密钥 |

密钥、模型、工具体、配额都是配置而不是环境变量：它们来自管理模块，在控制台编辑。

## 设计决策

- **容器就是边界。** 用户的文件、记忆、对话正文不出容器，除非经过平台自己持有的隧道。
- **对话正文是权威。** 控制台保留一份投影给界面；投影丢了，从卷里的 JSONL 重建。
- **工具先声明，再授权。** 目录说明「有什么」，工具体说明「这个角色能用什么」——
  不在工具体里的工具，压根不会出现在模型面前。
- **压缩是循环的一部分。** 长对话被总结成新的一段，而不是被截断，所以模型还记得来龙去脉。

## 文档

- [`docs/ARCHITECTURE.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/ARCHITECTURE.md) —— 平台与智能体的位置
- [`docs/BUNDLED_SKILLS_DESIGN.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/BUNDLED_SKILLS_DESIGN.md) —— 目录是怎么播种的
- [`docs/BUNDLED_PROMPTS_DESIGN.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/BUNDLED_PROMPTS_DESIGN.md) —— 提示词同理
- [`docs/README.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/README.md) —— 全部文档索引

## 状态

随 `v0.1.0` 发布；镜像 digest 钉在超仓的 `versions.lock` 里。

## 贡献与安全

开发与提交规则见
[`CONTRIBUTING.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/CONTRIBUTING.md)；
漏洞请按
[`SECURITY.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/SECURITY.md) 私下报告，
不要开公开 issue。

## 许可证

[AGPL-3.0](./LICENSE)。名称与标志另有说明 —— 见 [`NOTICE`](./NOTICE)。
