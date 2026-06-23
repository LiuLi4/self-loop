---
name: self-loop
description: >
  飞书（Feishu / Lark）需求驱动的自治开发 loop（Loop Engineering）。当用户发来一个飞书文档（docx 链接或
  document token，文档里记录了需求 / 业务流程 / 涉及接口）并要求据此开发时触发：拉取需求 → 冻结每个需求的
  验收标准(DoD) → 按需求 fan-out 到独立 git worktree 并行实现 → 独立 checker 对照 DoD 逐条校验 →
  发现的 issue 幂等回写飞书 Bitable 看板并标状态 → loop-until-dry，直到所有验收标准达标且无 open issue。
  触发于：用户粘贴飞书文档链接说"按这个开发 / 跑 self-loop / 开始 loop / 按这个飞书文档把需求做了"。
  自动做到绿灯+commit+push+PR+issue 全关，但不部署生产、不合并默认分支、不碰 secret。
---

# self-loop — 飞书需求驱动的自治开发 loop

收到飞书文档即开跑。你的职责是**做好前置校验、在缺配置时引导用户建立本地环境变量，然后把控制权交给确定性的 Workflow 脚本**，最后汇报收敛结果。不要自己手搓 loop——编排逻辑在 `self-loop.workflow.js` 里。

## 0. 解析输入

用户会给一个飞书文档，形式可能是：
- 完整链接 `https://<tenant>.feishu.cn/docx/<TOKEN>` → `document_id` = `<TOKEN>`（`docx/` 后那段）。
- 直接给 `document_id`。

提取出 `docId`。拿不准就用文本编号列表问用户确认，别猜。

## 1. 配置：全部走本地环境变量（缺啥引导建啥）

本 skill 不用配置文件，所有配置都是**本地环境变量**。Bash 工具从用户 shell profile 初始化，**临时 export 不会被后续工具调用继承**，所以必须写进 `~/.zshrc`（或等价 profile）并新开 shell / `source`。

| 变量 | 含义 | 是否密钥 |
|---|---|---|
| `FEISHU_APP_ID` | 飞书自建应用 app_id | 凭据，绝不打印/落盘 |
| `FEISHU_APP_SECRET` | 飞书自建应用 app_secret | 凭据，绝不打印/落盘 |
| `FEISHU_BITABLE_APP` | issue 看板的 Bitable app_token | 实例 id |
| `FEISHU_BITABLE_TABLE` | issue 看板的 table_id | 实例 id |
| `FEISHU_BASE_URL` | 可选，默认 `https://open.feishu.cn`（国际版填 larksuite 域名） | 否 |
| `SELF_LOOP_MAX_ROUNDS` | 可选，收敛轮数上限，默认 6 | 否 |
| `SELF_LOOP_SCOPE_RULE` | 可选，范围约束（自然语言）。设了才做边界守卫，越界需求只标 spec-question 不实现 | 否 |
| `SELF_LOOP_BRIDGE_CMD` | 可选，loop-bridge 调用方式，默认 `loop-bridge`（已装到 PATH）；也可设 `go run /path/to/loop-bridge` | 否 |

**引导流程**：开跑前逐个检查必填变量（`FEISHU_APP_ID`/`FEISHU_APP_SECRET`/`FEISHU_BITABLE_APP`/`FEISHU_BITABLE_TABLE`）。**只要有任一缺失，先停下来引导用户建立，不要带病启动**：

1. 用 `printenv` 探测哪些已存在（只判断是否为空，**不要回显凭据值**）：
   ```bash
   for v in FEISHU_APP_ID FEISHU_APP_SECRET FEISHU_BITABLE_APP FEISHU_BITABLE_TABLE; do
     if [ -n "$(printenv $v)" ]; then echo "$v: 已设置"; else echo "$v: 缺失"; fi
   done
   ```
2. 对缺失的变量，给用户一段可直接粘进 `~/.zshrc` 的模板，并说明取值来源：
   ```bash
   # —— self-loop 所需本地变量，粘进 ~/.zshrc 后执行 `source ~/.zshrc` 或新开终端 ——
   export FEISHU_APP_ID=cli_xxxxxxxx          # 飞书开放平台 > 你的自建应用 > 凭证与基础信息
   export FEISHU_APP_SECRET=xxxxxxxxxxxx      # 同页 App Secret（密钥，别外传）
   export FEISHU_BITABLE_APP=bascnXXXXXXXX    # 看板多维表格 URL 里的 app_token：/base/<app_token>?table=<table_id>
   export FEISHU_BITABLE_TABLE=tblXXXXXXXX    # 同 URL 里的 table_id
   ```
   并提醒：用户也可以让你帮忙把这些行追加到 `~/.zshrc`——但**密钥值要由用户自己填**，你不要把明文密钥写进任何文件或打印出来。
3. 引导完成后让用户确认"已 source / 已新开终端"，再重新探测；仍缺则继续停在本步。

## 2. preflight（变量齐全后，按顺序，任一不过就停下报告）

1. **工作目录**：必须在一个 git 仓内（`git rev-parse --show-toplevel`）。loop 只在当前仓内并行实现。
2. **loop-bridge 可用 + 飞书连通**：跑只读探活（`$SELF_LOOP_BRIDGE_CMD` 默认 `loop-bridge`）：
   ```bash
   ${SELF_LOOP_BRIDGE_CMD:-loop-bridge} issues-list --app "$FEISHU_BITABLE_APP" --table "$FEISHU_BITABLE_TABLE"
   ```
   返回 JSON（哪怕空数组）即通；报错则按错误信息修（loop-bridge 未装/权限/字段/网络），不要进入 loop。
   > 未安装 loop-bridge：`go install github.com/LiuLi4/self-loop/loop-bridge@latest`，或克隆本仓后 `go build -o ~/bin/loop-bridge ./loop-bridge`。
3. **看板 schema**：Bitable 必须含字段 `external_key`(单行文本,唯一键)、`requirement`、`title`、`type`、`status`、`severity`、`acceptance_ref`、`evidence`、`updated_round`(数字)。缺字段先引导用户补（一次性）。

## 3. 启动 loop（交给 Workflow）

preflight 全过后，读环境变量并用 **Workflow 工具**运行编排脚本（这会 fan-out 多个 agent + worktree，是用户已明确要求的）：

```
Workflow({
  scriptPath: "<本skill目录>/self-loop.workflow.js",
  args: {
    docId: "<TOKEN>",
    app:   "<$FEISHU_BITABLE_APP>",
    table: "<$FEISHU_BITABLE_TABLE>",
    maxRounds: <$SELF_LOOP_MAX_ROUNDS 或 6>,
    scopeRule: "<$SELF_LOOP_SCOPE_RULE，可空>",
    bridgeCmd: "<$SELF_LOOP_BRIDGE_CMD 或 loop-bridge>"
  }
})
```

> 注意：app/table 等值由你从环境变量读出后填进 args（Workflow 脚本本身不读 env）。凭据 `FEISHU_APP_ID/SECRET` 不进 args——它们由 loop-bridge 在子进程内自行从 env 读取。

脚本会自己跑完 Intake → loop(Build→Verify→Sync) → 收敛，期间后台运行，完成时回通知。

## 4. 汇报

脚本返回 `{ rounds, converged, inScope, openIssues }`。据此汇报：收敛与否 / 跑了几轮 / 纳入几个 in-scope 需求；看板剩余 open issue（`converged=false` 表示到 `maxRounds` 仍未全绿，附剩余 issue 交人工）；越界(spec-question)需求清单——**未**被实现，需人工决定是否立新 spec。

## 护栏（脚本已内置，汇报时也要守住，不得放宽）

- **范围守卫**：设了 `SELF_LOOP_SCOPE_RULE` 时，越界需求只标 `spec-question`，绝不自动实现或新建范围外 spec。
- **终态边界**：自动做到 绿灯+commit+push+PR开好+issue全关；**不自动**合并默认分支、不部署生产、不读写 secret。
- **DoD 冻结**：验收标准在 Intake 冻结，run 内只读，agent 不得为过关放宽。
- **maker≠checker**：实现 agent 与校验 agent 分离，checker 重跑命令档、反驳语义档，不采信自评。
- **单写者**：只有编排脚本的 sync 阶段写飞书看板，按 `external_key` 幂等 upsert。
- **收敛上限**：`maxRounds` 到顶仍未全绿则停下交人工，不无限烧 token。
- **脏改动守卫**：worktree 内开工先 `git status`，不覆盖非本任务改动。
- **凭据纪律**：环境变量里的 app_secret 绝不打印、不写文件、不传飞书以外服务。

## 相关文件

- `loop-bridge/` — 飞书 Open API 桥接 CLI（doc-dump / issues-list / issue-upsert，Go 标准库，无第三方依赖，凭据只读 env）。
- `self-loop.workflow.js` — 编排脚本（pipeline 并行 + worktree 隔离 + loop-until-dry）。
