/**
 * 代理概览浏览器回归：真实渲染应用，隔离 API 验证布局与固定节点弹窗，不写用户配置。
 * 运行：PLAYWRIGHT_MODULE=<playwright 模块路径> CHROMIUM_PATH=<浏览器路径> node e2e/proxy-overview-ui.cjs。
 * 可选 PROXYD_WEB_URL 指向已启动的前端；默认自动在随机回环端口启动仓库 Vite。
 */
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const { spawn } = require('node:child_process');

/** availablePort 获取空闲回环端口；无参数，返回 Promise<number>；监听错误时拒绝。 */
async function availablePort() {
  const server = net.createServer();
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  const port = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return port;
}

/** fixture 构造健康、失效和同名节点；无参数，返回 object；不访问外部数据，无错误。 */
function fixture() {
  return {
    mode: 'rule', mixed_port: 7890, main_auto: false, main_node: 'a', main_node_up: true,
    system_proxy: false, tun: {}, nodes: [
      { key: 'a', name: '香港 01', subscription: '测试订阅', type: 'ss', alive: true, delay: 32 },
      { key: 'b', name: '日本 02', subscription: '备用订阅', type: 'ss', alive: true, delay: 65 },
      { key: 'c', name: '失效节点', subscription: 'manual', type: 'ss', alive: false, delay: 0 },
      { key: 'd', name: '日本 02', subscription: '同名订阅', type: 'ss', alive: true, delay: 85 },
    ],
    subscriptions: [], groups: [], ports: [], port_assignments: [], manual_nodes: [], custom_rules: [], port_range: [20000, 21000],
  };
}

/** main 启动真实浏览器并断言完整交互；无参数，返回 Promise<void>；失败输出截图目录并非零退出。 */
async function main() {
  assert(process.env.PLAYWRIGHT_MODULE && process.env.CHROMIUM_PATH, '请设置 PLAYWRIGHT_MODULE 和 CHROMIUM_PATH');
  const { chromium } = require(process.env.PLAYWRIGHT_MODULE);
  const output = fs.mkdtempSync(path.join(os.tmpdir(), 'proxyd-overview-ui-'));
  let server; let browser; let page;
  try {
    let base = process.env.PROXYD_WEB_URL;
    if (!base) {
      const port = await availablePort();
      base = `http://127.0.0.1:${port}`;
      server = spawn(process.execPath, ['node_modules/vite/bin/vite.js', '--host', '127.0.0.1', '--port', String(port), '--strictPort'], { cwd: path.resolve('web'), stdio: 'ignore' });
      let ready = false;
      // 启动探测限定次数和单次超时；退出时终止自建服务，不影响已有用户实例。
      for (let attempt = 0; attempt < 80; attempt++) {
        try { ready = (await fetch(base, { signal: AbortSignal.timeout(200) })).ok; } catch {}
        if (ready) break;
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      assert(ready, '前端测试服务启动超时');
    }
    browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH, headless: true });
    page = await browser.newPage({ viewport: { width: 1366, height: 768 } });
    const errors = []; const writes = [];
    let overview = fixture(); let failPath = ''; let slowWrite = false;
    page.on('pageerror', (error) => errors.push(error.message));
    /** handleAPI 隔离并记录配置请求；route 为 Playwright Route，返回 Promise<void>；注入错误用于验证失败保留弹窗。 */
    async function handleAPI(route) {
      const request = route.request();
      const endpoint = new URL(request.url()).pathname;
      if (request.method() === 'POST') {
        const body = request.postDataJSON();
        writes.push({ endpoint, body });
        if (slowWrite) await new Promise((resolve) => setTimeout(resolve, 300));
        if (endpoint === failPath) return route.fulfill({ status: 500, body: '测试保存失败' });
        if (endpoint === '/api/main-node') overview.main_node = body.node;
        if (endpoint === '/api/main-auto') overview.main_auto = body.enabled;
        overview.main_node_up = !overview.main_auto && overview.nodes.some((node) => node.key === overview.main_node && node.alive);
        return route.fulfill({ json: {} });
      }
      const data = endpoint === '/api/overview' ? overview
        : endpoint === '/api/modules' ? [{ id: 'proxy', name: '代理', enabled: true, status: 'running' }]
        : endpoint === '/api/traffic' ? {} : [];
      return route.fulfill({ json: data });
    }
    await page.route('**/api/**', handleAPI);
    /** load 打开当前夹具页面；无参数，返回 Promise<void>；页面渲染超时则拒绝。 */
    async function load() {
      // 同一 hash 的 goto 可能仅触发文档内导航；先离开文档，保证每组夹具重新读取 API。
      await page.goto('about:blank');
      await page.goto(`${base}/#/proxy/overview`);
      await page.locator('.overview-shell').waitFor();
      await page.waitForFunction(() => document.getAnimations().every((animation) => animation.effect?.getTiming().iterations === Infinity || animation.playState === 'finished'));
    }
    await load();
    // 锁定真实父子布局链路：桌面没有整页滚动，常见笔记本上两个面板均无需滚动。
    for (const [width, height] of [[1440, 900], [1366, 768], [1280, 800], [1024, 768], [1440, 600]]) {
      await page.setViewportSize({ width, height });
      const sizes = await page.evaluate(() => ({ height: innerHeight, width: innerWidth, scrollHeight: document.documentElement.scrollHeight, scrollWidth: document.documentElement.scrollWidth, panes: [...document.querySelectorAll('.policy-pane,.overview-detail')].map((element) => ({ height: element.clientHeight, scroll: element.scrollHeight })) }));
      assert.equal(sizes.scrollHeight, height, `桌面页面纵向溢出：${width}×${height}`);
      assert.equal(sizes.scrollWidth, width, `桌面页面横向溢出：${width}×${height}`);
      if (width >= 1280 && height >= 768) assert(sizes.panes.every((pane) => pane.scroll <= pane.height + 1), `常用视口出现多余面板滚动：${JSON.stringify(sizes)}`);
    }
    await page.setViewportSize({ width: 1366, height: 768 });
    await page.screenshot({ path: path.join(output, 'overview-desktop.png'), animations: 'disabled' });
    const trigger = page.getByRole('button', { name: '切换节点', exact: true });
    const dialog = page.getByRole('dialog', { name: '选择固定节点', exact: true });
    await trigger.click();
    assert(await dialog.getByRole('button', { name: /失效节点/ }).isDisabled());
    assert(await dialog.getByRole('button', { name: '当前已使用', exact: true }).isDisabled());
    await dialog.getByRole('textbox', { name: '搜索节点' }).fill('备用订阅');
    assert.equal(await dialog.locator('.fixed-node-card').count(), 1);
    await dialog.locator('.fixed-node-card').click();
    await dialog.getByRole('button', { name: '取消', exact: true }).click();
    await dialog.waitFor({ state: 'hidden' });
    assert.equal(writes.length, 0, '取消不能写配置');
    assert(await trigger.evaluate((element) => document.activeElement === element), '弹窗关闭后未归还焦点');
    await trigger.click();
    await dialog.getByRole('button', { name: /日本 02.*备用订阅/ }).click();
    failPath = '/api/main-node';
    await dialog.getByRole('button', { name: '确认切换', exact: true }).click();
    await dialog.getByRole('alert').waitFor();
    assert.equal(overview.main_node, 'a', '失败时旧节点不能被界面改写');
    failPath = ''; slowWrite = true;
    await dialog.getByRole('button', { name: '确认切换', exact: true }).dblclick();
    await dialog.waitFor({ state: 'hidden' });
    assert.equal(writes.length, 2, '连点造成重复提交');
    assert.equal(overview.main_node, 'b', '同名节点必须按 key 切换');
    await page.locator('.exit-node strong').filter({ hasText: '日本 02' }).waitFor();
    slowWrite = false;
    await trigger.click();
    await dialog.getByRole('textbox', { name: '搜索节点' }).fill('不存在的节点');
    await dialog.getByText('没有匹配的节点，请调整关键词或来源筛选。').waitFor();
    await page.keyboard.press('Escape');
    await dialog.waitFor({ state: 'hidden' });
    // 从规则策略首次选择固定节点时留在概览，通过弹窗确认后才写入。
    overview = { ...fixture(), main_node: '', main_node_up: false };
    await load();
    await page.locator('.policy-option').filter({ hasText: '固定节点' }).click();
    await dialog.getByRole('button', { name: /香港 01/ }).click();
    await dialog.getByRole('button', { name: '确认切换', exact: true }).click();
    await dialog.waitFor({ state: 'hidden' });
    assert.equal(overview.main_node, 'a');
    assert(page.url().endsWith('#/proxy/overview'));
    // 自动策略优先级更高：先存节点，再关自动；第二步失败保留弹窗和真实自动策略。
    overview = { ...fixture(), main_auto: true, main_node_up: false };
    await load();
    const beforeAuto = writes.length;
    await page.locator('.policy-option').filter({ hasText: '固定节点' }).click();
    await dialog.getByRole('button', { name: /日本 02.*备用订阅/ }).click();
    failPath = '/api/main-auto';
    await dialog.getByRole('button', { name: '确认切换', exact: true }).click();
    await dialog.getByRole('alert').waitFor();
    assert.equal(overview.main_auto, true);
    assert.deepEqual(writes.slice(beforeAuto).map((write) => write.endpoint), ['/api/main-node', '/api/main-auto']);
    failPath = '';
    await dialog.getByRole('button', { name: '确认切换', exact: true }).click();
    await dialog.waitFor({ state: 'hidden' });
    assert.equal(overview.main_auto, false);
    assert.equal(overview.main_node, 'b');
    // 节点订阅消失仍允许从实际出口修复；空列表不产生无效提交。
    overview = { ...fixture(), main_node: 'missing', main_node_up: false, nodes: [] };
    await load();
    await trigger.click();
    await dialog.getByText('暂无节点，请先添加节点或同步订阅。').waitFor();
    assert(await dialog.getByRole('button', { name: '确认切换', exact: true }).isDisabled());
    await page.keyboard.press('Escape');
    // 手机长列表必须在弹窗中滚动，底部确认可见；长名称不能把页面或卡片横向撑开。
    overview = fixture();
    for (let index = 0; index < 50; index++) overview.nodes.push({ ...overview.nodes[0], key: `extra-${index}`, name: `节点${index}-${'LongName'.repeat(10)}` });
    await page.setViewportSize({ width: 390, height: 844 });
    await load();
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth), 390);
    await trigger.click();
    const list = dialog.locator('.fixed-node-list');
    assert(await list.evaluate((element) => element.scrollHeight > element.clientHeight), '长列表未限制滚动高度');
    const bounds = await dialog.boundingBox();
    assert(bounds.y >= 0 && bounds.y + bounds.height <= 844 && bounds.width <= 390, '弹窗溢出手机视口');
    const footer = await dialog.locator('.dialog-footer').boundingBox();
    assert(footer.y + footer.height <= 844, '确认按钮不可见');
    await page.screenshot({ path: path.join(output, 'node-picker-mobile.png'), animations: 'disabled' });
    await page.keyboard.press('Escape');
    await page.setViewportSize({ width: 1366, height: 768 });
    await trigger.click();
    await page.screenshot({ path: path.join(output, 'node-picker-desktop.png'), animations: 'disabled' });
    assert.deepEqual(errors, [], '浏览器发生运行错误');
    console.log(`代理概览布局及节点弹窗回归通过；截图：${output}`);
  } catch (error) {
    if (page) await page.screenshot({ path: path.join(output, 'failure.png'), fullPage: true }).catch(() => {});
    console.error(`失败截图目录：${output}`);
    throw error;
  } finally {
    await browser?.close();
    server?.kill();
  }
}

/** reportFailure 输出错误并设置失败退出码；error 为 Error，返回 void，不再抛出异常。 */
function reportFailure(error) { console.error(error); process.exitCode = 1; }
main().catch(reportFailure);
