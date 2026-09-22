package main

// indexHTML 登录页：本地渲染，内嵌 Cloudflare Turnstile（站点密钥取自官方前端）。
const indexHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>verdent2api · 登录</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
         max-width: 460px; margin: 8vh auto; padding: 0 20px; }
  h1 { font-size: 20px; margin: 0 0 4px; }
  p.sub { color: #888; margin: 0 0 24px; font-size: 13px; }
  label { display:block; font-size: 12px; color:#888; margin: 14px 0 6px; }
  input { width:100%; box-sizing:border-box; padding:10px 12px; border-radius:8px;
          border:1px solid #444; background:transparent; color:inherit; font-size:14px; }
  input:focus { outline:2px solid #3b82f6; outline-offset:1px; }
  .cf { margin: 20px 0; min-height: 65px; display:flex; justify-content:center; }
  button { width:100%; padding:12px; border:0; border-radius:8px; font-size:15px;
           background:#16a34a; color:#fff; cursor:pointer; }
  button:disabled { background:#555; cursor:not-allowed; }
  pre { white-space:pre-wrap; word-break:break-all; background:#111; color:#9fe6a8;
        padding:12px; border-radius:8px; font-size:12px; }
  .err { color:#f87171; font-size:13px; margin-top:12px; }
  .ok  { color:#34d399; }
  .hide{ display:none; }
</style>
<script>
// URL 重写层：把页面内所有 challenges.cloudflare.com 的请求改走本地反代。
// 这样国内浏览器只需连本服务，Turnstile 资源全部由本服务（国外机器）代取。
(function () {
  const CF = "https://challenges.cloudflare.com";
  const PROXY = "/__cf";
  function rewrite(u) {
    if (typeof u !== "string") return u;
    if (u.indexOf(CF) === 0) return PROXY + u.slice(CF.length);
    return u;
  }
  // 1) fetch
  const origFetch = window.fetch;
  window.fetch = function (input, init) {
    if (typeof input === "string") input = rewrite(input);
    else if (input && input.url) input = new Request(rewrite(input.url), input);
    return origFetch.call(this, input, init);
  };
  // 2) XMLHttpRequest
  const origOpen = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function (method, url) {
    return origOpen.apply(this, [method, rewrite(url)].concat([].slice.call(arguments, 2)));
  };
  // 3) 动态 script / img / iframe 的 src
  // 同时做双向映射：setter 改走代理，getter 返回原始 URL——
  // Turnstile 内部会校验 script.src 是否来自 challenges.cloudflare.com。
  const proto = ["HTMLScriptElement", "HTMLImageElement", "HTMLIFrameElement"];
  proto.forEach(function (name) {
    const C = window[name];
    if (!C || !C.prototype) return;
    const desc = Object.getOwnPropertyDescriptor(C.prototype, "src");
    if (!desc || !desc.set) return;
    Object.defineProperty(C.prototype, "src", {
      configurable: true,
      get: function () {
        const orig = this.dataset && this.dataset.__cfOrig;
        return orig || desc.get.call(this);
      },
      set: function (v) {
        const rewritten = rewrite(v);
        if (rewritten !== v) {
          this.dataset = this.dataset || {};
          this.dataset.__cfOrig = v;
        }
        desc.set.call(this, rewritten);
      },
    });
  });
  // 4) createElement 后立即设置 src 的场景（setAttribute 路径）
  const origCreate = document.createElement;
  document.createElement = function (tag) {
    const el = origCreate.apply(document, arguments);
    const lower = String(tag || "").toLowerCase();
    if (lower === "script" || lower === "img" || lower === "iframe") {
      const origSetAttr = el.setAttribute;
      el.setAttribute = function (n, v) {
        if (n && n.toLowerCase() === "src") v = rewrite(v);
        return origSetAttr.call(this, n, v);
      };
    }
    return el;
  };

  // 5) 动态注入 Turnstile 加载脚本。
  // 这里必须写原始 challenges URL：经过上面的 setter 改写后，
  // 实际请求走 /__cf 代理，而 script.src getter 仍返回原始 URL，
  // 满足 Turnstile 对加载来源的校验。
  const ts = document.createElement("script");
  ts.src = "https://challenges.cloudflare.com/turnstile/v0/api.js?onload=onloadTurnstileCallback&render=explicit";
  ts.async = true;
  ts.defer = true;
  document.head.appendChild(ts);

  // 6) postMessage origin 双向伪装。
  // Turnstile 的 widget iframe 通过本代理加载后，其 origin 变成本服务：
  //   a) 父页收消息时校验 event.origin —— 伪装回 challenges 域；
  //   b) 发消息时写 targetOrigin=challenges 域 —— 浏览器会因目标 origin
  //      不符直接拒绝，必须改写为 "*" 才能送达。
  // 注意：window 上有 postMessage 的 own property（遮蔽原型），所以
  // 原型与 window 实例两处都必须 hook，否则 Turnstile 调用 window.postMessage
  // 时仍走未改写的原始实现。
  const CF_ORIGIN = "https://challenges.cloudflare.com";
  try {
    const origOriginDesc = Object.getOwnPropertyDescriptor(MessageEvent.prototype, "origin");
    if (origOriginDesc && origOriginDesc.get) {
      const realGet = origOriginDesc.get;
      Object.defineProperty(MessageEvent.prototype, "origin", {
        configurable: true,
        get: function () {
          const real = realGet.call(this);
          if (real && real.indexOf(location.origin) === 0) return CF_ORIGIN;
          return real;
        },
      });
    }
    // 原型层（覆盖 iframe.contentWindow.postMessage 的查找路径）
    const origPM = Window.prototype.postMessage;
    Window.prototype.postMessage = function (message, targetOrigin) {
      if (targetOrigin === CF_ORIGIN) {
        const args = Array.prototype.slice.call(arguments);
        args[1] = "*";
        return origPM.apply(this, args);
      }
      return origPM.apply(this, arguments);
    };
    // 实例层（window 自身的 own property，Turnstile 实际调用的是这层）
    const origWinPM = window.postMessage;
    window.postMessage = function (message, targetOrigin) {
      if (targetOrigin === CF_ORIGIN) {
        const args = Array.prototype.slice.call(arguments);
        args[1] = "*";
        return origWinPM.apply(this, args);
      }
      return origWinPM.apply(this, arguments);
    };
  } catch (e) {}
})();
</script>
</head>
<body>
  <h1>Verdent 账号登录</h1>
  <p class="sub">登录成功后自动创建 api.verdent.ai 的 API key 并写入本地凭证文件（仅本机，不外发）。</p>

  <label for="email">邮箱</label>
  <input id="email" type="email" autocomplete="username" placeholder="you@example.com"/>

  <label for="password">密码</label>
  <input id="password" type="password" autocomplete="current-password" placeholder="••••••••"/>

  <div class="cf" id="ts"></div>

  <button id="btn" disabled>先完成人机验证</button>
  <div class="err hide" id="err"></div>
  <div class="hide" id="resultWrap">
    <p class="sub ok">创建成功！把下面的 key 配置到网关，或直接留在本文件里：</p>
    <pre id="result"></pre>
  </div>

<script>
const TS_SITE = "` + turnstileSiteKey + `";
let tsToken = "";
const btn = document.getElementById("btn");
const errBox = document.getElementById("err");

function setErr(msg) {
  if (!msg) { errBox.className = "err hide"; errBox.textContent = ""; return; }
  errBox.className = "err"; errBox.textContent = msg;
}

// Turnstile 通过 ?onload= 参数回调此全局函数。
function onloadTurnstileCallback() {
  if (!window.turnstile) return;
  window.turnstile.render("#ts", {
    sitekey: TS_SITE,
    callback: function (t) {
      tsToken = t;
      btn.disabled = false;
      btn.textContent = "登录并创建 API key";
      setErr("");
    },
    "expired-callback": function () {
      tsToken = ""; btn.disabled = true; btn.textContent = "验证已过期，请重试";
    },
    "error-callback": function () {
      tsToken = ""; btn.disabled = true; setErr("人机验证加载失败，检查网络后刷新");
    }
  });
}

btn.addEventListener("click", async function () {
  const email = document.getElementById("email").value.trim();
  const password = document.getElementById("password").value;
  if (!email || !password) { setErr("请输入邮箱和密码"); return; }
  if (!tsToken) { setErr("请先完成人机验证"); return; }

  btn.disabled = true; btn.textContent = "正在登录…"; setErr("");
  try {
    const resp = await fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ email, password, token: tsToken })
    });
    const data = await resp.json().catch(() => ({ message: "非法响应" }));
    if (resp.ok && data.ok) {
      btn.textContent = "完成";
      document.getElementById("resultWrap").className = "";
      document.getElementById("result").textContent = data.api_key || "(无)";
      setErr("");
    } else {
      btn.disabled = false; btn.textContent = "重试";
      setErr(data.message || ("HTTP " + resp.status));
    }
  } catch (e) {
    btn.disabled = false; btn.textContent = "重试";
    setErr(String(e && e.message || e));
  }
});
</script>
</body></html>`
