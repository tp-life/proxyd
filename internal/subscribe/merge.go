package subscribe

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"

	"proxyd/internal/config"
	"proxyd/internal/node"
)

// Merge 合并多个订阅的节点：
//   - 按 Node.Key() 去重，先出现的保留（订阅按名字排序后依次处理，保证结果稳定）；
//   - excludeRe 非 nil 时按节点名过滤掉匹配项；
//   - 保证 Name 全局唯一：冲突时追加 " (订阅名)"，仍冲突再追加序号；
//   - 同步设置 Mapping["name"] 和 Node.Subscription。
func Merge(subs map[string][]*node.Node, excludeRe *regexp.Regexp) []*node.Node {
	names := make([]string, 0, len(subs))
	for name := range subs {
		names = append(names, name)
	}
	sort.Strings(names)

	seenKey := map[string]bool{}
	usedName := map[string]bool{}
	var out []*node.Node
	for _, subName := range names {
		for _, n := range subs[subName] {
			if n == nil || n.Mapping == nil {
				continue
			}
			key := n.Key()
			if seenKey[key] {
				continue
			}
			seenKey[key] = true
			if excludeRe != nil && excludeRe.MatchString(n.Name) {
				continue
			}
			n.Subscription = subName
			n.Name = uniqueName(n.Name, subName, usedName)
			n.Mapping["name"] = n.Name
			out = append(out, n)
		}
	}
	return out
}

// uniqueName 保证节点名全局唯一：冲突时追加 " (订阅名)"，仍冲突再追加序号。
func uniqueName(name, subName string, used map[string]bool) string {
	if !used[name] {
		used[name] = true
		return name
	}
	cand := fmt.Sprintf("%s (%s)", name, subName)
	if !used[cand] {
		used[cand] = true
		return cand
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s %d", cand, i)
		if !used[c] {
			used[c] = true
			return c
		}
	}
}

// FetchAll 并发拉取所有订阅并合并节点。
// 单个订阅失败不影响其他订阅；返回与 subs 一一对应的错误切片（nil 表示成功，
// *FetchWarning 表示拉取失败但降级用了缓存）。
func FetchAll(ctx context.Context, subs []config.Subscription, stateDir string, excludeRe *regexp.Regexp) ([]*node.Node, []error) {
	nodesBySub := make(map[string][]*node.Node, len(subs))
	errs := make([]error, len(subs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, sub := range subs {
		wg.Add(1)
		go func(i int, sub config.Subscription) {
			defer wg.Done()
			nodes, err := Fetch(ctx, sub, stateDir)
			if err != nil {
				var w *FetchWarning
				if !errors.As(err, &w) {
					errs[i] = err
					return
				}
				// 警告：拉取失败但有缓存节点可用
				errs[i] = err
			}
			mu.Lock()
			nodesBySub[sub.Name] = nodes
			mu.Unlock()
		}(i, sub)
	}
	wg.Wait()
	return Merge(nodesBySub, excludeRe), errs
}
