/** RemoteDevicesIntro 负责devices任务面板，数据加载与配置事务由上层用例提供。 */

import { PanelTitle } from "@/components/PanelTitle";

/**
 * RemoteDevicesIntro 渲染独立任务视图。
 * 参数：解构字段为状态对象、表单值或事件回调，沿用 RemotePage 的接口约定。
 * 返回：React 元素；网络错误由用例回调处理，本组件不直接写配置。
 */
export function RemoteDevicesIntro({  }) {
 return (<section className="panel">
        <PanelTitle title="连接另一台机器" detail="先在对端开启服务，再将它添加为远程设备。" />
        <ol className="m-0 grid list-decimal gap-1 pl-5 text-sm text-muted-foreground">
          <li>在对端的「本机服务」开启远程连接，复制 token；通过「访问授权」管理允许登录的设备和公钥。</li>
          <li>在本页保存对端 token，点击「连接」获取 SSH/scp 命令。常驻转发在「端口转发」管理。</li>
        </ol>
      </section>);
}
