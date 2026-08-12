/*
 * Crucible Analytic beacon.
 *
 * Embed with:
 *   <script defer src="https://example.com/_ca/ca.js" data-site="mysite"></script>
 *
 * data-site is required and must match a site_id the beacon server is
 * configured to accept. data-host optionally overrides where events are
 * posted; by default they go to "event" alongside this script's own URL,
 * so serving the script and receiving events stay on one origin without
 * anything to configure.
 *
 * Opt out on a single browser with:
 *   localStorage.setItem('crucible.disabled', '1')
 *
 * Deliberately written in conservative ES5 with no build step: it is
 * served verbatim from the Go binary (go:embed), so what ships is what
 * is in this file, and it runs in anything without transpiling.
 */
(function () {
  var script = document.currentScript;
  if (!script) return;

  var site = script.getAttribute('data-site');
  if (!site) return;

  /* Derive the endpoint from this script's own src: ".../_ca/ca.js"
   * becomes ".../_ca/event". Same origin as the script by construction,
   * which is what keeps the common deployment CORS-free. */
  var endpoint =
    script.getAttribute('data-host') ||
    script.src.replace(/[^/]*$/, '') + 'event';

  /* localStorage throws outright in sandboxed iframes and in browsers
   * with storage disabled, so never let it break the page. */
  try {
    if (localStorage.getItem('crucible.disabled')) return;
  } catch (e) {}

  function currentURL() {
    return location.pathname + location.search;
  }

  /* Same-origin navigations are not an acquisition source; dropping
   * them here rather than server-side means an internal URL - which can
   * carry query parameters the server has no allowlist for - never
   * leaves the browser at all. */
  function referrer() {
    var r = document.referrer;
    if (!r) return '';
    try {
      if (new URL(r).host === location.host) return '';
    } catch (e) {
      return '';
    }
    return r;
  }

  function payload(type, name) {
    return {
      site: site,
      type: type,
      name: name || '',
      url: currentURL(),
      referrer: referrer(),
      title: document.title || '',
      screen_w: screen.width || 0,
      screen_h: screen.height || 0,
      language: navigator.language || ''
    };
  }

  function send(body) {
    var json = JSON.stringify(body);

    /* text/plain, not application/json, on purpose: application/json is
     * not a CORS-"simple" content type and would make every event cost
     * an extra OPTIONS round trip. The server parses the body as JSON
     * regardless of the declared type. */
    try {
      if (navigator.sendBeacon) {
        /* sendBeacon survives the page being closed mid-request, which
         * plain fetch does not - it returns false when the payload is
         * over the browser's queue limit, in which case fall through. */
        if (navigator.sendBeacon(endpoint, new Blob([json], { type: 'text/plain' }))) return;
      }
    } catch (e) {}

    try {
      fetch(endpoint, {
        method: 'POST',
        body: json,
        keepalive: true,
        mode: 'cors',
        credentials: 'omit',
        headers: { 'Content-Type': 'text/plain' }
      })['catch'](function () {});
    } catch (e) {}
  }

  var lastURL;

  function pageview() {
    var url = currentURL();
    /* Routers commonly replaceState on the URL they are already on;
     * without this, one navigation would be counted several times. */
    if (url === lastURL) return;
    lastURL = url;
    send(payload('pageview'));
  }

  /* Single-page apps never reload, so history is the only navigation
   * signal there is. */
  function hookHistory(name) {
    var original = history[name];
    if (typeof original !== 'function') return;
    history[name] = function () {
      var result = original.apply(this, arguments);
      /* Deferred a tick: pushState runs before the router has updated
       * document.title, so reading it synchronously would record the
       * previous page's title against the new path. */
      setTimeout(pageview, 0);
      return result;
    };
  }

  hookHistory('pushState');
  hookHistory('replaceState');
  addEventListener('popstate', pageview);

  /* crucible('event', 'signup') raises a named custom event.
   * crucible('pageview') forces one, for routers this script cannot
   * observe. */
  window.crucible = function (type, name) {
    if (type === 'event') {
      if (!name) return;
      send(payload('event', name));
    } else if (type === 'pageview') {
      lastURL = null;
      pageview();
    }
  };

  if (document.readyState === 'loading') {
    /* The script may be in <head> with document.title not yet parsed;
     * waiting costs nothing and avoids recording every page with an
     * empty title. */
    addEventListener('DOMContentLoaded', pageview);
  } else {
    pageview();
  }
})();
