/** 通用设置页管理守护进程、自启、版本和配置维护，独立于代理数据面。 */
import { Download, RotateCcw, Upload } from "lucide-react";
import { Button, ButtonLink } from "@/components/ui/button";
import { Switch as UISwitch } from "@/components/ui/switch";
import { PageHeader } from "@/components/PageHeader";
import { SettingTitle } from "@/components/SettingTitle";
import { SettingsJump } from "@/components/SettingsJump";
import { useSystemSettings } from "@/hooks/useSystemSettings";
import { classNames, versionCheckMessage } from "@/lib/format";

/** 公共帮助只描述进程级能力，代理业务说明留在代理设置中。 */
const SETTINGS_HELP = {
  updates: {
    heading: "版本检查会做什么",
    paragraphs: [
      "启用后，proxyd 只在启动阶段异步查询官方 GitHub Releases 的最新稳定版本，并把结果缓存在内存中。Web 轮询不会反复访问 GitHub。",
      "检查失败、网络超时或限流不会影响已启用模块和控制台；开发版或无法比较的版本号也不会产生升级误报。",
    ],
    note: "此开关只提供更新提示，不会自动下载、替换或重启当前程序。",
  },
  backup: {
    heading: "备份与导入的安全边界",
    paragraphs: [
      "打码配置会隐藏 secret、订阅凭据和远程访问凭据，适合排障分享；完整备份包含全部模块的真实凭据，只应保存在可信位置。",
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
    note: "重启期间已启用模块的连接与控制台会短暂中断；若新配置修改了 API 监听地址，恢复后需要访问新地址。",
  },
  autostart: {
    heading: "开机自启与模块开关",
    paragraphs: ["按平台注册 proxyd 系统启动项，macOS 使用 LaunchDaemon，可在用户登录前启动。", "启动后各模块继续遵循各自开关；注册自启不会擅自启用代理或远程服务。"],
    note: "自启不会自动安装 TUN 特权助手，需要使用 TUN 时请在代理设置中检查助手状态。",
  },
};

/**
 * SettingsPage 渲染独立的通用设置，不以代理概览加载成功为前提。
 * 参数：onImportConfig/onPost/onRestart 为 Function；返回 JSX。
 * 错误：状态加载失败显示重试入口并禁用状态开关；配置写入失败保留原快照并由父组件提示。
 */
export function SettingsPage({ onImportConfig, onPost, onRestart }) {
  const { data: settings, error, reload } = useSystemSettings();
  /** saveSetting 提交公共开关后同步状态；参数 ...args 为父级写入参数；返回 Promise<boolean>，失败不伪造开关状态。 */
  async function saveSetting(...args) {
    const saved = await onPost(...args);
    if (saved) reload();
    return saved;
  }
  return <div className="settings-layout">
    <PageHeader eyebrow="系统" title="通用设置" detail="管理 proxyd 的启动方式、版本检查和全部模块的配置备份。" />
    <SettingsJump sections={[{ id: "settings-startup", label: "启动与运行" }, { id: "settings-maintenance", label: "维护与备份" }]} />
    <section className="settings-section" id="settings-startup" aria-labelledby="settings-startup-title">
      <div className="settings-section-heading"><span>01</span><div><h2 id="settings-startup-title">启动与运行</h2><p>模块是否启用由模块管理决定，启动方式对整个进程生效。</p></div></div>
      {error && <div role="alert" className="p-4 text-sm text-destructive">{error}<Button className="ml-3" variant="outline" onClick={reload}>重新读取</Button></div>}
      <div className="settings-grid"><section className="setting-row">
        <SettingTitle title="开机自启" detail="系统启动时自动运行 proxyd，保留各模块原有开关" help={SETTINGS_HELP.autostart} />
        <div className="setting-control switch-stack">
          <UISwitch disabled={!settings || Boolean(error)} checked={Boolean(settings?.autostart)} label="系统启动时自动启动 proxyd" onCheckedChange={(enabled) => saveSetting("/api/autostart", { enabled }, enabled ? "开机自启已开启" : "开机自启已关闭")} />
          {settings?.autostart_runtime && <p role="status" className={classNames("permission-note", settings.autostart_runtime.running ? "ok" : "warn")}>{settings.autostart_runtime.message}</p>}
          {!settings && !error && <p role="status" className="text-sm text-muted-foreground">正在读取系统设置…</p>}
        </div>
      </section></div>
    </section>
      <section className="settings-section" id="settings-maintenance" aria-labelledby="settings-maintenance-title">
        <div className="settings-section-heading">
          <span>02</span>
          <div>
            <h2 id="settings-maintenance-title">维护与备份</h2>
            <p>控制版本检查，并安全导入或导出配置。</p>
          </div>
        </div>
        <div className="settings-grid">
          <section className="setting-row">
            <SettingTitle title="版本检查" detail="仅在启动时检查稳定版本，不影响已启用功能" help={SETTINGS_HELP.updates} />
            <div className="setting-control">
              <UISwitch disabled={!settings || Boolean(error)} checked={Boolean(settings?.version_check?.enabled)} label="启动时检查新版本" onCheckedChange={(enabled) => saveSetting("/api/update-check", { enabled }, enabled ? "版本检查已开启" : "版本检查已关闭")} />
              {settings?.version_check && <p className={classNames("permission-note", settings.version_check.state === "failed" ? "warn" : "ok")}>{versionCheckMessage(settings.version_check)}</p>}
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
    </div>;
}
