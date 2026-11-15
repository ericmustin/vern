// Vern score page. Every value from the API is set with textContent or
// setAttribute, never parsed as HTML. Scores are shown with the API's display
// strings (score_display, overall_display), never re-rounded here.
(function () {
  'use strict';

  var SVG_NS = 'http://www.w3.org/2000/svg';
  var SPEC_RULE_URL = 'https://github.com/instrumentation-score/spec/blob/main/rules/';
  var RUNS_URL = '/api/v1/runs?markdown=1';
  var EXAMPLE_URL = '/examples/demo.jsonl.gz';
  var EXAMPLE_NAME = 'demo.jsonl.gz';
  var SCORING_TEXT = 'Scoring: loading, evaluating, deleting…';
  var LIMITS_URL = '/api/v1/limits';
  // Replaced from LIMITS_URL when the page loads; this text stays if that fails.
  var LIMITS_TEXT = 'Uploads are limited to 30 MB compressed, 200 MB decompressed and 500,000 items.';

  var IMPACTS = [
    { name: 'Critical', weight: 40 },
    { name: 'Important', weight: 30 },
    { name: 'Normal', weight: 20 },
    { name: 'Low', weight: 10 }
  ];

  // Spec bands, low to high. cls matches the CSS band, swatch and cat classes.
  var BANDS = [
    { name: 'Poor', from: 0, to: 50, cls: 'poor' },
    { name: 'Needs Improvement', from: 50, to: 75, cls: 'needs' },
    { name: 'Good', from: 75, to: 90, cls: 'good' },
    { name: 'Excellent', from: 90, to: 100, cls: 'excellent' }
  ];

  var STATUS = {
    evaluated: { label: 'Evaluated', cls: 'badge-info' },
    not_applicable: { label: 'Not applicable', cls: '' },
    not_assessable: { label: 'Not assessable', cls: 'badge-warn' },
    not_implemented: { label: 'Not implemented', cls: '' },
    error: { label: 'Error', cls: 'badge-error' }
  };

  var numberFmt = new Intl.NumberFormat('en-US');
  var extentFmt = new Intl.NumberFormat('en-US', { maximumFractionDigits: 3 });

  var state = { report: null, service: '', busy: false };

  function $(id) {
    return document.getElementById(id);
  }

  // el builds an element. props: className, text, and attrs (via setAttribute).
  function el(tag, props, children) {
    var node = document.createElement(tag);
    props = props || {};
    if (props.className) node.className = props.className;
    if (props.text !== undefined && props.text !== null) node.textContent = String(props.text);
    if (props.attrs) {
      Object.keys(props.attrs).forEach(function (k) {
        node.setAttribute(k, props.attrs[k]);
      });
    }
    (children || []).forEach(function (c) {
      if (c) node.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
    });
    return node;
  }

  function svg(tag, attrs) {
    var node = document.createElementNS(SVG_NS, tag);
    Object.keys(attrs || {}).forEach(function (k) {
      node.setAttribute(k, attrs[k]);
    });
    return node;
  }

  function statusInfo(status) {
    return STATUS[status] || { label: String(status || 'unknown'), cls: '' };
  }

  function badge(text, cls) {
    return el('span', { className: 'badge' + (cls ? ' ' + cls : ''), text: text });
  }

  function bandFor(category) {
    for (var i = 0; i < BANDS.length; i++) {
      if (BANDS[i].name === category) return BANDS[i];
    }
    return null;
  }

  // ---- Status and errors ----

  function setStatus(text, busy) {
    var s = $('status');
    s.textContent = text || '';
    s.classList.toggle('is-busy', !!busy);
  }

  function showError(title, detail) {
    var box = $('error');
    box.replaceChildren(el('p', {}, [el('strong', { text: title })]));
    if (detail) box.appendChild(el('p', { text: detail }));
    box.hidden = false;
  }

  function clearError() {
    $('error').hidden = true;
    $('error').replaceChildren();
  }

  function setBusy(busy) {
    state.busy = busy;
    $('pick').disabled = busy;
    $('example').disabled = busy;
    $('dropzone').classList.toggle('is-busy', busy);
    $('dropzone').setAttribute('aria-busy', busy ? 'true' : 'false');
  }

  async function serverMessage(res) {
    try {
      var body = await res.json();
      if (body && typeof body.error === 'string') return body.error;
    } catch (e) {
      // Not JSON: fall through.
    }
    return '';
  }

  function retryText(res) {
    var n = parseInt(res.headers.get('Retry-After') || '', 10);
    if (isFinite(n) && n >= 0) return 'The scorer is busy, try again in ' + n + ' s.';
    return 'The scorer is busy, try again shortly.';
  }

  async function reportError(res) {
    var msg = await serverMessage(res);
    switch (res.status) {
      case 413:
        showError('That upload is too large.', (msg ? msg + '. ' : '') + LIMITS_TEXT);
        break;
      case 429:
      case 503:
        showError(retryText(res), msg);
        break;
      case 504:
        showError('Scoring timed out.', 'A run stops after 120 seconds. Try a smaller sample.');
        break;
      case 400:
        showError('The upload could not be scored.', msg || 'The server rejected the upload.');
        break;
      default:
        showError('Scoring failed (HTTP ' + res.status + ').', msg ? msg + '. Try again.' : 'Try again.');
    }
  }

  // ---- Upload ----

  // items: [{ blob, name }]
  async function score(items) {
    if (state.busy || items.length === 0) return;
    var form = new FormData();
    items.forEach(function (it) {
      form.append('files', it.blob, it.name);
    });
    clearError();
    // A new run replaces the old report, so a failure never sits beside it.
    $('results').hidden = true;
    state.report = null;
    setBusy(true);
    setStatus(SCORING_TEXT, true);
    try {
      var res;
      try {
        res = await fetch(RUNS_URL, { method: 'POST', body: form });
      } catch (e) {
        showError('Could not reach the server.', 'Check your connection and try again.');
        setStatus('');
        return;
      }
      if (!res.ok) {
        await reportError(res);
        setStatus('');
        return;
      }
      var report;
      try {
        report = await res.json();
      } catch (e) {
        showError('The server sent a response that is not a report.', 'Try again.');
        setStatus('');
        return;
      }
      setStatus('Scored ' + items.length + (items.length === 1 ? ' file.' : ' files.'));
      showReport(report);
    } finally {
      setBusy(false);
    }
  }

  function scoreFiles(fileList) {
    var items = [];
    for (var i = 0; i < fileList.length; i++) {
      items.push({ blob: fileList[i], name: fileList[i].name });
    }
    return score(items);
  }

  async function scoreExample() {
    if (state.busy) return;
    clearError();
    setBusy(true);
    setStatus('Fetching the example data…', true);
    var blob;
    try {
      var res = await fetch(EXAMPLE_URL);
      if (!res.ok) throw new Error('HTTP ' + res.status);
      blob = await res.blob();
    } catch (e) {
      showError('Could not load the example data.', 'Try again.');
      setStatus('');
      setBusy(false);
      return;
    }
    setBusy(false);
    await score([{ blob: blob, name: EXAMPLE_NAME }]);
  }

  function hasFiles(ev) {
    var types = ev.dataTransfer && ev.dataTransfer.types;
    return !!types && Array.prototype.indexOf.call(types, 'Files') !== -1;
  }

  function wireUpload() {
    var zone = $('dropzone');
    var input = $('file-input');
    var depth = 0;

    $('pick').addEventListener('click', function () {
      input.click();
    });
    input.addEventListener('change', function () {
      var files = input.files;
      if (files && files.length) scoreFiles(files);
      input.value = '';
    });
    $('example').addEventListener('click', scoreExample);

    zone.addEventListener('dragenter', function (ev) {
      if (!hasFiles(ev)) return;
      ev.preventDefault();
      depth++;
      zone.classList.add('is-over');
    });
    zone.addEventListener('dragover', function (ev) {
      if (!hasFiles(ev)) return;
      ev.preventDefault();
      ev.dataTransfer.dropEffect = state.busy ? 'none' : 'copy';
    });
    zone.addEventListener('dragleave', function () {
      depth = Math.max(0, depth - 1);
      if (depth === 0) zone.classList.remove('is-over');
    });
    zone.addEventListener('drop', function (ev) {
      ev.preventDefault();
      depth = 0;
      zone.classList.remove('is-over');
      var files = ev.dataTransfer && ev.dataTransfer.files;
      if (files && files.length) scoreFiles(files);
    });

    // A file dropped outside the zone must not make the browser navigate away.
    ['dragover', 'drop'].forEach(function (type) {
      window.addEventListener(type, function (ev) {
        if (hasFiles(ev) && !zone.contains(ev.target)) {
          ev.preventDefault();
          if (ev.dataTransfer) ev.dataTransfer.dropEffect = 'none';
        }
      });
    });
  }

  // ---- Report ----

  function showReport(report) {
    state.report = report;
    state.service = '';
    renderMeta(report);
    renderProblems(report);
    renderServiceSelect(report);
    renderScore();
    renderDisclosure(report);
    renderSample(report);
    renderRules();
    $('results').hidden = false;
    $('results-title').focus();
  }

  function renderMeta(report) {
    var parts = [];
    if (report.spec_version) parts.push('Spec ' + report.spec_version);
    if (report.evaluated_at) {
      var d = new Date(report.evaluated_at);
      if (!isNaN(d.getTime())) parts.push('Scored ' + d.toLocaleString());
    }
    $('run-meta').textContent = parts.join(' · ');
  }

  function renderProblems(report) {
    var list = $('problems-list');
    var problems = Array.isArray(report.problems) ? report.problems : [];
    list.replaceChildren();
    problems.forEach(function (p) {
      list.appendChild(el('li', { text: p }));
    });
    $('problems').hidden = problems.length === 0;
  }

  function services(report) {
    return (report.score && Array.isArray(report.score.services)) ? report.score.services : [];
  }

  function selectedService() {
    if (!state.service) return null;
    var list = services(state.report);
    for (var i = 0; i < list.length; i++) {
      if (list[i].service_name === state.service) return list[i];
    }
    return null;
  }

  function renderServiceSelect(report) {
    var sel = $('service');
    sel.replaceChildren(el('option', { text: 'Overall (all services)', attrs: { value: '' } }));
    services(report).forEach(function (s) {
      sel.appendChild(el('option', { text: s.service_name, attrs: { value: s.service_name } }));
    });
    sel.value = '';
    sel.disabled = services(report).length === 0;
  }

  // The score being shown: overall, or the selected service's.
  function currentScore() {
    var sc = state.report.score || {};
    var svc = selectedService();
    if (svc) {
      return { scored: true, display: svc.score_display, category: svc.category, counts: svc.counts };
    }
    return { scored: !!sc.scored, display: sc.overall_display, category: sc.overall_category, counts: sc.counts };
  }

  function renderScore() {
    var cur = currentScore();
    var band = cur.scored ? bandFor(cur.category) : null;
    var value = cur.scored ? parseFloat(cur.display) : NaN;

    $('gauge').replaceChildren(gauge(value, band, cur));

    var v = $('score-value');
    var c = $('score-category');
    if (cur.scored) {
      v.replaceChildren(document.createTextNode(String(cur.display)), el('small', { text: ' / 100' }));
      c.textContent = cur.category || '';
      c.className = 'score-category' + (band ? ' cat-' + band.cls : '');
    } else {
      v.textContent = '—';
      c.textContent = 'Not scored: no rule could be evaluated on this sample';
      c.className = 'score-category';
    }
    $('tally').textContent = tallyText(cur.counts);
  }

  function tallyText(counts) {
    if (!counts) return '';
    var parts = [];
    IMPACTS.forEach(function (imp) {
      var t = counts[imp.name];
      if (t && t.total) parts.push(imp.name + ' ' + t.passed + '/' + t.total);
    });
    return parts.length ? 'Checks passed: ' + parts.join(', ') : '';
  }

  // Gauge geometry: a half circle from 0 (left) to 100 (right).
  var G = { cx: 100, cy: 100, r: 80 };

  // coord rounds an SVG coordinate to 3 decimals (geometry only; never a score).
  function coord(n) {
    return String(Math.round(n * 1000) / 1000);
  }

  function point(v, r) {
    var a = Math.PI * (1 - Math.min(100, Math.max(0, v)) / 100);
    return { x: G.cx + r * Math.cos(a), y: G.cy - r * Math.sin(a) };
  }

  function arcPath(from, to) {
    var a = point(from, G.r);
    var b = point(to, G.r);
    return 'M ' + coord(a.x) + ' ' + coord(a.y) + ' A ' + G.r + ' ' + G.r + ' 0 0 1 ' + coord(b.x) + ' ' + coord(b.y);
  }

  function gauge(value, activeBand, cur) {
    var root = svg('svg', {
      viewBox: '0 0 200 118',
      class: 'gauge',
      role: 'img',
      'aria-label': cur.scored
        ? 'Score ' + cur.display + ' out of 100, ' + (cur.category || '')
        : 'Not scored'
    });
    var gap = 0.8;
    BANDS.forEach(function (b, i) {
      var from = b.from + (i === 0 ? 0 : gap / 2);
      var to = b.to - (i === BANDS.length - 1 ? 0 : gap / 2);
      var cls = 'gauge-band gauge-band-' + b.cls + (activeBand === b ? ' is-active' : '');
      root.appendChild(svg('path', { d: arcPath(from, to), class: cls }));
    });
    [0, 50, 75, 90, 100].forEach(function (t) {
      // The end labels sit below the arc ends so the needle (horizontal at
      // 0 and 100) never crosses them; the others sit inside the arc.
      var p = (t === 0 || t === 100) ? { x: point(t, G.r).x, y: G.cy + 10 } : point(t, G.r - 18);
      var label = svg('text', { x: coord(p.x), y: coord(p.y + (t === 0 || t === 100 ? 4 : 3)), class: 'gauge-tick', 'text-anchor': 'middle' });
      label.textContent = String(t);
      root.appendChild(label);
    });
    if (!isNaN(value)) {
      var tip = point(value, G.r - 4);
      root.appendChild(svg('line', {
        x1: G.cx, y1: G.cy, x2: coord(tip.x), y2: coord(tip.y), class: 'gauge-needle'
      }));
      root.appendChild(svg('circle', { cx: G.cx, cy: G.cy, r: 5, class: 'gauge-hub' }));
    }
    return root;
  }

  // passBlindSpots returns the sample's blind spots when a sensitive-data
  // rule passed for some service: that pass says nothing about what the
  // rule could not scan. Otherwise an empty list.
  function passBlindSpots(report) {
    var spots = report.sample && Array.isArray(report.sample.blind_spots) ? report.sample.blind_spots : [];
    var rules = Array.isArray(report.rules) ? report.rules : [];
    var passed = rules.some(function (r) {
      var outs = Array.isArray(r.outcomes) ? r.outcomes : [];
      return r.sensitive && r.status === 'evaluated' && outs.some(function (o) { return o.rule_passed; });
    });
    return passed ? spots : [];
  }

  function blindSpotsItem(spots) {
    return el('li', { className: 'disclosure-blind-spots' }, [
      el('strong', { text: 'Sensitive-data passes have blind spots. These parts of the sample could not be scanned:' }),
      list(spots)
    ]);
  }

  function renderDisclosure(report) {
    var rules = Array.isArray(report.rules) ? report.rules : [];
    var scored = rules.filter(function (r) { return r.scored; }).length;
    var unscored = rules.filter(function (r) { return !r.scored; });
    var spots = passBlindSpots(report);
    var box = $('disclosure');
    var list = $('disclosure-list');
    list.replaceChildren();

    if (unscored.length === 0) {
      box.classList.add('disclosure-full');
      box.classList.remove('banner-warn');
      $('disclosure-title').textContent = 'Full score: all ' + rules.length + ' rules scored';
      $('disclosure-text').textContent = 'Every rule in the spec was evaluated and counts toward this score.';
      if (spots.length) list.appendChild(blindSpotsItem(spots));
      return;
    }
    box.classList.remove('disclosure-full');
    box.classList.add('banner-warn');
    $('disclosure-title').textContent = 'Partial score: ' + scored + ' of ' + rules.length + ' rules scored';
    $('disclosure-text').textContent = 'Only scored rules count toward the score. These ' + unscored.length +
      (unscored.length === 1 ? ' rule was' : ' rules were') +
      ' left out, so compare this score only with scores over the same rules:';
    unscored.forEach(function (r) {
      list.appendChild(el('li', {}, [
        el('strong', { className: 'rule-id', text: r.rule_id }),
        badge(statusInfo(r.status).label, statusInfo(r.status).cls),
        r.advisory ? badge('Advisory', 'badge-info') : null,
        ' ',
        el('span', { text: unscoredReason(r) })
      ]));
    });
    if (spots.length) list.appendChild(blindSpotsItem(spots));
  }

  function unscoredReason(r) {
    if (r.reason) return r.reason;
    if (r.advisory) return 'Advisory: shown for information, not scored.';
    return '';
  }

  // ---- Sample ----

  function isRealTime(s) {
    if (!s) return false;
    var d = new Date(s);
    return !isNaN(d.getTime()) && d.getUTCFullYear() > 1;
  }

  function utc(s) {
    return new Date(s).toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC').replace(/Z$/, ' UTC');
  }

  function droppedText(dropped) {
    var parts = [];
    ['traces', 'logs', 'metrics'].forEach(function (sig) {
      var n = dropped[sig];
      if (!n || n <= 0) return;
      parts.push(sig === 'metrics'
        ? 'metrics: at least ' + numberFmt.format(n)
        : sig + ': ' + numberFmt.format(n));
    });
    return parts.join(', ');
  }

  function fact(dl, term, value) {
    dl.appendChild(el('dt', { text: term }));
    var dd = el('dd');
    if (value instanceof Node) dd.appendChild(value);
    else dd.textContent = String(value);
    dl.appendChild(dd);
  }

  function list(items) {
    var ul = el('ul');
    items.forEach(function (it) {
      ul.appendChild(el('li', { text: it }));
    });
    return ul;
  }

  function renderSample(report) {
    var dl = $('sample');
    dl.replaceChildren();
    var s = report.sample;
    if (!s) {
      fact(dl, 'Sample', 'The report has no sample details.');
      return;
    }
    var files = Array.isArray(s.files) ? s.files : [];
    var svcs = Array.isArray(s.services) ? s.services : [];
    var signals = Array.isArray(s.signals) ? s.signals : [];
    if (files.length) fact(dl, files.length === 1 ? 'File' : 'Files (' + files.length + ')', files.length === 1 ? files[0] : list(files));
    fact(dl, 'Services (' + svcs.length + ')', svcs.length ? list(svcs) : 'none found');
    fact(dl, 'Signals', signals.length ? signals.join(', ') : 'none found');
    fact(dl, 'Spans', el('span', { className: 'num', text: numberFmt.format(s.spans || 0) }));
    fact(dl, 'Span events', el('span', { className: 'num', text: numberFmt.format(s.span_events || 0) }));
    fact(dl, 'Log records', el('span', { className: 'num', text: numberFmt.format(s.log_records || 0) }));
    fact(dl, 'Metric data points', el('span', { className: 'num', text: numberFmt.format(s.data_points || 0) }));
    if (isRealTime(s.original_start) || isRealTime(s.original_end)) {
      var range = (isRealTime(s.original_start) ? utc(s.original_start) : '?') + ' to ' +
        (isRealTime(s.original_end) ? utc(s.original_end) : '?');
      fact(dl, 'Original time range', range);
    }
    var dropped = s.dropped ? droppedText(s.dropped) : '';
    if (dropped) fact(dl, 'Not indexed (left out of the score)', dropped);
    var notes = Array.isArray(s.notes) ? s.notes : [];
    if (notes.length) fact(dl, 'Notes', list(notes));
    var spots = Array.isArray(s.blind_spots) ? s.blind_spots : [];
    if (spots.length) fact(dl, 'Blind spots (not scanned for sensitive data)', list(spots));
  }

  // ---- Rules ----

  function outcomesFor(rule) {
    var all = Array.isArray(rule.outcomes) ? rule.outcomes : [];
    if (!state.service) return all;
    return all.filter(function (o) { return o.service_name === state.service; });
  }

  function renderRules() {
    var root = $('rules');
    root.replaceChildren();
    var rules = Array.isArray(state.report.rules) ? state.report.rules : [];
    var counts = currentScore().counts || {};
    $('rules-intro').textContent = state.service
      ? 'Results for ' + state.service + '. Each rule shows its status, the evidence found, why it matters and how to fix it.'
      : 'Results for all services. Each rule shows its status, the evidence found, why it matters and how to fix it.';

    IMPACTS.forEach(function (imp) {
      var group = rules.filter(function (r) { return r.impact === imp.name; });
      if (group.length === 0) return;
      group.sort(function (a, b) { return a.rule_id < b.rule_id ? -1 : a.rule_id > b.rule_id ? 1 : 0; });
      var t = counts[imp.name];
      var meta = 'weight ' + imp.weight + ' · ' + group.length + (group.length === 1 ? ' rule' : ' rules');
      if (t && t.total) meta += ' · ' + t.passed + ' of ' + t.total + ' checks passed';
      var ul = el('ul', { className: 'rule-list' });
      group.forEach(function (r) { ul.appendChild(ruleCard(r)); });
      root.appendChild(el('section', { className: 'impact-group', attrs: { 'aria-label': imp.name + ' rules' } }, [
        el('div', { className: 'impact-head' }, [
          el('h3', { text: imp.name }),
          el('span', { className: 'impact-meta', text: meta })
        ]),
        ul
      ]));
    });
    var other = rules.filter(function (r) {
      return !IMPACTS.some(function (imp) { return imp.name === r.impact; });
    });
    if (other.length) {
      var ul = el('ul', { className: 'rule-list' });
      other.forEach(function (r) { ul.appendChild(ruleCard(r)); });
      root.appendChild(el('section', { className: 'impact-group', attrs: { 'aria-label': 'Other rules' } }, [
        el('div', { className: 'impact-head' }, [el('h3', { text: 'Other' })]),
        ul
      ]));
    }
  }

  function ruleCard(r) {
    var info = statusInfo(r.status);
    var outs = r.status === 'evaluated' ? outcomesFor(r) : [];
    var failed = outs.filter(function (o) { return !o.rule_passed; }).length;
    var li = el('li', { className: 'rule' });
    if (r.status === 'evaluated' && outs.length) li.classList.add(failed ? 'is-fail' : 'is-pass');

    var head = el('div', { className: 'rule-head' }, [
      el('h4', { className: 'rule-id', text: r.rule_id }),
      badge(info.label, info.cls)
    ]);
    if (r.status === 'evaluated' && outs.length) {
      head.appendChild(failed
        ? badge(state.service ? 'Fail' : failed + ' of ' + outs.length + ' failing', 'badge-fail')
        : badge('Pass', 'badge-pass'));
    }
    if (r.target) head.appendChild(badge(r.target, ''));
    if (r.advisory) head.appendChild(badge('Advisory', 'badge-info'));
    if (r.heuristic) head.appendChild(badge('Heuristic', 'badge-warn'));
    li.appendChild(head);

    if (r.description) li.appendChild(el('p', { className: 'rule-desc', text: r.description }));

    if (r.status === 'evaluated') {
      if (outs.length) {
        var ul = el('ul', { className: 'outcomes', attrs: { 'aria-label': 'Evidence' } });
        outs.forEach(function (o) { ul.appendChild(outcomeItem(o)); });
        li.appendChild(ul);
      } else {
        li.appendChild(el('p', { className: 'reason', text: state.service ? 'No result for this service.' : 'No service produced a result for this rule.' }));
      }
    } else {
      li.appendChild(el('p', { className: 'reason' }, [
        el('span', { className: 'rule-label', text: 'Not scored: ' }),
        unscoredReason(r) || info.label + '.'
      ]));
    }

    if (r.advisory) {
      li.appendChild(el('p', { className: 'rule-note', text: 'Advisory: shown for information, not counted in the score.' }));
    }
    if (r.heuristic) {
      li.appendChild(el('p', { className: 'rule-note', text: 'Heuristic: this check approximates the spec and may misjudge some data.' }));
    }
    if (r.deviation) {
      li.appendChild(el('p', { className: 'rule-note' }, [
        el('span', { className: 'rule-label', text: 'Differs from the spec: ' }),
        r.deviation
      ]));
    }
    if (r.remediation) {
      li.appendChild(el('p', { className: 'rule-fix' }, [
        el('span', { className: 'rule-label', text: 'How to fix: ' }),
        r.remediation
      ]));
    }
    li.appendChild(el('p', { className: 'rule-links' }, [
      el('a', {
        text: 'Read ' + r.rule_id + ' in the spec',
        attrs: {
          href: SPEC_RULE_URL + encodeURIComponent(r.rule_id) + '.md',
          target: '_blank',
          rel: 'noopener noreferrer'
        }
      })
    ]));
    return li;
  }

  function outcomeItem(o) {
    var item = el('li', { className: 'outcome' });
    var head = el('div', { className: 'outcome-head' });
    if (!state.service) head.appendChild(el('span', { className: 'outcome-service', text: o.service_name }));
    head.appendChild(o.rule_passed ? badge('Pass', 'badge-pass') : badge('Fail', 'badge-fail'));
    if (typeof o.extent === 'number') {
      head.appendChild(el('span', { className: 'outcome-extent num', text: 'Extent ' + extentFmt.format(o.extent) }));
    }
    item.appendChild(head);
    if (o.example) item.appendChild(el('code', { className: 'evidence', text: o.example }));
    return item;
  }

  // ---- Downloads ----

  function download(name, type, text) {
    var url = URL.createObjectURL(new Blob([text], { type: type }));
    var a = el('a', { attrs: { href: url, download: name } });
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
  }

  function wireDownloads() {
    $('download-json').addEventListener('click', function () {
      if (!state.report) return;
      var copy = {};
      Object.keys(state.report).forEach(function (k) {
        if (k !== 'markdown') copy[k] = state.report[k];
      });
      download('vern-report.json', 'application/json', JSON.stringify(copy, null, 2) + '\n');
    });
    $('download-md').addEventListener('click', function () {
      if (!state.report) return;
      download('vern-report.md', 'text/markdown', String(state.report.markdown || ''));
    });
  }

  function megabytes(n) {
    return Math.round(n / (1 << 20)) + ' MB';
  }

  // Shows the limits the server enforces. A failure keeps the static text.
  async function loadLimits() {
    try {
      var res = await fetch(LIMITS_URL);
      if (!res.ok) return;
      var l = await res.json();
      var parts = [l.max_compressed_bytes, l.max_decompressed_bytes, l.max_items, l.run_timeout_seconds];
      if (!parts.every(function (n) { return typeof n === 'number' && isFinite(n) && n > 0; })) return;
      LIMITS_TEXT = 'Uploads are limited to ' + megabytes(l.max_compressed_bytes) + ' compressed, ' +
        megabytes(l.max_decompressed_bytes) + ' decompressed and ' + l.max_items.toLocaleString('en-US') + ' items.';
      $('limits').textContent = 'Limits: ' + megabytes(l.max_compressed_bytes) + ' compressed, ' +
        megabytes(l.max_decompressed_bytes) + ' decompressed, ' + l.max_items.toLocaleString('en-US') +
        ' items. A run stops after ' + l.run_timeout_seconds + ' seconds.';
    } catch (e) {
      // Keep the static text.
    }
  }

  function init() {
    loadLimits();
    wireUpload();
    wireDownloads();
    $('service').addEventListener('change', function (ev) {
      state.service = ev.target.value;
      renderScore();
      renderRules();
    });
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
