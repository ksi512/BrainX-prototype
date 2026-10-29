package recall_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ksi512/BrainX-prototype/internal/recall"
	"github.com/ksi512/BrainX-prototype/internal/store"
)

// fakeEmbeddings 返回固定维度的伪向量：把每条文本哈希成一个三维方向，
// 只要同一文本得到同一向量、不同文本方向不同，就够验证「向量路能召回」。
func fakeEmbeddings(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for _, text := range payload.Input {
			var a, b, c float32 = 1, 0.2, 0.1
			if strings.Contains(text, "缓存") {
				a, b, c = 0.1, 1, 0.2
			}
			if strings.Contains(text, "权限") {
				a, b, c = 0.2, 0.1, 1
			}
			data = append(data, item{Embedding: []float32{a, b, c}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

func TestVectorChannelParticipates(t *testing.T) {
	server := fakeEmbeddings(t)
	defer server.Close()

	s := newStore(t)
	s.Config.VectorEnabled = true
	s.Config.VectorEndpoint = server.URL
	s.Config.VectorModel = "fake-embed"

	// 这条正文里**不含**「缓存失效」，只有向量能把它和查询拉近
	hot := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "失效时间应该短于五分钟"})
	mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "权限模型按最小授权设计"})

	results, err := recall.Search(s, "缓存", "", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, result := range results {
		if result.Asset.ID == hot.ID {
			found = true
			hasVector := false
			for _, source := range result.Sources {
				if source == "vector" {
					hasVector = true
				}
			}
			if !hasVector {
				t.Fatalf("这条应当由向量路带出，实际来源：%v", result.Sources)
			}
		}
	}
	if !found {
		t.Fatal("向量路没有把语义相关的那条带出来")
	}
}

// 端点挂掉时向量路必须静默降级，不能让整个召回失败。
func TestVectorFailureDegradesSilently(t *testing.T) {
	s := newStore(t)
	s.Config.VectorEnabled = true
	s.Config.VectorEndpoint = "http://127.0.0.1:1/embeddings"
	s.Config.VectorModel = "fake-embed"
	mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "端点挂了也要能按关键词搜到"})

	results, err := recall.Search(s, "关键词", "", "", 5)
	if err != nil {
		t.Fatalf("向量端点不可用不应让召回报错：%v", err)
	}
	if len(results) == 0 {
		t.Fatal("词法路仍应返回结果")
	}
}
