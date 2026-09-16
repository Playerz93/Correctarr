/* Correctarr GUI – plain JS, no build step. */
(() => {
  const $ = (s, r = document) => r.querySelector(s);
  const $$ = (s, r = document) => [...r.querySelectorAll(s)];

  const api = async (method, url, body) => {
    const opt = { method, headers: {} };
    if (body !== undefined) { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
    const res = await fetch(url, opt);
    let data = null;
    try { data = await res.json(); } catch { /* empty */ }
    if (!res.ok) throw new Error((data && data.error) || `${res.status} ${res.statusText}`);
    return data;
  };

  const banner = (msg, kind = 'info', ms = 4000) => {
    const b = $('#banner');
    b.textContent = msg; b.className = `banner ${kind}`;
    clearTimeout(banner.t);
    banner.t = setTimeout(() => b.classList.add('hidden'), ms);
  };

  const fmtTime = (s) => s ? new Date(s).toLocaleString() : '–';
  const fmtDur = (a, b) => {
    if (!a || !b) return '–';
    const sec = Math.round((new Date(b) - new Date(a)) / 1000);
    if (sec < 60) return `${sec}s`;
    if (sec < 3600) return `${Math.floor(sec / 60)}m ${sec % 60}s`;
    return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m`;
  };
  const rel = (s) => {
    if (!s) return '';
    const d = (new Date(s) - Date.now()) / 1000;
    const abs = Math.abs(d);
    const unit = abs < 3600 ? [60, 'min'] : abs < 86400 ? [3600, 'h'] : [86400, 'd'];
    const n = Math.round(abs / unit[0]);
    return d > 0 ? `in ${n} ${unit[1]}` : `${n} ${unit[1]} ago`;
  };
  const issueLabel = {
    missing_symlink: 'missing file', broken_target: 'dead symlink', empty_file: 'empty file',
    integrity_failed: 'integrity failed', not_in_plex: 'not in Plex',
  };
  const statusLabel = { broken: 'broken', fixing: 'need to fix', fixed: 'fixed' };

  // ---------------------------------------------------------------- tabs
  $$('.tab').forEach(t => t.addEventListener('click', () => {
    $$('.tab').forEach(x => x.classList.toggle('active', x === t));
    $$('.tab-pane').forEach(p => p.classList.toggle('hidden', p.id !== `tab-${t.dataset.tab}`));
    if (t.dataset.tab === 'runs') loadRuns();
    if (t.dataset.tab === 'settings') loadSettings();
  }));

  // -------------------------------------------------------------- status
  let status = null;
  let wasRunning = false;
  async function refreshStatus() {
    try { status = await api('GET', '/api/status'); } catch (e) { banner(`Status: ${e.message}`, 'err'); return; }
    $('#version').textContent = status.version ? `v${status.version}` : '';
    $('#t-broken').textContent = status.tally.broken;
    $('#t-needfix').textContent = status.tally.need_fix;
    $('#t-fixed').textContent = status.tally.fixed;

    const running = status.sweep.running;
    $('#btn-sweep').disabled = running;
    $('#btn-dry').disabled = running;
    $('#btn-cancel').classList.toggle('hidden', !running);
    $('#progress').classList.toggle('hidden', !running);
    const fixall = $('#btn-fixall');
    fixall.disabled = running || !status.fix_all_armed || status.tally.broken === 0;
    fixall.title = status.fix_all_armed ? 'Apply the automatic fix to every broken finding' : 'Arm the Fix all button in Settings first';
    fixall.textContent = `Fix all${status.tally.broken ? ` (${status.tally.broken})` : ''}`;
    $('#btn-fixsel').disabled = running || selected.size === 0;
    $('#btn-fixsel').textContent = `Fix selected${selected.size ? ` (${selected.size})` : ''}`;
    if (running) {
      const p = status.sweep.progress;
      const pct = p.total ? Math.round(100 * p.done / p.total) : 0;
      $('#progress-bar').style.width = `${p.total ? pct : 3}%`;
      $('#progress-text').textContent = `${p.dry_run ? '[dry run] ' : ''}${p.phase}${p.total ? ` ${p.done}/${p.total} (${pct}%)` : ''}${p.current ? ' – ' + p.current : ''}`;
    }
    const parts = [];
    if (status.arr_count === 0) parts.push('No enabled Sonarr/Radarr instances – add them in Settings.');
    if (!status.plex_configured) parts.push('Plex not configured – Plex checks are skipped.');
    if (status.auto_fix) parts.push(`Auto-fix after scheduled sweeps is ON${status.auto_fix_dry_run ? ' (dry-run mode)' : ''}.`);
    if (status.last_run) parts.push(`Last run: ${fmtTime(status.last_run.finished_at)} (${status.last_run.status}, ${status.last_run.files_checked} files).`);
    if (status.sweep.next_run_at) parts.push(`Next scheduled: ${fmtTime(status.sweep.next_run_at)} (${rel(status.sweep.next_run_at)}).`);
    $('#runinfo').textContent = parts.join(' ');
    if (wasRunning && !running) {
      loadFindings();
      if (!$('#tab-runs').classList.contains('hidden')) loadRuns();
      showSummary(status.last_run);
    }
    wasRunning = running;
  }

  function showSummary(run) {
    const el = $('#summary');
    if (!run) { el.classList.add('hidden'); return; }
    const t = status.tally;
    let text = `Run #${run.id} ${run.status} – ${run.files_checked} files checked, ${run.new_broken} newly broken, ${run.newly_fixed} newly fixed. `;
    if (run.dry_run) text += 'Dry run: fixes were only described, see the run log. ';
    if (t.broken > 0) text += `${t.broken} broken item${t.broken === 1 ? '' : 's'} waiting – tick rows and press Fix selected, use the per-row buttons, or Fix all.`;
    else if (t.need_fix > 0) text += `${t.need_fix} item${t.need_fix === 1 ? '' : 's'} waiting for a replacement to arrive.`;
    else text += 'Nothing broken.';
    el.textContent = text;
    el.className = `summary ${t.broken > 0 ? 'has-broken' : 'clean'}`;
    $('#f-status').value = t.broken > 0 ? 'broken' : (t.need_fix > 0 ? 'fixing' : '');
    loadFindings();
  }

  // ------------------------------------------------------------ findings
  let findings = [];
  const selected = new Set();
  async function loadFindings() {
    const st = $('#f-status').value, inst = $('#f-instance').value;
    try { findings = await api('GET', `/api/findings?status=${st}&instance=${inst}`); } catch (e) { banner(e.message, 'err'); return; }
    renderFindings();
  }
  function renderFindings() {
    const q = $('#f-search').value.trim().toLowerCase();
    const rows = findings.filter(f => !q || f.title.toLowerCase().includes(q) || f.path.toLowerCase().includes(q));
    const tb = $('#findings tbody'); tb.innerHTML = '';
    $('#f-count').textContent = `${rows.length} shown`;
    $('#findings-empty').classList.toggle('hidden', rows.length > 0);
    const visible = new Set(rows.map(f => f.id));
    for (const id of [...selected]) if (!visible.has(id)) selected.delete(id);
    $('#chk-all').checked = rows.length > 0 && rows.every(f => f.status === 'fixed' || selected.has(f.id)) && selected.size > 0;
    for (const f of rows) {
      const tr = document.createElement('tr');
      const canPlex = status && status.plex_configured;
      tr.classList.toggle('selected', selected.has(f.id));
      tr.innerHTML = `
        <td class="chk">${f.status === 'fixed' ? '' : `<input type="checkbox" ${selected.has(f.id) ? 'checked' : ''}>`}</td>
        <td><span class="pill ${f.status}">${statusLabel[f.status] || f.status}</span></td>
        <td><span class="pill ${f.instance_type}">${f.instance_name}</span></td>
        <td class="title">${esc(f.title)}<div class="muted" style="font-size:11px">first seen ${fmtTime(f.first_seen)}</div></td>
        <td><span class="pill issue">${issueLabel[f.issue] || f.issue}</span></td>
        <td class="path">${esc(f.path)}${f.detail ? `<div>${esc(f.detail)}</div>` : ''}</td>
        <td class="last muted" style="font-size:12px">${esc(f.last_action || '')}${f.fix_requested_at ? `<div>fix sent ${fmtTime(f.fix_requested_at)}</div>` : ''}${f.fixed_at ? `<div>fixed ${fmtTime(f.fixed_at)}</div>` : ''}</td>
        <td class="actions"></td>`;
      const cb = $('td.chk input', tr);
      if (cb) cb.addEventListener('change', () => { cb.checked ? selected.add(f.id) : selected.delete(f.id); tr.classList.toggle('selected', cb.checked); updateSelectionUI(); });
      const act = $('.actions', tr);
      if (f.status !== 'fixed') {
        act.append(btn('Re-search', () => fix(f, 'research'), 'small', 'Delete the file record in the arr and search for a replacement'));
        if (canPlex) act.append(btn('Plex scan', () => fix(f, 'plexscan'), 'small', 'Ask Plex to scan this folder'));
      }
      act.append(btn('Re-check', () => recheck(f), 'small ghost', 'Verify this item again right now'));
      act.append(btn('✕', () => del(f), 'small ghost', 'Forget this finding'));
      tb.append(tr);
    }
    updateSelectionUI();
  }
  function updateSelectionUI() {
    const b = $('#btn-fixsel');
    b.disabled = selected.size === 0 || !!(status && status.sweep.running);
    b.textContent = `Fix selected${selected.size ? ` (${selected.size})` : ''}`;
  }
  $('#chk-all').addEventListener('change', (e) => {
    const q = $('#f-search').value.trim().toLowerCase();
    findings.filter(f => f.status !== 'fixed' && (!q || f.title.toLowerCase().includes(q) || f.path.toLowerCase().includes(q)))
      .forEach(f => e.target.checked ? selected.add(f.id) : selected.delete(f.id));
    renderFindings();
  });
  const esc = (s) => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
  const btn = (label, fn, cls = '', title = '') => { const b = document.createElement('button'); b.textContent = label; b.className = cls; b.title = title; b.addEventListener('click', fn); return b; };

  async function fix(f, action) {
    const q = action === 'research'
      ? `Delete the file record for "${f.title}" in ${f.instance_name} and tell it to search for a replacement?`
      : `Ask Plex to scan the folder of "${f.title}"?`;
    if (!confirm(q)) return;
    try {
      const r = await api('POST', `/api/findings/${f.id}/fix`, { action });
      banner(`Done: ${r.message}`, 'info', 6000);
      await Promise.all([loadFindings(), refreshStatus()]);
    } catch (e) { banner(e.message, 'err', 8000); }
  }
  async function recheck(f) {
    banner(`Re-checking "${f.title}"…`, 'info', 20000);
    try {
      const r = await api('POST', `/api/findings/${f.id}/recheck`);
      banner(`${f.title}: ${r.message}`, r.finding.status === 'fixed' ? 'info' : 'err', 8000);
      await Promise.all([loadFindings(), refreshStatus()]);
    } catch (e) { banner(e.message, 'err', 8000); }
  }
  async function del(f) {
    try { await api('DELETE', `/api/findings/${f.id}`); await Promise.all([loadFindings(), refreshStatus()]); } catch (e) { banner(e.message, 'err'); }
  }

  $('#f-status').addEventListener('change', loadFindings);
  $('#f-instance').addEventListener('change', loadFindings);
  $('#f-search').addEventListener('input', renderFindings);

  // ------------------------------------------------------------ controls
  $('#btn-sweep').addEventListener('click', () => startSweep('manual'));
  $('#btn-dry').addEventListener('click', () => startSweep('dry'));
  async function startSweep(mode) {
    try {
      await api('POST', '/api/sweep', { mode, depth: $('#sel-depth').value });
      banner(mode === 'dry' ? 'Dry run started – the fix plan goes into the run log, nothing is sent.' : 'Sweep started – findings appear when it finishes, then fix them from the table.');
      refreshStatus();
    } catch (e) { banner(e.message, 'err'); }
  }
  $('#btn-cancel').addEventListener('click', async () => { await api('POST', '/api/sweep/cancel'); refreshStatus(); });
  async function runFixAll(ids, label) {
    const b = ids ? $('#btn-fixsel') : $('#btn-fixall');
    const old = b.textContent; b.disabled = true; b.textContent = 'Fixing…';
    try {
      const r = await api('POST', '/api/findings/fix-all', ids ? { ids } : {});
      banner(`${label}: ${r.attempted} attempted, ${r.succeeded} ok, ${r.failed} failed.`, r.failed ? 'err' : 'info', 8000);
      if (r.lines && r.lines.length) console.log(r.lines.join('\n'));
      if (ids) selected.clear();
    } catch (e) { banner(e.message, 'err', 8000); }
    b.textContent = old;
    await Promise.all([loadFindings(), refreshStatus()]);
  }
  $('#btn-fixall').addEventListener('click', () => {
    const n = status.tally.broken;
    if (!confirm(`Apply the automatic fix to all ${n} broken findings?\n\nBroken files: the file record is deleted in Sonarr/Radarr and a search is triggered.\nNot in Plex: a Plex folder scan is triggered.`)) return;
    runFixAll(null, 'Fix all');
  });
  $('#btn-fixsel').addEventListener('click', () => {
    const ids = [...selected];
    if (!confirm(`Apply the automatic fix to the ${ids.length} selected finding${ids.length === 1 ? '' : 's'}?\n\nBroken files: the file record is deleted in Sonarr/Radarr and a search is triggered.\nNot in Plex: a Plex folder scan is triggered.`)) return;
    runFixAll(ids, 'Fix selected');
  });
  $('#btn-clearfixed').addEventListener('click', async () => {
    if (!confirm('Remove all findings in the Fixed state? The Fixed counter resets to 0.')) return;
    try { const r = await api('POST', '/api/findings/clear-fixed'); banner(`Cleared ${r.cleared} fixed findings.`); await Promise.all([loadFindings(), refreshStatus()]); } catch (e) { banner(e.message, 'err'); }
  });

  // ---------------------------------------------------------------- runs
  async function loadRuns() {
    let runs;
    try { runs = await api('GET', '/api/runs'); } catch (e) { banner(e.message, 'err'); return; }
    const tb = $('#runs tbody'); tb.innerHTML = '';
    for (const r of runs) {
      const tr = document.createElement('tr'); tr.className = 'clickable';
      tr.innerHTML = `<td>${r.id}</td><td>${fmtTime(r.started_at)}</td><td>${r.mode}${r.dry_run ? ' (dry)' : ''}</td><td>${r.depth}</td>
        <td><span class="pill ${r.status === 'done' ? 'fixed' : r.status === 'running' ? 'fixing' : 'broken'}">${r.status}</span></td>
        <td>${r.files_checked}</td><td>${r.new_broken}</td><td>${r.newly_fixed}</td>
        <td>${r.broken} / ${r.need_fix} / ${r.fixed}</td><td>${fmtDur(r.started_at, r.finished_at)}</td>`;
      tr.addEventListener('click', () => showRun(r.id));
      tb.append(tr);
    }
  }
  async function showRun(id) {
    try {
      const r = await api('GET', `/api/runs/${id}`);
      $('#runlog-title').textContent = `Run #${r.id} – ${r.mode}${r.dry_run ? ' (dry run)' : ''} – ${r.status}`;
      $('#runlog').textContent = r.log || '(no log)';
      $('#runlog-card').classList.remove('hidden');
      $('#runlog-card').scrollIntoView({ behavior: 'smooth' });
    } catch (e) { banner(e.message, 'err'); }
  }
  $('#runlog-close').addEventListener('click', () => $('#runlog-card').classList.add('hidden'));

  // ------------------------------------------------------------ settings
  async function loadSettings() {
    try {
      const [s, arrs] = await Promise.all([api('GET', '/api/settings'), api('GET', '/api/arrs')]);
      $('#plex-url').value = s.plex_url || '';
      $('#plex-token').value = s.plex_token || '';
      $('#s-interval').value = s.sweep_interval_hours;
      $('#s-depth').value = s.integrity_depth;
      $('#s-workers').value = s.integrity_workers;
      $('#s-ffprobe').value = s.ffprobe_timeout_seconds;
      $('#s-fixdelay').value = s.fix_delay_seconds;
      $('#s-autofix').checked = s.auto_fix;
      $('#s-autofix-dry').checked = s.auto_fix_dry_run;
      $('#s-fixall').checked = s.fix_all_armed;
      const list = $('#arr-list'); list.innerHTML = '';
      arrs.forEach(a => list.append(arrRow(a)));
      fillInstanceFilter(arrs);
    } catch (e) { banner(e.message, 'err'); }
  }
  function fillInstanceFilter(arrs) {
    const sel = $('#f-instance'); const cur = sel.value;
    sel.innerHTML = '<option value="">All instances</option>';
    arrs.forEach(a => { const o = document.createElement('option'); o.value = a.id; o.textContent = a.name; sel.append(o); });
    sel.value = cur;
  }
  function arrRow(a) {
    const el = $('#arr-row').content.firstElementChild.cloneNode(true);
    $('.a-name', el).value = a.name || '';
    $('.a-url', el).value = a.url || '';
    $('.a-key', el).value = a.api_key || '';
    $('.a-enabled', el).checked = a.enabled !== false;
    const typePill = $('.a-type', el);
    const setType = (t) => { typePill.textContent = t || 'unknown'; typePill.className = `pill a-type ${t || ''}`; };
    setType(a.type);
    const result = $('.a-result', el);
    const read = () => ({ id: a.id || 0, name: $('.a-name', el).value, url: $('.a-url', el).value, api_key: $('.a-key', el).value, type: a.type || '', enabled: $('.a-enabled', el).checked });
    $('.a-test', el).addEventListener('click', async () => {
      result.className = 'result'; result.textContent = 'Testing…';
      try {
        const r = await api('POST', '/api/arrs/test', { url: $('.a-url', el).value, api_key: $('.a-key', el).value });
        if (r.ok) { a.type = r.type; setType(r.type); result.className = 'result ok'; result.textContent = `OK – ${r.app} ${r.version}${r.instance ? ` (${r.instance})` : ''}`; }
        else { result.className = 'result err'; result.textContent = `Failed – ${r.error}`; }
      } catch (e) { result.className = 'result err'; result.textContent = e.message; }
    });
    $('.a-save', el).addEventListener('click', async () => {
      try {
        const saved = await api('POST', '/api/arrs', read());
        a = saved; result.className = 'result ok'; result.textContent = 'Saved.';
        const arrs = await api('GET', '/api/arrs'); fillInstanceFilter(arrs); refreshStatus();
      } catch (e) { result.className = 'result err'; result.textContent = e.message; }
    });
    $('.a-delete', el).addEventListener('click', async () => {
      if (a.id && !confirm(`Remove "${a.name}" and all of its findings?`)) return;
      try { if (a.id) await api('DELETE', `/api/arrs/${a.id}`); el.remove(); const arrs = await api('GET', '/api/arrs'); fillInstanceFilter(arrs); refreshStatus(); }
      catch (e) { result.className = 'result err'; result.textContent = e.message; }
    });
    return el;
  }
  $('#arr-add').addEventListener('click', () => $('#arr-list').append(arrRow({ enabled: true })));

  $('#plex-test').addEventListener('click', async () => {
    const out = $('#plex-test-result'); out.className = 'result'; out.textContent = 'Testing…';
    try {
      const r = await api('POST', '/api/plex/test', { url: $('#plex-url').value, token: $('#plex-token').value });
      if (r.ok) {
        const secs = r.sections.map(s => `${s.title} [${s.type}] → ${(s.locations || []).join(', ')}`).join('\n');
        out.className = 'result ok'; out.textContent = `OK – ${r.name} (Plex ${r.version}, ${r.platform})\n${secs || 'No movie/show sections visible'}`;
      } else { out.className = 'result err'; out.textContent = `Failed – ${r.error}`; }
    } catch (e) { out.className = 'result err'; out.textContent = e.message; }
  });

  $('#settings-save').addEventListener('click', async () => {
    const out = $('#settings-result');
    const body = {
      plex_url: $('#plex-url').value, plex_token: $('#plex-token').value,
      sweep_interval_hours: +$('#s-interval').value, integrity_depth: $('#s-depth').value,
      integrity_workers: +$('#s-workers').value, ffprobe_timeout_seconds: +$('#s-ffprobe').value,
      fix_delay_seconds: +$('#s-fixdelay').value,
      auto_fix: $('#s-autofix').checked, auto_fix_dry_run: $('#s-autofix-dry').checked, fix_all_armed: $('#s-fixall').checked,
    };
    if (body.auto_fix && !body.auto_fix_dry_run && !confirm('Auto-fix with dry-run mode OFF will delete file records in Sonarr/Radarr and trigger searches after every scheduled sweep. Continue?')) return;
    try { await api('PUT', '/api/settings', body); out.className = 'result ok'; out.textContent = 'Saved.'; refreshStatus(); }
    catch (e) { out.className = 'result err'; out.textContent = e.message; }
  });

  // ---------------------------------------------------------------- boot
  (async () => {
    await refreshStatus();
    try { fillInstanceFilter(await api('GET', '/api/arrs')); } catch { /* ignore */ }
    await loadFindings();
    setInterval(refreshStatus, 2000);
    setInterval(() => { if (!(status && status.sweep.running)) loadFindings(); }, 30000);
  })();
})();
