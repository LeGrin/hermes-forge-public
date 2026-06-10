/* filters.js — CON-010 lifecycle filter state
 * Provides chip state management for Active/Idle/Old lifecycle filters
 * and Envelope status chip helpers (ephemeral — no localStorage).
 * Envelope filter is session-only; lifecycle filters ARE persisted.
 */

(function(global) {
  'use strict';

  var LS_FILTERS = 'constellation.filters';

  /* ── Lifecycle filter state ── */

  function getDefaultFilters() {
    return { active: true, idle: true, old: true };
  }

  function loadFilters() {
    try {
      var raw = localStorage.getItem(LS_FILTERS);
      if (!raw) return getDefaultFilters();
      var parsed = JSON.parse(raw);
      // Ensure all keys exist
      return {
        active: Boolean(parsed.active),
        idle: Boolean(parsed.idle),
        old: Boolean(parsed.old),
      };
    } catch (_) {
      return getDefaultFilters();
    }
  }

  function saveFilters(filters) {
    try {
      localStorage.setItem(LS_FILTERS, JSON.stringify(filters));
    } catch (_) {}
  }

  function toggleFilter(key) {
    var filters = loadFilters();
    filters[key] = !filters[key];
    saveFilters(filters);
    return filters;
  }

  /* ── Envelope filter helpers (ephemeral — no localStorage) ── */

  var ENV_OPTIONS = ['all', 'active', 'done', 'blocked', 'failed'];

  /* ── Filter apply helpers ── */

  /** Returns true if agent passes the lifecycle filter. */
  function agentPassesFilter(agent, filters) {
    var state = agent.state || 'idle';
    var isActive = state === 'active';
    var isIdle = state === 'idle';
    var isOld = state === 'old' || state === 'terminated' || state === 'done';

    if (isActive && !filters.active) return false;
    if (isIdle && !filters.idle) return false;
    if (isOld && !filters.old) return false;

    // If none of the states matched, hide by default unless all filters are off
    if (!isActive && !isIdle && !isOld) {
      return filters.active || filters.idle || filters.old;
    }
    return true;
  }

  /** Returns envelope status category for chip matching. */
  function envelopeStatusCategory(status) {
    if (!status) return 'active';
    var s = String(status).toLowerCase();
    if (s === 'done' || s === 'closed') return 'done';
    if (s === 'blocked' || s === 'paused') return 'blocked';
    if (s === 'failed' || s === 'lost') return 'failed';
    return 'active';
  }

  /**
   * Returns true if envelope passes the envelope filter chip.
   * "active" chip shows everything except done/closed (includes blocked and failed).
   * "done" chip shows only done/closed.
   * "blocked"/"failed" show only those specific statuses.
   */
  function envelopePassesFilter(envelope, chipValue) {
    if (chipValue === 'all') return true;
    if (chipValue === 'active') {
      // Active = NOT done/closed - includes blocked and failed
      var cat = envelopeStatusCategory(envelope.status);
      return cat !== 'done';
    }
    return envelopeStatusCategory(envelope.status) === chipValue;
  }

  /** Returns count breakdown of envelopes by status category.
   *  Note: "active" count = everything that's NOT done/closed,
   *  because "active" filter means status != done (includes blocked/failed).
   */
  function countByStatus(envelopes) {
    var counts = { all: 0, active: 0, done: 0, blocked: 0, failed: 0 };
    for (var i = 0; i < envelopes.length; i++) {
      var env = envelopes[i];
      var cat = envelopeStatusCategory(env.status);
      counts.all++;
      counts[cat]++;
    }
    // "active" = all - done (everything that's not done/closed)
    counts.active = counts.all - counts.done;
    return counts;
  }

  /* ── Expose public API ── */
  global.Filters = {
    loadFilters: loadFilters,
    toggleFilter: toggleFilter,
    saveFilters: saveFilters,
    agentPassesFilter: agentPassesFilter,
    envelopePassesFilter: envelopePassesFilter,
    countByStatus: countByStatus,
    ENV_OPTIONS: ENV_OPTIONS,
  };

})(window);
