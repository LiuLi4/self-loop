# self-loop Rules

> 复制为目标仓根目录的 `self-loop.rules.md`（或用 `SELF_LOOP_RULES_PATH` 指定路径）。
> 每个 maker / checker agent 开工前都会读这份文件并严格遵守。
> `## Learned` 节由 loop 自动追加——人工也可以补充，但别删历史教训。

## Hard rules（硬约束，永不放宽）

- 不改需求正文、不改已冻结的 DoD。
- 不部署生产、不读写 secret、不合并默认分支（main）。
- 不覆盖非本任务的脏改动；开工前先 `git status`。
- 每个需求在独立 worktree / 分支 `self-loop/<key>-*` 上工作，互不干扰。
- 提交前必须过本项目质量门（测试 / 构建 / lint）；失败先修再提交。

## Project conventions（项目约定，按需填写）

<!-- 例：
- 后端用 Go，错误必须 wrap 上下文；新逻辑必须带单测。
- 前端用 Vue3 + TS，组件放 src/components，改样式走 design token。
- 接口契约改动必须同步 contracts/ 并跑契约测试。
-->

## Learned（loop 自动沉淀，[rN] 标记轮次）

<!-- loop 会把每轮 checker 的系统性教训追加到这里，例如：
- [r1] 涉及金额的接口，DoD 必须含金额精度断言，否则 checker 漏判。
-->
