package core

// 代理域：配置产物拆分（生成与自检分离）的契约测试。

import (
	"bytes"
	"testing"

	C "github.com/metacubex/mihomo/constant"

	"proxyd/internal/proxy/node"
)

// TestBuildWithStateDeterministic 验证相同输入必然产生逐字节相同的配置。
//
// 功能说明：
// 应用层靠「生成字节与此前生效字节是否相等」来决定跳过 mihomo 热更新，因此生成结果
// 必须是输入的纯函数；yaml.v3 对 map 键排序提供了这个保证，本测试把它固定下来，避免
// 将来引入 map 遍历顺序参与列表构造后，稳态快路径静默失效（表现为每轮都重建核心）。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无；两次生成不一致或自检结果与生成结果不一致时失败。
//
// 错误情况：配置文件自检需要 home 目录，这里用临时目录隔离，不读取用户真实状态。
func TestBuildWithStateDeterministic(t *testing.T) {
	C.SetHomeDir(t.TempDir())

	cfg := fakeConfig()
	assigns := []Assignment{
		{Port: 42001, Node: fakeSocks5("甲", "10.0.0.1", 1080)},
		{Port: 42002, Node: fakeSocks5("乙", "10.0.0.2", 1080)},
	}
	nodes := []*node.Node{assigns[0].Node, assigns[1].Node}

	first, err := BuildWithState(cfg, assigns, nodes, []string{"DOMAIN,imported.example,DIRECT"}, map[string]string{"PROXY": "甲"})
	if err != nil {
		t.Fatalf("首次生成失败: %v", err)
	}
	for i := range 10 {
		again, err := BuildWithState(cfg, assigns, nodes, []string{"DOMAIN,imported.example,DIRECT"}, map[string]string{"PROXY": "甲"})
		if err != nil {
			t.Fatalf("第 %d 次生成失败: %v", i+2, err)
		}
		if !bytes.Equal(first.YAML(), again.YAML()) {
			t.Fatalf("第 %d 次生成的配置与首次不一致，稳态跳过逻辑会失效", i+2)
		}
	}

	// 无 GEO 规则时自检不应改写配置：生成字节即最终生效字节，两次比较才有意义。
	validated, err := first.Validate()
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	if !bytes.Equal(validated, first.YAML()) {
		t.Error("自检不应改写不含 GEO 规则的配置")
	}
}

// TestBuildWithStateSkipsValidation 验证 Generate 系列仍然自带自检。
//
// 功能说明：
// 拆分后 BuildWithState 只做翻译与序列化，Generate/GenerateWithState 必须继续返回经过
// mihomo 自检的结果，否则既有调用方会失去「先验证再热更新」的保护。这里用一份 mihomo
// 无法解析的配置区分两条路径：Build 成功，Validate 与 Generate 失败。
//
// 参数：
//   - t: *testing.T，Go 测试上下文。
//
// 返回值：无；任一分支行为不符合预期时失败。
//
// 错误情况：构造的非法配置若被 mihomo 接受，说明测试夹具失效，直接失败。
func TestBuildWithStateSkipsValidation(t *testing.T) {
	C.SetHomeDir(t.TempDir())

	cfg := fakeConfig()
	cfg.Mode = "definitely-not-a-mode"

	built, err := BuildWithState(cfg, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildWithState 不应执行 mihomo 自检，却失败了: %v", err)
	}
	if len(built.YAML()) == 0 {
		t.Fatal("BuildWithState 应返回序列化结果")
	}
	if _, err := built.Validate(); err == nil {
		t.Fatal("非法 mode 必须被自检拒绝")
	}
	if _, err := Generate(cfg, nil, nil); err == nil {
		t.Fatal("Generate 必须保留自检，非法 mode 应报错")
	}
}
