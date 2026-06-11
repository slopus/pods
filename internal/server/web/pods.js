(function (global) {
  "use strict";

  function trimEndpoint(endpoint) {
    return String(endpoint || global.location.origin).replace(/\/+$/, "");
  }

  function encodePath(value) {
    return encodeURIComponent(String(value));
  }

  function normalizeWhere(where) {
    if (!where) return [];
    if (Array.isArray(where)) return where;
    return Object.keys(where).map(function (key) {
      return key + "=" + String(where[key]);
    });
  }

  function Pods(options) {
    options = options || {};
    var endpoint = trimEndpoint(options.endpoint);
    var secret = options.token || options.secret || "";

    function request(method, path, body) {
      var headers = { "Accept": "application/json" };
      if (secret) headers.Authorization = "Bearer " + secret;
      var init = { method: method, headers: headers };
      if (body !== undefined) {
        headers["Content-Type"] = "application/json";
        init.body = JSON.stringify(body);
      }
      return fetch(endpoint + path, init).then(function (res) {
        return res.text().then(function (text) {
          var data = text ? JSON.parse(text) : null;
          if (!res.ok) {
            var err = new Error(data && data.error ? data.error : res.status + " " + res.statusText);
            err.status = res.status;
            err.response = data;
            throw err;
          }
          return data;
        });
      });
    }

    function collection(name) {
      var base = "/api/db/" + encodePath(name);
      return {
        query: function (opts) {
          opts = opts || {};
          var params = new URLSearchParams();
          normalizeWhere(opts.where).forEach(function (w) { params.append("where", w); });
          if (opts.sort) params.set("sort", opts.sort);
          if (opts.limit) params.set("limit", String(opts.limit));
          if (opts.offset) params.set("offset", String(opts.offset));
          var qs = params.toString();
          return request("GET", base + (qs ? "?" + qs : ""));
        },
        create: function (doc) {
          return request("POST", base, doc);
        },
        get: function (id) {
          return request("GET", base + "/" + encodePath(id));
        },
        set: function (id, doc) {
          return request("PUT", base + "/" + encodePath(id), doc);
        },
        patch: function (id, doc) {
          return request("PATCH", base + "/" + encodePath(id), doc);
        },
        delete: function (id) {
          return request("DELETE", base + "/" + encodePath(id));
        },
        remove: function (id) {
          return request("DELETE", base + "/" + encodePath(id));
        },
        drop: function () {
          return request("DELETE", base);
        }
      };
    }

    function events(onEvent, opts) {
      opts = opts || {};
      var controller = new AbortController();
      var headers = { "Accept": "text/event-stream" };
      if (secret) headers.Authorization = "Bearer " + secret;
      fetch(endpoint + "/api/events", { headers: headers, signal: controller.signal }).then(function (res) {
        if (!res.ok) throw new Error(res.status + " " + res.statusText);
        var reader = res.body.getReader();
        var decoder = new TextDecoder();
        var buffer = "";
        function pump() {
          return reader.read().then(function (chunk) {
            if (chunk.done) return;
            buffer += decoder.decode(chunk.value, { stream: true });
            var parts = buffer.split("\n\n");
            buffer = parts.pop();
            parts.forEach(function (part) {
              var data = part.split("\n").filter(function (line) {
                return line.indexOf("data: ") === 0;
              }).map(function (line) {
                return line.slice(6);
              }).join("\n");
              if (data) onEvent(JSON.parse(data));
            });
            return pump();
          });
        }
        return pump();
      }).catch(function (err) {
        if (err.name !== "AbortError" && opts.onerror) opts.onerror(err);
      });
      return { close: function () { controller.abort(); } };
    }

    function authProviders(opts) {
      opts = opts || {};
      var path = "/api/auth/providers";
      if (opts.returnTo) path += "?return_to=" + encodeURIComponent(opts.returnTo);
      return request("GET", path).then(function (res) { return res.providers; });
    }

    return {
      endpoint: endpoint,
      me: function (opts) {
        opts = opts || {};
        return request("GET", "/api/me" + (opts.required ? "?required=1" : ""));
      },
      events: events,
      db: {
        collection: collection,
        collections: function () {
          return request("GET", "/api/db").then(function (res) { return res.collections; });
        }
      },
      sites: function () {
        return request("GET", "/api/sites").then(function (res) { return res.sites; });
      },
      auth: {
        providers: authProviders,
        logout: function () {
          return request("POST", "/api/auth/logout");
        }
      }
    };
  }

  global.Pods = Pods;
})(typeof window !== "undefined" ? window : globalThis);
