/**
 * Radix 数据表格适配层。
 *
 * 功能说明：
 * 使用 Radix ScrollArea 管理双向滚动条，并保留业务页面依赖的列声明、排序、空状态
 * 与列宽调整能力。HTML table 继续承担真正的行列语义，避免为了视觉效果破坏读屏
 * 软件对表头和单元格关系的识别。
 *
 * 可能的异常/错误情况：
 * 本表格不再执行虚拟化；如果未来单页出现数千行数据，应优先在 API 端分页，而不是
 * 让浏览器一次渲染全部连接记录。
 */
import React, { useMemo, useRef, useState } from "react";
import { ScrollArea } from "radix-ui";
import { ArrowDown, ArrowUp, ChevronsUpDown } from "lucide-react";
import { cn } from "../../lib/utils";

/**
 * readCell 读取某一列对应的展示内容。
 *
 * 参数说明：
 * - row: object，当前业务记录。
 * - column: object，列定义；cell 可提供自定义渲染函数。
 *
 * 返回值说明：
 * 优先返回 column.cell(row)，否则返回 row[column.key]，空值显示短横线。
 *
 * 可能的异常/错误情况：
 * 自定义 cell 抛出的渲染错误会交给 React 错误边界处理，组件不会静默吞掉。
 */
function readCell(row, column) {
  if (typeof column.cell === "function") return column.cell(row);
  const value = row?.[column.key];
  return value == null || value === "" ? "-" : value;
}

/**
 * readSortValue 获取用于比较的稳定原始值。
 *
 * 参数说明：row 为业务记录，column 为列定义。
 * 返回值说明：存在 sortValue 时使用其结果，否则读取 row[column.key]。
 * 可能的异常/错误情况：sortValue 抛错时错误继续向上冒泡，避免显示一个看似有效但
 * 实际顺序错误的表格。
 */
function readSortValue(row, column) {
  return typeof column.sortValue === "function" ? column.sortValue(row) : row?.[column.key];
}

/**
 * compareValues 对字符串、数字和空值执行可预测排序。
 *
 * 参数说明：left/right 为两侧值，direction 为 asc 或 desc。
 * 返回值说明：返回 Array.sort 所需的负数、零或正数。
 * 可能的异常/错误情况：非数字复杂对象会转成字符串比较；业务列若需要其他规则，
 * 应通过 sortValue 先转换成可比较值。
 */
function compareValues(left, right, direction) {
  const factor = direction === "desc" ? -1 : 1;
  if (left == null && right == null) return 0;
  if (left == null) return 1;
  if (right == null) return -1;
  if (typeof left === "number" && typeof right === "number") return (left - right) * factor;
  return String(left).localeCompare(String(right), "zh-CN", { numeric: true, sensitivity: "base" }) * factor;
}

/**
 * normalizeWidth 把列宽声明转换成可写入 style 的值。
 *
 * 参数说明：width 为 number、CSS 字符串或空值。
 * 返回值说明：数字转换为 px，字符串保持不变，空值返回 undefined。
 * 可能的异常/错误情况：无效 CSS 值会被浏览器忽略。
 */
function normalizeWidth(width) {
  if (typeof width === "number") return `${width}px`;
  return width || undefined;
}

/**
 * Table 渲染支持排序与调整列宽的桌面数据表格。
 *
 * 参数说明：
 * - data: Array<object>，业务行数据。
 * - columns: Array<object>，列定义，至少包含唯一 key 与 header。
 * - getRowId: (row, index) => string，生成稳定行标识。
 * - height: number|string，滚动视口高度。
 * - emptyState: ReactNode，无数据时内容。
 * - minColumnWidth: number，用户拖拽时允许的最小列宽。
 * - resizable: boolean，是否显示列宽拖拽手柄。
 * - className: string，容器附加样式。
 * - getRowClassName: (row) => string，可选行附加 class（如主端口节点高亮）。
 *
 * 返回值说明：
 * 返回 Radix ScrollArea 包裹的语义化 table。
 *
 * 可能的异常/错误情况：
 * columns 的 key 重复会破坏排序和列宽状态；getRowId 不稳定会导致 React 重新挂载
 * 行内容，调用方必须保证二者稳定。
 */
function Table({
  data = [],
  columns = [],
  getRowId,
  height = 440,
  emptyState = "暂无数据",
  minColumnWidth = 80,
  resizable = false,
  className,
  getRowClassName,
}) {
  const [sort, setSort] = useState(null);
  const [widths, setWidths] = useState({});
  const resizeRef = useRef(null);

  const rows = useMemo(() => {
    if (!sort) return data;
    const column = columns.find((item) => item.key === sort.key);
    if (!column) return data;
    return [...data].sort((left, right) => compareValues(
      readSortValue(left, column),
      readSortValue(right, column),
      sort.direction,
    ));
  }, [columns, data, sort]);

  /**
   * toggleSort 循环切换表头的升序和降序状态。
   *
   * 参数说明：column 为被点击的列定义。
   * 返回值说明：无；更新组件内部排序状态。
   * 可能的异常/错误情况：不可排序列会被直接忽略。
   */
  function toggleSort(column) {
    if (!column.sortable) return;
    setSort((current) => ({
      key: column.key,
      direction: current?.key === column.key && current.direction === "asc" ? "desc" : "asc",
    }));
  }

  /**
   * startResize 记录拖拽起点与原列宽。
   *
   * 参数说明：event 为 PointerEvent，column 为目标列，header 为实际表头元素。
   * 返回值说明：无；建立本次拖拽上下文并捕获指针。
   * 可能的异常/错误情况：浏览器不支持 pointer capture 时仍可拖拽，但指针离开手柄
   * 后可能停止更新；现代桌面浏览器均支持该能力。
   */
  function startResize(event, column, header) {
    event.preventDefault();
    event.currentTarget.setPointerCapture?.(event.pointerId);
    resizeRef.current = {
      key: column.key,
      pointerId: event.pointerId,
      startX: event.clientX,
      startWidth: header.getBoundingClientRect().width,
    };
  }

  /**
   * moveResize 根据指针位移实时计算列宽。
   *
   * 参数说明：event 为 PointerEvent。
   * 返回值说明：无；没有活动拖拽时不更新状态。
   * 可能的异常/错误情况：极端负向位移会被 minColumnWidth 截断，避免内容消失。
   */
  function moveResize(event) {
    const current = resizeRef.current;
    if (!current || current.pointerId !== event.pointerId) return;
    const nextWidth = Math.max(minColumnWidth, Math.round(current.startWidth + event.clientX - current.startX));
    setWidths((existing) => ({ ...existing, [current.key]: nextWidth }));
  }

  /**
   * endResize 结束列宽拖拽并释放上下文。
   *
   * 参数说明：event 为 PointerEvent。
   * 返回值说明：无。
   * 可能的异常/错误情况：非当前 pointerId 的结束事件会被忽略，防止多指针误终止。
   */
  function endResize(event) {
    if (resizeRef.current?.pointerId === event.pointerId) resizeRef.current = null;
  }

  /**
   * renderSortIcon 返回当前表头对应的排序图标。
   *
   * 参数说明：column 为列定义。
   * 返回值说明：返回 Lucide 图标元素，不可排序列返回 null。
   * 可能的异常/错误情况：无。
   */
  function renderSortIcon(column) {
    if (!column.sortable) return null;
    if (sort?.key !== column.key) return <ChevronsUpDown size={13} aria-hidden="true" />;
    return sort.direction === "asc"
      ? <ArrowUp size={13} aria-hidden="true" />
      : <ArrowDown size={13} aria-hidden="true" />;
  }

  const tableMinWidth = columns.reduce((total, column) => total + (widths[column.key] || minColumnWidth), 0);

  return (
    <ScrollArea.Root className={cn("radix-data-table data-table", className)} style={{ height }}>
      <ScrollArea.Viewport className="radix-data-table-viewport">
        <table style={{ minWidth: `max(100%, ${tableMinWidth}px)` }}>
          <colgroup>
            {columns.map((column) => (
              <col key={column.key} style={{ width: normalizeWidth(widths[column.key] || column.width) }} />
            ))}
          </colgroup>
          <thead>
            <tr>
              {columns.map((column) => (
                <th key={column.key} scope="col" style={{ textAlign: column.align || "left" }}>
                  <button
                    type="button"
                    className="radix-data-table-sort"
                    disabled={!column.sortable}
                    onClick={() => toggleSort(column)}
                  >
                    <span>{column.header}</span>
                    {renderSortIcon(column)}
                  </button>
                  {resizable ? (
                    <span
                      aria-hidden="true"
                      className="radix-data-table-resizer"
                      onPointerDown={(event) => startResize(event, column, event.currentTarget.parentElement)}
                      onPointerMove={moveResize}
                      onPointerUp={endResize}
                      onPointerCancel={endResize}
                    />
                  ) : null}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 ? (
              <tr>
                <td className="radix-data-table-empty" colSpan={Math.max(1, columns.length)}>{emptyState}</td>
              </tr>
            ) : rows.map((row, index) => (
              <tr key={getRowId ? getRowId(row, index) : String(index)} className={getRowClassName?.(row) || undefined}>
                {columns.map((column) => (
                  <td key={column.key} style={{ textAlign: column.align || "left" }}>{readCell(row, column)}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </ScrollArea.Viewport>
      <ScrollArea.Scrollbar className="radix-scrollbar" orientation="vertical">
        <ScrollArea.Thumb className="radix-scroll-thumb" />
      </ScrollArea.Scrollbar>
      <ScrollArea.Scrollbar className="radix-scrollbar" orientation="horizontal">
        <ScrollArea.Thumb className="radix-scroll-thumb" />
      </ScrollArea.Scrollbar>
      <ScrollArea.Corner className="radix-scroll-corner" />
    </ScrollArea.Root>
  );
}

export { Table };
