import { CircleHelp, Download, RotateCcw, SlidersHorizontal, Upload } from "lucide-react";
import { Button, ButtonLink } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { Switch as UISwitch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { classNames, formatDelay, versionCheckMessage } from "@/lib/format";

// SETTINGS_HELP 定义系统设置卡片的详细用途、优先级和风险边界。
// 内容集中维护可以保证悬浮说明与真实后端行为同步，避免各处零散文案产生矛盾。
const SETTINGS_HELP = {
  mainEntry: {
    heading: "主代理入口如何工作",
    paragraphs: [
      "主端口是应用最常使用的 HTTP + SOCKS5 混合代理入口。默认情况下，它按照当前的规则、全局或直连模式决定出口。",
      "开启“始终使用当前延迟最低的节点”后，主端口会绕过访问规则并交给 AUTO 测速组；固定节点同样会绕过规则。两者同时配置时，自动选优优先于固定节点。",
    ],
    note: "修改端口会热更新代理核心；端口不能与 API、节点映射、自动选优或策略分组端口冲突。",
  },
  nodePorts: {
    heading: "节点端口范围有什么作用",
    paragraphs: [
      "启用后，每个健康节点都会获得一个独立的本地 HTTP + SOCKS5 混合端口。连接某个端口即可固定从对应节点出站，适合多账号、爬虫或需要明确出口的任务。",
      "端口分配会持久化，同一节点在刷新和重启后会尽量沿用原端口。关闭开关只停止这些 listener，不会删除稳定分配，也不影响主端口、自动选优入口或策略分组。",
    ],
    note: "范围容量不足时只会为部分健康节点提供监听；起止端口还必须避开所有其他本机入口。",
  },
  autoPort: {
    heading: "自动选优入口有什么作用",
    paragraphs: [
      "这是一个独立于主端口的快捷入口，始终由 URL-Test 组选择当前延迟最低的健康节点，并绕过访问规则。",
      "它适合希望自动选择低延迟出口、但又不想改变主端口规则行为的应用。填写 0 会关闭此入口，主端口和节点映射不受影响。",
    ],
    note: "没有健康节点时不会启动该 listener；节点恢复后会在后续刷新中自动恢复。",
  },
  dns: {
    heading: "DNS 模式选择指南",
    paragraphs: [
      "DNS 决定域名如何解析，也影响 TUN 流量能否在解析阶段准确命中域名规则。切换预设会热更新 mihomo，不需要重启 proxyd。",
    ],
    items: [
      "不启用预设：沿用系统或配置文件的解析行为。未配置自定义 DNS 时，TUN 的 DNS 劫持和域名规则可能不完整。",
      "Fake IP：先返回保留网段中的虚拟地址，再由 mihomo 还原域名并选择规则。规则识别最稳定，通常是 TUN 的推荐模式；极少数依赖真实 IP、局域网发现或特殊校验的应用可能需要额外排除。",
      "Redir Host：向应用返回真实解析结果，兼容性更高，但域名还原和规则命中的稳定性通常弱于 Fake IP，也更依赖上游 DNS 质量。",
      "自定义 DNS：只要 YAML 中存在 dns 段，就拥有最高优先级，界面预设会被锁定。需要配置 nameserver、fallback、fake-ip-filter 等高级项时应使用这种方式。",
    ],
    note: "一般建议：仅使用系统代理可先保持关闭；开启 TUN 时优先选择 Fake IP，遇到特定应用兼容问题再改用 Redir Host 或自定义 DNS。",
  },
  takeover: {
    heading: "本机接管方式的区别",
    paragraphs: [
      "系统代理只修改操作系统的 HTTP、HTTPS 和 SOCKS 代理设置，适合浏览器及遵循系统代理的应用；进程退出时 proxyd 会尝试恢复原状态。",
      "TUN 在网络层接管 TCP/UDP 流量，可覆盖不读取系统代理的程序，但需要管理员或网络管理权限。通常选择系统代理或 TUN 其中一种即可，同时开启不会改变规则优先级。",
      "开机自启会按平台注册系统启动项；macOS 使用 LaunchDaemon，可在用户登录前启动。它不会自动提升 TUN 权限，启用 TUN 时仍需按当前平台完成授权。",
    ],
    note: "修改接管方式可能短暂影响现有连接；操作前请确认主端口和规则配置可用。",
  },
  updates: {
    heading: "版本检查会做什么",
    paragraphs: [
      "启用后，proxyd 只在启动阶段异步查询官方 GitHub Releases 的最新稳定版本，并把结果缓存在内存中。Web 轮询不会反复访问 GitHub。",
      "检查失败、网络超时或限流不会影响代理核心、订阅刷新和 API；开发版或无法比较的版本号也不会产生升级误报。",
    ],
    note: "此开关只提供更新提示，不会自动下载、替换或重启当前程序。",
  },
  backup: {
    heading: "备份与导入的安全边界",
    paragraphs: [
      "打码配置会隐藏 secret、订阅凭据和敏感查询参数，适合排障分享；完整备份包含真实订阅和代理凭据，只应保存在可信位置。",
      "导入会先完成格式校验并展示变更摘要，确认时还会校验文件摘要，避免预览后内容被替换。写盘采用临时文件和原子替换，失败不会覆盖现有配置。",
    ],
    note: "导入成功后必须重启 proxyd 才会整体生效，因为监听地址、状态目录和权限要求可能同时变化。",
  },
  restart: {
    heading: "重启会经历什么",
    paragraphs: [
      "重启会先让当前进程优雅退出（关闭监听、恢复系统代理等系统集成状态），再由独立子进程按当前配置文件重新拉起服务。",
      "导入新配置后必须重启才会整体生效；其他设置页操作大多已经热更新，无需重启。",
    ],
    note: "重启期间代理入口与控制台会短暂中断；若新配置修改了 API 监听地址，恢复后需要访问新地址。",
  },
};

/**
 * SettingsPage 渲染设置页。
 *
 * 参数说明：
 * - forms: object，表单状态。
 * - overview: object，概览数据。
 * - onForm/onPost: Function，表单与提交回调。
 * - onImportConfig: Function，上传配置文件的回调。
 * - onRestart: Function，确认并触发进程重启的回调。
 *
 * 返回值说明：
 * 返回设置页 React 元素。
 *
 * 可能的异常/错误情况：
 * 端口格式错误本地拦截；后端校验错误由父组件展示。
 */
export function SettingsPage({ forms, overview, onForm, onImportConfig, onPost, onRestart }) {
  // 固定节点下拉必须包含全部节点（含失效）：只列可用节点时，已固定但暂时失效的节点
  // 会不在选项里，Select 无法回显出具体节点名。失效节点标记文案并禁止新选。
  const selectableNodes = [...overview.nodes].sort((a, b) => Number(b.alive) - Number(a.alive) || a.delay - b.delay || a.name.localeCompare(b.name));
  const nodeOptions = selectableNodes.map((node) => ({
    value: node.key,
    label: node.alive ? `${node.name} · ${formatDelay(node)}` : `${node.name} · 失效`,
    disabled: !node.alive,
  }));
  // 已配置的固定节点不在当前列表（订阅刷新后消失）时补一个兜底项，让当前值仍可回显
  if (overview.main_node && !selectableNodes.some((node) => node.key === overview.main_node)) {
    nodeOptions.push({ value: overview.main_node, label: "已配置的节点（当前不在节点列表）", disabled: true });
  }
  return (
    <div className="settings-layout">
      <PageHeader eyebrow="系统配置" title="系统设置" detail="集中管理代理入口、本机网络接管以及配置维护。" />
      <nav className="settings-jump" aria-label="设置分组快速跳转">
        <span><SlidersHorizontal size={16} aria-hidden="true" />快速定位</span>
        <a href="#settings-ports">代理入口</a>
        <a href="#settings-network">本机网络</a>
        <a href="#settings-maintenance">维护与备份</a>
      </nav>

      <section className="settings-section" id="settings-ports" aria-labelledby="settings-ports-title">
        <div className="settings-section-heading">
          <span>01</span>
          <div>
            <h2 id="settings-ports-title">代理入口</h2>
            <p>管理主端口、节点端口范围和自动选优入口。</p>
          </div>
        </div>
        <div className="settings-grid">
          <section className="setting-row">
            <SettingTitle title="主代理入口" detail="应用通常只需要配置这个端口" help={SETTINGS_HELP.mainEntry} />
            <div className="setting-control">
              <div className="form-grid settings-form">
                <Field label="主端口"><input type="number" min="1" max="65535" value={forms.mainPort} onChange={(event) => onForm("mainPort", event.target.value)} /></Field>
                <Button className="form-submit" type="button" onClick={() => onPost("/api/main-port", { port: Number.parseInt(forms.mainPort, 10) }, `主端口已更新为 ${forms.mainPort}`)}>保存端口</Button>
              </div>
              <UISwitch checked={overview.main_auto} label="始终使用当前延迟最低的节点" onCheckedChange={(enabled) => onPost("/api/main-auto", { enabled }, enabled ? "主端口已切换为最优节点" : "主端口已恢复规则模式")} />
              <Field label="固定节点" hint={overview.main_auto ? "关闭自动选择后可固定节点" : "留空时跟随当前规则和模式"}>
                <Select
                  ariaLabel="主端口固定节点"
                  disabled={overview.main_auto}
                  value={overview.main_node || ""}
                  onValueChange={(node) => onPost("/api/main-node", { node }, node ? "主端口已固定到所选节点" : "主端口已恢复规则模式")}
                  options={[
                    { value: "", label: "跟随规则与模式" },
                    ...nodeOptions,
                  ]}
                />
              </Field>
            </div>
          </section>
          <section className="setting-row">
            <SettingTitle title="节点端口范围" detail="健康节点会依次分配到这个范围内" help={SETTINGS_HELP.nodePorts} />
            <div className="setting-control">
              <UISwitch checked={Boolean(overview.port_mapping_enabled)} label="启用节点一对一端口映射" onCheckedChange={(enabled) => onPost("/api/port-mapping", { enabled }, enabled ? "节点端口映射已开启" : "节点端口映射已关闭")} />
              <div className="form-grid settings-form range-form">
                <Field label="起始端口"><input type="number" min="1" max="65535" value={forms.rangeLo} onChange={(event) => onForm("rangeLo", event.target.value)} /></Field>
                <Field label="结束端口"><input type="number" min="1" max="65535" value={forms.rangeHi} onChange={(event) => onForm("rangeHi", event.target.value)} /></Field>
                <Button className="form-submit" type="button" onClick={() => onPost("/api/port-range", { range: `${forms.rangeLo}-${forms.rangeHi}` }, "端口区间已更新")}>保存范围</Button>
              </div>
              <p className="permission-note ok">关闭后保留稳定分配；主端口、自动选优与分组端口继续工作。</p>
            </div>
          </section>
          <section className="setting-row">
            <SettingTitle title="自动选优入口" detail="提供一个始终指向低延迟节点的独立端口" help={SETTINGS_HELP.autoPort} />
            <div className="form-grid settings-form">
              <Field label="端口（0 表示关闭）"><input type="number" min="0" max="65535" value={forms.autoPort} onChange={(event) => onForm("autoPort", event.target.value)} /></Field>
              <Button className="form-submit" type="button" onClick={() => onPost("/api/auto-port", { port: Number.parseInt(forms.autoPort, 10) || 0 }, "自动选优端口已更新")}>保存端口</Button>
            </div>
          </section>
        </div>
      </section>

      <section className="settings-section" id="settings-network" aria-labelledby="settings-network-title">
        <div className="settings-section-heading">
          <span>02</span>
          <div>
            <h2 id="settings-network-title">本机网络</h2>
            <p>配置 DNS、系统代理、TUN 与开机启动。</p>
          </div>
        </div>
        <div className="settings-grid">
          <section className="setting-row">
            <SettingTitle title="DNS 处理" detail="TUN 模式通常与 Fake IP 配合使用" help={SETTINGS_HELP.dns} />
            <div className="setting-control">
              <Field label="DNS 预设">
                <Select
                  ariaLabel="DNS 预设"
                  disabled={overview.dns_custom}
                  value={overview.dns_custom ? "custom" : (overview.dns_preset || "off")}
                  onValueChange={(preset) => onPost("/api/dns-preset", { preset }, `DNS 预设已切换为 ${preset}`)}
                  options={[
                    ...(overview.dns_custom ? [{ value: "custom", label: "使用配置文件中的自定义 DNS" }] : []),
                    { value: "off", label: "不启用 DNS 预设" },
                    { value: "fake-ip", label: "Fake IP" },
                    { value: "redir-host", label: "Redir Host" },
                  ]}
                />
              </Field>
              {overview.dns_custom && <p className="permission-note ok">配置文件中的 DNS 段优先生效</p>}
              {!overview.dns_custom && overview.tun?.enabled && overview.dns_preset === "off" && <p className="permission-note warn">TUN 已开启，建议选择 Fake IP</p>}
            </div>
          </section>
          <section className="setting-row">
            <SettingTitle title="本机接管" detail="这些开关会修改当前设备的网络集成状态" help={SETTINGS_HELP.takeover} />
            <div className="setting-control switch-stack">
              <UISwitch checked={overview.system_proxy} label="接管系统代理" onCheckedChange={(enabled) => onPost("/api/system-proxy", { enabled }, enabled ? "系统代理已开启" : "系统代理已关闭")} />
              <UISwitch checked={Boolean(overview.tun?.enabled)} label="启用 TUN 模式" onCheckedChange={(enabled) => onPost("/api/tun", { enabled }, enabled ? "TUN 已开启" : "TUN 已关闭")} />
              {overview.tun && <p className={classNames("permission-note", overview.tun.allowed && (!overview.tun.enabled || overview.tun.active) ? "ok" : "warn")}>{overview.tun.enabled && !overview.tun.active ? "TUN 配置已开启但实际未生效，请检查日志" : overview.tun.allowed ? `${overview.tun.platform} 权限可用` : overview.tun.permission}</p>}
              <UISwitch checked={overview.autostart} label="系统启动时自动启动 proxyd" onCheckedChange={(enabled) => onPost("/api/autostart", { enabled }, enabled ? "开机自启已开启" : "开机自启已关闭")} />
            </div>
          </section>
        </div>
      </section>

      <section className="settings-section" id="settings-maintenance" aria-labelledby="settings-maintenance-title">
        <div className="settings-section-heading">
          <span>03</span>
          <div>
            <h2 id="settings-maintenance-title">维护与备份</h2>
            <p>控制版本检查，并安全导入或导出配置。</p>
          </div>
        </div>
        <div className="settings-grid">
          <section className="setting-row">
            <SettingTitle title="版本检查" detail="仅在启动时检查稳定版本，不影响代理运行" help={SETTINGS_HELP.updates} />
            <div className="setting-control">
              <UISwitch checked={Boolean(overview.version_check?.enabled)} label="启动时检查新版本" onCheckedChange={(enabled) => onPost("/api/update-check", { enabled }, enabled ? "版本检查已开启" : "版本检查已关闭")} />
              {overview.version_check && <p className={classNames("permission-note", overview.version_check.state === "failed" ? "warn" : "ok")}>{versionCheckMessage(overview.version_check)}</p>}
            </div>
          </section>
          <section className="setting-row">
            <SettingTitle title="配置备份" detail="分享配置时使用打码导出；完整备份包含敏感信息" help={SETTINGS_HELP.backup} />
            <div className="config-actions">
              <ButtonLink className="beui-link-button" href="/api/config/export" variant="outline" size="md" download><Download size={16} aria-hidden="true" />导出打码配置</ButtonLink>
              <ButtonLink className="beui-link-button" href="/api/config/export?mask_tokens=false" variant="outline" size="md" download><Download size={16} aria-hidden="true" />下载完整备份</ButtonLink>
              <label className="beui-link-button config-upload"><Upload size={16} aria-hidden="true" />导入配置<input accept=".yaml,.yml,application/yaml,text/yaml" type="file" onChange={(event) => { onImportConfig(event.target.files?.[0]); event.target.value = ""; }} /></label>
            </div>
          </section>
          <section className="setting-row">
            <SettingTitle title="重启应用" detail="导入配置或修改监听地址后，需要重启才能整体生效" help={SETTINGS_HELP.restart} />
            <div className="setting-control">
              <Button variant="outline" size="md" type="button" onClick={onRestart}><RotateCcw size={16} aria-hidden="true" />重启 proxyd</Button>
            </div>
          </section>
        </div>
      </section>
    </div>
  );
}

/**
 * SettingTitle 渲染带详细帮助入口的系统设置标题。
 *
 * 功能说明：
 * 保留设置卡片原有的标题与摘要层级，并使用 Radix Tooltip 提供更完整的用途、
 * 优先级和风险边界说明。触发按钮同时支持 hover、键盘聚焦和触屏点击。
 *
 * 参数说明：
 * - title: string，设置卡片标题。
 * - detail: string，卡片内常驻显示的简短摘要。
 * - help: object，详细帮助模型，包含 heading、paragraphs、items 和 note。
 *
 * 返回值说明：
 * 返回包含标题、帮助触发按钮、摘要和 TooltipContent 的 React 元素。
 *
 * 可能的异常/错误情况：
 * help 缺失时仅渲染普通标题与摘要；帮助段落或列表为空时自动省略对应区块，
 * 不影响设置控件的提交和状态更新。
 */
function SettingTitle({ title, detail, help }) {
  return (
    <div className="panel-heading setting-heading">
      <div className="setting-title-line">
        <h2 className="panel-title">{title}</h2>
        {help && (
          <Tooltip delayDuration={120}>
            <TooltipTrigger asChild>
              <button className="setting-help-trigger" type="button" aria-label={`查看“${title}”详细说明`}>
                <CircleHelp size={16} aria-hidden="true" />
              </button>
            </TooltipTrigger>
            <TooltipContent className="setting-help-content" side="right" sideOffset={8}>
              <div className="setting-help-body">
                <strong>{help.heading}</strong>
                {(help.paragraphs || []).map((paragraph) => <p key={paragraph}>{paragraph}</p>)}
                {(help.items || []).length > 0 && (
                  <ul>
                    {help.items.map((item) => <li key={item}>{item}</li>)}
                  </ul>
                )}
                {help.note && <p className="setting-help-note">{help.note}</p>}
              </div>
            </TooltipContent>
          </Tooltip>
        )}
      </div>
      {detail && <p>{detail}</p>}
    </div>
  );
}
