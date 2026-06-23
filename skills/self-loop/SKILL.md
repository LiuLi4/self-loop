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

## 0. 解析输入（支持 docx 文档 + wiki 知识库两种链接）

用户给的飞书在线文档可能是这几种形式，先识别 `kind` 和 `token`：
- **docx 文档**：`https://<tenant>.feishu.cn/docx/<TOKEN>` → `kind=docx`，`token=<TOKEN>`（`docx/` 后、`?` 前那段）。
- **wiki 知识库**：`https://<tenant>.feishu.cn/wiki/<TOKEN>` → `kind=wiki`，`token=<TOKEN>`（`wiki/` 后、`?` 前那段）。wiki 节点 token 不是 document_id，**需在 preflight 用 `resolve-wiki` 换成底层 docx id**。
- 直接给裸 token：问用户是 docx 还是 wiki，别猜。

docx 的 `token` 即 `docId`；wiki 的 `token` 记为 `wikiNode`，待 preflight 解析出 `docId`。拿不准就用文本编号列表问用户确认。

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
| `SELF_LOOP_RULES_PATH` | 可选，规则记忆文件路径，默认仓根 `self-loop.rules.md`（见下方 Rules & Memory） | 否 |

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
3. **看板 schema（可自动建字段）**：看板需含 9 个字段 `external_key`(唯一键)、`requirement`、`title`、`type`、`status`、`severity`、`acceptance_ref`、`evidence`、`updated_round`(数字)。
   用户只需手动**新建一个空多维表格**并共享给应用，给出 app_token/table_id；字段由你用 `ensure-board` 幂等建好（需 `bitable:app` 写权限）：
   ```bash
   ${SELF_LOOP_BRIDGE_CMD:-loop-bridge} ensure-board --app "$FEISHU_BITABLE_APP" --table "$FEISHU_BITABLE_TABLE"
   ```
   输出 `{created:[...], fields_total:9}` 即建好（已存在字段自动跳过，可重复跑）。
4. **wiki 解析 + 文档类型判定**（仅当 §0 识别为 `kind=wiki`）：
   ```bash
   ${SELF_LOOP_BRIDGE_CMD:-loop-bridge} resolve-wiki --node "<wikiNode>"
   ```
   输出 `{obj_token, obj_type}`，据 `obj_type` 定 `docId=obj_token` 和 `docKind`：
   - `obj_type=docx` → `docKind=docx`（doc-dump 读）；
   - `obj_type=sheet` → `docKind=sheet`（sheet-dump 读，需应用有 `sheets:spreadsheet:readonly` 权限）；
   - 其它(`bitable`/`mindnote` 等) → 停下告知当前只支持 docx 与 sheet。
   - 直接 docx 链接(§0 kind=docx) → `docKind=docx`。
   > 权限报错时提示补对应 scope 并**发布版本**：wiki 读 `wiki:wiki:readonly`、表格读 `sheets:spreadsheet:readonly`；并确认应用对该文档有访问权。
4. **Rules 文件**：检查仓根 `self-loop.rules.md`（或 `$SELF_LOOP_RULES_PATH`）。不存在则引导用户从 `self-loop.rules.example.md` 复制一份并填上项目约定——所有 maker/checker 都会读它。
5. **续跑探测**：检查 `.self-loop/run/<docId 前12位>/` 是否已有 `meta.json` + `dod/`。有则提示用户"检测到既有 run，将断点续跑（不重新冻结 DoD）"；这是正常的，确认后继续。

## 2.5 Rules & Memory（规则记忆 + 状态外置 + 断点续跑）

self-loop 的"记忆"分两层，都外置在文件 / 飞书，不依赖运行中进程的内存：

- **Rules（策略记忆）** = `self-loop.rules.md`：硬约束 + 项目约定 + `## Learned`（loop 每轮把系统性教训自动追加进去）。每个 maker/checker 开工前必读。跨 run 持续生效、越用越聪明。
- **State（执行记忆，可断点续跑）**：
  - `.self-loop/run/<id>/meta.json` 存需求集、`dod/*.json` 存冻结验收契约、`progress.json` 存轮次——首次 intake 写入；
  - **飞书 Bitable 看板**是 issue 状态的持久真相源；
  - 重跑同一文档时，workflow 的 boot 阶段会读回 meta+progress+看板，**从断点轮次继续，不重新解析文档、不重新冻结 DoD**。
  - 会话内崩溃/中断还可叠加 Workflow 工具自带的 `resumeFromRunId`（重放已完成 agent 的缓存）——两层 resume 互补：native 管会话内、文件+看板管跨会话。

## 2.6 issue 问答闭环（需用户回答的 issue：先问，再建子记录存答案）

Intake 把 issue 写进看板后，对其中**需要用户回答/澄清**的（`type=spec-question`，或 `blocker`/`gap` 里 evidence 标注"需外部输入"的），在启动 build 前用问答逐个解决——不要带着这些悬而未决就开跑：

1. `issues-list` 拉当前看板，筛出 open 且需用户回答的 issue。
2. 用 **AskUserQuestion** 一次性抛给用户（每个 issue 一道题，附你的推荐选项；无 AskUserQuestion 时降级为纯文本编号列表）。
3. 对每个得到回答的 issue，用 `issue-upsert` 幂等写回：
   - **建子记录**存答案：`external_key=<qkey>#answer`、`parent_key=<qkey>`、`requirement` 同父、`type` 同父、`status=resolved`、`title=[答复] <原标题>`、`evidence=<用户原话答案>`、`updated_round=<当前轮>`；
   - 父 issue 置 `status=resolved`，evidence 追加"已由用户答复，见子记录 <qkey>#answer"。
   ```bash
   echo '<JSON>' | ${SELF_LOOP_BRIDGE_CMD:-loop-bridge} \
     issue-upsert --app "$FEISHU_BITABLE_APP" --table "$FEISHU_BITABLE_TABLE" --key-field external_key
   ```
4. 用户选择"延后/暂不回答"的，保持 open 并记 evidence，不强逼。
5. 全部可回答 issue 处理完，再进 §3。

> 每个用户决策在看板成对留痕（父问题 + 子答案），可追溯；build agent 也能从看板读到答案。`parent_key` 字段由 `ensure-board` 自动建好。

## 3. 启动 loop（交给 Workflow）

preflight 全过后，读环境变量并用 **Workflow 工具**运行编排脚本（这会 fan-out 多个 agent + worktree，是用户已明确要求的）：

```
Workflow({
  scriptPath: "<本skill目录>/self-loop.workflow.js",
  args: {
    docId:   "<docKind=docx 时为 docx document_id；docKind=sheet 时为 spreadsheet_token>",
    docKind: "<docx | sheet，来自 §preflight.4>",
    app:   "<$FEISHU_BITABLE_APP>",
    table: "<$FEISHU_BITABLE_TABLE>",
    maxRounds: <$SELF_LOOP_MAX_ROUNDS 或 6>,
    scopeRule: "<$SELF_LOOP_SCOPE_RULE，可空>",
    bridgeCmd: "<$SELF_LOOP_BRIDGE_CMD 或 loop-bridge>",
    rulesPath: "<$SELF_LOOP_RULES_PATH 或 self-loop.rules.md>"
  }
})

> 若上面探测到既有 run，**直接用同样的 args 再跑一次**即可——boot 阶段会自动续跑。要从干净状态重来，先删 `.self-loop/run/<id>/`。会话内中断想省 token，可改用 `Workflow({scriptPath, resumeFromRunId:"<上次 runId>"})`。
```

> 注意：app/table 等值由你从环境变量读出后填进 args（Workflow 脚本本身不读 env）。凭据 `FEISHU_APP_ID/SECRET` 不进 args——它们由 loop-bridge 在子进程内自行从 env 读取。

脚本会自己跑完 Intake → loop(Build→Verify→Sync) → 收敛，期间后台运行，完成时回通知。

## 3.5 Review 门（multica 风格：看 diff + 摘要，Approve / Request-changes）

把看板当成 human+agent 共用的看板，**状态即列**：Backlog(`open`) → In-progress(`in_progress`) → **Review(`verifying`)** → Done(`resolved`)；Blocked = `blocker` 型 open。agent 是"队友"：每条 issue 记 `assignee`（如 `build:REQ-1` / `checker` / `user`），完成后主动进 Review、主动报 blocker。

每个需求经 build+checker 跑绿、开出 PR 后，**不要直接判完成**——先进 Review 门，像 multica 那样给用户一张 **review 卡**：

1. **展示**：改动 `git diff --stat origin/main..<branch>` + checker 逐条 verdict + maker 一句话摘要 + 关联 issue + PR/MR 链接。
2. **问**（AskUserQuestion）：`Approve` / `Request changes(附意见)` / `Hold(先放着)`。
3. **据答幂等写回看板**：
   - **Approve** → 该需求 `#intake` 置 `resolved`(Done)、`assignee=user`；再单独问是否要执行 merge（外发动作，分开确认）。
   - **Request changes** → 用户意见写成**子记录**（`external_key=<key>#review-rN`、`parent_key=<key>`、`type=gap`、`assignee=user`、evidence=意见），父 `#intake` 退回 `in_progress`，下一轮 build 带上反馈重做。
   - **Hold** → 留 `verifying`，记 evidence。
4. 多个需求同时到 Review 时，**一次性把多张卡一起呈现**（一个 AskUserQuestion 多道题），别让用户逐个等。

> 对应 multica：Review 列 + Approve/Request-changes 审批门；agent 当队友(assignee)、主动报 blocker；成功经验沉淀进 Rules `## Learned`（≈ multica 的 compound skills）。

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
- `self-loop.workflow.js` — 编排脚本（pipeline 并行 + worktree 隔离 + loop-until-dry + 续跑 + 规则沉淀）。
- `self-loop.rules.example.md` — 规则记忆模板，复制为目标仓根 `self-loop.rules.md`。
- 运行期外置状态：`.self-loop/run/<id>/{meta,progress}.json` 与 `dod/*.json`（断点续跑用，gitignore）。
