const STORAGE = {
  token: "maccy_admin_token",
  email: "maccy_admin_email",
  theme: "maccy_admin_theme"
};

const VIEW_META = {
  users: {
    title: "重点账号队列",
    subtitle: "不是把全部用户堆上来，而是把最需要干预的对象顶到前排。",
    columns: ["用户", "套餐", "存储", "到期", "状态"]
  },
  subscriptions: {
    title: "订阅清单",
    subtitle: "从到期时间和套餐覆盖率看谁真正有同步权限。",
    columns: ["用户", "套餐", "存储", "到期", "状态"]
  },
  cards: {
    title: "卡密库存",
    subtitle: "当前后台已生成的兑换码、有效天数和使用状态。",
    columns: ["卡密", "天数", "使用者", "创建时间", "状态"]
  }
};

let currentView = "users";
let cache = {
  users: [],
  subscriptions: [],
  cards: [],
  auth: null
};

function $(id) {
  return document.getElementById(id);
}

function toast(message) {
  const el = $("toast");
  el.textContent = message;
  el.classList.add("show");
  clearTimeout(window.__toastTimer);
  window.__toastTimer = setTimeout(() => el.classList.remove("show"), 2800);
}

function setTheme(theme) {
  const next = theme === "light" ? "light" : "dark";
  document.body.setAttribute("data-theme", next);
  localStorage.setItem(STORAGE.theme, next);
  $("themeToggle").textContent = next === "dark" ? "切换浅色" : "切换深色";
  $("loginThemeToggle").textContent = next === "dark" ? "切换浅色" : "切换深色";
}

function toggleTheme() {
  setTheme(document.body.getAttribute("data-theme") === "dark" ? "light" : "dark");
}

function getToken() {
  return (localStorage.getItem(STORAGE.token) || "").trim();
}

function setToken(token, email) {
  localStorage.setItem(STORAGE.token, token);
  if (email) {
    localStorage.setItem(STORAGE.email, email);
  }
}

function clearToken() {
  localStorage.removeItem(STORAGE.token);
  localStorage.removeItem(STORAGE.email);
}

async function readJson(response) {
  try {
    return await response.json();
  } catch {
    return {};
  }
}

async function api(path, options = {}) {
  const headers = new Headers(options.headers || {});
  const token = getToken();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  if (!headers.has("Content-Type") && options.body && typeof options.body === "string") {
    headers.set("Content-Type", "application/json");
  }

  const response = await fetch(path, { ...options, headers });
  if (response.status === 401) {
    clearToken();
    showLogin("登录已失效，请重新登录。");
    throw new Error("Unauthorized");
  }
  return response;
}

function showLogin(message = "等待登录") {
  $("loginOverlay").classList.add("visible");
  $("loginHint").textContent = message;
  $("loginExpiry").textContent = "";
  $("adminMeta").textContent = "尚未登录";
}

function hideLogin() {
  $("loginOverlay").classList.remove("visible");
}

function formatBytes(bytes) {
  const value = Number(bytes || 0);
  if (!Number.isFinite(value) || value <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let size = value;
  let index = 0;
  while (size >= 1024 && index < units.length - 1) {
    size /= 1024;
    index += 1;
  }
  return `${size >= 10 || index === 0 ? size.toFixed(0) : size.toFixed(1)} ${units[index]}`;
}

function formatTime(value) {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  return date.toLocaleString("zh-CN", { hour12: false });
}

function isFuture(value) {
  if (!value) return false;
  const time = new Date(value).getTime();
  return Number.isFinite(time) && time > Date.now();
}

function escapeHtml(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll("\"", "&quot;")
    .replaceAll("'", "&#39;");
}

function sortUsersByPriority(users) {
  return [...users].sort((a, b) => {
    const aTier = a.tier ? 0 : 1;
    const bTier = b.tier ? 0 : 1;
    if (aTier !== bTier) return aTier - bTier;
    const aOver = a.over_limit ? 0 : 1;
    const bOver = b.over_limit ? 0 : 1;
    if (aOver !== bOver) return aOver - bOver;
    return Number(b.storage_bytes || 0) - Number(a.storage_bytes || 0);
  });
}

function getUsageTone(item) {
  if (item.over_limit) {
    return { text: "已超限", cls: "warn" };
  }
  const used = Number(item.storage_bytes || 0);
  const limit = Number(item.storage_limit_bytes || 0);
  if (limit > 0 && used / limit >= 0.85) {
    return { text: "接近上限", cls: "warn" };
  }
  return { text: "正常", cls: "ok" };
}

function getUserStatus(user) {
  if (!user.tier) return { text: "未分配套餐，无法同步", cls: "warn" };
  if (user.over_limit) return { text: "已超套餐上限，需要清理备份", cls: "warn" };
  if (!isFuture(user.expires_at)) return { text: "订阅失效，自动同步已停用", cls: "warn" };
  return { text: "稳定，可继续自动同步", cls: "ok" };
}

function renderTableHead(view) {
  $("tableHead").innerHTML = VIEW_META[view].columns.map(text => `<div>${text}</div>`).join("");
  $("tableTitle").textContent = VIEW_META[view].title;
  $("tableSubtitle").textContent = VIEW_META[view].subtitle;
}

function renderRows(view) {
  const body = $("tableBody");
  if (view === "users") {
    const items = sortUsersByPriority(cache.users).slice(0, 8);
    if (!items.length) {
      body.innerHTML = `<div class="table-row"><div class="table-cell muted">暂无用户数据</div></div>`;
      return;
    }
    body.innerHTML = items.map(user => {
      const usage = `${formatBytes(user.storage_bytes)} / ${user.storage_limit_bytes ? formatBytes(user.storage_limit_bytes) : "-"}`;
      const status = getUserStatus(user);
      return `
        <div class="table-row">
          <div class="table-cell strong" data-label="用户">${escapeHtml(user.email || "-")}</div>
          <div class="table-cell mono strong" data-label="套餐">${escapeHtml(user.tier || "-")}</div>
          <div class="table-cell" data-label="存储">${usage}</div>
          <div class="table-cell" data-label="到期">${formatTime(user.expires_at)}</div>
          <div class="table-cell ${status.cls}" data-label="状态">${status.text}</div>
        </div>`;
    }).join("");
    return;
  }

  if (view === "subscriptions") {
    const items = [...cache.subscriptions].sort((a, b) => {
      const aTime = new Date(a.expires_at || 0).getTime();
      const bTime = new Date(b.expires_at || 0).getTime();
      return aTime - bTime;
    }).slice(0, 8);
    if (!items.length) {
      body.innerHTML = `<div class="table-row"><div class="table-cell muted">暂无订阅数据</div></div>`;
      return;
    }
    body.innerHTML = items.map(item => {
      const usageTone = getUsageTone(item);
      const usage = `${formatBytes(item.storage_bytes)} / ${item.storage_limit_bytes ? formatBytes(item.storage_limit_bytes) : "-"}`;
      const status = isFuture(item.expires_at) ? "可同步" : "已过期";
      const statusClass = isFuture(item.expires_at) ? "ok" : "warn";
      return `
        <div class="table-row">
          <div class="table-cell strong" data-label="用户">${escapeHtml(item.email || item.user_id || "-")}</div>
          <div class="table-cell mono strong" data-label="套餐">${escapeHtml(item.tier || "-")}</div>
          <div class="table-cell ${usageTone.cls}" data-label="存储">${usage}</div>
          <div class="table-cell" data-label="到期">${formatTime(item.expires_at)}</div>
          <div class="table-cell ${statusClass}" data-label="状态">${status}</div>
        </div>`;
    }).join("");
    return;
  }

  const items = [...cache.cards].slice(0, 8);
  if (!items.length) {
    body.innerHTML = `<div class="table-row"><div class="table-cell muted">暂无卡密库存</div></div>`;
    return;
  }
  body.innerHTML = items.map(card => `
    <div class="table-row">
      <div class="table-cell mono strong" data-label="卡密">${escapeHtml(card.code || "-")}</div>
      <div class="table-cell mono" data-label="天数">${escapeHtml(String(card.duration_days ?? "-"))}</div>
      <div class="table-cell" data-label="使用者">${escapeHtml(card.used_by || "-")}</div>
      <div class="table-cell" data-label="创建时间">${formatTime(card.created_at)}</div>
      <div class="table-cell ${card.used_by ? "muted" : "ok"}" data-label="状态">${card.used_by ? "已使用" : "未使用"}</div>
    </div>
  `).join("");
}

function renderView(view) {
  currentView = view;
  document.querySelectorAll("[data-view-button]").forEach(button => {
    button.classList.toggle("active", button.dataset.viewButton === view);
  });
  document.querySelectorAll(".nav-item").forEach(button => {
    button.classList.toggle("active", button.dataset.view === view);
  });
  renderSidePanel(view);
  renderTableHead(view);
  renderRows(view);
}

function renderSidePanel(view) {
  const show = (id, visible) => {
    const el = $(id);
    if (el) {
      el.classList.toggle("hidden-panel", !visible);
    }
  };

  show("overviewPanel", view === "users");
  show("cardsPanel", view === "cards");
  show("subscriptionPanel", view === "subscriptions");
  show("passwordPanel", view === "subscriptions");

  const title = $("quickActionsCard").querySelector("h3");
  const desc = $("quickActionsCard").querySelector("p");
  if (view === "users") {
    title.textContent = "高频操作";
    desc.textContent = "把最常用的后台动作放到一列，避免在多个页面和接口之间来回跳转。";
  } else if (view === "cards") {
    title.textContent = "卡密操作";
    desc.textContent = "这一屏只处理卡密生成和库存动作，避免和用户管理混在一起。";
  } else {
    title.textContent = "用户与套餐";
    desc.textContent = "这一屏集中处理续期、套餐变更和密码重置，不把复杂操作堆在首页。";
  }
}

function renderMetrics() {
  const users = cache.users || [];
  const subscriptions = cache.subscriptions || [];
  const tiered = users.filter(item => item.tier).length;
  const storageTotal = users.reduce((sum, item) => sum + Number(item.storage_bytes || 0), 0);
  const activeSubs = subscriptions.filter(item => isFuture(item.expires_at)).length;
  const riskyUsers = users.filter(item => item.over_limit || !item.tier || !isFuture(item.expires_at));

  $("metricUsers").textContent = String(users.length);
  $("metricUsersTag").textContent = users.length ? `已连接 ${users.length}` : "暂无数据";
  $("metricUsersTag").className = `metric-tag ${users.length ? "success" : "warning"}`;

  $("metricTiered").textContent = String(tiered);
  $("metricTieredTag").textContent = tiered ? `${tiered} 个已分配` : "等待分配";
  $("metricTieredTag").className = `metric-tag ${tiered ? "success" : "warning"}`;

  $("metricStorage").textContent = formatBytes(storageTotal);
  $("metricStorageTag").textContent = riskyUsers.some(item => item.over_limit) ? "存在超限" : "正常";
  $("metricStorageTag").className = `metric-tag ${riskyUsers.some(item => item.over_limit) ? "warning" : "success"}`;

  $("metricSubs").textContent = String(activeSubs);
  $("metricSubsTag").textContent = `${activeSubs} 个有效`;
  $("metricSubsTag").className = `metric-tag ${activeSubs ? "success" : "warning"}`;

  const riskyCount = riskyUsers.length;
  $("statusPillText").textContent = riskyCount ? "需要人工关注" : "实时健康中";
  $("statusBannerText").textContent = riskyCount
    ? `当前共有 ${riskyCount} 个账号需要处理，优先关注套餐缺失、订阅过期或容量超限对象。`
    : "近 24 小时上传成功率稳定，当前没有超限用户，备份裁剪策略处于正常工作状态。";
}

function renderRisks() {
  const root = $("riskList");
  const users = sortUsersByPriority(cache.users).filter(item => item.over_limit || !item.tier || !isFuture(item.expires_at)).slice(0, 4);
  if (!users.length) {
    root.innerHTML = `
      <div class="risk-item">
        <strong>当前无高风险账号</strong>
        <p>所有已展示用户都处于可同步状态，可以继续做回归测试或批量发卡。</p>
      </div>`;
    return;
  }
  root.innerHTML = users.map(user => {
    let title = "需要处理";
    let detail = "请打开用户详情确认当前同步状态。";
    if (!user.tier) {
      title = "缺少套餐分配";
      detail = `${user.email} 目前没有套餐，客户端登录后也无法同步。`;
    } else if (!isFuture(user.expires_at)) {
      title = "订阅已过期";
      detail = `${user.email} 当前订阅已失效，需要续期后才能恢复云同步。`;
    } else if (user.over_limit) {
      title = "容量已经超限";
      detail = `${user.email} 当前使用 ${formatBytes(user.storage_bytes)}，建议先清理旧备份。`;
    }
    return `
      <div class="risk-item">
        <strong>${escapeHtml(title)}</strong>
        <p>${escapeHtml(detail)}</p>
      </div>`;
  }).join("");
}

async function loadAuthState() {
  const response = await api("/admin/auth/me");
  const data = await readJson(response);
  cache.auth = data;
  $("adminName").textContent = data.email || localStorage.getItem(STORAGE.email) || "Baymax Ops";
  $("adminMeta").textContent = data.authType === "legacy-key"
    ? "旧密钥兼容模式"
    : (data.accessTokenExpiresAt ? `有效至 ${formatTime(data.accessTokenExpiresAt)}` : "已登录");
  hideLogin();
}

async function loadDashboard() {
  const [usersRes, subscriptionsRes, cardsRes] = await Promise.all([
    api("/admin/users/list"),
    api("/admin/subscriptions/list"),
    api("/admin/cards/list")
  ]);

  const usersData = await readJson(usersRes);
  const subscriptionsData = await readJson(subscriptionsRes);
  const cardsData = await readJson(cardsRes);

  cache.users = Array.isArray(usersData.users) ? usersData.users : [];
  cache.subscriptions = Array.isArray(subscriptionsData.subscriptions) ? subscriptionsData.subscriptions : [];
  cache.cards = Array.isArray(cardsData.cards) ? cardsData.cards : [];

  renderMetrics();
  renderView(currentView);
  renderRisks();
}

async function refreshAll() {
  await loadAuthState();
  await loadDashboard();
}

async function login(event) {
  event.preventDefault();
  const email = $("adminEmail").value.trim();
  const password = $("adminPassword").value;
  $("loginHint").textContent = "正在登录…";

  const response = await fetch("/admin/auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email, password })
  });
  const data = await readJson(response);
  if (!response.ok) {
    $("loginHint").textContent = data.error || "登录失败";
    throw new Error(data.error || "登录失败");
  }

  setToken(data.accessToken, data.email || email);
  $("loginExpiry").textContent = data.accessTokenExpiresAt ? `Token 至 ${formatTime(data.accessTokenExpiresAt)}` : "";
  $("loginHint").textContent = "登录成功，正在载入后台…";
  await refreshAll();
  toast("后台登录成功");
}

function requireTarget() {
  const value = $("upgradeTarget").value.trim();
  if (!value) {
    toast("请先输入用户邮箱或用户 ID");
    return null;
  }
  return value;
}

async function generateCards() {
  const count = Number($("cardCount").value || 1);
  const durationDays = Number($("cardDays").value || 30);
  const response = await api("/admin/card/generate", {
    method: "POST",
    body: JSON.stringify({ count, durationDays })
  });
  const data = await readJson(response);
  if (!response.ok) {
    throw new Error(data.error || "生成卡密失败");
  }
  $("cardResult").innerHTML = (data.codes || []).map(code => `<div class="mono strong">${escapeHtml(code)}</div>`).join("");
  await refreshAll();
  renderView("cards");
  toast("卡密生成成功");
}

async function assignProTierQuickly() {
  const users = cache.users || [];
  const target = users.find(item => !item.tier) || users[0];
  if (!target) {
    toast("当前没有可分配套餐的用户");
    return;
  }
  const response = await api("/admin/user/tier", {
    method: "POST",
    body: JSON.stringify({ emailOrUserId: target.email || target.id, tier: "Pro" })
  });
  const data = await readJson(response);
  if (!response.ok) {
    throw new Error(data.error || "快速分配失败");
  }
  await refreshAll();
  toast(`已为 ${target.email || target.id} 分配 Pro 套餐`);
}

async function upgradeUser() {
  const emailOrUserId = requireTarget();
  if (!emailOrUserId) return;
  const durationDays = Number($("upgradeDays").value || 0);
  const tier = $("upgradeTier").value || null;
  const response = await api("/admin/user/upgrade", {
    method: "POST",
    body: JSON.stringify({ emailOrUserId, durationDays, tier })
  });
  const data = await readJson(response);
  if (!response.ok) {
    throw new Error(data.error || "升级失败");
  }
  await refreshAll();
  renderView("users");
  toast("用户订阅和套餐已更新");
}

async function setTierOnly() {
  const emailOrUserId = requireTarget();
  if (!emailOrUserId) return;
  const tier = $("upgradeTier").value || null;
  if (!tier) {
    toast("请先选择套餐档位");
    return;
  }
  const response = await api("/admin/user/tier", {
    method: "POST",
    body: JSON.stringify({ emailOrUserId, tier })
  });
  const data = await readJson(response);
  if (!response.ok) {
    throw new Error(data.error || "设置套餐失败");
  }
  await refreshAll();
  toast("套餐设置成功");
}

async function clearTier() {
  const emailOrUserId = requireTarget();
  if (!emailOrUserId) return;
  const response = await api("/admin/user/tier", {
    method: "POST",
    body: JSON.stringify({ emailOrUserId, tier: null })
  });
  const data = await readJson(response);
  if (!response.ok) {
    throw new Error(data.error || "清除套餐失败");
  }
  await refreshAll();
  toast("套餐已清除");
}

async function resetPassword() {
  const emailOrUserId = requireTarget();
  if (!emailOrUserId) return;
  const newPassword = $("resetPassword").value;
  if (!newPassword || newPassword.length < 8) {
    toast("新密码至少需要 8 位");
    return;
  }
  const response = await api("/admin/user/reset-password", {
    method: "POST",
    body: JSON.stringify({ emailOrUserId, newPassword })
  });
  const data = await readJson(response);
  if (!response.ok) {
    throw new Error(data.error || "重置密码失败");
  }
  toast("用户密码已重置，旧 refresh token 已失效");
}

function handleError(error) {
  const message = error instanceof Error ? error.message : String(error || "请求失败");
  if (message !== "Unauthorized") {
    toast(message);
  }
}

function bindEvents() {
  $("themeToggle").addEventListener("click", toggleTheme);
  $("loginThemeToggle").addEventListener("click", toggleTheme);
  $("refreshButton").addEventListener("click", async () => {
    try {
      await refreshAll();
      toast("数据已刷新");
    } catch (error) {
      handleError(error);
    }
  });
  $("generateTopButton").addEventListener("click", async () => {
    try {
      await generateCards();
    } catch (error) {
      handleError(error);
    }
  });
  $("logoutButton").addEventListener("click", () => {
    clearToken();
    showLogin("已退出登录");
    toast("已退出后台");
  });
  $("loginForm").addEventListener("submit", async (event) => {
    try {
      await login(event);
    } catch (error) {
      handleError(error);
    }
  });
  $("generateButton").addEventListener("click", async () => {
    try {
      await generateCards();
    } catch (error) {
      handleError(error);
    }
  });
  $("generateButtonAlt").addEventListener("click", async () => {
    try {
      await generateCards();
    } catch (error) {
      handleError(error);
    }
  });
  $("setProButton").addEventListener("click", async () => {
    try {
      await assignProTierQuickly();
    } catch (error) {
      handleError(error);
    }
  });
  $("upgradeButton").addEventListener("click", async () => {
    try {
      await upgradeUser();
    } catch (error) {
      handleError(error);
    }
  });
  $("setTierButton").addEventListener("click", async () => {
    try {
      await setTierOnly();
    } catch (error) {
      handleError(error);
    }
  });
  $("clearTierButton").addEventListener("click", async () => {
    try {
      await clearTier();
    } catch (error) {
      handleError(error);
    }
  });
  $("resetPasswordButton").addEventListener("click", async () => {
    try {
      await resetPassword();
    } catch (error) {
      handleError(error);
    }
  });
  document.querySelectorAll("[data-view-button], .nav-item").forEach(button => {
    button.addEventListener("click", () => renderView(button.dataset.viewButton || button.dataset.view));
  });
}

async function bootstrap() {
  setTheme(localStorage.getItem(STORAGE.theme) || "dark");
  bindEvents();
  renderView("users");
  renderRisks();

  if (!getToken()) {
    showLogin("请输入管理员账号密码");
    return;
  }

  try {
    await refreshAll();
  } catch (error) {
    handleError(error);
  }
}

bootstrap();
