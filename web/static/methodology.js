// Vern methodology page: lists every spec rule from GET /api/v1/rules. The
// spec text is shown as plain text in a <pre>; nothing is parsed as HTML.
(function () {
  'use strict';

  var LINK_PREFIXES = [
    'https://github.com/instrumentation-score/spec/',
    'https://github.com/ericmustin/vern/'
  ];

  var IMPACTS = [
    { name: 'Critical', weight: 40 },
    { name: 'Important', weight: 30 },
    { name: 'Normal', weight: 20 },
    { name: 'Low', weight: 10 }
  ];

  var STATUS = {
    implemented: { label: 'Implemented', cls: 'badge-pass' },
    not_implemented: { label: 'Not implemented', cls: '' },
    error: { label: 'Error', cls: 'badge-error' }
  };

  function $(id) {
    return document.getElementById(id);
  }

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

  function badge(text, cls) {
    return el('span', { className: 'badge' + (cls ? ' ' + cls : ''), text: text });
  }

  // Only the spec and repository links the API is documented to return.
  function safeLink(url, text) {
    if (typeof url !== 'string') return null;
    var ok = LINK_PREFIXES.some(function (p) { return url.indexOf(p) === 0; });
    if (!ok) return null;
    return el('a', { text: text, attrs: { href: url, target: '_blank', rel: 'noopener noreferrer' } });
  }

  function note(label, text) {
    return el('p', { className: 'rule-note' }, [el('span', { className: 'rule-label', text: label }), text]);
  }

  function ruleCard(r) {
    var info = STATUS[r.status] || { label: String(r.status || 'unknown'), cls: '' };
    var li = el('li', { className: 'rule' });
    var head = el('div', { className: 'rule-head' }, [
      el('h4', { className: 'rule-id', text: r.id }),
      badge(info.label, info.cls)
    ]);
    if (r.target) head.appendChild(badge(r.target, ''));
    if (r.advisory) head.appendChild(badge('Advisory', 'badge-info'));
    if (r.heuristic) head.appendChild(badge('Heuristic', 'badge-warn'));
    li.appendChild(head);

    if (r.description) li.appendChild(el('p', { className: 'rule-desc', text: r.description }));
    if (r.reason) li.appendChild(note(r.status === 'error' ? 'Error: ' : 'Why not: ', r.reason));
    if (r.advisory) li.appendChild(note('Advisory: ', 'shown for information, not counted in the score.'));
    if (r.heuristic) li.appendChild(note('Heuristic: ', 'this check approximates the spec and may misjudge some data.'));
    if (r.deviation) li.appendChild(note('Differs from the spec: ', r.deviation));
    if (r.remediation) {
      li.appendChild(el('p', { className: 'rule-fix' }, [
        el('span', { className: 'rule-label', text: 'How to fix: ' }),
        r.remediation
      ]));
    }

    var links = el('p', { className: 'rule-links' }, [
      safeLink(r.spec_url, 'Rule in the spec'),
      // A not-implemented rule has no query to point at, even when mapped.
      r.status === 'not_implemented' ? null : safeLink(r.implementation_url, 'Vern’s query')
    ]);
    if (links.childNodes.length) li.appendChild(links);

    if (r.spec_markdown) {
      li.appendChild(el('details', {}, [
        el('summary', { text: 'Spec text' }),
        el('pre', { className: 'spec-text', text: r.spec_markdown })
      ]));
    }
    return li;
  }

  function render(data) {
    var rules = Array.isArray(data.rules) ? data.rules : [];
    var implemented = rules.filter(function (r) { return r.status === 'implemented'; }).length;
    var meta = rules.length + ' rules';
    if (data.spec_version) meta += ' from spec ' + data.spec_version;
    meta += '. Vern implements ' + implemented + ' of them; which of those are scored for an upload depends on the data in it.';
    $('rules-meta').textContent = meta;

    var root = $('rules');
    root.replaceChildren();
    IMPACTS.forEach(function (imp) {
      var group = rules.filter(function (r) { return r.impact === imp.name; });
      if (group.length === 0) return;
      var ul = el('ul', { className: 'rule-list' });
      group.forEach(function (r) { ul.appendChild(ruleCard(r)); });
      root.appendChild(el('section', { className: 'impact-group', attrs: { 'aria-label': imp.name + ' rules' } }, [
        el('div', { className: 'impact-head' }, [
          el('h3', { text: imp.name }),
          el('span', { className: 'impact-meta', text: 'weight ' + imp.weight + ' · ' + group.length + (group.length === 1 ? ' rule' : ' rules') })
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

  async function load() {
    try {
      var res = await fetch('/api/v1/rules');
      if (!res.ok) throw new Error('HTTP ' + res.status);
      render(await res.json());
    } catch (e) {
      $('rules-meta').textContent = '';
      var box = $('rules-error');
      box.replaceChildren(el('p', { text: 'Could not load the rules. Reload the page to try again.' }));
      box.hidden = false;
    }
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', load);
  else load();
})();
