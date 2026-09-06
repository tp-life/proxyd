package remote

// 本文件集中适配 DERP 发现与持久区域缓存，避免网络引导依赖散落在应用层。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
)

// 默认地图不可达时使用 Tailscale 官方地图；自定义来源绝不混入公共区域。
const fallbackDERPMapURL = "https://controlplane.tailscale.com/derpmap/default"

// derpRegionCache 保存来源与完整区域，不包含隧道私钥；来源变更必须重新发现。
type derpRegionCache struct {
	Source string              `json:"source"`
	Region *tailcfg.DERPRegion `json:"region"`
}

// discoverDERPRegion 从有序来源发现区域，每个来源最多占用五秒，总期限由 ctx 控制。
// 参数：ctx 为 context.Context；id 为区域编号（-1 自动）；sources 为地图 URL 列表。
// 返回：完整区域及 error；所有来源失败时合并错误，不生成虚假的可用区域。
// 先获取地图再探测，避免首个坏域名耗尽全部探测预算；每次使用独立 ConnInfo 防止部分失败污染重试。
func discoverDERPRegion(ctx context.Context, id tailcfg.DERPRegionID, sources []string) (*tailcfg.DERPRegion, error) {
	var failures []error
	for _, source := range sources {
		fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		dm, err := tailcat.FetchDERPMap(fetchCtx, tailcat.DERPMapURL(source), tailcat.ExpandForServer)
		cancel()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		// 公共地图可能包含空值或仅供 STUN 使用的节点，过滤后才交给上游选择器。
		for key, region := range dm.Regions {
			if !usableDERPRegion(region) {
				delete(dm.Regions, key)
			}
		}
		ci := &tailcat.ConnInfo{RegionID: id}
		if err = ci.Expand(ctx, dm, tailcat.ExpandForServer); err == nil && len(ci.Region) > 0 && usableDERPRegion(ci.Region[0]) {
			return ci.Region[0], nil
		}
		if err == nil {
			err = fmt.Errorf("地图中没有可用 DERP 区域")
		}
		failures = append(failures, err)
	}
	return nil, fmt.Errorf("探测 DERP 区域失败（可配置 remote.derp-map-url 或 remote.region 自建中继）: %w", errors.Join(failures...))
}

// usableDERPRegion 验证缓存或外部地图中至少存在可连接的中继节点。
// 参数：region 为可空 *tailcfg.DERPRegion；返回 bool；无错误，空节点与纯 STUN 区域返回 false。
func usableDERPRegion(region *tailcfg.DERPRegion) bool {
	if region == nil || region.RegionID <= 0 {
		return false
	}
	for _, node := range region.Nodes {
		if node != nil && !node.STUNOnly && node.HostName != "" {
			return true
		}
	}
	return false
}

// loadDERPRegion 恢复同来源的粘性区域，跨重启避免 token 漂移和重复地图查询。
// 参数：source 为配置 URL；返回 *tailcfg.DERPRegion，缺失、损坏或来源不匹配返回 nil。
// 调用者持有 Manager 锁；只读本模块缓存，不读取或迁移用户身份文件。
func (m *Manager) loadDERPRegion(source string) *tailcfg.DERPRegion {
	data, err := os.ReadFile(filepath.Join(m.stateDir, "remote", "derp-region.json"))
	if err != nil {
		return nil
	}
	var saved derpRegionCache
	if json.Unmarshal(data, &saved) != nil || saved.Source != source || !usableDERPRegion(saved.Region) {
		return nil
	}
	return saved.Region
}

// saveDERPRegion 以同目录临时文件原子保存选定区域。
// 参数：source 为配置来源，region 为有效区域；返回 error，文件写入失败时上层记录日志但保留本次可用连接。
// Manager 锁串行化写入，rename 避免崩溃产生半份 JSON；临时文件始终删除。
func (m *Manager) saveDERPRegion(source string, region *tailcfg.DERPRegion) error {
	data, err := json.Marshal(derpRegionCache{Source: source, Region: region})
	if err != nil {
		return err
	}
	dir := filepath.Join(m.stateDir, "remote")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".derp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "derp-region.json"))
}
