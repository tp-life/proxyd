/** 浏览器回归自建隔离守护进程，覆盖导航、终端生命周期、诊断及配置恢复，不连接用户实例。 */
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const net = require('node:net');
const crypto = require('node:crypto');
const { spawn } = require('node:child_process');

/** availablePort 获取一个空闲回环端口；无参数，返回 Promise<number>；监听失败时拒绝。 */
async function availablePort() {
  const listener = net.createServer();
  await new Promise((resolve, reject) => { listener.once('error', reject); listener.listen(0, '127.0.0.1', resolve); });
  const port = listener.address().port;
  await new Promise((resolve) => listener.close(resolve));
  return port;
}

/** main 构建临时配置并执行真实浏览器流程；无参数，返回 Promise<void>；失败保留日志与截图目录。 */
async function main() {
  if (!process.env.PLAYWRIGHT_MODULE || !process.env.CHROMIUM_PATH) throw Error('请设置 PLAYWRIGHT_MODULE 和 CHROMIUM_PATH，见 docs/reliability.md');
  const { chromium } = require(process.env.PLAYWRIGHT_MODULE);
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'proxyd-web-reliability-'));
  fs.mkdirSync(path.join(root, 'home'));
  const port = await availablePort();
  const secret = crypto.randomBytes(24).toString('hex');
  const config = path.join(root, 'config.yaml');
  fs.writeFileSync(config, `proxy-disabled: true\napi-listen: 127.0.0.1:${port}\napi-secret: ${secret}\nstate-dir: ${JSON.stringify(path.join(root, 'state'))}\ncheck-updates: false\nremote:\n  enabled: false\n  web-terminal: true\n`, { mode: 0o600 });
  const base = `http://127.0.0.1:${port}`;
  const child = spawn(path.resolve(process.env.PROXYD_BINARY || 'bin/proxyd'), ['serve', '-c', config], { env: { ...process.env, ZDOTDIR: path.join(root, 'home') }, stdio: ['ignore', 'pipe', 'pipe'] });
  let log = ''; let startupError = null;
  child.on('error', (error) => { startupError = error; });
  child.stdout.on('data', (data) => { log = (log + data).slice(-32000); });
  child.stderr.on('data', (data) => { log = (log + data).slice(-32000); });
  let browser;
  try {
    let ready = false;
    for (let attempt = 0; attempt < 100; attempt++) {
      if (startupError) throw startupError;
      if (child.exitCode !== null) throw Error(`测试实例启动失败: ${log}`);
      try { ready = (await fetch(`${base}/healthz`, { signal: AbortSignal.timeout(200), headers: { Authorization: `Basic ${Buffer.from(`proxyd:${secret}`).toString("base64")}` } })).ok; } catch {}
      if (ready) break;
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    if (!ready) throw Error('测试实例启动超时');
    browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH, headless: true });
    const context = await browser.newContext({ httpCredentials: { username: 'proxyd', password: secret }, viewport: { width: 1440, height: 1000 } });
    const page = await context.newPage(); const errors = []; const sockets = [];
    page.on('pageerror', (error) => errors.push(error.message));
    page.on('websocket', (socket) => { if (socket.url().includes('/remote/terminal')) sockets.push(socket); });
    // 纯远程实例默认进入全局总览，禁用代理的卡片与侧边栏都不应出现。
    await page.goto(`${base}/`);
    await page.getByRole('heading', { name: '总览', exact: true }).waitFor();
    await page.getByRole('article', { name: '远程访问摘要' }).waitFor();
    await page.getByText('隧道服务端', { exact: true }).waitFor();
    if (await page.getByRole('article', { name: '代理摘要' }).count()) throw Error('纯远程总览仍显示代理卡片');
    if (await page.locator('#app-sidebar').count()) throw Error('全局总览不应展示侧栏');
    await page.screenshot({ path: path.join(root, 'dashboard-remote-only.png'), fullPage: true });
    await page.goto(`${base}/#/remote/services`);
    await page.getByRole('heading', { name: '本机服务', exact: true }).waitFor();
    const nav = page.getByRole('navigation', { name: '当前大类子菜单' });
    const top = page.getByRole('navigation', { name: '业务大类' });
    await top.getByRole('button', { name: '远程访问', exact: true }).waitFor();
    if (await nav.getByRole('button').count() !== 6) throw Error('远程子菜单数量错误');
    for (const label of ['访问授权', '端口转发', '连接审计', '设备与连接', '本机服务']) {
      await nav.getByRole('button', { name: label, exact: true }).click();
      await page.getByRole('heading', { name: label, exact: true }).waitFor();
    }
    // 禁用代理时，顶部和搜索都不能出现该模块入口或操作；系统模块管理必须仍然可达。
    if (await top.getByRole('button', { name: '代理', exact: true }).count()) throw Error('禁用代理仍显示顶部入口');
    await page.getByRole('button', { name: '打开命令菜单', exact: true }).click();
    const palette = page.getByRole('dialog', { name: '命令菜单', exact: true });
    for (const label of ['代理节点', '代理设置', '刷新订阅', '测速']) {
      if (await palette.getByRole('button', { name: new RegExp(label) }).count()) throw Error('禁用代理仍显示命令：' + label);
    }
    if (!(await palette.getByRole('button', { name: /模块管理/ }).count())) throw Error('重新启用入口丢失');
    await page.keyboard.press('Escape');
    await page.getByRole('button', { name: '禁用模块', exact: true }).click();
    await page.getByRole('alertdialog').getByRole('button', { name: '禁用模块', exact: true }).click();
    await page.getByRole('heading', { name: '模块管理', exact: true }).waitFor();
    if (await top.getByRole('button', { name: '远程访问', exact: true }).count()) throw Error('停用当前模块后入口未隐藏');
    const remoteCard = page.locator('section').filter({ has: page.getByRole('heading', { name: '远程访问', exact: true }) });
    await remoteCard.getByRole('button', { name: '启用模块', exact: true }).click();
    await top.getByRole('button', { name: '远程访问', exact: true }).waitFor();
    await top.getByRole('button', { name: '远程访问', exact: true }).click();
    await nav.getByRole('button', { name: '本机服务', exact: true }).click();
    await page.getByRole('button', { name: '打开终端', exact: true }).click();
    await page.locator('.terminal-dialog').getByText('已连接', { exact: true }).waitFor({ timeout: 15000 });
    await page.getByRole('button', { name: '最小化 Web Terminal', exact: true }).click();
    await top.getByRole('button', { name: '总览', exact: true }).click();
    await page.getByRole('heading', { name: '总览', exact: true }).waitFor();
    if (sockets.length !== 1 || sockets[0].isClosed()) throw Error('切换总览中断终端会话');
    await top.getByRole('button', { name: '系统', exact: true }).click();
    await page.getByRole('heading', { name: '模块管理', exact: true }).waitFor();
    await page.getByRole('button', { name: '恢复 Web Terminal', exact: true }).click();
    await page.locator('.terminal-dialog').getByText('已连接', { exact: true }).waitFor();
    if (sockets.length !== 1 || sockets[0].isClosed()) throw Error('最小化恢复没有复用原连接');
    await page.getByRole('button', { name: '最小化 Web Terminal', exact: true }).click();
    // 从服务端终止已最小化会话，验证停用通知仍能更新右下角状态，恢复不会偷偷重连。
    const response = await context.request.post(`${base}/api/remote/web-terminal`, { data: { enabled: false } });
    if (!response.ok()) throw Error(`测试停用终端失败: ${await response.text()}`);
    await page.locator('.terminal-dock').getByText(/已断开/).waitFor();
    await page.getByRole('button', { name: '恢复 Web Terminal', exact: true }).click();
    await page.locator('.terminal-dialog').getByText('已断开', { exact: true }).waitFor();
    if (sockets.length !== 1) throw Error('恢复已断开的会话时意外重连');
    await page.getByRole('button', { name: '关闭 Web Terminal', exact: true }).click();
    const off = await context.request.post(`${base}/api/modules/remote`, { data: { enabled: false } });
    if (!off.ok()) throw Error('测试停用远程模块失败');
    await top.getByRole('button', { name: '远程访问', exact: true }).waitFor({ state: 'hidden' });
    // 地址栏、旧书签与历史导航均不能继续进入禁用模块，包括同属远程访问的桌面页面。
    for (const view of ['remote', 'desktop', 'nodes', 'proxy/overview', 'proxy/settings']) {
      await page.evaluate((value) => { window.location.hash = `/${value}`; }, view);
      await page.waitForFunction(() => window.location.hash === '#/modules');
      await page.getByRole('heading', { name: '模块管理', exact: true }).waitFor();
    }
    await page.goBack();
    await page.getByRole('heading', { name: '模块管理', exact: true }).waitFor();
    await page.reload();
    await page.getByRole('heading', { name: '模块管理', exact: true }).waitFor();
    await page.waitForFunction(() => document.querySelector('[aria-label="业务大类"]').querySelectorAll('button').length === 2);
    await top.getByRole('button', { name: '总览', exact: true }).click();
    await page.getByRole('heading', { name: '尚未启用功能模块', exact: true }).waitFor();
    if (await page.getByRole('article').count()) throw Error('全部禁用时仍展示模块卡片');
    await page.setViewportSize({ width: 390, height: 844 });
    if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw Error('总览移动端水平溢出');
    await page.screenshot({ path: path.join(root, 'dashboard-empty-mobile.png'), fullPage: true });
    await page.getByRole('button', { name: '前往模块管理', exact: true }).click();
    await page.getByRole('button', { name: '打开导航', exact: true }).click();
    if (await nav.getByRole('button', { name: '本机服务', exact: true }).count()) throw Error('移动端显示禁用模块子菜单');
    await nav.getByRole('button', { name: '模块管理', exact: true }).click();
    await page.setViewportSize({ width: 1440, height: 1000 });
    await nav.getByRole('button', { name: '通用设置', exact: true }).click();
    await page.getByRole('heading', { name: '通用设置', exact: true }).waitFor();
    await page.getByRole('switch', { name: '启动时检查新版本', exact: true }).waitFor();
    await page.waitForFunction(() => !document.querySelector('[role="switch"][aria-label="启动时检查新版本"]')?.disabled);
    if (await page.getByRole('heading', { name: '主代理入口', exact: true }).count()) throw Error('通用设置混入代理设置');
    await page.getByRole('navigation', { name: '设置分组快速跳转' }).getByRole('button', { name: '维护与备份', exact: true }).click();
    if (new URL(page.url()).hash !== '#/settings') throw Error('设置定位改写了业务路由');
    await page.screenshot({ path: path.join(root, 'system-settings-desktop.png'), fullPage: true });
    await page.setViewportSize({ width: 390, height: 844 });
    if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw Error('通用设置移动端溢出');
    // 手机端定位后章节标题须落在顶部菜单下方；截图前归零滚动，避免捕获平滑滚动中间帧。
    await page.getByRole('navigation', { name: '设置分组快速跳转' }).getByRole('button', { name: '维护与备份', exact: true }).click();
    await page.waitForFunction(() => { const y = document.getElementById('settings-maintenance').getBoundingClientRect().top; return y >= 60 && y < innerHeight / 2; });
    await page.evaluate(() => window.scrollTo({ top: 0, behavior: 'instant' }));
    await page.screenshot({ path: path.join(root, 'system-settings-mobile.png'), fullPage: true });
    await page.setViewportSize({ width: 1440, height: 1000 });
    await nav.getByRole('button', { name: '诊断中心', exact: true }).click();
    await page.getByRole('button', { name: '开始诊断', exact: true }).click();
    await page.getByRole('button', { name: '导出脱敏报告', exact: true }).waitFor();
    const diagnosisDownload = page.waitForEvent('download');
    await page.getByRole('button', { name: '导出脱敏报告', exact: true }).click();
    const diagnosis = await diagnosisDownload; const diagnosisFile = path.join(root, 'diagnostics.json'); await diagnosis.saveAs(diagnosisFile);
    const report = JSON.parse(fs.readFileSync(diagnosisFile, 'utf8'));
    if (!report.steps.length || JSON.stringify(report).includes(secret)) throw Error('诊断报告缺失或泄密');
    await page.screenshot({ path: path.join(root, 'diagnostics-desktop.png'), fullPage: true });
    await nav.getByRole('button', { name: '配置历史', exact: true }).click();
    await page.getByRole('button', { name: '预检并恢复', exact: true }).first().waitFor();
    const historyDownload = page.waitForEvent('download');
    await page.getByRole('link', { name: '下载脱敏版本', exact: true }).first().click();
    const history = await historyDownload; const historyFile = path.join(root, 'history.yaml'); await history.saveAs(historyFile);
    if (fs.readFileSync(historyFile, 'utf8').includes(secret)) throw Error('历史下载包含管理凭据');
    await page.getByRole('button', { name: '预检并恢复', exact: true }).first().click();
    await page.getByRole('alertdialog').getByRole('button', { name: '恢复配置', exact: true }).click();
    await page.getByText('配置已写入，等待重启生效。请重启后再修改设置。', { exact: true }).waitFor();
    const systemStatus = await (await context.request.get(`${base}/api/system/status`)).json();
    if (!systemStatus.pending_restart || systemStatus.uptime_seconds < 0) throw Error('系统摘要未同步待重启状态');
    if ((await context.request.post(`${base}/api/modules/remote`, { data: { enabled: true } })).ok()) throw Error('待重启配置被旧运行态覆盖');
    await page.screenshot({ path: path.join(root, 'history-desktop.png'), fullPage: true });
    await page.setViewportSize({ width: 390, height: 844 });
    if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw Error('配置历史移动端水平溢出');
    await page.screenshot({ path: path.join(root, 'history-mobile.png'), fullPage: true });
    // 下列只读响应夹具覆盖双模块、异常和局部数据失败，避免为 UI 测试启动真实代理或外网隧道。
    const proxyOverview = await (await context.request.get(`${base}/api/overview`)).json();
    const fixtures = {
      '/api/modules': [{ id: 'proxy', name: '代理', enabled: true, phase: 'running' }, { id: 'remote', name: '远程访问', enabled: true, phase: 'retrying', error: '远程服务等待重试', next_retry_at: new Date(Date.now() + 30000).toISOString() }],
      '/api/overview': { ...proxyOverview, mode: 'rule', nodes: [{ name: '测试节点', key: 'test', alive: true, delay: 10, port: 40001 }] },
      '/api/system/status': { uptime_seconds: 90061, pending_restart: true },
      '/api/connections': { connections: [{ id: 'test' }] },
      '/api/remote': { enabled: true, running: false, peers: [{ active: 2 }], ssh_keys: [{ expires_at: new Date(Date.now() + 86400000).toISOString() }], forwards: [] },
      '/api/remote/remotes': { remotes: [{ name: 'home' }] },
      '/api/desktop': { sessions: [] },
      '/api/traffic': { up: 1024, down: 2048 },
    };
    /** routeDashboardFixture 仅替换已声明的只读来源；参数 route 为 Playwright Route，返回 Promise；未知路径交给真实实例。 */
    async function routeDashboardFixture(route) {
      const value = fixtures[new URL(route.request().url()).pathname];
      if (value === undefined) return route.continue();
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(value) + '\n' });
    }
    await page.route('**/api/**', routeDashboardFixture);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(`${base}/#/overview`);
    await page.getByRole('article', { name: '代理摘要' }).waitFor();
    await page.getByText('SSH 授权需要检查', { exact: true }).waitFor();
    await page.getByText('配置已恢复，等待重启生效', { exact: true }).waitFor();
    await page.getByText('1 天 1 小时', { exact: false }).waitFor();
    await page.screenshot({ path: path.join(root, 'dashboard-desktop.png'), fullPage: true });
    await page.getByRole('button', { name: '进入运行概览', exact: true }).click();
    await page.waitForFunction(() => window.location.hash === '#/proxy/overview');
    await page.getByRole('heading', { name: '主入口策略', exact: true }).waitFor();
    if (await nav.getByRole('button', { name: '运行概览', exact: true }).getAttribute('aria-current') !== 'page') throw Error('代理运行概览归属错误');
    await page.reload();
    await page.getByRole('heading', { name: '主入口策略', exact: true }).waitFor();
    await page.getByRole('button', { name: '打开代理设置', exact: true }).click();
    await page.getByRole('heading', { name: '代理设置', exact: true }).waitFor();
    await page.getByRole('switch', { name: '启用 TUN 模式', exact: true }).waitFor();
    if (await page.getByRole('switch', { name: '系统启动时自动启动 proxyd', exact: true }).count()) throw Error('代理设置仍包含进程自启');
    if (await page.getByRole('heading', { name: '配置备份', exact: true }).count()) throw Error('代理设置仍包含全局备份');
    await page.getByRole('navigation', { name: '设置分组快速跳转' }).getByRole('button', { name: '本机网络', exact: true }).click();
    if (new URL(page.url()).hash !== '#/proxy/settings') throw Error('代理设置定位丢失路由');
    await page.screenshot({ path: path.join(root, 'proxy-settings-desktop.png'), fullPage: true });
    await page.setViewportSize({ width: 390, height: 844 });
    if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw Error('代理设置移动端溢出');
    await page.getByRole('navigation', { name: '设置分组快速跳转' }).getByRole('button', { name: '本机网络', exact: true }).click();
    await page.waitForFunction(() => { const y = document.getElementById('settings-network').getBoundingClientRect().top; return y >= 60 && y < innerHeight / 2; });
    await page.evaluate(() => window.scrollTo({ top: 0, behavior: 'instant' }));
    await page.screenshot({ path: path.join(root, 'proxy-settings-mobile.png'), fullPage: true });
    await page.setViewportSize({ width: 1440, height: 1000 });
    await top.getByRole('button', { name: '总览', exact: true }).click();
    await page.getByText('SSH 授权需要检查', { exact: true }).waitFor();
    await page.setViewportSize({ width: 390, height: 844 });
    if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw Error('双模块总览移动端水平溢出');
    await page.screenshot({ path: path.join(root, 'dashboard-mobile.png'), fullPage: true });
    await page.getByRole('button', { name: '切换到明亮模式', exact: true }).click();
    await page.screenshot({ path: path.join(root, 'dashboard-mobile-light.png'), fullPage: true });
    // 代理来源失败不能阻断全局页面、远程摘要和系统待办；失败不能被展示成健康。
    await page.route('**/api/overview', (route) => route.fulfill({ status: 503, body: '测试来源不可用' }));
    await page.route('**/api/desktop', (route) => route.fulfill({ status: 503, body: '测试来源不可用' }));
    await page.reload();
    await page.getByText('代理概览暂不可用；已显示的数据可能尚未更新。', { exact: true }).waitFor();
    await page.getByText('桌面会话：暂时无法读取，稍后自动重试', { exact: true }).waitFor();
    await page.getByRole('article', { name: '远程访问摘要' }).waitFor();
    await page.getByRole('heading', { name: '部分状态尚未确认', exact: true }).waitFor();
    await page.getByText('配置已恢复，等待重启生效', { exact: true }).waitFor();
    // 即使代理概览故障，旧 settings 链接仍能独立读取公共设置。
    await page.goto(`${base}/#/settings`);
    await page.getByRole('heading', { name: '通用设置', exact: true }).waitFor();
    await page.getByRole('switch', { name: '系统启动时自动启动 proxyd', exact: true }).waitFor();
    if (await page.getByText('正在连接 proxyd', { exact: true }).count()) throw Error('通用设置仍依赖代理概览');
    if (errors.length) throw Error(errors.join('\n'));
    process.stdout.write(`浏览器回归通过：公共/代理设置分离与定位、全局总览/代理概览/摘要异常/局部失败/主题/移动端、模块菜单隐藏/恢复/旧链接/移动端、终端恢复与断线、诊断、脱敏下载、配置恢复、移动端。截图：${root}\n`);
  } finally {
    if (browser) await browser.close();
    if (child.exitCode === null) {
      child.kill('SIGTERM');
      // 仅清理此脚本创建的子进程，超时才强制回收，不按端口或进程名终止用户实例。
      await Promise.race([new Promise((resolve) => child.once('exit', resolve)), new Promise((resolve) => { const timer = setTimeout(() => { child.kill('SIGKILL'); resolve(); }, 5000); timer.unref(); })]);
    }
    fs.writeFileSync(path.join(root, 'daemon.log'), log, { mode: 0o600 });
  }
}
main().catch((error) => { process.stderr.write(`${error.stack}\n`); process.exitCode = 1; });
