export const meta = {
  name: 'self-loop-engineering',
  description: '飞书需求驱动的自治开发 loop：拉需求→冻结DoD→worktree并行实现→独立checker校验→issue回写飞书→loop-until-dry，状态外置可断点续跑，规则记忆随轮沉淀',
  phases: [
    { title: 'Intake', detail: 'boot 续跑探测 + 拉需求 + (可选)范围守卫 + 冻结 DoD' },
    { title: 'Build', detail: '读规则+DoD，每需求独立 worktree 实现' },
    { title: 'Verify', detail: 'checker 重跑命令档 + 反驳语义档' },
    { title: 'Sync', detail: 'issue 幂等回写飞书 + 写检查点 + 沉淀规则' },
  ],
}

// args = { docId, app, table, maxRounds?, runDir?, bridgeCmd?, scopeRule?, rulesPath? }
const docId = args?.docId
const app = args?.app
const table = args?.table
const MAX_ROUNDS = args?.maxRounds ?? 6
const RUN = args?.runDir ?? `.self-loop/run/${(docId ?? 'run').slice(0, 12)}`
const BRIDGE = args?.bridgeCmd ?? 'loop-bridge'
// 文档类型：'docx'（默认）走 doc-dump，'sheet' 走 sheet-dump。由 SKILL.md 据 wiki 解析结果传入。
const DOC_KIND = args?.docKind ?? 'docx'
const READ_CMD = DOC_KIND === 'sheet'
  ? `${BRIDGE} sheet-dump --sheet ${docId}`
  : `${BRIDGE} doc-dump --doc ${docId}`
const SCOPE_RULE = args?.scopeRule ?? ''
// Rules（策略记忆）：所有 maker/checker 开工前必读；loop 会把历轮教训追加到它的 "## Learned" 节
const RULES = args?.rulesPath ?? 'self-loop.rules.md'
if (!docId || !app || !table) throw new Error('缺少 args.docId / args.app / args.table')

// 外置状态文件（断点续跑用）：
//   meta.json     = { docId, requirements:[...] }      —— 首次 intake 写一次
//   progress.json = { round, converged }               —— 每轮重写
//   dod/<key>.json= 冻结的验收契约                       —— 首次 intake 写，run 内只读
const META = `${RUN}/meta.json`
const PROGRESS = `${RUN}/progress.json`

// ---- schemas ----
const BOOT_SCHEMA = {
  type: 'object', required: ['resume'],
  properties: {
    resume: { type: 'boolean', description: '是否检测到可续跑的既有 run（meta.json + dod 存在）' },
    lastRound: { type: 'integer', description: 'progress.json 里的已完成轮次，无则 0' },
    requirements: { type: 'array', items: REQ() },
    boardIssues: { type: 'array', items: ISSUE(), description: '从飞书看板载入的现有 issue' },
  },
}
const REQ_SCHEMA = { type: 'object', required: ['requirements'], properties: { requirements: { type: 'array', items: REQ() } } }
const BOUNDARY_SCHEMA = { type: 'object', required: ['inScope', 'reason'], properties: { inScope: { type: 'boolean' }, reason: { type: 'string' } } }
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
    requirement: { type: 'string' }, branch: { type: 'string' }, prUrl: { type: 'string' },
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
function REQ() {
  return {
    type: 'object', required: ['key', 'title', 'flow', 'apis'],
    properties: {
      key: { type: 'string', description: '稳定需求 id，如 REQ-1' }, title: { type: 'string' },
      flow: { type: 'string', description: '业务流程摘要' },
      apis: { type: 'array', items: { type: 'string' }, description: '涉及接口清单' },
    },
  }
}
function ISSUE() {
  return {
    type: 'object', required: ['external_key', 'requirement', 'title', 'type', 'status'],
    properties: {
      external_key: { type: 'string', description: '幂等键，如 REQ-1#issue-2' },
      requirement: { type: 'string' }, title: { type: 'string' },
      type: { type: 'string', enum: ['缺陷', '缺口', '阻塞', '待澄清'] },
      status: { type: 'string', enum: ['待处理', '进行中', '待评审', '已完成', '不做'] },
      severity: { type: 'string', enum: ['高', '中', '低'] },
      acceptance_ref: { type: 'string' }, evidence: { type: 'string' },
    },
  }
}

// ============================================================
// Intake：先 boot 探测续跑；非续跑才解析文档 + 冻结
// ============================================================
phase('Intake')
const boot = await agent(
  `自治 loop 启动探测（外置状态在目录 ${RUN}）：
   1) 若 ${META} 存在则读出其中的 requirements（这是断点续跑的依据）；并读 ${PROGRESS} 取 round（无则 0）。
   2) 列出 ${RUN}/dod/ 下已冻结的 DoD 文件，确认哪些需求已冻结。
   3) 运行 \`${BRIDGE} issues-list --app ${app} --table ${table}\` 拉取飞书看板现有 issue（恢复 issue 记忆）。
   判定 resume：当 ${META} 存在且 requirements 非空且对应 dod 文件齐全时 resume=true。
   返回 {resume, lastRound, requirements, boardIssues}。`,
  { label: 'intake:boot', phase: 'Intake', schema: BOOT_SCHEMA })

let requirements, startRound, openIssues
if (boot.resume && (boot.requirements?.length ?? 0) > 0) {
  requirements = boot.requirements
  startRound = boot.lastRound ?? 0
  openIssues = boot.boardIssues ?? []
  log(`↻ 续跑：从第 ${startRound + 1} 轮继续，${requirements.length} 个需求，看板载入 ${openIssues.length} 个 issue`)
} else {
  // —— 全新 intake：解析文档 → 范围守卫 → 冻结 DoD → 写 meta ——
  const dump = await agent(
    `运行 \`${READ_CMD}\`。${DOC_KIND === 'sheet'
      ? '输出是电子表格各分表的单元格二维数组(sheets[].values)；通常首行是表头、每行一个需求，列对应 标题/业务流程/接口 等——按表头语义切分。'
      : '输出是文档全部 block 的扁平文本 JSON——据此语义切分。'}
     切分成"需求"数组：每个需求含 key（稳定 id，如 REQ-1）、title、flow、apis。仅返回结构化结果。`,
    { label: 'intake:parse', phase: 'Intake', schema: REQ_SCHEMA })

  const inScope = []
  const outOfScope = []
  for (const r of dump.requirements) {
    if (!SCOPE_RULE) { inScope.push(r); continue }
    const g = await agent(
      `判定需求是否落在本项目允许的范围内。范围约束：${SCOPE_RULE}
       需求：${JSON.stringify(r)}。若超出范围 → inScope=false。`,
      { label: `intake:boundary:${r.key}`, phase: 'Intake', schema: BOUNDARY_SCHEMA })
    if (g.inScope) inScope.push(r)
    else {
      log(`⚠ ${r.key} 越界(${g.reason}) → 标 spec-question，本轮不实现`)
      outOfScope.push({ external_key: `${r.key}#scope`, requirement: r.key, title: r.title,
        type: '待澄清', status: '待处理', evidence: g.reason })
    }
  }

  if (inScope.length === 0) {
    log('无 in-scope 需求，仅回写越界 spec-question 后结束')
    await syncIssues(outOfScope, 0)
    return { rounds: 0, converged: true, inScope: 0, openIssues: outOfScope }
  }

  // 冻结 DoD（写 RUN/dod/<key>.json）+ 写 meta.json（外置需求集，供续跑）
  await parallel(inScope.map(r => () =>
    agent(
      `为需求 ${r.key} 生成机器可校验的 DoD 验收契约，写入文件 ${RUN}/dod/${r.key}.json。
       每条标准必须可命令判定(test/build/lint/fe，给出 cmd 与 must=exit 0)或可证据核验(api 接口全覆盖 / scope)。
       用本项目实际的质量门命令。需求+接口：${JSON.stringify(r)}`,
      { label: `intake:dod:${r.key}`, phase: 'Intake', schema: DOD_SCHEMA })))
  await agent(
    `把以下内容原样写入 ${META}（用于断点续跑）：${JSON.stringify({ docId, requirements: inScope })}`,
    { label: 'intake:meta', phase: 'Intake' })

  requirements = inScope
  startRound = 0
  openIssues = [...outOfScope]
}

// 依赖/并行分析：把需求分成有序"波次"——同波次互相独立可并行，后波依赖前波
const WAVES_SCHEMA = {
  type: 'object', required: ['waves'],
  properties: { waves: { type: 'array', items: { type: 'array', items: { type: 'string' } } }, reason: { type: 'string' } },
}
const reqByKey = Object.fromEntries(requirements.map(r => [r.key, r]))
let waves
{
  const wv = await agent(
    `分析这些需求间的实现依赖，分成有序"波次"：同一波次内的需求互相独立、可并行；靠后的波次依赖靠前波次的产物。倾向尽量并行（彼此独立就放同一波）。
     需求：${JSON.stringify(requirements.map(r => ({ key: r.key, title: r.title, flow: r.flow, apis: r.apis })))}
     返回 {waves: [["REQ-1","REQ-2"],["REQ-3"]], reason}`,
    { label: 'intake:waves', phase: 'Intake', schema: WAVES_SCHEMA })
  const seen = new Set()
  waves = (wv.waves || []).map(w => w.filter(k => reqByKey[k] && !seen.has(k) && seen.add(k))).filter(w => w.length)
  const missing = requirements.map(r => r.key).filter(k => !seen.has(k))
  if (missing.length) waves.push(missing) // 漏掉的需求兜底放最后一波
  if (!waves.length) waves = [requirements.map(r => r.key)]
  log(`并行波次：${waves.map((w, i) => `W${i + 1}[${w.join(',')}]`).join(' → ')}`)
}

// ============================================================
// loop-until-dry（round 从 startRound 续；每轮按波次顺序、波次内并行）
// ============================================================
let round = startRound
let converged = false

while (round < MAX_ROUNDS) {
  round++
  log(`=== Round ${round}: ${requirements.length} 需求并行 ===`)

  const results = []
  for (const wave of waves) {
    const waveReqs = wave.map(k => reqByKey[k]).filter(Boolean)
    if (!waveReqs.length) continue
    log(`  ▶ 波次 [${wave.join(', ')}] 并行 ${waveReqs.length} 个`)
    const wr = await pipeline(waveReqs,
      // ---- maker：先读规则 + 冻结 DoD（从磁盘），再实现 ----
      (r) => agent(
        `【先读规则】开工前先读 ${RULES}（含历轮沉淀的 "## Learned" 教训），严格遵守其中所有约束。
         【读 DoD】读取冻结的验收契约文件 ${RUN}/dod/${r.key}.json（不得修改它）。
         在隔离 worktree 内推进需求 ${r.key} 的实现，目标是让该 DoD 逐条达标。
         走本项目的 SDLC/构建流程（若装了 /sdlc 之类 skill 则用之）。
         完成质量门后 git 提交、推送、开/更新 PR（分支 self-loop/${r.key}-*）。
         已知 open issue（与本需求相关的请修复并标已完成）：${JSON.stringify(openIssues.filter(i => i.requirement === r.key && i.status !== '已完成'))}
         【硬护栏】禁止：改需求正文、改冻结 DoD、部署生产、读写 secret、合并默认分支(main)、覆盖非本任务脏改动。
         返回每条 DoD 自评 + 本轮新发现 issue（external_key 用 ${r.key}#issue-N）。`,
        { label: `build:${r.key}`, phase: 'Build', isolation: 'worktree', schema: BUILD_SCHEMA }),

      // ---- checker：独立校验，重跑命令 + 反驳语义 ----
      (build, r) => agent(
        `你是独立校验者。先读 ${RULES} 了解约束，再读冻结 DoD 文件 ${RUN}/dod/${r.key}.json，对需求 ${r.key} 逐条判定，不得采信 maker 自评。
         命令档(test/build/lint/fe)：实际重跑 cmd，看 exit code，贴关键输出当 evidence。
         语义档(api/scope)：核验证据并主动尝试反驳；证据不足即判 fail。
         maker 自评（仅参考）：${JSON.stringify(build?.selfReport ?? [])}
         返回每条 verdict(pass/fail)+evidence(其中 id 对应 DoD 的 criteria.id)，以及校验中新发现的 issue。`,
        { label: `verify:${r.key}`, phase: 'Verify', schema: VERDICT_SCHEMA })
    )
    results.push(...wr)
  }

  // 汇总：合并 issue（按 external_key 去重，新状态覆盖旧）
  const fresh = results.filter(Boolean).flatMap(v => v.issues ?? [])
  openIssues = dedupeByKey([...openIssues, ...fresh])
  // checker 判 pass 的标准 → 关联 issue 标已完成
  for (const v of results.filter(Boolean)) {
    const passed = new Set((v.verdicts ?? []).filter(c => c.pass).map(c => c.id))
    for (const i of openIssues) {
      if (i.requirement === v.requirement && passed.has(i.acceptance_ref)) i.status = '已完成'
    }
  }

  const allGreen = results.filter(Boolean).length === requirements.length &&
    results.filter(Boolean).every(v => (v.verdicts ?? []).length > 0 && v.verdicts.every(c => c.pass))
  const noOpen = openIssues.every(i => i.status === '已完成' || i.status === '不做')
  converged = allGreen && noOpen

  // —— 状态外置（断点续跑）：回写飞书看板 + 写 progress 检查点 ——
  await syncIssues(openIssues, round)
  await agent(`把以下内容原样写入 ${PROGRESS}（检查点）：${JSON.stringify({ round, converged })}`,
    { label: `checkpoint:r${round}`, phase: 'Sync' })

  // —— 规则记忆沉淀：把本轮系统性教训追加到 RULES 的 "## Learned" 节 ——
  const fails = results.filter(Boolean).flatMap(v => (v.verdicts ?? []).filter(c => !c.pass))
  if (fails.length > 0 || fresh.length > 0) {
    await agent(
      `审视本轮 checker 的失败判定与新发现 issue，提炼**可复用的系统性教训**（非一次性 bug；如"某类接口必须先写契约测试"）。
       若有，用 markdown 列表项 append 到 ${RULES} 文件的 "## Learned" 节（每条一行，带 [r${round}] 前缀）；不要重复已存在的教训；无则不写。
       失败判定：${JSON.stringify(fails.slice(0, 20))}
       仅返回追加了几条。`,
      { label: `learn:r${round}`, phase: 'Sync' })
  }

  if (converged) { log(`✅ Round ${round}: 全部 DoD 达标且无 open issue，收敛`); break }
  log(`Round ${round} 未收敛：剩 ${openIssues.filter(i => i.status === '待处理' || i.status === '进行中').length} 个未关 issue`)
}

return {
  rounds: round,
  converged,
  inScope: requirements.length,
  openIssues: openIssues.filter(i => i.status !== '已完成' && i.status !== '不做'),
  resumeHint: converged ? null : `未收敛。重跑同一文档(${docId})会自动从 ${RUN} 续跑，无需重新冻结 DoD。`,
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
