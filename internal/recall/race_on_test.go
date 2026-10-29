//go:build race

package recall_test

// raceEnabled 在 go test -race 时为真：race 插桩会把延迟放大 10–50 倍，
// 性能断言（P95 < 60ms）在这种构建下没有意义。
const raceEnabled = true
