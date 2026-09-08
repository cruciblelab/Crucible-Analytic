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
 *   crucible.optOut()      // stops sending, remembers the choice
 *   crucible.optIn()       // undoes it
 *   crucible.status()      // 'out' or 'in'
 *
 * Show what is measured, inside your own privacy page, by putting an
 * anchor anywhere in it:
 *   <div data-crucible-privacy></div>
 * The script fills that with the disclosure and draws nothing when it is
 * absent. The same text is at <prefix>/privacy.html as a page of its
 * own, and the facts behind it are JSON at <prefix>/privacy for a site
 * that would rather write its own wording. See fillDisclosure below.
 *
 * The underlying flag is localStorage's 'crucible.disabled', which is
 * what it has always been - setting it by hand still works and still
 * means the same thing. The three calls exist because that sentence was
 * only ever written in this comment, in the README and in the data
 * inventory, and a visitor to somebody's shop reads none of the three.
 *
 * They are calls rather than a banner of our own on purpose: the site
 * already has a cookie banner or a consent platform, and a second box
 * beside it helps nobody. The site wires its own switch to these.
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

  /* The opt-out state, and the reason this is a variable rather than an
   * early return.
   *
   * It used to be one: the whole script stopped before defining
   * window.crucible. That worked for the flag and cannot work for the
   * calls - a visitor who had opted out would have no optIn() to call,
   * and a consent banner asking status() would get a TypeError on
   * exactly the browsers where the answer matters most. So the script
   * always finishes loading, always defines its function, and what the
   * choice decides is whether anything is *sent*.
   *
   * localStorage throws outright in sandboxed iframes and in browsers
   * with storage disabled, so no read or write of it is unguarded. A
   * browser that cannot store the choice still honours it for the life
   * of the page; see optOut. */
  var disabled = false;
  try {
    disabled = !!localStorage.getItem('crucible.disabled');
  } catch (e) {}

  /* remember writes the choice, and says whether it stuck.
   *
   * The return value is the honest half of this pair: a banner that
   * drew a switch needs to know the switch will still be there
   * tomorrow, and in a sandboxed iframe it will not be. status() stays
   * a statement about behaviour - what this page will do - and this
   * stays a statement about durability. One value trying to carry both
   * would be wrong about one of them. */
  function remember(on) {
    try {
      if (on) localStorage.setItem('crucible.disabled', '1');
      else localStorage.removeItem('crucible.disabled');
      return true;
    } catch (e) {
      return false;
    }
  }

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
    /* The one gate, and it is here rather than at each caller for the
     * reason this project applies everywhere else: four guards are four
     * places for the fifth caller to be forgotten. Every path that
     * reaches the network - the load pageview, the history hooks, the
     * popstate listener, crucible('event') - comes through here. */
    if (disabled) return;

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

  /* The three visitor-facing calls, added as properties of the function
   * rather than replacing it.
   *
   * crucible is a *function* in every site that already embeds this, and
   * crucible('event', 'signup') sits in their pages today. JavaScript
   * lets a function carry properties; using that is the difference
   * between adding three calls and breaking every existing embed, and it
   * is one line either way. */

  /* status() is what this page will do, not what is stored.
   *
   * A banner asks it to draw its switch in the right position, so the
   * answer has to be about behaviour: in a browser that cannot store
   * anything, a visitor who pressed the switch is opted out for this
   * page and status() says so. */
  window.crucible.status = function () {
    return disabled ? 'out' : 'in';
  };

  /* optOut() takes effect immediately and returns whether the choice
   * was persisted.
   *
   * Immediately, because the visitor asked now: a browser that cannot
   * remember the decision must still stop sending for the rest of this
   * page rather than doing nothing at all. */
  window.crucible.optOut = function () {
    disabled = true;
    return remember(true);
  };

  /* optIn() undoes it, and does not send anything by itself.
   *
   * The next navigation is counted; this one is not. Firing a pageview
   * from here would record a visit the visitor had already declined to
   * report, and a site that does want the current page counted has
   * crucible('pageview') for exactly that. */
  window.crucible.optIn = function () {
    disabled = false;
    return remember(false);
  };

  /* The embedded disclosure.
   *
   * A site that wants "what is measured here" inside its own privacy
   * page puts an anchor in it:
   *
   *   <div data-crucible-privacy></div>
   *
   * and this fills it. No anchor, nothing drawn - the script never adds
   * anything to a page that did not ask, which is the difference between
   * a feature and a widget nobody consented to.
   *
   * # Why a frame rather than text written here
   *
   * Because the sentences would then be here as well as on the page,
   * and the whole point of that page is that it is derived from the
   * setting in force at that moment. A copy in this file would keep
   * saying "masked" after somebody switched to full precision, and it
   * would be the copy the visitor is looking at. An invariant test
   * refuses a second copy of the prose anywhere in the repository; this
   * is the shape that obeys it rather than the shape that has to be
   * excused from it.
   *
   * It also means nothing is injected into the site's DOM. The frame
   * carries a document from our origin, under its own strict policy;
   * the alternative - fetching and inserting markup - would put a
   * response from one origin into another origin's page, which is the
   * shape of every XSS advisory ever written.
   *
   * The costs, plainly: the frame does not inherit the site's fonts, and
   * a site with a strict Content-Security-Policy needs frame-src for
   * this origin - the same origin it already allows for the script. A
   * site that would rather have neither reads <prefix>/privacy and
   * renders the facts itself, which is what that endpoint is for.
   *
   * The sandbox is everything off except one thing: a click by the
   * visitor may navigate the top page. The disclosure carries a link to
   * the operator's own policy page, and a link that silently does
   * nothing is worse than no link. Scripts, forms, storage and
   * downloads stay off - the page uses none of them.
   *
   * This runs for a visitor who has opted out, deliberately. The frame
   * is a page they asked to read, not a measurement: no event is sent
   * for it and none is recorded. Withholding the explanation from the
   * people who went looking for the switch would be the wrong way
   * round. */
  function fillDisclosure() {
    var slots = document.querySelectorAll('[data-crucible-privacy]');
    if (!slots.length) return;

    /* Derived from this script's own src, exactly like the event
     * endpoint: ".../_ca/ca.js" becomes ".../_ca/privacy.html". A site
     * that moved the prefix, or put the beacon on a subdomain, gets the
     * right URL without a second attribute to keep in step. */
    var src = script.src.replace(/[^/]*$/, '') + 'privacy.html';

    for (var i = 0; i < slots.length; i++) {
      var slot = slots[i];
      /* Two copies of the snippet on one page is a mistake, not a
       * request for two frames. */
      if (slot.getAttribute('data-crucible-filled')) continue;
      slot.setAttribute('data-crucible-filled', '1');

      var frame = document.createElement('iframe');
      frame.setAttribute('sandbox', 'allow-top-navigation-by-user-activation');
      frame.setAttribute('referrerpolicy', 'no-referrer');
      frame.setAttribute('loading', 'lazy');
      /* The frame needs an accessible name and this script must not be
       * the place a sentence about privacy is written, so the site
       * supplies it in its own language, and the fallback is the
       * product's name rather than a claim. */
      frame.setAttribute('title', slot.getAttribute('data-title') || 'Crucible Analytic');
      frame.style.width = '100%';
      frame.style.border = '0';
      /* A frame cannot size itself to its content across origins, so
       * there is a default and the site can override it - on the anchor
       * for one page, or in its own stylesheet for all of them. */
      frame.style.height = slot.getAttribute('data-height') || '34rem';
      frame.src = src;
      slot.appendChild(frame);
    }
  }

  if (document.readyState === 'loading') {
    /* The script may be in <head> with document.title not yet parsed;
     * waiting costs nothing and avoids recording every page with an
     * empty title. Also the point at which the anchor below exists: a
     * script in <head> runs before the body it is looking for. */
    addEventListener('DOMContentLoaded', function () {
      pageview();
      fillDisclosure();
    });
  } else {
    pageview();
    fillDisclosure();
  }
})();
