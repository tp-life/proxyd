/** 代理设置页仅管理 proxy 上下文的入口、解析与网络接管。 */
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { Switch as UISwitch } from "@/components/ui/switch";
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { SettingTitle } from "@/components/SettingTitle";
import { SettingsJump } from "@/components/SettingsJump";
import { classNames, formatDelay } from "@/lib/format";

/** 代理帮助说明与所属设置一同维护，避免公共页面混入业务规则。 */
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
    ],
    note: "修改接管方式可能短暂影响现有连接；操作前请确认主端口和规则配置可用。",
  },
};

/**
 * ProxySettingsPage 渲染代理配置表单。
 * 参数：forms/overview 为 object，onForm/onPost 为 Function；返回 JSX。
 * 错误：端口与运行约束由已有 API 校验；提交失败由父组件提示，失效固定节点保留回显。
 */
export function ProxySettingsPage({ forms, overview, onForm, onPost }) {
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
      <PageHeader eyebrow="代理" title="代理设置" detail="管理代理入口、节点映射、DNS 和本机网络接管。" />
      <SettingsJump sections={[{ id: "settings-ports", label: "代理入口" }, { id: "settings-network", label: "本机网络" }]} />

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
            <p>配置代理 DNS、系统代理与 TUN。</p>
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
            </div>
          </section>
        </div>
      </section>

    </div>
  );
}
