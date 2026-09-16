package subscribe

// 订阅草稿领域：比较同一订阅应用前后的节点实体集合，只输出不含凭据的差异值对象。

import (
	"sort"

	"proxyd/internal/proxy/node"
)

// DraftNodeChange 表示一个可安全展示的节点变化。
//
// 该值对象刻意不包含 Node.Key 或 Mapping：稳定身份可能包含密码、UUID、私钥等凭据，
// 差异预览只能把展示名、健康状态与延迟返回到 API/UI。
type DraftNodeChange struct {
	Status      string `json:"status"` // added|removed|updated
	Name        string `json:"name"`
	BeforeAlive *bool  `json:"before_alive,omitempty"`
	AfterAlive  *bool  `json:"after_alive,omitempty"`
	BeforeDelay uint16 `json:"before_delay,omitempty"`
	AfterDelay  uint16 `json:"after_delay,omitempty"`
}

// DraftDiff 是一次订阅刷新草稿的领域差异摘要。
type DraftDiff struct {
	BeforeTotal int               `json:"before_total"`
	AfterTotal  int               `json:"after_total"`
	BeforeAlive int               `json:"before_alive"`
	AfterAlive  int               `json:"after_alive"`
	Added       int               `json:"added"`
	Removed     int               `json:"removed"`
	Updated     int               `json:"updated"`
	Unchanged   int               `json:"unchanged"`
	Changes     []DraftNodeChange `json:"changes"`
}

// CompareDraftNodes 比较同一订阅应用前后的节点集合。
//
// 参数：
//   - before: []*node.Node，当前已生效的订阅节点；nil 项会被忽略。
//   - after: []*node.Node，草稿候选订阅节点；nil 项会被忽略。
//
// 返回值：DraftDiff，包含总量、健康数量与新增/删除/状态变化明细；明细按状态和名称
// 稳定排序，确保 API 响应与测试不受 map 遍历顺序影响。
//
// 错误情况：本函数不返回错误。重复稳定身份以切片中最后一个实体为准；正常调用点的
// 集合已经过 MergeFiltered 去重，因此该保护只用于容忍异常输入。
func CompareDraftNodes(before, after []*node.Node) DraftDiff {
	diff := DraftDiff{
		BeforeTotal: countDraftNodes(before),
		AfterTotal:  countDraftNodes(after),
		BeforeAlive: countAliveDraftNodes(before),
		AfterAlive:  countAliveDraftNodes(after),
		Changes:     []DraftNodeChange{},
	}
	beforeByKey := indexDraftNodes(before)
	afterByKey := indexDraftNodes(after)

	for key, previous := range beforeByKey {
		candidate, exists := afterByKey[key]
		if !exists {
			alive := previous.Alive
			diff.Removed++
			diff.Changes = append(diff.Changes, DraftNodeChange{
				Status: "removed", Name: previous.Name, BeforeAlive: &alive, BeforeDelay: previous.Delay,
			})
			continue
		}
		if previous.Name == candidate.Name && previous.Alive == candidate.Alive && previous.Delay == candidate.Delay {
			diff.Unchanged++
			continue
		}
		beforeAlive, afterAlive := previous.Alive, candidate.Alive
		diff.Updated++
		diff.Changes = append(diff.Changes, DraftNodeChange{
			Status: "updated", Name: candidate.Name,
			BeforeAlive: &beforeAlive, AfterAlive: &afterAlive,
			BeforeDelay: previous.Delay, AfterDelay: candidate.Delay,
		})
	}
	for key, candidate := range afterByKey {
		if _, exists := beforeByKey[key]; exists {
			continue
		}
		alive := candidate.Alive
		diff.Added++
		diff.Changes = append(diff.Changes, DraftNodeChange{
			Status: "added", Name: candidate.Name, AfterAlive: &alive, AfterDelay: candidate.Delay,
		})
	}
	sort.Slice(diff.Changes, func(i, j int) bool {
		if diff.Changes[i].Status != diff.Changes[j].Status {
			return diff.Changes[i].Status < diff.Changes[j].Status
		}
		return diff.Changes[i].Name < diff.Changes[j].Name
	})
	return diff
}

// indexDraftNodes 按仅限进程内使用的稳定身份建立节点索引。
//
// 参数：nodes 为待索引节点集合。
// 返回值：map[string]*node.Node，键可能包含凭据，只允许留在领域计算内部。
// 错误情况：nil 节点会被忽略；重复身份以后出现者覆盖前者。
func indexDraftNodes(nodes []*node.Node) map[string]*node.Node {
	indexed := make(map[string]*node.Node, len(nodes))
	for _, candidate := range nodes {
		if candidate != nil {
			indexed[candidate.Key()] = candidate
		}
	}
	return indexed
}

// countDraftNodes 统计非 nil 节点数。
//
// 参数：nodes 为待统计集合。
// 返回值：int，集合中的有效实体数量。
// 错误情况：无；nil 集合返回 0。
func countDraftNodes(nodes []*node.Node) int {
	count := 0
	for _, candidate := range nodes {
		if candidate != nil {
			count++
		}
	}
	return count
}

// countAliveDraftNodes 统计非 nil 且健康的节点数。
//
// 参数：nodes 为待统计集合。
// 返回值：int，Alive=true 的实体数量。
// 错误情况：无；nil 集合返回 0。
func countAliveDraftNodes(nodes []*node.Node) int {
	count := 0
	for _, candidate := range nodes {
		if candidate != nil && candidate.Alive {
			count++
		}
	}
	return count
}
