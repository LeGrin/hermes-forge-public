/* topology-force.js — CON-010 Force Clusters v2 topology renderer
 * Replaces radial layout with D3 v7 force-directed clusters anchored
 * to VPS zone (left) and Mac zone (right).
 *
 * Key features:
 * - Zone anchoring: VPS left / Mac right via forceX
 * - Stable seeded layout via hash of node ID
 * - KITT avatar node with tooltip on hover
 * - Project-aware agent icons from /registry/projects
 * - Lifecycle filter (active/idle/old) via Filters API
 * - Two edge dialects: parent-child (grey) + live traffic (animated)
 * - Handles up to 40 agents with collision avoidance
 */

(function(global) {
  'use strict';

  /* ── D3 CDN load check ── */
  function hasD3() {
    return typeof window.d3 !== 'undefined' && typeof window.d3.forceSimulation === 'function';
  }

  /* ── Token helpers (mirror constellation.js pattern) ── */
  var computedStyle = null;
  function getToken(name) {
    if (!computedStyle) computedStyle = getComputedStyle(document.documentElement);
    return computedStyle.getPropertyValue(name).trim();
  }
  function tok(name) { return getToken('--constellation-' + name); }

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

  /* ── State ── */
  var agentsCache = [];
  var projectIcons = {}; // projectName → iconPath
  var simulation = null;
  var svg = null;
  var filterState = { active: true, idle: true, old: true };
  var nodeMap = {}; // id → d3 node
  var trafficEdges = {}; // "from->to" → { linkEl, trackedLink, timeout }
  var linkEls = []; // All link elements for filtering
  var groupedAgentCache = []; // Cached grouped agents [{id, label, members, zone, ...}]

  function resolveNode(id) {
    return id ? nodeMap[id] : null;
  }

  function resolveNodeID(id) {
    var n = resolveNode(id);
    return n ? n.id : id;
  }

  /* ── Constants ── */
  var WIDTH = 1200;
  var HEIGHT = 700;
  var VPS_CENTER_X = 300;  // Left zone centroid
  var MAC_CENTER_X = 900;  // Right zone centroid
  var ANCHOR_Y = 350;      // Vertical center for main anchors
  var ZONE_GAP = 60;       // Vertical separator gap

  /* ── Hash-based stable seeding ── */
  function hashStr(s) {
    var h = 0;
    for (var i = 0; i < s.length; i++) {
      h = ((h << 5) - h) + s.charCodeAt(i);
      h = h & h; // 32-bit
    }
    return Math.abs(h);
  }

  function seededX(id, zone) {
    var h = hashStr(id);
    // Spread within zone: VPS [80, 520], Mac [680, 1120]
    var rangeMin = zone === 'vps' ? 80 : 680;
    var rangeMax = zone === 'vps' ? 520 : 1120;
    return rangeMin + (h % (rangeMax - rangeMin));
  }

  function seededY(id) {
    var h = hashStr(id + '_y');
    return 120 + (h % (HEIGHT - 240));
  }

  function stableGroupID(key) {
    var rawKey = String(key || 'unknown');
    var safeKey = rawKey.replace(/[^A-Za-z0-9_.:-]/g, '_').slice(0, 64);
    return 'group:' + hashStr(rawKey) + ':' + safeKey;
  }

  /* ── Zone classification ── */
  function getZone(agent) {
    // VPS zone: KITT, Hermes, Forge, or agents hosted on VPS
    if (!agent) return 'vps';
    var host = agent.host || '';
    var executor = agent.executor || '';

    if (executor === 'kitt' || host.includes('vps') || host.includes('kitt')) {
      return 'vps';
    }
    if (executor === 'forge' || host.includes('mac') || host.includes('forge')) {
      return 'mac';
    }
    // Sub-agents: parent determines zone
    if (agent.parent_kind === 'forge') return 'mac';
    return 'vps';
  }

  /* ── Slice Aggregation 1: Agent grouping ── */

  /**
   * Compute a frontend grouping key for an agent.
   * Priority:
   *   1. session_id if present
   *   2. host + project + executor
   *   3. host + executor + parent_kind
   */
  function computeGroupKey(agent) {
    if (!agent) return 'unknown';

    // Priority 1: session_id
    if (agent.session_id) {
      return 'session:' + agent.session_id;
    }

    var host = agent.host || '';
    var project = agent.project || '';
    var executor = agent.executor || '';
    var parent_kind = agent.parent_kind || '';

    // Priority 2: host + project + executor
    if (project) {
      return host + '|' + project + '|' + executor;
    }

    // Priority 3: host + executor + parent_kind
    return host + '|' + executor + '|' + parent_kind;
  }

  /**
   * Build a human-readable label for a group of agents.
   * Uses the same priority as existing basename behavior:
   * session_id > title > project basename > executor
   */
  function buildGroupLabel(members) {
    if (!members || members.length === 0) return 'Group';

    // Try session_id first
    for (var i = 0; i < members.length; i++) {
      if (members[i].session_id) {
        return truncate(members[i].session_id, 14);
      }
    }

    // Try title
    for (var i = 0; i < members.length; i++) {
      if (members[i].title) {
        return truncate(members[i].title, 14);
      }
    }

    // Try project basename
    for (var i = 0; i < members.length; i++) {
      if (members[i].project) {
        return truncate(basename(members[i].project), 14);
      }
    }

    // Fall back to executor
    var executor = members[0].executor || 'unknown';
    return truncate(executor, 14);
  }

  /**
   * Determine the aggregate state for a group.
   * active > idle > old (return the "most active" state)
   */
  function aggregateGroupState(members) {
    var states = { active: 0, idle: 0, old: 0 };
    for (var i = 0; i < members.length; i++) {
      var s = members[i].state || 'idle';
      if (s === 'active') states.active++;
      else if (s === 'idle') states.idle++;
      else states.old++;
    }
    if (states.active > 0) return 'active';
    if (states.idle > 0) return 'idle';
    return 'old';
  }

  /**
   * Group agents by their computed grouping key.
   * Returns an array of group objects suitable for buildNodes.
   */
  function groupAgents(agents) {
    var groups = {};
    for (var i = 0; i < agents.length; i++) {
      var agent = agents[i];
      var key = computeGroupKey(agent);
      if (!groups[key]) {
        groups[key] = [];
      }
      groups[key].push(agent);
    }

    var result = [];
    for (var key in groups) {
      if (!groups.hasOwnProperty(key)) continue;
      var members = groups[key];
      var first = members[0];
      var groupId = stableGroupID(key);

      result.push({
        id: groupId,
        label: buildGroupLabel(members),
        executor: first.executor || 'unknown',
        project: first.project || '',
        state: aggregateGroupState(members),
        host: first.host || '',
        parent_id: first.parent_id || null,
        parent_kind: first.parent_kind || null,
        zone: getZone(first),
        isAnchor: false,
        iconPath: projectIcons[first.project] || null,
        x: 0, // Will be set by seeded positioning
        y: 0,
        vx: 0, vy: 0,
        // Group-specific fields
        isGroup: members.length > 1,
        groupSize: members.length,
        members: members,
        _key: key,
      });
    }
    return result;
  }

  /* ── Build node data from agents ── */
  function buildNodes(agents) {
    nodeMap = {};
    var nodes = [];
    var anchors = [
      { id: 'kitt', label: 'KITT', executor: 'kitt', zone: 'vps', isAnchor: true },
      { id: 'hermes', label: 'Hermes', executor: 'hermes', zone: 'vps', isAnchor: true },
      { id: 'forge', label: 'Forge', executor: 'forge', zone: 'mac', isAnchor: true },
    ];

    // Add anchors
    anchors.forEach(function(a) {
      var n = {
        id: a.id,
        label: a.label,
        executor: a.executor,
        zone: a.zone,
        isAnchor: true,
        x: a.zone === 'vps' ? VPS_CENTER_X : MAC_CENTER_X,
        y: ANCHOR_Y + (a.id === 'kitt' ? -180 : a.id === 'forge' ? 180 : 0),
        vx: 0, vy: 0,
        fx: a.zone === 'vps' ? VPS_CENTER_X : MAC_CENTER_X, // Fix anchor position
        fy: ANCHOR_Y + (a.id === 'kitt' ? -180 : a.id === 'forge' ? 180 : 0),
      };
      nodes.push(n);
      nodeMap[a.id] = n;
    });

    // Group agents using Slice Aggregation 1 logic
    var groupedAgents = groupAgents(agents);
    groupedAgentCache = groupedAgents;

    // Add grouped agents
    groupedAgents.forEach(function(grp) {
      var n = {
        id: grp.id,
        label: grp.label,
        executor: grp.executor,
        project: grp.project || '',
        state: grp.state || 'idle',
        host: grp.host || '',
        parent_id: grp.parent_id || null,
        parent_kind: grp.parent_kind || null,
        zone: grp.zone,
        isAnchor: false,
        iconPath: grp.iconPath || null,
        x: seededX(grp.id, grp.zone),
        y: seededY(grp.id),
        vx: 0, vy: 0,
        // Store full agent/group data for detail view
        _agent: grp.members[0],
        // Slice Aggregation 1: group metadata
        isGroup: grp.isGroup || false,
        groupSize: grp.groupSize || 1,
        members: grp.members,
        _key: grp._key,
      };
      nodes.push(n);
      nodeMap[grp.id] = n;
      grp.members.forEach(function(member) {
        if (member && member.id) {
          nodeMap[member.id] = n;
        }
      });
    });

    return nodes;
  }

  /* ── Build link data ── */
  function buildLinks(nodes) {
    var links = [];

    // Spine links: KITT↔Hermes↔Forge
    links.push({ source: 'kitt', target: 'hermes', type: 'spine', fromNode: 'kitt', toNode: 'hermes' });
    links.push({ source: 'hermes', target: 'forge', type: 'spine', fromNode: 'hermes', toNode: 'forge' });

    // Parent-child links. Agents are grouped for display, so parent_id values
    // from raw agent records must be resolved through nodeMap to their group.
    var seenParentLinks = {};
    nodes.forEach(function(n) {
      var members = n.members || [];
      members.forEach(function(member) {
        var parentNode = resolveNode(member.parent_id);
        if (!parentNode || parentNode.id === n.id) return;
        var key = parentNode.id + '->' + n.id;
        if (seenParentLinks[key]) return;
        seenParentLinks[key] = true;
        links.push({
          source: parentNode.id,
          target: n.id,
          type: 'parent',
          fromNode: parentNode.id,
          toNode: n.id,
        });
      });
    });

    return links;
  }

  /**
   * Add a live-traffic edge between two nodes.
   * Creates the edge visually if both nodes exist, removes after duration.
   * Called by pulseEdge for CON-006 traffic pulse events.
   */
  function addTrafficEdge(fromNode, toNode, duration) {
    var resolvedFrom = resolveNodeID(fromNode);
    var resolvedTo = resolveNodeID(toNode);
    if (!svg || !nodeMap[resolvedFrom] || !nodeMap[resolvedTo]) return;
    var key = resolvedFrom + '->' + resolvedTo;
    var d = duration || 3000;

    // Remove existing traffic edge if present
    removeTrafficEdge(key);

    // Create traffic edge link data
    var linkData = {
      source: nodeMap[resolvedFrom],
      target: nodeMap[resolvedTo],
      type: 'traffic',
      fromNode: resolvedFrom,
      toNode: resolvedTo,
    };

    // Render the traffic edge
    var linkEl = renderLink(linkData);
    var linkGroup = svg.querySelector('.links');
    if (linkGroup) {
      // Insert before nodes so links appear behind
      linkGroup.appendChild(linkEl);
    }

    var trackedLink = trackLinkEl(linkData, linkEl);

    // Store reference
    trafficEdges[key] = {
      linkEl: linkEl,
      trackedLink: trackedLink,
      timeout: setTimeout(function() {
        removeTrafficEdge(key);
      }, d),
    };

    applyFilters();
  }

  function trackLinkEl(link, el) {
    var tracked = { el: el, link: link };
    linkEls.push(tracked);
    return tracked;
  }

  function untrackLinkEl(tracked) {
    if (!tracked) return;
    var idx = linkEls.indexOf(tracked);
    if (idx >= 0) {
      linkEls.splice(idx, 1);
    }
  }

  function removeTrafficEdge(key) {
    var trafficEdge = trafficEdges[key];
    if (!trafficEdge) return;

    clearTimeout(trafficEdge.timeout);
    untrackLinkEl(trafficEdge.trackedLink);
    if (trafficEdge.linkEl && trafficEdge.linkEl.parentNode) {
      trafficEdge.linkEl.parentNode.removeChild(trafficEdge.linkEl);
    }
    delete trafficEdges[key];
  }

  /**
   * Public API: add live traffic pulse between two nodes.
   * Called from SSE event handler via Constellation.pulseEdge or ForceTopology.pulseEdge.
   */
  function pulseEdge(fromNode, toNode) {
    if (!svg || !fromNode || !toNode) return;

    // Add a live traffic edge that will appear and fade
    addTrafficEdge(fromNode, toNode, 3000);
  }

  /**
   * Pulse multiple edges for a path (e.g., KITT->Hermes->Forge).
   * Used when a status/decision event flows through the spine.
   */
  function pulsePath(pathNodes) {
    if (!Array.isArray(pathNodes) || pathNodes.length < 2) return;
    for (var i = 0; i < pathNodes.length - 1; i++) {
      pulseEdge(pathNodes[i], pathNodes[i + 1]);
    }
  }

  /* ── SVG helpers ── */
  function createSVGEl(tag) {
    return document.createElementNS('http://www.w3.org/2000/svg', tag);
  }

  function createSVG() {
    var root = document.getElementById('constellation');
    if (!root) return null;
    root.innerHTML = '';

    var el = createSVGEl('svg');
    el.setAttribute('viewBox', '0 0 ' + WIDTH + ' ' + HEIGHT);
    el.setAttribute('preserveAspectRatio', 'xMidYMid meet');
    el.classList.add('constellation-svg');
    return el;
  }

  function appendDefs(svgEl) {
    ensureTokens();
    var defs = createSVGEl('defs');

    // Dot pattern
    var pattern = createSVGEl('pattern');
    pattern.setAttribute('id', 'dots');
    pattern.setAttribute('width', '24');
    pattern.setAttribute('height', '24');
    pattern.setAttribute('patternUnits', 'userSpaceOnUse');
    var dot = createSVGEl('circle');
    dot.setAttribute('cx', '4');
    dot.setAttribute('cy', '4');
    dot.setAttribute('r', '1.2');
    dot.setAttribute('fill', TOKENS.dot);
    dot.setAttribute('opacity', '0.45');
    pattern.appendChild(dot);
    defs.appendChild(pattern);

    // Soft shadow
    var softShadow = createSVGEl('filter');
    softShadow.setAttribute('id', 'softShadow');
    softShadow.setAttribute('x', '-40%');
    softShadow.setAttribute('y', '-40%');
    softShadow.setAttribute('width', '180%');
    softShadow.setAttribute('height', '180%');
    var feShadow = createSVGEl('feDropShadow');
    feShadow.setAttribute('dx', '0');
    feShadow.setAttribute('dy', '4');
    feShadow.setAttribute('stdDeviation', '6');
    feShadow.setAttribute('flood-color', TOKENS.shadow);
    feShadow.setAttribute('flood-opacity', '0.25');
    softShadow.appendChild(feShadow);
    defs.appendChild(softShadow);

    // Glow filter
    var glow = createSVGEl('filter');
    glow.setAttribute('id', 'nodeGlow');
    glow.setAttribute('x', '-60%');
    glow.setAttribute('y', '-60%');
    glow.setAttribute('width', '220%');
    glow.setAttribute('height', '220%');
    var feGlow = createSVGEl('feDropShadow');
    feGlow.setAttribute('dx', '0');
    feGlow.setAttribute('dy', '6');
    feGlow.setAttribute('stdDeviation', '12');
    feGlow.setAttribute('flood-color', TOKENS.shadow);
    feGlow.setAttribute('flood-opacity', '0.35');
    glow.appendChild(feGlow);
    defs.appendChild(glow);

    // Avatar clip for KITT
    var clip = createSVGEl('clipPath');
    clip.setAttribute('id', 'kittAvatarClip');
    var clipCircle = createSVGEl('circle');
    clipCircle.setAttribute('cx', '0');
    clipCircle.setAttribute('cy', '0');
    clipCircle.setAttribute('r', '28');
    clip.appendChild(clipCircle);
    defs.appendChild(clip);

    // Arrow marker for directional traffic edges
    var arrowDef = createSVGEl('marker');
    arrowDef.setAttribute('id', 'trafficArrow');
    arrowDef.setAttribute('viewBox', '0 0 10 10');
    arrowDef.setAttribute('refX', '9');
    arrowDef.setAttribute('refY', '5');
    arrowDef.setAttribute('markerWidth', '6');
    arrowDef.setAttribute('markerHeight', '6');
    arrowDef.setAttribute('orient', 'auto-start-reverse');
    var arrowPath = createSVGEl('path');
    arrowPath.setAttribute('d', 'M 0 0 L 10 5 L 0 10 z');
    arrowPath.setAttribute('fill', '#5b8def');
    arrowDef.appendChild(arrowPath);
    defs.appendChild(arrowDef);

    svgEl.appendChild(defs);
  }

  function appendZoneSeparator(svgEl) {
    // Vertical separator line
    var line = createSVGEl('line');
    line.setAttribute('x1', WIDTH / 2);
    line.setAttribute('y1', ZONE_GAP);
    line.setAttribute('x2', WIDTH / 2);
    line.setAttribute('y2', HEIGHT - ZONE_GAP);
    line.setAttribute('stroke', TOKENS.lineSoft);
    line.setAttribute('stroke-width', '1');
    line.setAttribute('stroke-dasharray', '8 6');
    line.setAttribute('opacity', '0.5');
    svgEl.appendChild(line);

    // VPS zone label
    var vpsLabel = createSVGEl('text');
    vpsLabel.setAttribute('x', '20');
    vpsLabel.setAttribute('y', HEIGHT - 16);
    vpsLabel.setAttribute('fill', TOKENS.textMuted);
    vpsLabel.setAttribute('font-size', '12');
    vpsLabel.setAttribute('font-family', 'Helvetica, Arial, sans-serif');
    vpsLabel.setAttribute('letter-spacing', '1');
    vpsLabel.textContent = 'VPS ZONE';
    svgEl.appendChild(vpsLabel);

    // Mac zone label
    var macLabel = createSVGEl('text');
    macLabel.setAttribute('x', WIDTH - 90);
    macLabel.setAttribute('y', HEIGHT - 16);
    macLabel.setAttribute('fill', TOKENS.textMuted);
    macLabel.setAttribute('font-size', '12');
    macLabel.setAttribute('font-family', 'Helvetica, Arial, sans-serif');
    macLabel.setAttribute('letter-spacing', '1');
    macLabel.textContent = 'MAC ZONE';
    svgEl.appendChild(macLabel);
  }

  /* ── Node rendering ── */

  function renderNode(d) {
    var g = createSVGEl('g');
    g.classList.add('node');
    g.setAttribute('data-node', d.id);
    g.setAttribute('data-zone', d.zone);
    g.setAttribute('data-state', d.state);

    var r = d.isAnchor ? (d.id === 'forge' ? 48 : 40) : 28;

    // Backing circle
    var circle = createSVGEl('circle');
    circle.setAttribute('cx', 0);
    circle.setAttribute('cy', 0);
    circle.setAttribute('r', r);
    circle.setAttribute('fill', 'white');
    circle.classList.add('node-circle');
    if (!d.isAnchor) circle.setAttribute('filter', 'url(#softShadow)');
    else circle.setAttribute('filter', 'url(#nodeGlow)');
    g.appendChild(circle);

    // State-based tint
    var tintColor = TOKENS[d.executor] || TOKENS.subagent;
    var tintOpacity = '0.2';
    var hasOutline = false;
    var isPulsing = false;

    if (d.state === 'active') {
      tintOpacity = '0.3';
      hasOutline = true;
      isPulsing = true;
    } else if (d.state === 'idle') {
      tintOpacity = '0.15';
      hasOutline = true;
    } else if (d.state === 'old' || d.state === 'done' || d.state === 'terminated') {
      tintOpacity = '0.1';
    }

    var tint = createSVGEl('circle');
    tint.setAttribute('cx', 0);
    tint.setAttribute('cy', 0);
    tint.setAttribute('r', r - 3);
    tint.setAttribute('fill', tintColor);
    tint.setAttribute('opacity', tintOpacity);
    g.appendChild(tint);

    // Border ring
    var border = createSVGEl('circle');
    border.setAttribute('cx', 0);
    border.setAttribute('cy', 0);
    border.setAttribute('r', r);
    border.setAttribute('fill', 'none');
    border.setAttribute('stroke', hasOutline ? tintColor : TOKENS.border);
    border.setAttribute('stroke-width', hasOutline ? '2' : '1.5');
    border.setAttribute('opacity', d.state === 'old' ? '0.5' : '1');
    g.appendChild(border);

    // KITT avatar image
    if (d.id === 'kitt') {
      renderKittAvatar(g, r);
    } else if (!d.isAnchor && d.iconPath) {
      // Agent with project icon
      renderProjectIcon(g, r, d.iconPath);
    } else {
      // Fallback: render executor letter
      renderExecutorGlyph(g, d.executor, r);
    }

    // Label (hidden for KITT, shown for anchors and agents)
    if (d.id !== 'kitt') {
      var label = createSVGEl('text');
      label.setAttribute('x', 0);
      label.setAttribute('y', r + 18);
      label.setAttribute('text-anchor', 'middle');
      label.setAttribute('font-family', 'Helvetica, Arial, sans-serif');
      label.setAttribute('font-size', d.isAnchor ? '14' : '11');
      label.setAttribute('font-weight', '600');
      label.setAttribute('fill', TOKENS.text);
      label.classList.add('node-label');
      label.textContent = truncate(d.label, 14);
      g.appendChild(label);

      // Slice Aggregation 1: ×N badge for grouped nodes with >1 member
      if (d.isGroup && d.groupSize > 1) {
        var badge = createSVGEl('text');
        badge.setAttribute('x', r + 6);
        badge.setAttribute('y', -(r - 8));
        badge.setAttribute('text-anchor', 'start');
        badge.setAttribute('font-family', 'Helvetica, Arial, sans-serif');
        badge.setAttribute('font-size', '10');
        badge.setAttribute('font-weight', '700');
        badge.setAttribute('fill', TOKENS.text);
        badge.classList.add('group-badge');
        badge.textContent = '\xD7' + d.groupSize;
        g.appendChild(badge);
      }
    }

    // Invisible overlay for tooltip interaction
    var hitArea = createSVGEl('circle');
    hitArea.setAttribute('cx', 0);
    hitArea.setAttribute('cy', 0);
    hitArea.setAttribute('r', r + 10);
    hitArea.setAttribute('fill', 'transparent');
    hitArea.style.cursor = 'pointer';
    g.appendChild(hitArea);

    // Tooltip on hover
    g.addEventListener('mouseenter', function() { showTooltip(d); });
    g.addEventListener('mouseleave', hideTooltip);
    g.addEventListener('click', function() {
      if (typeof showAgentDetail === 'function') {
        if (d.isAnchor) {
          // Anchor nodes get a constructed object
          showAgentDetail({
            id: d.id,
            executor: d.executor,
            host: d.id === 'kitt' ? 'vps-kitt' : d.id,
            title: d.label,
            state: 'anchor',
            parent_kind: 'none',
            session_id: null,
          });
        } else if (d.isGroup && d.members) {
          // Slice Aggregation 1: grouped node - pass primary agent + member list
          var detailObj = {};
          // Copy primary agent data
          var primary = d.members[0];
          for (var key in primary) {
            if (primary.hasOwnProperty(key)) {
              detailObj[key] = primary[key];
            }
          }
          // Annotate with group info
          detailObj._isGroup = true;
          detailObj._groupSize = d.groupSize;
          detailObj._members = d.members;
          detailObj.id = d.id;
          detailObj.title = d.label;
          showAgentDetail(detailObj);
        } else if (d._agent) {
          // Pass full agent data for detail view
          showAgentDetail(d._agent);
        } else {
          // Fallback to node data
          showAgentDetail({
            id: d.id,
            executor: d.executor,
            host: d.host,
            title: d.label,
            state: d.state,
            project: d.project,
            parent_kind: d.parent_kind,
            session_id: d.session_id,
          });
        }
      }
    });

    return g;
  }

  function renderKittAvatar(g, r) {
    // Try to load real KITT avatar image, fall back to glyph on error
    var img = createSVGEl('image');
    img.setAttribute('x', -24);
    img.setAttribute('y', -24);
    img.setAttribute('width', '48');
    img.setAttribute('height', '48');
    img.setAttribute('href', '/icons/kitt.png');
    img.setAttribute('preserveAspectRatio', 'xMidYMid meet');
    img.setAttribute('clip-path', 'url(#kittAvatarClip)');

    // Fallback glyph shown on image load error
    img.onerror = function() {
      // Remove failed image and show glyph instead
      var parent = this.parentNode;
      if (parent) {
        // Remove this image
        parent.removeChild(this);
        // Add glyph
        var glyph = createSVGEl('text');
        glyph.setAttribute('x', 0);
        glyph.setAttribute('y', 6);
        glyph.setAttribute('text-anchor', 'middle');
        glyph.setAttribute('font-family', 'Helvetica, Arial, sans-serif');
        glyph.setAttribute('font-size', '20');
        glyph.setAttribute('font-weight', '700');
        glyph.setAttribute('fill', TOKENS.text);
        glyph.textContent = 'K';
        parent.appendChild(glyph);
      }
    };

    g.appendChild(img);
  }

  function renderProjectIcon(g, r, iconPath) {
    // Tint background
    var bg = createSVGEl('circle');
    bg.setAttribute('cx', 0);
    bg.setAttribute('cy', 0);
    bg.setAttribute('r', r - 8);
    bg.setAttribute('fill', TOKENS.subagent);
    bg.setAttribute('opacity', '0.25');
    g.appendChild(bg);

    // Icon image
    var img = createSVGEl('image');
    img.setAttribute('x', -14);
    img.setAttribute('y', -14);
    img.setAttribute('width', '28');
    img.setAttribute('height', '28');
    img.setAttribute('href', iconPath);
    img.setAttribute('preserveAspectRatio', 'xMidYMid meet');
    g.appendChild(img);
  }

  function renderExecutorGlyph(g, executor, r) {
    var glyph = createSVGEl('text');
    glyph.setAttribute('x', 0);
    glyph.setAttribute('y', 5);
    glyph.setAttribute('text-anchor', 'middle');
    glyph.setAttribute('font-family', 'Helvetica, Arial, sans-serif');
    glyph.setAttribute('font-size', '14');
    glyph.setAttribute('font-weight', '700');
    glyph.setAttribute('fill', TOKENS.text);
    var letter = executor ? executor.charAt(0).toUpperCase() : '?';
    glyph.textContent = letter;
    g.appendChild(glyph);
  }

  /* ── Link rendering ── */

  function renderLink(d) {
    var line = createSVGEl('line');
    line.classList.add('link');

    if (d.type === 'spine') {
      line.classList.add('spine-line');
    } else if (d.type === 'parent') {
      line.classList.add('parent-edge');
    } else if (d.type === 'traffic') {
      line.classList.add('traffic-edge');
      // Directional with arrow
      line.setAttribute('marker-end', 'url(#trafficArrow)');
    }

    // Set data-edge for pulse targeting
    if (d.fromNode && d.toNode) {
      line.setAttribute('data-edge', d.fromNode + '-' + d.toNode);
    }

    return line;
  }

  /* ── Tooltip ── */

  var tooltipEl = null;

  function showTooltip(d) {
    if (!tooltipEl) {
      tooltipEl = document.createElement('div');
      tooltipEl.className = 'force-tooltip';
      document.body.appendChild(tooltipEl);
    }

    var zone = d.zone === 'vps' ? 'VPS' : 'Mac';
    var state = d.state || 'unknown';
    var project = d.project ? basename(d.project) : '—';
    var host = d.host || '—';
    var age = '';

    // Calculate age from agent data if available
    if (d._agent && d._agent.last_seen_at) {
      age = formatAge(d._agent.last_seen_at);
    } else if (d.last_seen_at) {
      age = formatAge(d.last_seen_at);
    }

    // Build tooltip with project icon if available
    var projectHtml = project;
    if (d.iconPath) {
      projectHtml = '<img src="' + escHtml(d.iconPath) + '" width="14" height="14" style="vertical-align:middle;margin-right:4px;">' + escHtml(project);
    }

    // Slice Aggregation 1: add group count to label
    var labelText = d.label;
    if (d.isGroup && d.groupSize > 1) {
      labelText = d.label + ' (\xD7' + d.groupSize + ')';
    }

    tooltipEl.innerHTML = '<strong>' + escHtml(labelText) + '</strong>' +
      '<span class="tt-zone">' + zone + '</span>' +
      '<span class="tt-row">state: <em>' + state + '</em></span>' +
      '<span class="tt-row">project: ' + projectHtml + '</span>' +
      '<span class="tt-row">host: ' + escHtml(host) + '</span>' +
      (age ? '<span class="tt-row">last seen: <em>' + age + '</em></span>' : '');

    tooltipEl.style.display = 'block';
  }

  function formatAge(timestamp) {
    if (!timestamp) return '';
    var d = new Date(timestamp);
    if (isNaN(d.getTime())) return '';
    var diffMs = Date.now() - d.getTime();
    var diffSec = Math.floor(diffMs / 1000);
    if (diffSec < 60) return diffSec + 's ago';
    var diffMin = Math.floor(diffSec / 60);
    if (diffMin < 60) return diffMin + 'm ago';
    var diffHr = Math.floor(diffMin / 60);
    if (diffHr < 24) return diffHr + 'h ago';
    var diffDay = Math.floor(diffHr / 24);
    return diffDay + 'd ago';
  }

  function hideTooltip() {
    if (tooltipEl) tooltipEl.style.display = 'none';
  }

  function moveTooltip(e) {
    if (!tooltipEl) return;
    tooltipEl.style.left = (e.clientX + 16) + 'px';
    tooltipEl.style.top = (e.clientY + 16) + 'px';
  }

  document.addEventListener('mousemove', moveTooltip);

  /* ── D3 Force Simulation ── */

  // Zone boundaries for hard containment
  var VPS_ZONE_MIN_X = 60;
  var VPS_ZONE_MAX_X = 540;
  var MAC_ZONE_MIN_X = 660;
  var MAC_ZONE_MAX_X = 1140;

  function initSimulation(nodes, links, svgEl) {
    if (!hasD3()) {
      console.error('D3 not loaded - force topology unavailable');
      return null;
    }

    var d3 = window.d3;

    // Create link elements
    var linkGroup = createSVGEl('g');
    linkGroup.classList.add('links');
    svgEl.appendChild(linkGroup);

    var nodeGroup = createSVGEl('g');
    nodeGroup.classList.add('nodes');
    svgEl.appendChild(nodeGroup);

    var sim = d3.forceSimulation(nodes)
      .force('link', d3.forceLink(links)
        .id(function(d) { return d.id; })
        .distance(function(d) {
          if (d.type === 'spine') return 180;
          return 80;
        })
        .strength(function(d) {
          if (d.type === 'spine') return 0.8;
          if (d.type === 'parent') return 0.3;
          return 0.1;
        }))
      .force('x', d3.forceX(function(d) {
        if (d.isAnchor) {
          return d.zone === 'vps' ? VPS_CENTER_X : MAC_CENTER_X;
        }
        // Agents: bias to their zone centroid
        return d.zone === 'vps' ? VPS_CENTER_X : MAC_CENTER_X;
      }).strength(0.08))
      .force('y', d3.forceY(HEIGHT / 2).strength(0.03))
      .force('collide', d3.forceCollide(function(d) {
        return d.isAnchor ? 60 : 38;
      }).strength(0.8))
      .force('charge', d3.forceManyBody()
        .strength(function(d) {
          if (d.isAnchor) return -400;
          return -150;
        })
        .distanceMax(400))
      .alphaDecay(0.02)
      .velocityDecay(0.4);

    // Draw links (use module-level linkEls for filtering)
    linkEls = [];
    links.forEach(function(l) {
      var el = renderLink(l);
      // Mark traffic edges for pulse targeting
      if (l.type === 'traffic') {
        el.setAttribute('data-edge', l.fromNode + '-' + l.toNode);
      }
      trackLinkEl(l, el);
      linkGroup.appendChild(el);
    });

    // Draw nodes
    var nodeEls = [];
    nodes.forEach(function(n) {
      var el = renderNode(n);
      nodeEls.push({ el: el, node: n });
      nodeGroup.appendChild(el);
    });

    sim.on('tick', function() {
      // Clamp nodes to their zones (hard containment)
      nodes.forEach(function(n) {
        if (n.fx !== undefined && n.fx !== null) {
          // Anchors stay fixed
          return;
        }
        if (n.zone === 'vps') {
          n.x = Math.max(VPS_ZONE_MIN_X, Math.min(VPS_ZONE_MAX_X, n.x));
        } else {
          n.x = Math.max(MAC_ZONE_MIN_X, Math.min(MAC_ZONE_MAX_X, n.x));
        }
        // Keep within vertical bounds
        n.y = Math.max(40, Math.min(HEIGHT - 40, n.y));
      });

      linkEls.forEach(function(l) {
        var s = l.link.source;
        var t = l.link.target;
        if (s && t && typeof s === 'object' && typeof t === 'object') {
          l.el.setAttribute('x1', String(Math.round(s.x)));
          l.el.setAttribute('y1', String(Math.round(s.y)));
          l.el.setAttribute('x2', String(Math.round(t.x)));
          l.el.setAttribute('y2', String(Math.round(t.y)));
        }
      });

      nodeEls.forEach(function(n) {
        n.el.setAttribute('transform',
          'translate(' + Math.round(n.node.x) + ',' + Math.round(n.node.y) + ')');
        n.el.style.opacity = getNodeOpacity(n.node);
      });
    });

    // Apply initial filter state
    applyFilters();

    return sim;
  }

  function getNodeOpacity(node) {
    if (!filterState) return 1;
    if (node.isAnchor) return 1;

    var state = node.state || 'idle';
    if (state === 'active' && !filterState.active) return 0;
    if ((state === 'idle') && !filterState.idle) return 0;
    if ((state === 'old' || state === 'done' || state === 'terminated') && !filterState.old) return 0;
    return 1;
  }

  function applyFilters() {
    if (!svg) return;

    // First pass: update node opacity
    var nodeEls = svg.querySelectorAll('.nodes .node');
    nodeEls.forEach(function(n) {
      var nodeData = nodeMap[n.getAttribute('data-node')];
      if (!nodeData) return;
      n.style.opacity = getNodeOpacity(nodeData);
    });

    // Second pass: update link opacity based on connected nodes
    // Spine links always visible; parent/traffic links hidden if either endpoint hidden
    linkEls.forEach(function(l) {
      var link = l.link;
      var sourceNode = typeof link.source === 'object' ? link.source : nodeMap[link.source];
      var targetNode = typeof link.target === 'object' ? link.target : nodeMap[link.target];

      if (!sourceNode || !targetNode) {
        l.el.style.opacity = '0';
        return;
      }

      var sourceOpacity = getNodeOpacity(sourceNode);
      var targetOpacity = getNodeOpacity(targetNode);

      if (link.type === 'spine') {
        // Spine links always visible
        l.el.style.opacity = '0.7';
      } else {
        // Parent/traffic links: show only if both endpoints visible
        l.el.style.opacity = (sourceOpacity > 0 && targetOpacity > 0) ? '0.6' : '0';
      }
    });
  }

  /* ── Public API ── */

  function renderForceTopology(agents, projects) {
    if (!hasD3()) {
      console.warn('D3 not available - cannot render force topology');
      return;
    }

    agentsCache = Array.isArray(agents) ? agents : [];
    ensureTokens();

    // Build project icon map
    projectIcons = {};
    if (projects && Array.isArray(projects)) {
      projects.forEach(function(p) {
        if (p && p.project && p.icon_path) {
          projectIcons[p.project] = p.icon_path;
        }
      });
    }

    filterState = global.Filters ? global.Filters.loadFilters() : { active: true, idle: true, old: true };

    svg = createSVG();
    if (!svg) return;

    appendDefs(svg);
    appendZoneSeparator(svg);

    var root = document.getElementById('constellation');
    root.appendChild(svg);

    var nodes = buildNodes(agentsCache);
    var links = buildLinks(nodes);

    simulation = initSimulation(nodes, links, svg);

    if (!agentsCache.length) {
      var empty = document.createElement('div');
      empty.className = 'constellation-empty';
      empty.textContent = 'No agents reporting yet. Forge reports every 60 s.';
      root.appendChild(empty);
    }
  }

  function refreshFilters() {
    filterState = global.Filters ? global.Filters.loadFilters() : { active: true, idle: true, old: true };
    applyFilters();
  }

  function destroyForceTopology() {
    if (simulation) {
      simulation.stop();
      simulation = null;
    }
    // Clear traffic edge timeouts
    Object.keys(trafficEdges).forEach(function(key) {
      removeTrafficEdge(key);
    });
    linkEls = [];
    var root = document.getElementById('constellation');
    if (root) root.innerHTML = '';
    svg = null;
    nodeMap = {};
  }

  /* ── Helpers ── */
  function truncate(s, maxLen) {
    s = String(s || '');
    return s.length > maxLen ? s.slice(0, maxLen - 1) + '\u2026' : s;
  }

  function escHtml(s) {
    var d = document.createElement('div');
    d.textContent = (s === undefined || s === null) ? '' : String(s);
    return d.innerHTML;
  }

  function basename(path) {
    if (!path) return '';
    var s = String(path);
    var idx = s.lastIndexOf('/');
    return idx >= 0 ? s.slice(idx + 1) : s;
  }

  /* ── Expose public API ── */
  global.ForceTopology = {
    render: renderForceTopology,
    destroy: destroyForceTopology,
    pulseEdge: pulseEdge,
    pulsePath: pulsePath,
    refreshFilters: refreshFilters,
  };

})(window);
