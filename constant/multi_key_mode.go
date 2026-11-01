package constant

type MultiKeyMode string

const (
	MultiKeyModeRandom     MultiKeyMode = "random"     // 随机
	MultiKeyModePolling    MultiKeyMode = "polling"    // 轮询
	MultiKeyModeSequential MultiKeyMode = "sequential" // 顺序：始终使用下标最小的可用key，被禁用后切换到下一个
)
