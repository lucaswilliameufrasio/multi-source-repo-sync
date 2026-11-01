'use strict'

// Everything the page shows from GitHub or the manifest goes in as text, never as markup.
const el = (tag, props = {}, ...children) => {
  const node = document.createElement(tag)
  for (const [key, value] of Object.entries(props)) {
    if (key === 'class') node.className = value
    else if (key.startsWith('on')) node.addEventListener(key.slice(2), value)
    else if (value !== undefined && value !== null && value !== false) node.setAttribute(key, value === true ? '' : value)
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue
    node.append(child.nodeType ? child : document.createTextNode(String(child)))
  }
  return node
}

const state = { manifest: null, rows: new Map(), busy: false }
const $ = (id) => document.getElementById(id)

async function call(path, options = {}) {
  const response = await fetch(path, {
    credentials: 'same-origin',
    headers: options.body ? { 'Content-Type': 'application/json' } : {},
    ...options,
    body: options.body ? JSON.stringify(options.body) : undefined
  })
  const text = await response.text()
  let data = null
  try { data = text ? JSON.parse(text) : null } catch { /* a plain-text error */ }
  if (!response.ok) {
    const error = new Error((data && data.message) || text || `HTTP ${response.status}`)
    error.data = data
    throw error
  }
  return data
}

function say(message, isError = false) {
  const node = $('status')
  node.textContent = message
  node.className = isError ? 'error' : 'muted'
}

const key = (service, pair) => `${service}|${pair}`

// How an assessment reads, and how loud it is.
function describe(entry, branch) {
  const [left, right] = entry.pair.split('/')
  const a = branch.assessment
  if (a === 'in_sync') return ['Em sincronia', 'ok']
  if (a === 'different_heads') {
    return branch.action === 'ancestry_not_checked' ? ['Diferente — analise o histórico', 'warn'] : ['Diferente', 'warn']
  }
  if (a === 'clean_merge') return [`Combinável → PR para ${(branch.pr_targets || []).join(', ')}`, 'info']
  if (a === 'merge_conflict') return ['Conflito', 'danger']
  if (a === 'unrelated_histories') return ['Históricos sem ancestral comum', 'danger']
  if (a === 'missing_on_both') return ['Não existe nos dois lados', 'neutral']
  if (a === 'preserved') return ['Preservada', 'neutral']
  if (a === 'repository_unavailable' || a === 'history_unavailable') return ['Indisponível', 'danger']
  if (a === `missing_on_${left}`) return [`Só em ${right}`, 'neutral']
  if (a === `missing_on_${right}`) return [`Só em ${left}`, 'neutral']
  return [a, 'neutral']
}

function render() {
  const host = $('services')
  host.replaceChildren()
  for (const service of state.manifest.services) {
    const section = el('section', { class: 'service', 'aria-labelledby': `svc-${service.name}` }, el('h2', { id: `svc-${service.name}` }, service.name))
    if (service.pairs.length === 0) {
      section.append(el('p', { class: 'muted' }, 'Sem cópia em duas organizações do manifesto.'))
    }
    for (const pair of service.pairs) {
      const entry = state.rows.get(key(service.name, pair))
      section.append(row(service.name, pair, entry))
    }
    host.append(section)
  }
}

function row(service, pair, entry) {
  const chips = el('div', { class: 'chips' })
  if (!entry) {
    chips.append(el('span', { class: 'chip tone-neutral' }, 'Carregando…'))
  } else {
    for (const error of entry.errors || []) chips.append(el('span', { class: 'chip tone-danger' }, `Erro: ${error}`))
    for (const branch of (entry.branches || []).filter((b) => b.policy !== 'inventory_only')) {
      const [text, tone] = describe(entry, branch)
      const clickable = branch.policy === 'sync_candidate'
      const chip = el(clickable ? 'button' : 'span', { class: `chip tone-${tone}`, type: clickable ? 'button' : undefined }, el('span', { class: 'branch' }, branch.name), text)
      if (clickable) chip.addEventListener('click', () => openDetail(service, pair, branch.name))
      chips.append(chip)
    }
  }
  const analyze = el('button', { type: 'button', class: 'secondary small', 'aria-label': `Analisar o histórico de ${service} entre ${pair}`, onclick: () => load('plan', service, pair) }, 'Analisar')
  return el('div', { class: 'pair-row' }, el('span', { class: 'pair-name' }, pair.replace('/', ' ↔ ')), chips, analyze)
}

async function load(mode, service, pair) {
  if (state.busy) return
  state.busy = true
  const what = service ? `${service} (${pair})` : 'tudo'
  say(mode === 'plan' ? `Analisando o histórico de ${what}… pode levar um minuto.` : `Lendo ${what} no GitHub…`)
  try {
    const query = new URLSearchParams({ mode })
    if (service) query.set('service', service)
    if (pair) query.set('pair', pair)
    const report = await call(`/api/report?${query}`)
    for (const entry of report.repositories) state.rows.set(key(entry.service, entry.pair), entry)
    render()
    say(mode === 'plan' ? 'Análise concluída.' : 'Situação atualizada.')
  } catch (error) {
    say(`Não consegui ler: ${error.message}`, true)
  } finally {
    state.busy = false
  }
}

const dialog = () => $('detail')

function prList(side) {
  const prs = side.open_pull_requests || []
  if (prs.length === 0) return el('p', { class: 'muted' }, `${side.label}: nenhuma PR aberta.`)
  return el('div', {}, el('h3', {}, `${side.label}: ${prs.length} PR(s) aberta(s)`), el('ul', { class: 'plain' }, prs.map((pr) => {
    const label = `#${pr.number} ${pr.head_branch} → ${pr.base_branch}: ${pr.title}`
    const safe = typeof pr.url === 'string' && pr.url.startsWith('https://github.com/')
    return el('li', {}, safe ? el('a', { href: pr.url, target: '_blank', rel: 'noopener noreferrer' }, label) : label)
  })))
}

async function openDetail(service, pair, branchName) {
  const entry = state.rows.get(key(service, pair))
  const branch = (entry.branches || []).find((b) => b.name === branchName)
  $('detail-title').textContent = `${service} · ${branchName} · ${pair.replace('/', ' ↔ ')}`
  const body = $('detail-body')
  body.replaceChildren()
  const [text, tone] = describe(entry, branch)
  body.append(
    el('p', {}, el('span', { class: `chip tone-${tone}` }, text)),
    el('dl', {},
      el('dt', {}, entry.left.label), el('dd', { class: 'mono' }, branch.left_sha || '—'),
      el('dt', {}, entry.right.label), el('dd', { class: 'mono' }, branch.right_sha || '—')),
    branch.reason && branch.action !== 'ancestry_not_checked' ? el('p', { class: 'muted' }, branch.reason) : null,
    prList(entry.left), prList(entry.right)
  )
  if (branch.assessment === 'merge_conflict') {
    const target = entry.right.label
    body.append(el('div', { class: 'callout danger' },
      el('p', {}, 'Há conflito: a resolução é assistida e acontece na linha de comando, em uma cópia local (nada é enviado até você publicar):'),
      el('pre', {}, `multi-source-repo-sync resolve prepare --service ${service} --branch ${branchName} --pair ${pair} --target ${target}`),
      el('p', { class: 'muted' }, `Troque o alvo por ${entry.left.label} para abrir a PR no outro lado.`)))
  } else if (branch.assessment === 'different_heads' || branch.assessment === 'clean_merge') {
    body.append(el('div', { id: 'proposal' },
      el('button', { type: 'button', id: 'propose', onclick: () => propose(service, pair, branchName) }, 'Ver o que seria criado')))
  }
  dialog().showModal()
}

async function propose(service, pair, branch) {
  const holder = $('proposal')
  holder.replaceChildren(el('p', { class: 'muted', role: 'status' }, 'Analisando o histórico…'))
  try {
    const proposal = await call('/api/sync/propose', { method: 'POST', body: { service, branch, pair } })
    state.rows.set(key(service, pair), proposal.repository)
    render()
    holder.replaceChildren(proposalView(proposal))
  } catch (error) {
    holder.replaceChildren(el('p', { class: 'error', role: 'alert' }, error.message))
  }
}

function proposalView(proposal) {
  const nodes = []
  if (proposal.blocked) {
    nodes.push(el('div', { class: 'callout danger' }, `Nada será criado: ${proposal.blocked}`))
    return el('div', {}, nodes)
  }
  nodes.push(el('h3', {}, 'O que será criado'))
  nodes.push(el('ul', { class: 'plain' }, proposal.writes.map((write) => el('li', {},
    `Branch novo `, el('code', {}, write.candidate_branch), ` em ${write.target_repository}, e uma PR para ${write.base_branch}. Histórico de origem: ${write.source_repository} em `, el('code', {}, write.source_sha.slice(0, 12)), '.'))))
  nodes.push(el('p', { class: 'muted' }, 'Nada é mesclado, sobrescrito, apagado ou enviado com force. Abrir a PR pode disparar as verificações do GitHub. Na PR, use “merge commit” (não squash nem rebase).'))
  const input = el('input', { type: 'text', id: 'confirm', autocomplete: 'off', spellcheck: 'false', 'aria-describedby': 'confirm-hint' })
  const submit = el('button', { type: 'button', id: 'apply', disabled: true }, 'Criar branches e PRs')
  input.addEventListener('input', () => { submit.disabled = input.value !== proposal.phrase })
  submit.addEventListener('click', () => applySync(proposal, input.value, submit))
  nodes.push(el('label', { for: 'confirm' }, 'Para confirmar, digite exatamente:'))
  nodes.push(el('pre', { id: 'confirm-hint' }, proposal.phrase))
  nodes.push(input, el('p', {}, submit), el('div', { id: 'result', role: 'status' }))
  return el('div', {}, nodes)
}

async function applySync(proposal, confirmation, button) {
  const result = $('result')
  button.disabled = true
  result.replaceChildren(el('p', { class: 'muted' }, 'Criando… não feche esta janela.'))
  try {
    const answer = await call('/api/sync/apply', {
      method: 'POST',
      body: {
        service: proposal.service, branch: proposal.branch.name, pair: proposal.pair,
        confirmation, left_sha: proposal.branch.left_sha, right_sha: proposal.branch.right_sha
      }
    })
    result.replaceChildren(el('div', { class: 'callout ok' }, el('h3', {}, 'Pronto'), results(answer.results)))
    await load('status', proposal.service, proposal.pair)
  } catch (error) {
    const partial = error.data && error.data.results && error.data.results.length ? results(error.data.results) : null
    result.replaceChildren(el('div', { class: 'callout danger', role: 'alert' }, el('p', {}, error.message), partial))
    button.disabled = false
  }
}

function results(list) {
  return el('ul', { class: 'plain' }, (list || []).map((item) => {
    const safe = typeof item.pull_request_url === 'string' && item.pull_request_url.startsWith('https://github.com/')
    return el('li', {}, `${item.target_side}: branch `, el('code', {}, item.candidate_branch), item.branch_created ? ' criado' : ' já existia', '; ',
      item.pull_request_url ? [item.pull_request_created ? 'PR aberta: ' : 'PR existente: ', safe ? el('a', { href: item.pull_request_url, target: '_blank', rel: 'noopener noreferrer' }, item.pull_request_url) : item.pull_request_url] : 'sem PR (veja o erro)')
  }))
}

// ---- Empurrar uma branch para as outras cópias ----

const stateText = {
  new: ['Cria a branch e abre a PR', 'info'],
  already_there: ['Já tem o mesmo commit; só abre a PR', 'info'],
  different_history: ['Já existe uma branch com esse nome e outro conteúdo; não será tocada', 'warn'],
  no_base: ['Não tem a branch de destino da PR; será pulada', 'warn'],
  merge_commits_off: ['Não aceita merge commit; será pulada', 'warn']
}

function fill(select, values, labelOf = (v) => v) {
  select.replaceChildren(...values.map((value) => el('option', { value }, labelOf(value))))
}

function sourcesOf(service) {
  const labels = new Set()
  for (const pair of state.manifest.services.find((s) => s.name === service)?.pairs ?? []) pair.split('/').forEach((l) => labels.add(l))
  return [...labels]
}

async function loadBranches() {
  const service = $('p-service').value
  const source = $('p-source').value
  $('p-result').replaceChildren()
  fill($('p-branch'), [])
  if (!service || !source) return
  try {
    const data = await call(`/api/branches?${new URLSearchParams({ service, org: source })}`)
    fill($('p-branch'), data.branches)
    $('p-propose').disabled = data.branches.length === 0
    if (data.branches.length === 0) $('p-result').replaceChildren(el('p', { class: 'muted' }, 'Essa cópia não tem outra branch além das sincronizadas.'))
  } catch (error) {
    $('p-result').replaceChildren(el('p', { class: 'error', role: 'alert' }, error.message))
  }
}

function setupPropagation() {
  const services = state.manifest.services.filter((s) => s.pairs.length > 0).map((s) => s.name)
  fill($('p-service'), services)
  const refreshSources = () => {
    fill($('p-source'), sourcesOf($('p-service').value))
    return loadBranches()
  }
  $('p-service').addEventListener('change', refreshSources)
  $('p-source').addEventListener('change', loadBranches)
  $('p-propose').addEventListener('click', proposePropagation)
  return refreshSources()
}

async function proposePropagation() {
  const holder = $('p-result')
  const body = { service: $('p-service').value, source: $('p-source').value, branch: $('p-branch').value, targets: [] }
  holder.replaceChildren(el('p', { class: 'muted', role: 'status' }, 'Lendo os repositórios…'))
  try {
    holder.replaceChildren(propagationView(await call('/api/propagate/propose', { method: 'POST', body })))
  } catch (error) {
    holder.replaceChildren(el('p', { class: 'error', role: 'alert' }, error.message))
  }
}

function propagationView(proposal) {
  const nodes = [
    el('h3', {}, `${proposal.branch} (${proposal.source}, ${proposal.source_sha.slice(0, 12)}) → PR para ${proposal.base}`),
    el('ul', { class: 'plain' }, proposal.targets.map((target) => {
      const [text, tone] = stateText[target.state] || [target.state, 'neutral']
      const pull = typeof target.open_pull_request === 'string' && target.open_pull_request.startsWith('https://github.com/')
      return el('li', {}, el('strong', {}, `${target.label} `), el('span', { class: `chip tone-${tone}` }, text), ' ', target.repository,
        pull ? [' · já há uma PR aberta: ', el('a', { href: target.open_pull_request, target: '_blank', rel: 'noopener noreferrer' }, target.open_pull_request)] : null)
    }))
  ]
  const writable = proposal.targets.some((t) => t.state === 'new' || t.state === 'already_there')
  if (!writable) {
    nodes.push(el('div', { class: 'callout danger' }, 'Nada pode ser criado em nenhuma cópia.'))
    return el('div', {}, nodes)
  }
  nodes.push(el('p', { class: 'muted' }, 'Nada é mesclado, sobrescrito, apagado ou enviado com force. Na PR, use “merge commit” (não squash nem rebase).'))
  const input = el('input', { type: 'text', id: 'p-confirm', autocomplete: 'off', spellcheck: 'false' })
  const submit = el('button', { type: 'button', id: 'p-apply', disabled: true }, 'Criar branches e PRs')
  input.addEventListener('input', () => { submit.disabled = input.value !== proposal.phrase })
  submit.addEventListener('click', () => applyPropagation(proposal, input.value, submit))
  nodes.push(el('label', { for: 'p-confirm' }, 'Para confirmar, digite exatamente:'), el('pre', {}, proposal.phrase), input, el('p', {}, submit), el('div', { id: 'p-done', role: 'status' }))
  return el('div', {}, nodes)
}

async function applyPropagation(proposal, confirmation, button) {
  const done = $('p-done')
  button.disabled = true
  done.replaceChildren(el('p', { class: 'muted' }, 'Criando… não feche esta janela.'))
  try {
    const answer = await call('/api/propagate/apply', {
      method: 'POST',
      body: {
        service: proposal.service, branch: proposal.branch, source: proposal.source, base: proposal.base,
        targets: proposal.targets.map((t) => t.label), confirmation, source_sha: proposal.source_sha
      }
    })
    done.replaceChildren(el('div', { class: 'callout ok' }, el('h3', {}, 'Resultado'), el('ul', { class: 'plain' }, answer.results.map((r) => {
      const safe = typeof r.pull_request_url === 'string' && r.pull_request_url.startsWith('https://github.com/')
      return el('li', {}, el('strong', {}, `${r.label}: `), r.status,
        r.pull_request_url ? [' · ', r.pull_request_created ? 'PR aberta: ' : 'PR existente: ', safe ? el('a', { href: r.pull_request_url, target: '_blank', rel: 'noopener noreferrer' }, r.pull_request_url) : r.pull_request_url] : null,
        r.detail ? ` — ${r.detail}` : null)
    }))))
  } catch (error) {
    done.replaceChildren(el('div', { class: 'callout danger', role: 'alert' }, error.message))
    button.disabled = false
  }
}

async function start() {
  try {
    state.manifest = await call('/api/manifest')
  } catch (error) {
    say(error.message, true)
    return
  }
  $('orgs').textContent = state.manifest.organizations.map((o) => `${o.label} = ${o.owner}`).join(' · ')
  $('refresh').addEventListener('click', () => load('status'))
  $('analyze-all').addEventListener('click', () => {
    if (confirm('Analisar o histórico de todos os pares baixa os repositórios e pode levar vários minutos. Continuar?')) load('plan')
  })
  render()
  await setupPropagation()
  await load('status')
}

start()
