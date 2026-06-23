export const meta = {
  name: 'self-loop-engineering',
  description: '飞书需求驱动的自治开发 loop：拉需求→冻结DoD→worktree并行实现→独立checker对照DoD校验→issue幂等回写飞书→loop-until-dry',
  phases: [
    { title: 'Intake', detail: 'doc-dump 拉需求 + (可选)范围守卫 + 冻结 DoD' },
    { title: 'Build', detail: '每需求独立 worktree 实现' },
    { title: 'Verify', detail: 'checker 重跑命令档 + 反驳语义档' },
    { title: 'Sync', detail: 'issue 幂等回写飞书 Bitable 看板' },
  ],
}

// args = { docId, app, table, maxRounds?, runDir?, bridgeCmd?, scopeRule? }  —— 由 SKILL.md 调用 Workflow 时传入
const docId = args?.docId
const app = args?.app
const table = args?.table
const MAX_ROUNDS = args?.maxRounds ?? 6
const RUN = args?.runDir ?? `.self-loop/run/${(docId ?? 'run').slice(0, 12)}`
// loop-bridge 调用方式：默认假设已 `go install` 到 PATH；也可传 'go run ./loop-bridge' 等
const BRIDGE = args?.bridgeCmd ?? 'loop-bridge'
// 可选的范围约束（自然语言）。提供则做边界守卫，越界需求只标 spec-question 不实现；不提供则全部视为 in-scope。
const SCOPE_RULE = args?.scopeRule ?? ''
if (!docId || !app || !table) throw new Error('缺少 args.docId / args.app / args.table')

// ---- schemas ----
const REQ_SCHEMA = {
  type: 'object', required: ['requirements'],
  properties: { requirements: { type: 'array', items: {
    type: 'object', required: ['key', 'title', 'flow', 'apis'],
    properties: {
      key: { type: 'string', description: '稳定需求 id，如 REQ-1' },
      title: { type: 'string' },
      flow: { type: 'string', description: '业务流程摘要' },
      apis: { type: 'array', items: { type: 'string' }, description: '涉及接口清单' },
    },
  } } },
}
const BOUNDARY_SCHEMA = {
  type: 'object', required: ['inScope', 'reason'],
  properties: { inScope: { type: 'boolean' }, reason: { type: 'string' } },
}
const DOD_SCHEMA = {
  type: 'object', required: ['requirement', 'criteria'],
  properties: {
    requirement: { type: 'string' },
    criteria: { type: 'array', items: {
      type: 'object', required: ['id', 'kind', 'desc'],
      properties: {
        id: { type: 'string' }, kind: { type: 'string', enum: ['test', 'build', 'lint', 'fe', 'api', 'scope'] },
        desc: { type: 'string' }, cmd: { type: 'string' }, must: { type: 'string' },
      },
    } },
  },
}
const BUILD_SCHEMA = {
  type: 'object', required: ['requirement', 'selfReport', 'issues'],
  properties: {
    requirement: { type: 'string' },
    branch: { type: 'string' }, prUrl: { type: 'string' },
    selfReport: { type: 'array', items: { type: 'object', properties: {
      id: { type: 'string' }, pass: { type: 'boolean' }, evidence: { type: 'string' } } } },
    issues: { type: 'array', items: ISSUE() },
  },
}
const VERDICT_SCHEMA = {
  type: 'object', required: ['requirement', 'verdicts', 'issues'],
  properties: {
    requirement: { type: 'string' },
    verdicts: { type: 'array', items: { type: 'object', required: ['id', 'pass'], properties: {
      id: { type: 'string' }, pass: { type: 'boolean' }, evidence: { type: 'string' } } } },
    issues: { type: 'array', items: ISSUE() },
  },
}
function ISSUE() {
  return {
    type: 'object', required: ['external_key', 'requirement', 'title', 'type', 'status'],
    properties: {
      external_key: { type: 'string', description: '幂等键，如 REQ-1#issue-2' },
      requirement: { type: 'string' }, title: { type: 'string' },
      type: { type: 'string', enum: ['bug', 'gap', 'blocker', 'spec-question'] },
      status: { type: 'string', enum: ['open', 'in_progress', 'verifying', 'resolved', 'wont_fix'] },
      severity: { type: 'string', enum: ['p0', 'p1', 'p2'] },
      acceptance_ref: { type: 'string' }, evidence: { type: 'string' },
    },
  }
}

// ============================================================
// Intake：拉需求 →（可选）范围守卫 → 冻结 DoD
// ============================================================
phase('Intake')
const dump = await agent(
  `运行 \`${BRIDGE} doc-dump --doc ${docId}\`，它输出文档全部 block 的扁平文本 JSON。
   据此把文档语义切分成"需求"数组：每个需求含 key（稳定 id，如 REQ-1）、title、flow（业务流程）、apis（接口清单）。
   仅返回结构化结果。`,
  { label: 'intake:parse', phase: 'Intake', schema: REQ_SCHEMA })

const inScope = []
const outOfScope = [] // 越界需求保留为 spec-question issue 回写
for (const r of dump.requirements) {
  if (!SCOPE_RULE) { inScope.push(r); continue }
  const g = await agent(
    `判定需求是否落在本项目允许的范围内。范围约束：${SCOPE_RULE}
     需求：${JSON.stringify(r)}。若超出范围（需新建范围外 spec 或属其它领域）→ inScope=false。`,
    { label: `intake:boundary:${r.key}`, phase: 'Intake', schema: BOUNDARY_SCHEMA })
  if (g.inScope) { inScope.push(r) }
  else {
    log(`⚠ ${r.key} 越界(${g.reason}) → 标 spec-question，本轮不实现`)
    outOfScope.push({ external_key: `${r.key}#scope`, requirement: r.key, title: r.title,
      type: 'spec-question', status: 'open', evidence: g.reason })
  }
}

if (inScope.length === 0) {
  log('无 in-scope 需求，仅回写越界 spec-question 后结束')
  await syncIssues(outOfScope, 0)
  return { rounds: 0, converged: true, inScope: 0, openIssues: outOfScope }
}

// 冻结每个需求的 DoD（写入 RUN/dod/<key>.json，run 内只读）
const dods = await parallel(inScope.map(r => () =>
  agent(
    `为需求 ${r.key} 生成机器可校验的 DoD 验收契约，并写入文件 ${RUN}/dod/${r.key}.json。
     每条标准必须可命令判定(test/build/lint/fe，给出 cmd 与 must=exit 0)或可证据核验(api 接口全覆盖 / scope 落在范围内)。
     用本项目实际的质量门命令（如 测试/构建/lint/前端构建）。
     需求+接口：${JSON.stringify(r)}`,
    { label: `intake:dod:${r.key}`, phase: 'Intake', schema: DOD_SCHEMA })))
const dodByKey = Object.fromEntries(dods.filter(Boolean).map(d => [d.requirement, d]))

// ============================================================
// loop-until-dry
// ============================================================
let round = 0
let openIssues = [...outOfScope]

while (round < MAX_ROUNDS) {
  round++
  log(`=== Round ${round}: ${inScope.length} 需求并行 ===`)

  const results = await pipeline(inScope,
    // ---- maker：worktree 内实现 ----
    (r) => agent(
      `在隔离 worktree 内，对需求 ${r.key} 推进实现，目标是让冻结的 DoD 逐条达标。
       DoD：${JSON.stringify(dodByKey[r.key] ?? {})}
       走本项目的 SDLC/构建流程（若安装了 /sdlc 之类的 skill 则用之），spec 直接取自需求(${r.key})的业务流程与接口清单。
       完成质量门后在 git 提交、推送、开/更新 PR（分支 self-loop/${r.key}-*）。
       已知 open issue（若与本需求相关请修复并标 resolved）：${JSON.stringify(openIssues.filter(i => i.requirement === r.key))}
       【硬护栏】禁止：改需求正文、改冻结 DoD、部署生产、读写 secret、合并默认分支(main)、覆盖非本任务的脏改动。
       返回每条 DoD 自评 + 本轮新发现 issue（external_key 用 ${r.key}#issue-N）。`,
      { label: `build:${r.key}`, phase: 'Build', isolation: 'worktree', schema: BUILD_SCHEMA }),

    // ---- checker：独立校验，重跑命令 + 反驳语义 ----
    (build, r) => agent(
      `你是独立校验者，对需求 ${r.key} 对照冻结 DoD 逐条判定，不得采信 maker 自评。
       DoD：${JSON.stringify(dodByKey[r.key] ?? {})}
       命令档(test/build/lint/fe)：实际重跑 cmd，看 exit code，贴关键输出当 evidence。
       语义档(api/scope)：核验证据并主动尝试反驳；证据不足即判 fail。
       maker 自评（仅参考）：${JSON.stringify(build?.selfReport ?? [])}
       返回每条 verdict(pass/fail)+evidence，以及校验中新发现的 issue。`,
      { label: `verify:${r.key}`, phase: 'Verify', schema: VERDICT_SCHEMA })
  )

  // 汇总本轮：合并 issue（按 external_key 去重，新状态覆盖旧）
  const fresh = results.filter(Boolean).flatMap(v => v.issues ?? [])
  openIssues = dedupeByKey([...openIssues, ...fresh])

  // checker 判 pass 的标准 → 关联 issue 标 resolved
  for (const v of results.filter(Boolean)) {
    const passed = new Set((v.verdicts ?? []).filter(c => c.pass).map(c => c.id))
    for (const i of openIssues) {
      if (i.requirement === v.requirement && passed.has(i.acceptance_ref)) i.status = 'resolved'
    }
  }

  await syncIssues(openIssues, round)

  const allGreen = results.filter(Boolean).length === inScope.length &&
    results.filter(Boolean).every(v => (v.verdicts ?? []).length > 0 && v.verdicts.every(c => c.pass))
  const noOpen = openIssues.every(i => i.status === 'resolved' || i.status === 'wont_fix')
  if (allGreen && noOpen) { log(`✅ Round ${round}: 全部 DoD 达标且无 open issue，收敛`); break }
  log(`Round ${round} 未收敛：剩 ${openIssues.filter(i => i.status === 'open' || i.status === 'in_progress').length} 个未关 issue`)
}

return {
  rounds: round,
  converged: round < MAX_ROUNDS,
  inScope: inScope.length,
  openIssues: openIssues.filter(i => i.status !== 'resolved' && i.status !== 'wont_fix'),
}

// ---- helpers ----
function dedupeByKey(issues) {
  const m = new Map()
  for (const i of issues) m.set(i.external_key, { ...(m.get(i.external_key) ?? {}), ...i })
  return [...m.values()]
}

// syncIssues：单写者回写飞书。把 issue 数组转成 bridge 期望的 {records:[{fields}]} 并幂等 upsert。
async function syncIssues(issues, r) {
  if (issues.length === 0) return
  phase('Sync')
  const records = issues.map(i => ({ fields: { ...i, updated_round: r } }))
  await agent(
    `把以下 issue 幂等回写飞书 Bitable 看板（单写者，第 ${r} 轮）：
     执行 \`echo '<JSON>' | ${BRIDGE} issue-upsert --app ${app} --table ${table} --key-field external_key\`，
     其中 <JSON> = ${JSON.stringify({ records })}
     确认 bridge 返回 {created,updated} 后结束。`,
    { label: `sync:r${r}`, phase: 'Sync' })
}
