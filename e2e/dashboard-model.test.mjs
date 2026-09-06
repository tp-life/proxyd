/** 总览展示模型回归：验证禁用隔离、状态语义与授权到期边界，不访问外部服务。 */
import test from "node:test";
import assert from "node:assert/strict";
import { buildDashboard, remoteSummary, proxySummary } from "../web/src/lib/dashboard.js";

/** 验证禁用模块不泄漏旧异常；参数无，返回无；断言不满足时测试失败。 */
test("禁用模块隐藏卡片与旧待办，系统待重启仍可处理", () => {
  const result = buildDashboard([{ id: "remote", enabled: false, phase: "failed", error: "旧错误" }], { data: { system: { pending_restart: true }, remote: { enabled: true, error: "旧启动失败" } } });
  assert.deepEqual(result.cards, []);
  assert.deepEqual(result.items.map((item) => item.id), ["system-restart"]);
});

/** 验证缺失数据不能表示零连接或模块停止；参数无，返回无；误报时断言失败。 */
test("未知指标保留未知，服务端未启用不等同整个远程模块异常", () => {
  assert.equal(proxySummary({ data: {} }).metrics.find((item) => item.label === "活动代理连接").value, null);
  const result = buildDashboard([{ id: "remote", enabled: true, phase: "running", name: "远程访问" }], { data: { remote: { enabled: false, running: false, peers: [{ active: 2 }], ssh_keys: [{ active: 2 }] }, devices: { remotes: [{ name: "home" }] } } });
  assert.equal(result.cards[0].attention, false);
  assert.deepEqual(result.items, []);
  assert.equal(result.cards[0].metrics.find((item) => item.label === "隧道服务端").value, "未启用");
  assert.equal(result.cards[0].metrics.find((item) => item.label === "入站隧道连接").value, 2);
});

/** 验证 7 天到期阈值、禁用密钥与禁用转发排除；参数无，返回无；边界误判使测试失败。 */
test("授权到期提醒遵循时间边界且不提醒已禁用项目", () => {
  const now = Date.parse("2026-09-06T00:00:00Z");
  const result = remoteSummary({ now, data: { remote: { ssh_keys: [
    { expires_at: "2026-09-06T00:00:00Z" },
    { expires_at: "2026-09-13T00:00:00Z" },
    { expires_at: "2026-09-13T00:00:01Z" },
    { expired: true, expires_at: "2026-09-07T00:00:00Z" },
    { disabled: true, expires_at: "2026-09-05T00:00:00Z" },
  ], forwards: [{ enabled: false, last_error: "旧错误" }] } } });
  assert.equal(result.items.length, 1);
  assert.equal(result.items[0].detail, "2 个密钥已到期，1 个将在 7 天内到期。");
});

/** 验证生命周期错误有可操作入口与重试时间；参数无，返回无；缺少入口使测试失败。 */
test("部分运行失败仍保留模块卡片和处理入口", () => {
  const result = buildDashboard([{ id: "remote", name: "远程访问", enabled: true, phase: "degraded", error: "转发不可用", next_retry_at: "2026-09-06T00:01:00Z" }], { data: {} });
  assert.equal(result.cards[0].attention, true);
  assert.equal(result.items[0].view, "modules");
  assert.equal(result.items[0].retryAt, "2026-09-06T00:01:00Z");
});
