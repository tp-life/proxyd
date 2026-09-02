# Design QA

## 基准与环境

- Source visual truth：`.omx/plans/clash-verge-ui-port-mapping-plan.md`
- 同状态旧版基线：`.omx/audits/clash-verge-gap/screenshots/01-overview-desktop.png`、`02-nodes-subscriptions-desktop.png`、`08-settings-desktop.png`、`09-settings-mobile.png`
- Implementation screenshots：`.omx/audits/clash-verge-gap/screenshots-final/`
- 浏览器与状态：Chromium 无头浏览器；审计专用本地配置；2 个不可用示例节点、2 个停用订阅、1 个策略分组、1 条自定义规则、端口映射开启、活动连接为空。
- 桌面 viewport / CSS size：1440 × 1000；源图与实现图像素尺寸均为 1440 × 1000；deviceScaleFactor = 1。
- 移动 viewport / CSS size：390 × 844；源图与实现图像素尺寸均为 390 × 844；deviceScaleFactor = 1。
- Density normalization：源图与实现图均为 1 CSS px = 1 image px，无缩放换算。
- 说明：本次是按已确认计划进行 Clash Verge 风格重排，不是像素复刻；旧版截图用于保持相同业务状态，计划文档定义信息架构、布局、色彩、动效和功能目标。

## 完整视图证据

- 桌面全量页面：`01-overview-desktop.png` 至 `09-settings-desktop.png`，覆盖运行概况、代理节点、订阅管理、代理入口、策略分组、访问规则、运行日志、活动连接和系统设置。
- 移动端：`10-settings-mobile.png` 验证系统设置窄屏排版；`11-mobile-navigation.png` 验证抽屉导航、遮罩、选中态和底部运行状态。
- 模态交互：`12-add-node-dialog.png`、`13-add-subscription-dialog.png` 验证 beUI Dialog 的标题、字段、按钮层级、遮罩和关闭入口。

## 聚焦区域证据

- 节点工作台：旧版 `02-nodes-subscriptions-desktop.png` 与新版 `02-nodes-desktop.png` 在同一次视觉比较中核验，重点检查页头动作、筛选区、表头/列对齐、状态徽标和稳定端口展示。
- 系统设置：旧版 `08-settings-desktop.png` 与新版 `09-settings-desktop.png` 在同一次视觉比较中核验，重点检查页内锚点、标题/控件双列对齐、按钮长度和分区密度。
- 弹窗：`12-add-node-dialog.png`、`13-add-subscription-dialog.png` 以原始分辨率检查输入控件、主要/次要动作、关闭按钮和背景层级；没有发现裁切、溢出或不可读文字。
- 设置说明：旧版 `08-settings-desktop.png`、新版 `09-settings-desktop.png`、DNS 说明展开态 `14-settings-dns-help.png` 与移动端 `10-settings-mobile.png` 在同一次视觉比较中核验，重点检查卡片首行留白、帮助入口对齐、长说明可读性、视口边界和窄屏布局。
- 订阅管理：以 `09-settings-desktop.png` 中已确认的内容起始节奏作为产品内视觉基准，与 `03-subscriptions-desktop.png` 在同一次视觉比较中核验；订阅卡片已与标题分隔线保持一致的 12px 呼吸空间。

## 必检保真面

- 字体与排版：沿用项目现有字体栈、标题层级和正文灰阶；帮助标题、段落、列表与建议区形成清晰的阅读层次。
- 间距与布局节奏：每张设置卡片首行增加顶部空间，桌面双列和移动单列均保持一致间距；帮助按钮不会挤压设置名称。
- 色彩与设计令牌：沿用现有明亮绿色主色、边框色和浮层阴影，不引入新的孤立色值体系。
- 图标与资产：帮助入口使用项目现有 Lucide `CircleHelp` 图标；本页没有需要复核清晰度或裁切的位图资产。
- 文案与内容：七个设置分区均提供详细用途、影响范围和适用场景；DNS 额外说明关闭预设、Fake IP、Redir Host、自定义 DNS 及 TUN 场景建议。

## 比较历史

### Iteration 1

- P1：节点与订阅混排导致首屏被两个常驻表单占据，主要任务入口不清晰。修复为独立“代理节点”和“订阅管理”页面，并将添加动作收进 Dialog。复核证据：`02-nodes-desktop.png`、`03-subscriptions-desktop.png`、`12-add-node-dialog.png`、`13-add-subscription-dialog.png`。
- P1：系统设置长页依赖多个 Tab，设置项少时仍需切换；按钮被布局拉长。修复为三段页内锚点导航、稳定双列设置行和内容宽度按钮。复核证据：`09-settings-desktop.png`、`10-settings-mobile.png`。
- P1：逐节点端口映射缺少总开关，用户无法保留分配同时停止监听。修复为概览、代理入口和系统设置三处一致开关，关闭后保留稳定分配，主端口/自动选优/分组入口不受影响。浏览器实测关闭后 API 返回 false，再次开启后返回 true。
- P2：侧边栏分组标题较多，核心页面入口被拆散且移动端扫描成本高。修复为扁平任务导航与统一选中态。复核证据：`01-overview-desktop.png`、`11-mobile-navigation.png`。
- P2：表格存在自定义大写表头、阴影和间距覆盖，与 beUI 官方组件差距明显。修复为官方 Registry Table 的边框、表头、排序与可调列宽语义，只保留业务所需的响应式适配。复核证据：`02-nodes-desktop.png`、`08-connections-desktop.png`。
- P2：功能差距包括订阅编辑/启停、分组编辑、规则编辑/排序、日志搜索/暂停/导出、连接暂停刷新和配置导入预览。上述入口均已补齐；对应页面完整截图无布局回归。

### Iteration 2

- 复核全部 P1/P2 修复后的桌面与移动截图，无剩余可执行 P0、P1 或 P2 视觉问题。
- P3（非阻断）：示例节点全部不可用时，代理入口与活动连接页面留白较多；当前空状态有明确原因和下一步提示，属于真实稀疏数据状态。
- P3（非阻断）：绿色背景与主按钮比 Clash Verge 默认主题更明亮；这是用户明确要求的设计方向，文字对比与状态色仍可辨识。

### Iteration 3

- P2：设置卡片首行与分区标题之间留白不足，名称和控件在视觉上偏拥挤。修复为首行统一增加 `margin-top` 与顶部内边距，桌面和移动端均保持稳定节奏。复核证据：`09-settings-desktop.png`、`10-settings-mobile.png`。
- P2：设置项只有短副标题，无法说明配置影响、适用场景和边界条件。修复为七个设置分区统一增加可悬停、聚焦和点击的帮助入口，并补全逐项详细说明；DNS 单独覆盖四种工作方式及 TUN 选择建议。复核证据：`14-settings-dns-help.png`。
- P2：首次展开长 DNS 说明时，浮层的静态位移被 Motion 动画的 `transform` 覆盖，导致浮层定位偏移并出现视口裁切。修复为直接计算带边界约束的左上坐标，让动画只负责透明度和缩放；再次比较后说明完整可见，未遮挡触发按钮，也没有横向溢出。
- 复核全部新增改动后，无剩余可执行 P0、P1 或 P2 视觉问题。

### Iteration 4

- P2：订阅卡片顶部紧贴页面标题的底部分隔线，标题区与内容区缺少明确层次，视觉密度高于系统设置页。修复为订阅卡片容器统一增加 12px 顶部外边距；空订阅状态也通过同一容器继承一致的内容起始位置。
- 修复后在 1440 × 1000、deviceScaleFactor = 1 的相同主题和示例数据状态下重新截图，并与系统设置已确认的间距节奏共同检查。卡片不再贴线，内部尺寸、列数、按钮位置及页面首屏容量没有发生回归。复核证据：`03-subscriptions-desktop.png`、`09-settings-desktop.png`。
- 本轮复核无剩余可执行 P0、P1 或 P2 视觉问题。

## 功能与控制台检查

- 浏览器渲染截图：已完成，所有证据均来自本地实际构建产物，不是静态 mock。
- 主要交互：添加节点 Dialog、添加订阅 Dialog、新建策略分组 Dialog、规则编辑 Dialog 均成功打开并通过可访问关闭按钮退出。
- 设置说明：DNS 帮助入口已通过浏览器点击验证，内容包含 Fake IP、Redir Host、自定义 DNS 与一般建议；帮助入口同时支持鼠标悬停、键盘聚焦和触屏点击。
- 订阅管理：添加订阅 Dialog 仍可正常打开并关闭；新增顶部间距没有改变卡片按钮、启停开关和响应式网格行为。
- 端口映射：浏览器点击开关完成关闭和恢复，服务端状态依次为 false / true。
- Console：无运行时异常、无 `console.error`、无未处理 Promise；端口映射热更新期间 `/api/traffic` 长连接出现可恢复 502 并自动重连，已按 URL 识别为预期生命周期事件，其他资源和接口无错误。
- 构建与测试：`npm run build` 成功；`go test ./...` 全量成功。

Previous final result: passed

## 方案 3「桌面分栏台」概览页改版

### 基准、视口与状态

- Source visual truth：`/Users/tp/.codex/generated_images/01a06077-1a3c-7092-aa58-bfd832f4afdd/exec-c9c0560d-0f0d-477b-91bf-2337bb558299.png`。
- 最终桌面实现：`.omx/audits/desktop-split-redesign/implementation-desktop-exact.png`。
- 同屏比较证据：`.omx/audits/desktop-split-redesign/comparison-exact-final.png`，左侧为参考稿，右侧为浏览器实际渲染。
- Source 与 implementation 均为 1487 × 1058 像素，CSS viewport 为 1487 × 1058，`deviceScaleFactor = 1`；没有缩放、裁切或设备边框差异。
- 移动端实现：`.omx/audits/desktop-split-redesign/implementation-mobile-final.png`，CSS viewport 与图片均为 390 × 844，`deviceScaleFactor = 1`。
- 状态差异：参考稿展示规则分流与非零示例流量；实现图读取当前运行中的 proxyd 状态，固定节点不可用并回退规则、实时速率接近零。比较时只核验共同的布局、层级、色彩和控件状态表达，不把真实数据差异当成视觉偏差。

### 完整视图与聚焦证据

- 桌面全视图同时核验三栏比例、暖白页面底色、蓝色主操作、青色健康状态、琥珀提醒、分隔线、标题层级和主要内容间隔。
- 中栏重点核验策略选项、选中态、模式分段控件和主入口信息；右栏重点核验状态 Hero、四段路由链、实际出口、流量趋势、提醒区和快速开关。
- 390px 视图重点核验策略区压缩、操作按钮、路由步骤纵向排列与无横向溢出。最终精确设备视口截图中三个策略均完整可见，按钮和文字没有裁切。
- 未单独生成局部裁图：最终并排图以原始分辨率打开后，图标、11px 辅助文字、边框和控件均可清楚辨认，额外裁图不会提供新的保真判断依据。

### Iteration 5

- P2：固定节点失效时，初版仍把“固定节点”直接写入匹配策略与出口状态，和后端已回退规则处理的事实不一致。修复为同时保留用户配置意图与实际生效状态：标题显示“固定节点（已回退）”，实际出口显示“按规则动态选择”，状态显示“动态”。
- P2：初版 390px 策略项采用横向滚动，第三个“固定节点”在首屏只露出局部，核心策略不可一次比较。修复为三列紧凑策略布局，隐藏窄屏冗长副标题并保留图标、名称与选中态。
- P2：初次窄屏命令行截图受 macOS Headless Chrome 最小窗口宽度影响，画面右侧被物理裁切。该问题先通过精确 CDP 设备视口复核，确认不是页面媒体查询溢出；最终证据使用 390 × 844 CSS 视口重新捕获，没有把截图工具偏差作为产品缺陷。

### Iteration 6

- 在相同 1487 × 1058 尺寸下重新捕获桌面实现，并与参考稿放入同一张 `comparison-exact-final.png` 检查。三栏信息架构、平面分隔、状态色职责、内容密度与留白节奏均与所选方向一致。
- 重新捕获 390 × 844 移动端视图；策略、模式、状态 Hero、三枚操作按钮和首个路由步骤完整可见，无横向溢出或文本裁切。
- 最终复核没有剩余可执行的 P0、P1 或 P2 视觉问题。

### 功能与构建检查

- 浏览器只读交互：通过 Chrome DevTools Protocol 实际点击“代理节点”与“运行概况”导航，节点页标题返回“代理节点”；概览当前策略按下态返回“固定节点”。
- 命令菜单：实际点击概览搜索按钮后 Dialog 成功出现，点击遮罩后成功关闭。
- 为保护正在运行的用户代理配置，验收没有点击策略、系统代理、TUN 或端口映射写操作；这些控件仍连接既有 API 回调，没有替换为静态占位。
- `npm run build` 成功；仅保留 Vite 对既有 500kB 以上 chunk 的非阻断提醒。
- `go test ./internal/api ./internal/app` 成功。

Previous final result: passed

## 全系统桌面显示层重构

### 基准、视口与页面覆盖

- Source visual truth：`/Users/tp/.codex/generated_images/01a06077-1a3c-7092-aa58-bfd832f4afdd/exec-c9c0560d-0f0d-477b-91bf-2337bb558299.png`，用于约束暖白背景、蓝色主操作、青色健康状态、琥珀告警、平面分栏和紧凑间距。
- 重构前全页面证据：`.omx/audits/full-ui-redesign/before/01-overview.png` 至 `09-settings.png`；九页联系表：`.omx/audits/full-ui-redesign/before-contact-sheet.png`。
- 重构后全页面证据：`.omx/audits/full-ui-redesign/after-1/01-overview.png` 至 `09-settings.png`；九页联系表：`.omx/audits/full-ui-redesign/after-1-contact-sheet.png`。
- 同屏比较证据：`.omx/audits/full-ui-redesign/reference-vs-overview-after-1.png` 将参考图和真实概览实现并排；`.omx/audits/full-ui-redesign/before-vs-after-1.png` 将九页重构前后联系表并排。
- 所有完整页面与参考图均为 1487 × 1058 像素，Chrome CSS viewport 为 1487 × 1058，`deviceScaleFactor = 1`；没有缩放、裁切或移动端适配判断。本轮按用户要求只验收桌面端。
- 状态来自隔离的审计后端：2 个不可用手动节点、2 个停用订阅、1 个策略分组、2 条自定义规则、连接上游不可用和 6 条运行日志。该状态同时覆盖成功色、告警色、失败色、空状态、表格、卡片与日志输出。

### Iteration 7

- P1：概览以外页面同时渲染全局 Topbar 与页面标题，形成重复标题、重复动作和不稳定的内容起点；代理节点页在切换动效中还会被截到空白工作区。修复为每页唯一的 `PageHeader`，标题、模块标签、说明和主操作共享同一基线，并为浏览器截图留足页面入场时间。复核后八个业务页均只有一个 `h1`，节点列表稳定可见。
- P1：系统设置以全宽长列表堆叠，快速定位栏横向占据内容上方，用户很难同时维持章节语境和编辑焦点。修复为 208px 固定章节索引栏与弹性设置内容栏；索引随桌面滚动保持可见，三个设置分区统一为同一表单行网格。
- P2：页面之间混用顶部分隔、透明面板、阴影卡片、悬浮位移和胶囊按钮，导致表格、筛选器与卡片看起来来自不同组件系统。修复为产品内统一组件视觉层：12px 面板圆角、单层边框、无悬浮抬升、36px 常用控件高度、矩形按钮和轻量分段控件。
- P2：规则、连接与日志页面缺少独立页级语境，过滤器和输出区的层次不一致。修复为统一页头，筛选器放入浅色工具面板，连接状态合并到页头动作区，日志输出建立独立深色诊断区并保留明确的自动刷新状态。
- P2：稀疏数据页面把少量内容散落在无边界空白中。修复为订阅卡片网格、入口表格面板、分组列表面板和带虚线边界的连接空状态，空白现在表达“暂无更多数据”，而不是未完成布局。

### Iteration 8

- 在相同 1487 × 1058 视口重新捕获九个页面，并把参考图、实现图以及全页面重构前后状态分别放入同一比较输入中检查。
- 字体层级、内容间隔、边框半径、操作按钮基线、筛选器高度、表格列边界、状态色和设置页章节结构均保持一致；未发现裁切、错位、重复标题或横向溢出。
- 概览继续保留已确认的三栏信息架构；其他页面采用相同颜色令牌和表面语言，而不是机械复制概览的业务布局。最终没有剩余可执行的 P0、P1 或 P2 视觉问题。

### 功能、构建与控制台检查

- Chrome 浏览器实际点击验证命令菜单打开与 Escape 关闭、节点添加对话框、订阅添加对话框、策略分组对话框、设置页章节定位；测试没有提交表单或改变代理配置。
- 逐页结构检查验证代理节点、订阅管理、代理入口、策略分组、访问规则、活动连接、运行日志和系统设置均只有一个一级标题，且 `documentElement` 没有横向溢出。
- 全页面截图和交互回归期间没有 `Runtime.exceptionThrown`；九个页面均由当前 Vite 构建与隔离 API 的真实浏览器渲染生成。
- `npm run build` 成功；仅有既存的 Vite 500kB chunk 非阻断提醒。
- `go test ./internal/api ./internal/app` 成功。

Previous final result: passed

## Radix UI 全组件迁移与规则模式说明

### 基准、视口与覆盖范围

- Source visual truth：`/Users/tp/.codex/generated_images/01a06077-1a3c-7092-aa58-bfd832f4afdd/exec-c9c0560d-0f0d-477b-91bf-2337bb558299.png`。
- Radix 迁移后九页浏览器证据：`.omx/audits/radix-ui-migration/after/01-overview.png` 至 `09-settings.png`；均为 Chrome 真实渲染，不是静态 mock。
- 复合组件展开态：`10-select-open.png` 验证 Radix Select 的 Portal、锚点宽度、选中标记和层级；`11-dialog-open.png` 验证 Radix Dialog 的遮罩、焦点首项、表单间距和操作层级。
- 同屏比较证据：`.omx/audits/radix-ui-migration/reference-vs-radix-overview.png` 将参考稿与 Radix 概览实现按相同 1487 × 1058 尺寸并排；`.omx/audits/radix-ui-migration/before-vs-radix-contact-sheet.png` 将迁移前后的九页联系表置于同一比较输入。
- CSS viewport 与所有完整页面截图均为 1487 × 1058，`deviceScaleFactor = 1`；本轮遵循用户要求，不把移动端适配列入验收范围。

### Iteration 9

- P1：首次迁移后，设置页的下载按钮通过 Radix Slot 组合时同时传入条件加载节点与链接节点，违反 Slot 的唯一子元素约束，切到设置页会触发运行时异常并显示空白页。修复为 `asChild` 独立渲染分支，只向 Slot 传递唯一链接元素；重新捕获九页后均正常显示，Runtime 异常为零。
- P2：原生 Select、手写 Dialog、Motion Tooltip/Switch/Tabs、虚拟表格和通知栈仍分别维护交互状态，键盘行为与浮层层级不一致。修复为 Radix Select、Dialog/AlertDialog、Tooltip、Switch、ToggleGroup、ScrollArea、Toast 与 Slot 适配层；按钮、表格和通知视觉继续使用已确认的桌面令牌，没有引入第二套配色或间距。
- P2：规则模式原来只有“仅在规则分流策略下生效”的短句，无法解释三种 mode、规则优先级和独立端口边界。修复为当前模式动态说明：rule 明确“自定义规则 → 远程规则源 → 内置规则、首条命中”，global 明确进入 PROXY 选择组，direct 明确跳过规则与代理；同时标注只影响主入口，节点端口、自动选优入口和分组端口不受影响。
- P2：固定节点不可用时，初版说明把所有回退都称为“规则模式”，没有体现顶层 `mode` 仍可能是 global 或 direct。修复为区分持久化策略与运行时入口形态：固定 listener 失效后恢复 mixed-port 并继续遵循当前 mode，出口名称、状态徽标和告警文案均按 rule/global/direct 显示真实结果。
- 迁移前后同屏检查确认：暖白背景、蓝/青/琥珀状态职责、三栏概览、各页标题基线、筛选器密度、表格列边界与设置页双列节奏保持一致；没有可执行的 P0、P1 或 P2 视觉问题。

### 组件、交互与构建检查

- 已彻底移除旧 `components/motion/`、Motion 动效令牌和悬停能力辅助文件，并卸载 `motion`、`@tanstack/react-virtual`；应用源码不再引用旧 beUI/Motion 组件。
- Chrome 实际交互通过：命令菜单打开与 Escape 关闭；Radix Select 打开、Escape 关闭与焦点归还；添加节点、添加订阅、策略分组三个 Dialog 打开/关闭；设置页章节定位。
- 规则模式结构断言通过：三个 ToggleGroup 选项中始终只有一个 `data-state="on"`，详细说明块存在；验收没有点击 mode、系统代理、TUN 或端口映射等写操作，不改变代理配置。
- 八个业务页均保持唯一一级标题且无横向溢出；全页面截图和交互过程中没有 `Runtime.exceptionThrown`。
- `npm run build`、`go test ./internal/api ./internal/app`、`git diff --check` 均成功；本地预览与隔离 API 分别返回 HTTP 200。

final result: passed
