package dream

import "testing"

// 这组数据是实测校准的结果，不是拍脑袋定的数字。
// 以后调 similarThreshold 必须先跑它——否则很容易把召回调回「只抓完全相同」。
func TestSimilarityThreshold(t *testing.T) {
	same := [][2]string{
		{"部署一律用 docker compose，数据目录 /vol2/1000/docker", "docker 部署请使用 compose，数据都放在 /vol2/1000/docker 目录下"},
		{"优先用本地模型，不用产品名作代称", "用本地模型，别写产品名"},
		{"向量检索默认关闭，需要显式配置端点", "向量检索默认是关闭的，要显式配端点才能用"},
	}
	different := [][2]string{
		{"MCP 协议栈笔记：sampling 已弃用", "用 MCP 接 ComfyUI 的两种方式"},
		{"结论先行，不铺垫，不奉承", "先给结论，不要客套"},
	}
	for _, p := range same {
		if got := jaccard(bigramSet(p[0]), bigramSet(p[1])); got < similarThreshold {
			t.Errorf("同义改写被漏掉：%.3f < %.2f  %q vs %q", got, similarThreshold, p[0], p[1])
		}
	}
	for _, p := range different {
		if got := jaccard(bigramSet(p[0]), bigramSet(p[1])); got >= similarThreshold {
			t.Errorf("无关内容被误判：%.3f >= %.2f  %q vs %q", got, similarThreshold, p[0], p[1])
		}
	}
}
