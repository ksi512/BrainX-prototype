package index

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Embedder 是唯一会调模型的组件（提示词 §3.4）。默认关闭，零配置即关闭。
//
// 它只做一件事：把文本换成向量。**不做重试风暴**——失败就是失败，
// 写路径记下错误、检索时该 chunk 只走 FTS5。
type Embedder struct {
	Endpoint string
	Model    string
	APIKey   string
	Client   *http.Client
}

// NewEmbedder 构造一个 OpenAI 兼容的 embeddings 客户端。
func NewEmbedder(endpoint, model, apiKey string) *Embedder {
	if endpoint != "" && !strings.Contains(endpoint, "/embeddings") {
		endpoint = strings.TrimSuffix(endpoint, "/") + "/embeddings"
	}
	return &Embedder{Endpoint: endpoint, Model: model, APIKey: apiKey, Client: &http.Client{Timeout: 30 * time.Second}}
}

// Enabled 判断是否真的可用：端点与模型缺一不可。
// 只填一个等于没配——静默降级成「有时算有时不算」比直接关掉更糟。
func (e *Embedder) Enabled() bool {
	return e != nil && e.Endpoint != "" && e.Model != ""
}

// ModelTag 是写进 chunks.embed_model 的标识：模型名 + 维度。
// 不同模型的向量空间不兼容，混用会让余弦距离失去意义且**不报错**（§3.4）。
func (e *Embedder) ModelTag(dim int) string {
	if dim <= 0 {
		return e.Model
	}
	return fmt.Sprintf("%s:%d", e.Model, dim)
}

// Embed 调用 /embeddings。批量传，减少往返。
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if !e.Enabled() {
		return nil, fmt.Errorf("向量未启用（需要 vector.endpoint 与 vector.model）")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	payload := map[string]any{"model": e.Model, "input": texts}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	response, err := e.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("embeddings 请求失败：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings 返回 HTTP %d", response.StatusCode)
	}
	var decoded struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("embeddings 响应无法解析：%w", err)
	}
	out := make([][]float32, 0, len(decoded.Data))
	for _, item := range decoded.Data {
		out = append(out, item.Embedding)
	}
	if len(out) != len(texts) {
		return nil, fmt.Errorf("embeddings 返回 %d 条，请求了 %d 条", len(out), len(texts))
	}
	return out, nil
}

// EncodeVector / DecodeVector 用 float32 小端序存 BLOB。
// 不用 JSON：同样的向量，JSON 体积约为二进制的一半精度却大 3 倍。
func EncodeVector(vector []float32) []byte {
	buffer := make([]byte, 4*len(vector))
	for i, value := range vector {
		binary.LittleEndian.PutUint32(buffer[i*4:], math.Float32bits(value))
	}
	return buffer
}

func DecodeVector(buffer []byte) []float32 {
	if len(buffer)%4 != 0 {
		return nil
	}
	out := make([]float32, len(buffer)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buffer[i*4:]))
	}
	return out
}

// Cosine 计算余弦相似度。向量已归一化时可以退化成点积，
// 但这里不假设上游做了归一化——不同服务端的默认行为不一致。
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
