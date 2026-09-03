import { useMemo } from "react";
import { CircleAlert, Clock3, Link2, RefreshCw, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Table } from "@/components/ui/data-table";
import { Input } from "@/components/ui/input";
import { SegmentedControl } from "@/components/ui/segmented-control";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { PanelTitle } from "@/components/PanelTitle";
import { classNames, formatBytes, tableViewportHeight } from "@/lib/format";

/**
 * ConnectionsPage 渲染活动连接页。
 *
 * 功能说明：
 * 该页面把“摘要、筛选、桌面表格、移动卡片、关闭操作”放在一屏里，目标是让普通用户
 * 先看懂哪里在连、连到了哪、为什么还在跑，再决定是否关闭单条或关闭全部。
 *
 * 参数说明：
 * - activeCount: number，活动连接总数。
 * - closingAll: boolean，是否正在执行关闭全部。
 * - error: string，最近一次加载错误文本。
 * - hasLoaded: boolean，是否至少完成过一次加载尝试。
 * - loading: boolean，是否处于首次加载。
 * - pendingIds: Set<string>，正在关闭中的连接 id 集合。
 * - query: string，搜索词。
 * - refreshing: boolean，是否正在刷新列表。
 * - retryConnections: Function，重新加载列表。
 * - rows: Array<object>，全部连接模型。
 * - summary: object，活动数、累计流量和内存摘要。
 * - transport: string，当前协议筛选值。
 * - updatedAt: Date | null，最近一次刷新时间。
 * - visibleRows: Array<object>，当前筛选后可见的连接。
 * - setQuery/setTransport: Function，搜索与协议筛选 setter。
 * - closeAllConnections/closeConnection: Function，关闭全部与关闭单条动作。
 *
 * 返回值说明：
 * 返回活动连接页 React 元素。
 *
 * 可能的异常/错误情况：
 * - 列表为空或筛选无结果时渲染空状态。
 * - 加载失败时保留旧数据并显示错误条带，用户可点击重试。
 */
export function ConnectionsPage({
  activeCount,
  closingAll,
  error,
  hasLoaded,
  loading,
  pendingIds,
  query,
  refreshing,
  retryConnections,
  rows,
  summary,
  transport,
  updatedAt,
  visibleRows,
  setQuery,
  setTransport,
  closeAllConnections,
  closeConnection,
}) {
  const initialLoading = loading && !hasLoaded && rows.length === 0;
  const updatedText = updatedAt
    ? updatedAt.toLocaleTimeString("zh-CN", { hour12: false })
    : "—";
  const visibleCount = visibleRows.length;
  const hasRows = rows.length > 0;
  const hasVisibleRows = visibleRows.length > 0;
  const memoryValue = summary.memory ?? "—";
  const memoryFormatter =
    typeof memoryValue === "number" ? formatBytes : undefined;

  const emptyTitle =
    error && !hasRows
      ? "连接列表加载失败"
      : hasRows && !hasVisibleRows
        ? "没有匹配的连接"
        : "当前没有活动连接";
  const emptyDetail =
    error && !hasRows
      ? `${error}，点击重试重新加载。`
      : hasRows && !hasVisibleRows
        ? "当前筛选条件下没有匹配的连接。"
        : "当代理开始接入流量后，这里会显示域名、入口、出口链和开始时间。";

  const connectionColumns = useMemo(
    () => [
      {
        key: "targetLabel",
        header: "目标域名/地址",
        sortable: true,
        width: "28%",
        cell: (row) => (
          <div className="connections-cell-stack">
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <span className="min-w-0 break-words font-medium text-foreground">
                {row.targetLabel}
              </span>
              <Badge variant="outline">{row.protocolLabel}</Badge>
            </div>
            {row.targetDetail && <small>{row.targetDetail}</small>}
          </div>
        ),
      },
      {
        key: "entryLabel",
        header: "入口与来源",
        sortable: true,
        width: "19%",
        cell: (row) => (
          <div className="connections-cell-stack">
            <span>
              <b>入口：</b>
              {row.entryLabel}
            </span>
            <small>
              <b>来源：</b>
              {row.sourceLabel}
            </small>
          </div>
        ),
        sortValue: (row) => `${row.entryLabel} ${row.sourceLabel}`,
      },
      {
        key: "exitLabel",
        header: "出口链",
        sortable: true,
        width: "20%",
        cell: (row) => (
          <span className="connections-chain-text">{row.exitLabel}</span>
        ),
      },
      {
        key: "totalBytes",
        header: "累计流量",
        sortable: true,
        width: "150px",
        cell: (row) => (
          <div className="connections-cell-stack">
            <span>{formatBytes(row.totalBytes)}</span>
            <small>
              ↑ {formatBytes(row.uploadBytes)} / ↓{" "}
              {formatBytes(row.downloadBytes)}
            </small>
          </div>
        ),
      },
      {
        key: "startedLabel",
        header: "开始时间",
        sortable: true,
        width: "124px",
        cell: (row) => (
          <time
            className="connections-time-cell"
            dateTime={row.startedDateTime || undefined}
          >
            {row.startedLabel}
          </time>
        ),
        sortValue: (row) => row.startedDateTime || row.startedLabel,
      },
      {
        key: "close",
        header: "关闭",
        align: "center",
        width: "84px",
        cell: (row) => {
          const isPending = pendingIds.has(row.closeId);
          return (
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  aria-label={`关闭连接 ${row.targetLabel}`}
                  disabled={!row.closeable || isPending}
                  loading={isPending}
                  size="icon"
                  type="button"
                  variant="destructive-ghost"
                  onClick={() => closeConnection(row)}
                >
                  {!isPending && <X size={16} aria-hidden="true" />}
                </Button>
              </TooltipTrigger>
              <TooltipContent>关闭连接</TooltipContent>
            </Tooltip>
          );
        },
      },
    ],
    [closeConnection, pendingIds],
  );

  return (
    <section
      aria-busy={loading || refreshing}
      aria-label="活动连接"
      className="connections-page grid gap-4"
    >
      {(loading || refreshing) && (
        <span className="sr-only" role="status">
          正在更新活动连接
        </span>
      )}
      <PageHeader
        eyebrow="运行监测"
        title="活动连接"
        detail="查看实时流量、出口链与连接来源，并可按协议快速筛选。"
      >
        <div className="page-actions">
          <div className="connection-refresh-state">
            <div>
              {refreshing && <Badge variant="secondary">更新中</Badge>}
              <Badge variant="outline">
                {visibleCount}/{activeCount}
              </Badge>
            </div>
            <p>
              <Clock3 size={14} aria-hidden="true" />
              <span>刷新于</span>
              <time dateTime={updatedAt?.toISOString() || undefined}>
                {updatedText}
              </time>
            </p>
          </div>
          <Button
            disabled={loading}
            loading={refreshing}
            type="button"
            variant="outline"
            onClick={retryConnections}
            aria-label="刷新活动连接"
          >
            <RefreshCw
              className={classNames(refreshing && "animate-spin")}
              size={16}
              aria-hidden="true"
            />
            <span>刷新</span>
          </Button>
          <Button
            className="border-destructive/40 text-destructive hover:bg-destructive/10"
            disabled={!hasRows || closingAll}
            loading={closingAll}
            type="button"
            variant="outline"
            onClick={closeAllConnections}
            aria-label="关闭全部活动连接"
          >
            <X size={16} aria-hidden="true" />
            <span>关闭全部</span>
          </Button>
        </div>
      </PageHeader>

      {error && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-warning/30 bg-warning-soft px-4 py-3 text-sm text-warning">
          <div className="flex min-w-0 items-start gap-2">
            <CircleAlert size={16} aria-hidden="true" />
            <span className="min-w-0 break-words">{error}</span>
          </div>
          <Button
            className="h-11"
            type="button"
            variant="outline"
            onClick={retryConnections}
            aria-label="重试加载活动连接"
          >
            <RefreshCw size={16} aria-hidden="true" />
            <span>重试</span>
          </Button>
        </div>
      )}

      {initialLoading ? (
        <ConnectionsSkeleton />
      ) : (
        <>
          <div className="grid grid-cols-2 gap-3 xl:grid-cols-4">
            <Metric
              detail={
                visibleCount === activeCount
                  ? "当前筛选显示全部连接"
                  : `当前筛选显示 ${visibleCount} 条`
              }
              label="活动连接"
              value={summary.activeCount}
            />
            <Metric
              detail="当前连接快照"
              format={formatBytes}
              label="累计上行"
              value={summary.uploadBytes}
            />
            <Metric
              detail="当前连接快照"
              format={formatBytes}
              label="累计下行"
              value={summary.downloadBytes}
            />
            <Metric
              detail="内核运行占用"
              format={memoryFormatter}
              label="内存"
              value={memoryValue}
            />
          </div>

          <section className="panel">
            <PanelTitle title="筛选与搜索" />
            <div className="grid gap-3 xl:grid-cols-[minmax(0,1fr)_auto] xl:items-end">
              <Field label="搜索">
                <Input
                  aria-label="搜索活动连接"
                  className="h-11"
                  placeholder="搜索域名 / IP / 进程 / 节点链"
                  value={query}
                  onChange={(event) => setQuery(event.target.value)}
                />
              </Field>
              <Field compact label="协议筛选">
                <SegmentedControl
                  ariaLabel="连接协议筛选"
                  onValueChange={setTransport}
                  options={[
                    { value: "all", label: "全部" },
                    { value: "tcp", label: "TCP" },
                    { value: "udp", label: "UDP" },
                  ]}
                  value={transport}
                />
              </Field>
            </div>
          </section>

          {!hasVisibleRows ? (
            <ConnectionsEmptyState
              detail={emptyDetail}
              onRetry={error ? retryConnections : null}
              title={emptyTitle}
            />
          ) : (
            <>
              <Table
                className="data-table hidden rounded-xl shadow-sm md:block"
                columns={connectionColumns}
                data={visibleRows}
                emptyState="暂无活动连接"
                getRowId={(row) => row.id}
                height={tableViewportHeight(visibleRows.length, 960)}
                minColumnWidth={88}
                resizable
              />

              <div className="grid gap-3 md:hidden">
                {visibleRows.map((row) => {
                  const isPending = pendingIds.has(row.closeId);
                  return (
                    <article
                      className="grid gap-4 rounded-md border bg-card p-4 shadow-sm"
                      key={row.id}
                    >
                      <div className="flex items-start justify-between gap-3">
                        <div className="min-w-0">
                          <div className="flex flex-wrap items-center gap-2">
                            <h3 className="min-w-0 break-words text-sm font-semibold text-foreground">
                              {row.targetLabel}
                            </h3>
                            <Badge variant="outline">{row.protocolLabel}</Badge>
                          </div>
                          {row.targetDetail && (
                            <p className="mt-1 break-words text-xs leading-5 text-muted-foreground">
                              {row.targetDetail}
                            </p>
                          )}
                        </div>
                        <Button
                          aria-label={`关闭连接 ${row.targetLabel}`}
                          className="h-11 w-11"
                          disabled={!row.closeable || isPending}
                          loading={isPending}
                          size="icon"
                          type="button"
                          variant="destructive-ghost"
                          onClick={() => closeConnection(row)}
                        >
                          <X size={16} aria-hidden="true" />
                        </Button>
                      </div>

                      <dl className="grid gap-3 text-sm">
                        <div className="grid gap-1">
                          <dt className="text-xs font-medium text-muted-foreground">
                            入口与来源
                          </dt>
                          <dd className="break-words text-foreground">
                            <span className="font-medium">入口：</span>
                            {row.entryLabel}
                            <br />
                            <span className="font-medium">来源：</span>
                            {row.sourceLabel}
                          </dd>
                        </div>
                        <div className="grid gap-1">
                          <dt className="text-xs font-medium text-muted-foreground">
                            出口链
                          </dt>
                          <dd className="break-words text-foreground">
                            {row.exitLabel}
                          </dd>
                        </div>
                        <div className="grid gap-1">
                          <dt className="text-xs font-medium text-muted-foreground">
                            累计流量
                          </dt>
                          <dd className="text-foreground">
                            {formatBytes(row.totalBytes)}
                            <span className="mt-1 block text-xs text-muted-foreground">
                              ↑ {formatBytes(row.uploadBytes)} / ↓{" "}
                              {formatBytes(row.downloadBytes)}
                            </span>
                          </dd>
                        </div>
                        <div className="grid gap-1">
                          <dt className="text-xs font-medium text-muted-foreground">
                            开始时间
                          </dt>
                          <dd>
                            <time dateTime={row.startedDateTime || undefined}>
                              {row.startedLabel}
                            </time>
                          </dd>
                        </div>
                      </dl>
                    </article>
                  );
                })}
              </div>
            </>
          )}
        </>
      )}
    </section>
  );
}

/**
 * ConnectionsSkeleton 渲染活动连接页的首次加载骨架。
 *
 * 功能说明：
 * 首次进入页面时，连接数据通常还没回来。这里不用空白页面，而是给用户一个和
 * 实际布局相近的骨架，让他们知道页面在加载什么内容，同时避免等待时发生布局跳变。
 *
 * 参数说明：
 * 无。
 *
 * 返回值说明：
 * 返回骨架屏 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；仅用于表现层。
 */
function ConnectionsSkeleton() {
  return (
    <div className="grid gap-4" aria-hidden="true">
      <div className="grid grid-cols-2 gap-3 xl:grid-cols-4">
        {Array.from({ length: 4 }).map((_, index) => (
          <div
            className="grid min-h-[112px] gap-3 rounded-md border bg-card p-4"
            key={index}
          >
            <div className="h-3 w-20 animate-pulse rounded-full bg-muted" />
            <div className="h-9 w-28 animate-pulse rounded-md bg-muted" />
            <div className="h-3 w-32 animate-pulse rounded-full bg-muted" />
          </div>
        ))}
      </div>
      <section className="panel">
        <div className="grid gap-3 xl:grid-cols-[minmax(0,1fr)_auto] xl:items-end">
          <div className="grid gap-2">
            <div className="h-3 w-24 animate-pulse rounded-full bg-muted" />
            <div className="h-11 w-full animate-pulse rounded-md bg-muted" />
          </div>
          <div className="grid gap-2">
            <div className="h-3 w-20 animate-pulse rounded-full bg-muted" />
            <div className="h-11 w-[300px] max-w-full animate-pulse rounded-md bg-muted" />
          </div>
        </div>
      </section>
      <div className="hidden gap-0 overflow-hidden rounded-md border bg-card md:block">
        <div className="grid gap-0">
          {Array.from({ length: 5 }).map((_, index) => (
            <div
              className="grid grid-cols-6 gap-4 border-b px-4 py-4 last:border-b-0"
              key={index}
            >
              {Array.from({ length: 6 }).map((__, columnIndex) => (
                <div
                  className="h-4 w-full animate-pulse rounded-full bg-muted"
                  key={columnIndex}
                />
              ))}
            </div>
          ))}
        </div>
      </div>
      <div className="grid gap-3 md:hidden">
        {Array.from({ length: 3 }).map((_, index) => (
          <article
            className="grid gap-3 rounded-md border bg-card p-4"
            key={index}
          >
            <div className="flex items-start justify-between gap-3">
              <div className="grid gap-2">
                <div className="h-4 w-40 animate-pulse rounded-full bg-muted" />
                <div className="h-3 w-24 animate-pulse rounded-full bg-muted" />
              </div>
              <div className="h-11 w-11 animate-pulse rounded-md bg-muted" />
            </div>
            <div className="grid gap-2">
              <div className="h-3 w-28 animate-pulse rounded-full bg-muted" />
              <div className="h-3 w-48 animate-pulse rounded-full bg-muted" />
              <div className="h-3 w-36 animate-pulse rounded-full bg-muted" />
            </div>
          </article>
        ))}
      </div>
    </div>
  );
}

/**
 * ConnectionsEmptyState 渲染活动连接页的空状态。
 *
 * 功能说明：
 * 空状态需要区分两种情况：完全没有活动连接，和筛选条件把结果过滤空了。
 * 如果是加载失败，这里还能提供一个明确的重试入口，减少用户在错误条带和页面
 * 中间来回寻找操作按钮的负担。
 *
 * 参数说明：
 * - title: string，空状态标题。
 * - detail: string，空状态说明。
 * - onRetry: Function | null，是否提供重试按钮。
 *
 * 返回值说明：
 * 返回空状态 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；按钮缺省时只展示说明文本。
 */
function ConnectionsEmptyState({ title, detail, onRetry }) {
  return (
    <section className="empty-state">
      <Link2 size={28} />
      <h2>{title}</h2>
      <p>{detail}</p>
      {onRetry && (
        <Button
          className="mt-4 h-11"
          type="button"
          variant="outline"
          onClick={onRetry}
          aria-label="重试加载活动连接"
        >
          <RefreshCw size={16} aria-hidden="true" />
          <span>重试</span>
        </Button>
      )}
    </section>
  );
}

/**
 * Metric 渲染指标卡。
 *
 * 功能说明：
 * 这个组件已经在概览页里承担了“快速扫一眼数字”的职责，因此连接页继续复用它，
 * 只在字节类指标上通过可选 `format` 控制展示单位，避免重复造另一套统计卡。
 *
 * 参数说明：
 * - label: string，指标名。
 * - value: string | number，指标值。
 * - detail: string，补充信息。
 * - format: Function | undefined，数字格式化函数；仅在 `value` 是数字时生效。
 *
 * 返回值说明：
 * 返回指标卡 React 元素。
 *
 * 可能的异常/错误情况：
 * - `format` 抛错会继续向外传播，方便快速定位格式化逻辑问题。
 */
function Metric({ label, value, detail, format }) {
  return (
    <section className="metric">
      <span>{label}</span>
      <strong className="tabular-nums">
        {typeof value === "number"
          ? format
            ? format(value)
            : Math.round(value).toString()
          : value}
      </strong>
      <small>{detail}</small>
    </section>
  );
}
