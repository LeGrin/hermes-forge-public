/* constellation.js — SVG topology renderer for CON-001
 * Replaces the card-grid Constellation render with an SVG-based
 * static topology layer matching constellation-spec.md.
 *
 * Scope (CON-001):
 *   SVG canvas with viewBox 0 0 1200 800, min-height 600px
 *   Three anchor nodes: KITT (top), Hermes (middle), Forge (bottom)
 *   Fixed spine lines: KITT↔Hermes↔Forge (always-on)
 *   Forge-parent agents → radial circles around Forge
 *   Other parent_kind → user-sessions cluster to the right
 *
 * Out of scope (deferred):
 *   Activity pulse animation (CON-006)
 *   Sub-agent edges via parent_id (CON-004)
 *   Log popups (CON-003)
 */

(function(global) {
  'use strict';

  /* ── Colour tokens: read from CSS custom properties (CON-002) ──
   * Single source of truth is tokens.css :root block.
   * These helpers avoid hardcoded duplicates in JS.
   */
  var computedStyle = null;
  function getToken(name) {
    if (!computedStyle) computedStyle = getComputedStyle(document.documentElement);
    return computedStyle.getPropertyValue(name).trim();
  }
  function tok(name) { return getToken('--constellation-' + name); }

  /* Cached token values — populated on first render */
  var TOKENS = null;
  function ensureTokens() {
    if (TOKENS) return;
    TOKENS = {
      bg:        tok('bg'),
      dot:       tok('dot'),
      line:      tok('line'),
      lineSoft:  tok('line-soft'),
      text:      tok('text'),
      textMuted: tok('text-muted'),
      pulse:     tok('pulse'),
      pulseGlow: tok('pulse-glow'),
      kitt:      tok('kitt'),
      hermes:    tok('hermes'),
      forge:     tok('forge'),
      claude:    tok('claude'),
      opencode:  tok('opencode'),
      subagent:  tok('subagent'),
      lifeOs:    tok('life-os'),
      rookery:   tok('rookery'),
      border:    tok('border'),
      shadow:    tok('shadow'),
    };
  }

  /* ── Anchor positions for viewBox 0 0 1200 800 ──
   * KITT top-centre, Hermes middle, Forge bottom.
   * Scaled from spec's 1600×1000 canonical viewBox.
   */
  var SPINE = {
    kitt:    { x: 600, y: 130, r: 56, label: 'KITT',   sublabel: 'orchestrator \xb7 VPS' },
    hermes:  { x: 600, y: 390, r: 60, label: 'Hermes', sublabel: 'transport authority' },
    forge:   { x: 600, y: 640, r: 72, label: 'Forge',  sublabel: 'execution gateway \xb7 Mac' },
  };

  /* ── Executor tint by executor kind ── */
  var EXECUTOR_COLORS = null; /* initialized in ensureTokens() */
  function ensureExecutorColors() {
    ensureTokens();
    EXECUTOR_COLORS = {
      claude:   TOKENS.claude,
      opencode: TOKENS.opencode,
      kitt:     TOKENS.kitt,
      forge:    TOKENS.forge,
      unknown:  TOKENS.subagent,
      'life-os': TOKENS.lifeOs,
      rookery:  TOKENS.rookery,
    };
  }

  /* ── State ── */
  var agentsCache = [];
  // positionMap: agentID → {x, y} for agents placed on the canvas.
  // Used to draw parent→child edges in appendParentEdges.
  var positionMap = {};

  /* ── Public API ── */

  function renderConstellation(agents, projects) {
    // Delegate to ForceTopology if available (CON-010)
    if (typeof window.ForceTopology !== 'undefined' && ForceTopology.render) {
      ForceTopology.render(agents, projects);
      return;
    }
    // Fallback to radial layout
    agentsCache = Array.isArray(agents) ? agents : [];
    ensureTokens();
    ensureExecutorColors();
    buildSVG();
  }

  function destroyConstellation() {
    if (typeof ForceTopology !== 'undefined' && ForceTopology.destroy) {
      ForceTopology.destroy();
      return;
    }
    var root = document.getElementById('constellation');
    if (root) root.innerHTML = '';
  }

  /* ── SVG Building ── */

  function buildSVG() {
    var root = document.getElementById('constellation');
    if (!root) return;
    root.innerHTML = '';
    positionMap = {};

    var svg = createSVG();
    appendDefs(svg);
    appendBackground(svg);
    appendSpine(svg);
    appendAnchorNodes(svg);
    appendForgeAgents(svg);
    appendUserSessionCluster(svg);
    appendParentEdges(svg);
    root.appendChild(svg);

    if (!agentsCache.length) {
      var empty = document.createElement('div');
      empty.className = 'constellation-empty';
      empty.textContent = 'No agents reporting yet. Forge reports every 60 s — if you just restarted, give it a minute.';
      root.appendChild(empty);
    }
  }

  function createSVG() {
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 1200 800');
    svg.setAttribute('preserveAspectRatio', 'xMidYMid meet');
    svg.classList.add('constellation-svg');
    return svg;
  }

  /* ── SVG <defs>: dot pattern, filters, gradients ── */

  function appendDefs(svg) {
    var defs = svgEl('defs');

    /* Dot field pattern */
    var pattern = svgEl('pattern');
    pattern.setAttribute('id', 'dots');
    pattern.setAttribute('width', '24');
    pattern.setAttribute('height', '24');
    pattern.setAttribute('patternUnits', 'userSpaceOnUse');
    var dot = svgEl('circle');
    dot.setAttribute('cx', '4');
    dot.setAttribute('cy', '4');
    dot.setAttribute('r', '1.2');
    dot.setAttribute('fill', TOKENS.dot);
    dot.setAttribute('opacity', '0.45');
    pattern.appendChild(dot);
    defs.appendChild(pattern);

    /* Soft shadow for satellite nodes */
    var softShadow = svgEl('filter');
    softShadow.setAttribute('id', 'softShadow');
    softShadow.setAttribute('x', '-40%');
    softShadow.setAttribute('y', '-40%');
    softShadow.setAttribute('width', '180%');
    softShadow.setAttribute('height', '180%');
    appendFESoftShadow(softShadow, 14, 0.22);
    defs.appendChild(softShadow);

    /* Glow for main anchors */
    var nodeGlow = svgEl('filter');
    nodeGlow.setAttribute('id', 'nodeGlow');
    nodeGlow.setAttribute('x', '-60%');
    nodeGlow.setAttribute('y', '-60%');
    nodeGlow.setAttribute('width', '220%');
    nodeGlow.setAttribute('height', '220%');
    appendFESoftShadow(nodeGlow, 20, 0.30);
    defs.appendChild(nodeGlow);

    /* Radial ambient gradient */
    var radial = svgEl('radialGradient');
    radial.setAttribute('id', 'ambient');
    radial.setAttribute('cx', '50%');
    radial.setAttribute('cy', '35%');
    radial.setAttribute('r', '70%');
    var stop1 = svgEl('stop');
    stop1.setAttribute('offset', '0%');
    stop1.setAttribute('stop-color', 'white');
    stop1.setAttribute('stop-opacity', '0.9');
    var stop2 = svgEl('stop');
    stop2.setAttribute('offset', '100%');
    stop2.setAttribute('stop-color', TOKENS.bg);
    stop2.setAttribute('stop-opacity', '1');
    radial.appendChild(stop1);
    radial.appendChild(stop2);
    defs.appendChild(radial);

    svg.appendChild(defs);
  }

  function appendFESoftShadow(filter, stdDev, floodOpacity) {
    var fe = svgEl('feDropShadow');
    fe.setAttribute('dx', '0');
    fe.setAttribute('dy', String(stdDev));
    fe.setAttribute('stdDeviation', String(stdDev));
    fe.setAttribute('flood-color', TOKENS.shadow);
    fe.setAttribute('flood-opacity', String(floodOpacity));
    filter.appendChild(fe);
  }

  /* ── Background ── */

  function appendBackground(svg) {
    var bg = svgEl('rect');
    bg.setAttribute('width', '100%');
    bg.setAttribute('height', '100%');
    bg.setAttribute('fill', 'url(#ambient)');
    svg.appendChild(bg);

    var dots = svgEl('rect');
    dots.setAttribute('width', '100%');
    dots.setAttribute('height', '100%');
    dots.setAttribute('fill', 'url(#dots)');
    svg.appendChild(dots);
  }

  /* ── Spine: always-on KITT↔Hermes↔Forge lines ── */

  function appendSpine(svg) {
    var g = svgEl('g');
    g.classList.add('spine-group');

    /* KITT → Hermes */
    appendLine(g, SPINE.kitt.x, SPINE.kitt.y, SPINE.hermes.x, SPINE.hermes.y, 'kitt-hermes');
    /* Hermes → Forge */
    appendLine(g, SPINE.hermes.x, SPINE.hermes.y, SPINE.forge.x, SPINE.forge.y, 'hermes-forge');

    svg.appendChild(g);
  }

  function appendLine(g, x1, y1, x2, y2, edgeName) {
    var line = svgEl('line');
    line.setAttribute('x1', String(x1));
    line.setAttribute('y1', String(y1));
    line.setAttribute('x2', String(x2));
    line.setAttribute('y2', String(y2));
    line.classList.add('spine-line');
    if (edgeName) line.setAttribute('data-edge', edgeName);
    g.appendChild(line);
  }

  /* ── Anchor nodes ── */

  function appendAnchorNodes(svg) {
    appendAnchorNode(svg, SPINE.kitt,    'kitt',    'kitt');
    appendAnchorNode(svg, SPINE.hermes,  'hermes',  'hermes');
    appendForgeNode(svg);
  }

  function appendAnchorNode(svg, anchor, nodeType, executorHint) {
    var cx = anchor.x, cy = anchor.y, r = anchor.r;
    var tint = TOKENS[nodeType] || TOKENS.subagent;

    var g = svgEl('g');
    g.classList.add('node-anchor', 'anchor-' + nodeType);
    g.setAttribute('data-node', nodeType);

    /* White backing circle + soft glow */
    var shadow = svgEl('circle');
    shadow.setAttribute('cx', cx); shadow.setAttribute('cy', cy);
    shadow.setAttribute('r', r);
    shadow.setAttribute('fill', 'white');
    shadow.setAttribute('filter', 'url(#nodeGlow)');
    shadow.classList.add('node-circle');
    g.appendChild(shadow);

    /* Tint wash */
    var tintCircle = svgEl('circle');
    tintCircle.setAttribute('cx', cx); tintCircle.setAttribute('cy', cy);
    tintCircle.setAttribute('r', r - 7);
    tintCircle.setAttribute('fill', tint);
    tintCircle.setAttribute('opacity', '0.25');
    g.appendChild(tintCircle);

    /* Border ring */
    var border = svgEl('circle');
    border.setAttribute('cx', cx); border.setAttribute('cy', cy);
    border.setAttribute('r', r);
    border.setAttribute('fill', 'none');
    border.setAttribute('stroke', TOKENS.border);
    border.setAttribute('stroke-width', '1.5');
    g.appendChild(border);

    /* Icon */
    appendIcon(g, cx, cy, executorHint, true);

    /* Labels */
    appendLabel(g, cx, cy, r, anchor.label, anchor.sublabel);

    /* CON-003: Click handler for anchor nodes — opens detail with KITT logs */
    g.addEventListener('click', function() {
      if (typeof showAgentDetail === 'function') {
        showAgentDetail({
          id: 'anchor-' + nodeType,
          executor: nodeType,
          host: nodeType === 'kitt' ? 'vps-kitt' : nodeType,
          title: anchor.label,
          state: 'anchor',
          parent_kind: 'none',
          session_id: null,
        });
      }
    });

    svg.appendChild(g);
  }

  function appendForgeNode(svg) {
    var cx = SPINE.forge.x, cy = SPINE.forge.y, r = SPINE.forge.r;
    var tint = TOKENS.forge;

    /* Orbit guide ring */
    var orbit = svgEl('circle');
    orbit.setAttribute('cx', cx); orbit.setAttribute('cy', cy);
    orbit.setAttribute('r', '130');
    orbit.classList.add('executor-orbit');
    svg.appendChild(orbit);

    var g = svgEl('g');
    g.classList.add('node-anchor', 'anchor-forge');
    g.setAttribute('data-node', 'forge');

    var shadow = svgEl('circle');
    shadow.setAttribute('cx', cx); shadow.setAttribute('cy', cy);
    shadow.setAttribute('r', r);
    shadow.setAttribute('fill', 'white');
    shadow.setAttribute('filter', 'url(#nodeGlow)');
    shadow.classList.add('node-circle');
    g.appendChild(shadow);

    var tintCircle = svgEl('circle');
    tintCircle.setAttribute('cx', cx); tintCircle.setAttribute('cy', cy);
    tintCircle.setAttribute('r', r - 7);
    tintCircle.setAttribute('fill', tint);
    tintCircle.setAttribute('opacity', '0.25');
    g.appendChild(tintCircle);

    var border = svgEl('circle');
    border.setAttribute('cx', cx); border.setAttribute('cy', cy);
    border.setAttribute('r', r);
    border.setAttribute('fill', 'none');
    border.setAttribute('stroke', TOKENS.border);
    border.setAttribute('stroke-width', '1.5');
    g.appendChild(border);

    appendIcon(g, cx, cy, 'forge', true);
    appendLabel(g, cx, cy, r, SPINE.forge.label, SPINE.forge.sublabel);

    /* CON-003: Click handler for Forge anchor — opens detail panel */
    g.addEventListener('click', function() {
      if (typeof showAgentDetail === 'function') {
        showAgentDetail({
          id: 'anchor-forge',
          executor: 'forge',
          host: 'forge',
          title: SPINE.forge.label,
          state: 'anchor',
          parent_kind: 'none',
          session_id: null,
        });
      }
    });

    svg.appendChild(g);
  }

  function appendLabel(g, cx, cy, r, label, sublabel) {
    var labelEl = svgEl('text');
    labelEl.setAttribute('x', cx);
    labelEl.setAttribute('y', cy + r + 34);
    labelEl.setAttribute('text-anchor', 'middle');
    labelEl.setAttribute('font-family', 'Helvetica, Arial, sans-serif');
    labelEl.setAttribute('font-size', '22');
    labelEl.setAttribute('font-weight', '600');
    labelEl.setAttribute('fill', TOKENS.text);
    labelEl.classList.add('node-label');
    labelEl.textContent = label;
    g.appendChild(labelEl);

    var sublabelEl = svgEl('text');
    sublabelEl.setAttribute('x', cx);
    sublabelEl.setAttribute('y', cy + r + 54);
    sublabelEl.setAttribute('text-anchor', 'middle');
    sublabelEl.setAttribute('font-family', 'Helvetica, Arial, sans-serif');
    sublabelEl.setAttribute('font-size', '13');
    sublabelEl.setAttribute('font-weight', '500');
    sublabelEl.setAttribute('fill', TOKENS.textMuted);
    sublabelEl.setAttribute('letter-spacing', '0.3');
    sublabelEl.classList.add('node-sublabel');
    sublabelEl.textContent = sublabel;
    g.appendChild(sublabelEl);
  }

  /* ── Icon drawing helpers ── */

  /* Map executor name → icon symbol id (CON-002) */
  function getIconId(executor) {
    var iconMap = {
      'kitt':      'icon-kitt',
      'hermes':    'icon-hermes',
      'forge':     'icon-forge',
      'claude':    'icon-claude',
      'opencode':  'icon-opencode',
      'life-os':   'icon-life-os',
      'rookery':   'icon-rookery',
    };
    return iconMap[executor] || 'icon-unknown';
  }

  /* Use <use> with SVG symbol for tinting (CON-002) */
  function appendIcon(g, cx, cy, executor, isLarge) {
    var iconId = getIconId(executor);
    var size = isLarge ? 18 : 14;

    var useEl = svgEl('use');
    useEl.setAttribute('href', '#' + iconId);
    useEl.setAttribute('x', String(cx - size / 2));
    useEl.setAttribute('y', String(cy - size / 2));
    useEl.setAttribute('width', String(size));
    useEl.setAttribute('height', String(size));
    useEl.setAttribute('color', TOKENS.text);
    g.appendChild(useEl);
  }

  /* ── Forge-parent agents: radial cluster around Forge ── */

  function appendForgeAgents(svg) {
    var forgeAgents = agentsCache.filter(function(a) {
      return a.parent_kind === 'forge';
    });

    if (!forgeAgents.length) return;

    var cx = SPINE.forge.x;
    var cy = SPINE.forge.y;
    var orbitRadius = 190; /* radial distance from Forge centre */
    var count = forgeAgents.length;

    forgeAgents.forEach(function(agent, idx) {
      /* Distribute evenly around the circle */
      var angle = (2 * Math.PI * idx / count) - Math.PI / 2;
      var ax = cx + orbitRadius * Math.cos(angle);
      var ay = cy + orbitRadius * Math.sin(angle);

      /* Store position for parent edge drawing */
      positionMap[agent.id] = {x: ax, y: ay};

      /* Draw edge line Forge → agent */
      appendEdge(svg, cx, cy, ax, ay, 'forge-agent-edge', 'forge-' + agent.id);

      /* Draw agent node */
      appendAgentNode(svg, ax, ay, agent, 38);
    });
  }

  function appendEdge(svg, x1, y1, x2, y2, cssClass, edgeName) {
    var line = svgEl('line');
    line.setAttribute('x1', String(x1));
    line.setAttribute('y1', String(y1));
    line.setAttribute('x2', String(x2));
    line.setAttribute('y2', String(y2));
    line.classList.add(cssClass || 'spine-line');
    if (edgeName) line.setAttribute('data-edge', edgeName);
    svg.insertBefore(line, svg.querySelector('.anchor-forge') || svg.firstChild);
  }

  /* ── User-sessions cluster: all non-forge-parent agents ── */

  function appendUserSessionCluster(svg) {
    var userAgents = agentsCache.filter(function(a) {
      return a.parent_kind !== 'forge';
    });

    if (!userAgents.length) return;

    /* Cluster positioned to the right of the main spine */
    var clusterCX = 1000;
    var clusterCY = 380;
    var clusterRadius = 160;

    /* Cluster label */
    var labelEl = svgEl('text');
    labelEl.setAttribute('x', clusterCX);
    labelEl.setAttribute('y', clusterCY - clusterRadius - 24);
    labelEl.setAttribute('text-anchor', 'middle');
    labelEl.setAttribute('font-family', 'Helvetica, Arial, sans-serif');
    labelEl.setAttribute('font-size', '14');
    labelEl.setAttribute('font-weight', '600');
    labelEl.setAttribute('fill', TOKENS.textMuted);
    labelEl.setAttribute('letter-spacing', '0.5');
    labelEl.classList.add('cluster-label');
    labelEl.textContent = 'USER SESSIONS';
    svg.appendChild(labelEl);

    /* Dashed orbit guide */
    var orbit = svgEl('circle');
    orbit.setAttribute('cx', clusterCX);
    orbit.setAttribute('cy', clusterCY);
    orbit.setAttribute('r', clusterRadius);
    orbit.classList.add('cluster-orbit');
    svg.appendChild(orbit);

    var count = userAgents.length;
    userAgents.forEach(function(agent, idx) {
      var angle = (2 * Math.PI * idx / count) - Math.PI / 2;
      var ax = clusterCX + clusterRadius * Math.cos(angle);
      var ay = clusterCY + clusterRadius * Math.sin(angle);

      /* Store position for parent edge drawing */
      positionMap[agent.id] = {x: ax, y: ay};

      /* Edge from cluster centre to agent (visual only — no parent relation) */
      appendEdge(svg, clusterCX, clusterCY, ax, ay, 'cluster-edge', 'cluster-' + agent.id);

      appendAgentNode(svg, ax, ay, agent, 32);
    });
  }

  /* ── Parent-child edges (CON-004) ── */
  // Draw edges from parent agent to child agent for agents that have
  // a parent_id set. Called after all agents have been placed so
  // positionMap is fully populated.
  function appendParentEdges(svg) {
    for (var i = 0; i < agentsCache.length; i++) {
      var child = agentsCache[i];
      if (!child.parent_id) continue;
      var childPos = positionMap[child.id];
      var parentPos = positionMap[child.parent_id];
      if (!childPos || !parentPos) continue;
      appendEdge(svg, parentPos.x, parentPos.y, childPos.x, childPos.y, 'parent-edge', 'parent-' + child.id);
    }
  }

  /* ── Generic agent node ── */

  function appendAgentNode(svg, cx, cy, agent, r) {
    var tint = EXECUTOR_COLORS[agent.executor] || EXECUTOR_COLORS.unknown;

    var g = svgEl('g');
    g.classList.add('agent-node', 'node-anchor');
    g.setAttribute('data-node', 'agent');
    g.setAttribute('data-executor', agent.executor || 'unknown');

    /* Shadow backing */
    var shadow = svgEl('circle');
    shadow.setAttribute('cx', cx); shadow.setAttribute('cy', cy);
    shadow.setAttribute('r', r);
    shadow.setAttribute('fill', 'white');
    shadow.setAttribute('filter', 'url(#softShadow)');
    shadow.classList.add('node-circle');
    g.appendChild(shadow);

    /* Tint wash */
    var tintCircle = svgEl('circle');
    tintCircle.setAttribute('cx', cx); tintCircle.setAttribute('cy', cy);
    tintCircle.setAttribute('r', r - 5);
    tintCircle.setAttribute('fill', tint);
    tintCircle.setAttribute('opacity', '0.25');
    g.appendChild(tintCircle);

    /* Border */
    var border = svgEl('circle');
    border.setAttribute('cx', cx); border.setAttribute('cy', cy);
    border.setAttribute('r', r);
    border.setAttribute('fill', 'none');
    border.setAttribute('stroke', TOKENS.border);
    border.setAttribute('stroke-width', '1.5');
    g.appendChild(border);

    /* Icon */
    appendIcon(g, cx, cy, agent.executor || 'unknown', false);

    /* Label: project basename or session_id or executor */
    var name = (agent.project ? basename(agent.project) : null) || agent.session_id || agent.title ||
               ((agent.executor || '?') + ' #' + agent.pid);
    var labelEl = svgEl('text');
    labelEl.setAttribute('x', cx);
    labelEl.setAttribute('y', cy + r + 20);
    labelEl.setAttribute('text-anchor', 'middle');
    labelEl.setAttribute('font-family', 'Helvetica, Arial, sans-serif');
    labelEl.setAttribute('font-size', '13');
    labelEl.setAttribute('font-weight', '600');
    labelEl.setAttribute('fill', TOKENS.text);
    labelEl.classList.add('node-label');
    labelEl.textContent = truncate(name, 16);
    g.appendChild(labelEl);

    /* Click → open existing detail panel */
    g.addEventListener('click', function() {
      if (typeof showAgentDetail === 'function') {
        showAgentDetail(agent);
      }
    });

    svg.appendChild(g);
  }

  /* ── Helpers ── */

  function svgEl(tag) {
    return document.createElementNS('http://www.w3.org/2000/svg', tag);
  }

  function truncate(s, maxLen) {
    s = String(s || '');
    return s.length > maxLen ? s.slice(0, maxLen - 1) + '\u2026' : s;
  }

  function basename(path) {
    if (!path) return '';
    var s = String(path);
    var idx = s.lastIndexOf('/');
    return idx >= 0 ? s.slice(idx + 1) : s;
  }

  /* ── Pulse API (CON-006) ──
   * Accepts fromNode and toNode strings that map to data-edge values.
   * Examples:
   *   pulseEdge('kitt', 'hermes')   → pulses Kitt→Hermes spine
   *   pulseEdge('hermes', 'forge')  → pulses Hermes→Forge spine
   *   pulseEdge('forge', agentId)   → pulses Forge→agent edge
   * Handles concurrent/repeated pulses cleanly via reflow trick.
   */
  function pulseEdge(fromNode, toNode) {
    // Delegate to ForceTopology if available (CON-010)
    if (typeof ForceTopology !== 'undefined' && ForceTopology.pulseEdge) {
      ForceTopology.pulseEdge(fromNode, toNode);
      return;
    }
    if (!fromNode || !toNode) return;
    var edgeName = fromNode + '-' + toNode;
    var edge = document.querySelector('[data-edge="' + edgeName + '"]');
    if (!edge) return;

    /* Remove class and force reflow to allow animation restart */
    edge.classList.remove('edge-pulse');
    void edge.offsetWidth;

    /* Apply pulse — CSS keyframes handle decay to baseline */
    edge.classList.add('edge-pulse');

    /* Clean up after 3 s so repeated calls don't accumulate class */
    setTimeout(function() {
      edge.classList.remove('edge-pulse');
    }, 3000);
  }

  /* ── Expose public API ── */
  global.Constellation = {
    render: renderConstellation,
    destroy: destroyConstellation,
    pulseEdge: pulseEdge,
  };

})(window);
